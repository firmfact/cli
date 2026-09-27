//go:build windows

package ui

import (
	"os"
	"sync"

	"golang.org/x/sys/windows"
)

// A Windows console prints escape sequences as text ("←[38;5;208m") unless
// virtual-terminal processing is on for it. Windows Terminal turns it on
// for every program; the classic console host, behind cmd.exe and older
// PowerShell windows, leaves it to each program to ask. Windows older than
// Windows 10 version 1511 has no such mode, so there the CLI draws no
// colour at all.

var (
	consoleMu sync.Mutex
	// escapes records, per console handle, whether escape sequences work
	// there, so each handle is asked once.
	escapes = map[windows.Handle]bool{}
	// restores put back the console modes this process changed.
	restores []func()
)

// escapesWork turns on virtual-terminal processing for the console behind
// f, and reports whether escape sequences now work there.
func escapesWork(f *os.File) bool {
	h := windows.Handle(f.Fd())
	consoleMu.Lock()
	defer consoleMu.Unlock()
	if ok, asked := escapes[h]; asked {
		return ok
	}
	var mode uint32
	ok := windows.GetConsoleMode(h, &mode) == nil
	if ok && mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING == 0 {
		ok = windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) == nil
		if ok {
			restores = append(restores, func() { _ = windows.SetConsoleMode(h, mode) })
		}
	}
	escapes[h] = ok
	return ok
}

// restoreConsoles puts every console mode escapesWork changed back as it
// was, so the shell the CLI returns to finds its console unchanged.
func restoreConsoles() {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	for i := len(restores) - 1; i >= 0; i-- {
		restores[i]()
	}
	restores = nil
	clear(escapes)
}
