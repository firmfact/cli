package ui

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Thinking shows "Thinking... 4s" on w while the caller waits for an
// answer, from the first second on and counting up, so a chat that takes a
// minute does not look like a hang. Only a terminal gets it: in a log or a
// pipe, a line redrawn in place is noise.
//
// The returned stop takes the line away again; after an interrupt (ctx
// done) it ends the line instead, which leaves the ^C the terminal echoed
// on it. Nothing else may write to w until stop has returned, but for a
// line of its own through Interject.
func Thinking(ctx context.Context, w io.Writer) (stop func()) {
	if !IsTerminal(w) {
		return func() {}
	}
	return thinking(ctx, w, time.Second)
}

// thinking is Thinking on any writer, redrawn every tick (tests pass a
// short one).
func thinking(ctx context.Context, w io.Writer, tick time.Duration) func() {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(tick)
		defer t.Stop()
		width := 0
		for n := 1; ; n++ {
			select {
			case <-t.C:
				if ctx.Err() != nil {
					continue // drawing now would cover the ^C
				}
				// The count only grows, so each line covers the one before.
				line := fmt.Sprintf("Thinking... %ds", n)
				fmt.Fprint(w, "\r"+line)
				width = len(line)
			case <-done:
				switch {
				case width == 0:
					// Nothing was drawn.
				case ctx.Err() != nil:
					EndInterruptedLine(w)
				default:
					// Spaces rather than an erase sequence, so a terminal
					// that knows no escapes (TERM=dumb) is left clean too.
					fmt.Fprint(w, "\r"+strings.Repeat(" ", width)+"\r")
				}
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			<-finished
		})
	}
}

// Interject writes line on a line of its own on w, a terminal where a live
// line may be under way (the thinking count, a setup progress bar). The
// live line gives way to it and is drawn again below at its next update.
// Where escape sequences work (see Escapes; NO_COLOR or not) an erase
// sequence takes the live line away. Where they do not (TERM=dumb), the
// only one drawn is the thinking count, and a carriage return and a line
// at least as wide as any count do it.
func Interject(w io.Writer, line string) {
	if Escapes(w) {
		fmt.Fprint(w, "\r\x1b[K"+line+"\n")
		return
	}
	fmt.Fprintf(w, "\r%-*s\n", countWidth, line)
}

// countWidth is wider than the thinking count gets in any wait the CLI
// allows.
const countWidth = len("Thinking... 9999999s")
