package ui

import (
	"fmt"
	"io"
	"os"
	"sync/atomic"

	"golang.org/x/term"
)

// Ctrl-C makes the terminal echo ^C where the cursor is, and the shell prompt
// that follows the CLI would start on that same line: bash, for one, only
// adds a newline for a program the signal killed, and the CLI now exits by
// itself after tidying up. The code that had the cursor mid-line when it was
// interrupted (a prompt, the live setup progress line) ends that line itself,
// usually to say something after it; RestoreTerminal ends it otherwise.
var lineEnded atomic.Bool

// EndInterruptedLine ends the line an interrupted prompt or live progress
// line left the cursor on. It writes nothing when w is not a terminal.
func EndInterruptedLine(w io.Writer) {
	if IsTerminal(w) {
		fmt.Fprintln(w)
		lineEnded.Store(true)
	}
}

// RestoreTerminal tidies up after an interrupted command, on the first of ws
// that is a terminal: it shows the cursor (hidden while the logo animates)
// and ends the line the ^C was echoed on, unless EndInterruptedLine already
// did. Scripts, with no terminal, get nothing but the exit status.
func RestoreTerminal(ws ...io.Writer) {
	for _, w := range ws {
		if !IsTerminal(w) {
			continue
		}
		if Detect(w) != NoColor {
			fmt.Fprint(w, "\x1b[?25h")
		}
		if !lineEnded.Swap(false) {
			fmt.Fprintln(w)
		}
		return
	}
}

// IsTerminal reports whether w is a terminal, where a person is reading.
func IsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}
