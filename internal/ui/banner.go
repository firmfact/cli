package ui

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// Tagline under the banner. Chosen for the CLI on 2026-09-26; the website
// still carries "Software that already knows your next step." until the
// brand line is changed everywhere.
const Tagline = "Your firm's single source of fact."

// The delta mark and the wordmark, drawn in the same heavy block style as
// the Omarchy logo. The delta is kept separate so it can be painted orange.
var bigDelta = []string{
	"                 ",
	"      ▄███▄      ",
	"     ▄██▀██▄     ",
	"    ▄██▀ ▀██▄    ",
	"   ▄██▀   ▀██▄   ",
	"  ▄██▀     ▀██▄  ",
	" ▄██▀       ▀██▄ ",
	"▄██▄▄▄▄▄▄▄▄▄▄▄██▄",
	"▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀",
	"                 ",
}

var bigWord = []string{
	"                                   ▄▄▄",
	"   ▄████████  ▄█    ▄███████  ▄███████████▄     ▄████████   ▄███████  ▄███████    ███",
	"  ███    ███ ███   ███   ███ ███   ███   ███   ███    ███  ███   ███ ███   ███ ▀█████████▄",
	"  ███    █▀  ███▌  ███   ███ ███   ███   ███   ███    █▀   ███   ███ ███   █▀     ▀███▀▀██",
	" ▄███▄▄▄     ███▌ ▄███▄▄▄██▀ ███   ███   ███  ▄███▄▄▄     ▄███▄▄▄███ ███           ███   ▀",
	"▀▀███▀▀▀     ███▌ ▀███▀▀▀▀   ███   ███   ███ ▀▀███▀▀▀     ▀███▀▀▀███ ███           ███",
	"  ███        ███  ██████████ ███   ███   ███   ███         ███   ███ ███   █▄      ███",
	"  ███        ███   ███   ███ ███   ███   ███   ███         ███   ███ ███   ███     ███",
	"  ███        █▀    ███   ███  ▀█   ███   █▀    ███         ███   █▀  ███████▀     ▄████▀",
	"                   ███   █▀",
}

// For terminals narrower than the big banner.
var smallDelta = []string{" ▄▀▄ ", "▀▀▀▀▀"}
var smallWord = []string{
	"█▀▀ █ █▀█ █▀▄▀█ █▀▀ ▄▀█ █▀▀ ▀█▀",
	"█▀  █ █▀▄ █ ▀ █ █▀  █▀█ █▄▄  █ ",
}

type art struct {
	delta, word []string
	gap         string
}

var (
	bigArt   = art{bigDelta, bigWord, "  "}
	smallArt = art{smallDelta, smallWord, "  "}
)

// width is how many columns the widest logo row takes (109 for the big
// logo, 38 for the small one).
func (a art) width() int {
	n := 0
	for i := range a.delta {
		n = max(n, len([]rune(strings.TrimRight(a.delta[i]+a.gap+a.word[i], " "))))
	}
	return n
}

// fits reports whether the logo fits a terminal width columns wide, with
// room to spare: a row as wide as the terminal wraps on some.
func (a art) fits(width int) bool { return width >= a.width()+2 }

// artFor is the logo for a terminal width columns wide: the big one where
// it fits, else the small one. ok is false when not even that fits, as on
// a phone's terminal, where every row would wrap into noise.
func artFor(width int) (a art, ok bool) {
	if bigArt.fits(width) {
		return bigArt, true
	}
	return smallArt, smallArt.fits(width)
}

// Banner prints the logo and tagline for a terminal of the given width
// (see Width), coloured as m says: NoColor draws it plain. It prints
// nothing on a terminal too narrow for the small logo. Whether to show a
// logo at all is the caller's to decide: a person at a terminal gets one,
// a script or a pipe does not. It returns how many lines the cursor now
// sits below the first logo row, for AnimateBanner, or 0 when it printed
// nothing.
func Banner(w io.Writer, m ColorMode, width int) int {
	a, ok := artFor(width)
	if !ok {
		return 0
	}
	fmt.Fprintln(w)
	for i := range a.delta {
		fmt.Fprintln(w, brandRow(m, a, i))
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, m.dim(Tagline))
	fmt.Fprintln(w)
	return len(a.delta) + 3
}

// brandRow is a logo row at rest: the delta in brand orange, the wordmark
// in the terminal's own colour.
func brandRow(m ColorMode, a art, i int) string {
	return strings.TrimRight(m.Orange(a.delta[i])+a.gap+m.bold(a.word[i]), " ")
}

const sweepFrames = 28

// sweepFrame is the delay between frames (about 1.1 s in total); a variable
// so tests need not wait.
var sweepFrame = 40 * time.Millisecond

// AnimateBanner runs a rainbow once across the logo that sits `up` lines
// above the cursor, then leaves it in its brand colours, so the mark is only
// recoloured while it moves. It returns false, and draws nothing, when the
// logo has scrolled out of reach (up beyond the terminal height), the
// terminal is too narrow for one (see Banner) or colour is off: the sweep
// is all colour. Cancelling ctx (Ctrl-C) skips to the last frame.
func AnimateBanner(ctx context.Context, w io.Writer, m ColorMode, width, height, up int) bool {
	a, ok := artFor(width)
	rows := len(a.delta)
	if !ok || m == NoColor || up < rows || up >= height {
		return false
	}
	lineWidth := a.width()
	fmt.Fprint(w, "\x1b[?25l") // hide the cursor while drawing
	defer fmt.Fprint(w, "\x1b[?25h")
	for f := 0; f <= sweepFrames; f++ {
		var b strings.Builder
		fmt.Fprintf(&b, "\x1b[%dA", up)
		for i := 0; i < rows; i++ {
			b.WriteString("\r\x1b[2K")
			if f == sweepFrames {
				b.WriteString(brandRow(m, a, i))
			} else {
				b.WriteString(sweepRow(m, a, i, f, lineWidth))
			}
			b.WriteString("\n")
		}
		if rest := up - rows; rest > 0 {
			fmt.Fprintf(&b, "\x1b[%dB", rest)
		}
		fmt.Fprint(w, b.String())
		// Interrupted, the logo is still put back in its brand colours:
		// one frame, where stopping here would leave it half a rainbow.
		if f < sweepFrames && !Pause(ctx, sweepFrame) {
			f = sweepFrames - 1
		}
	}
	return true
}

// Pause waits for d between the frames of something drawn over time, and
// reports whether it did; false means ctx was cancelled (Ctrl-C) first.
func Pause(ctx context.Context, d time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// sweepRow colours one logo row for frame f: a rainbow band that travels
// from left to right, slanted a little per row.
func sweepRow(m ColorMode, a art, row, f, lineWidth int) string {
	line := []rune(strings.TrimRight(a.delta[row]+a.gap+a.word[row], " "))
	phase := float64(f) / float64(sweepFrames) * 2.2
	var b strings.Builder
	for c, r := range line {
		if r == ' ' {
			b.WriteRune(r)
			continue
		}
		t := float64(c+row*2)/float64(lineWidth) - phase + 1
		t -= float64(int(t))
		if t < 0 {
			t++
		}
		b.WriteString(m.fg(hue(t)))
		b.WriteRune(r)
	}
	b.WriteString(m.reset())
	return b.String()
}
