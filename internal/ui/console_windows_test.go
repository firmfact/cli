//go:build windows

package ui

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// A file is no console: escape sequences cannot be switched on for it.
func TestEscapesNeedAConsole(t *testing.T) {
	t.Cleanup(restoreConsoles)
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if escapesWork(f) {
		t.Error("escapes work on a file")
	}
}

// A console gets virtual-terminal processing, and its own mode back when
// the CLI is done.
func TestConsoleGetsItsModeBack(t *testing.T) {
	t.Cleanup(restoreConsoles)
	f, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no console to test with: %v", err)
	}
	defer f.Close()
	h := windows.Handle(f.Fd())
	var before, during, after uint32
	if err := windows.GetConsoleMode(h, &before); err != nil {
		t.Skipf("no console to test with: %v", err)
	}
	if !escapesWork(f) {
		t.Skip("this console has no virtual-terminal processing")
	}
	if err := windows.GetConsoleMode(h, &during); err != nil || during&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING == 0 {
		t.Errorf("mode %#x (%v), want virtual-terminal processing on", during, err)
	}
	restoreConsoles()
	if err := windows.GetConsoleMode(h, &after); err != nil || after != before {
		t.Errorf("mode %#x (%v) afterwards, want %#x as before", after, err, before)
	}
}
