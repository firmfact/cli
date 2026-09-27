// Package ui is the CLI's look: the block-letter banner with the delta mark,
// brand orange, and a rainbow gradient for the moments worth celebrating.
//
// Colour follows the terminal: truecolor when COLORTERM says so, 256 colours
// on 256color terminals, basic ANSI otherwise, and none at all when NO_COLOR
// is set or the output is not a terminal (scripts and --json stay plain).
package ui

import (
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

type ColorMode int

const (
	NoColor ColorMode = iota
	Basic
	Color256
	TrueColor
)

// Brand orange (#FF8C00). The delta is always drawn in it; the brand
// guidelines say never to recolour the mark.
var orange = rgb{255, 140, 0}

type rgb struct{ r, g, b uint8 }

// Detect picks the colour mode for w. It says whether to colour and
// nothing more: a person at a terminal with NO_COLOR set still gets the
// logo, the hints and the live progress line, in plain text. Whether a
// person is there to see them is the caller's question.
func Detect(w io.Writer) ColorMode {
	if os.Getenv("NO_COLOR") != "" || !Escapes(w) {
		return NoColor
	}
	switch strings.ToLower(os.Getenv("COLORTERM")) {
	case "truecolor", "24bit":
		return TrueColor
	}
	if strings.Contains(os.Getenv("TERM"), "256color") {
		return Color256
	}
	return Basic
}

// Escapes reports whether w is a terminal that acts on escape sequences,
// the ones that move the cursor and erase a line as well as colours: a
// live line is redrawn with them. NO_COLOR has no say in this. TERM=dumb
// does, and so does a Windows console that cannot be switched to escape
// sequences, which would show them as text.
func Escapes(w io.Writer) bool {
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd())) && escapesWork(f)
}

// PrepareConsole readies the terminals behind files for colour before
// anything is printed: a Windows console must be asked to understand escape
// sequences. It returns a func that puts the consoles back as they were;
// elsewhere both do nothing.
func PrepareConsole(files ...*os.File) (restore func()) {
	for _, f := range files {
		// A Windows process started without a console can have no
		// standard output at all.
		if f != nil {
			escapesWork(f)
		}
	}
	return restoreConsoles
}

// Height is the terminal height of w, or 24 when unknown.
func Height(w io.Writer) int {
	if f, ok := w.(*os.File); ok {
		if _, rows, err := term.GetSize(int(f.Fd())); err == nil && rows > 0 {
			return rows
		}
	}
	return 24
}

// RowCounter passes output through and counts the terminal rows it takes,
// including lines that wrap, so the caller knows how far below an earlier
// line the cursor is. Only plain text is expected (no escape sequences).
type RowCounter struct {
	W     io.Writer
	Width int
	rows  int
	col   int
}

func (c *RowCounter) Write(p []byte) (int, error) {
	for _, r := range string(p) {
		switch r {
		case '\n':
			c.rows++
			c.col = 0
		case '\r':
			c.col = 0
		default:
			if c.Width > 0 && c.col >= c.Width {
				c.rows++
				c.col = 0
			}
			c.col++
		}
	}
	return c.W.Write(p)
}

// Rows is how many rows the output moved the cursor down. Output that does
// not end in a newline leaves the cursor on its last row, which is not
// counted.
func (c *RowCounter) Rows() int { return c.rows }

// Columns is how many columns s takes on a terminal: its characters, less
// the escape sequences (CSI, as colours are) in it. A wide character counts
// as one, as in the tables.
func Columns(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			// Parameters and intermediates, up to a final byte in @ to ~.
			i += 2
			for i < len(s) && (s[i] < 0x40 || s[i] > 0x7e) {
				i++
			}
			i++
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		n++
		i += size
	}
	return n
}

// Width is the terminal width of w, or 80 when unknown.
func Width(w io.Writer) int {
	if f, ok := w.(*os.File); ok {
		if cols, _, err := term.GetSize(int(f.Fd())); err == nil && cols > 0 {
			return cols
		}
	}
	return 80
}

func (m ColorMode) fg(c rgb) string {
	switch m {
	case TrueColor:
		return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", c.r, c.g, c.b)
	case Color256:
		return fmt.Sprintf("\x1b[38;5;%dm", to256(c))
	case Basic:
		return fmt.Sprintf("\x1b[%dm", toBasic(c))
	}
	return ""
}

func (m ColorMode) reset() string {
	if m == NoColor {
		return ""
	}
	return "\x1b[0m"
}

func (m ColorMode) bold(s string) string {
	if m == NoColor {
		return s
	}
	return "\x1b[1m" + s + "\x1b[0m"
}

// Dim renders s faint.
func (m ColorMode) Dim(s string) string { return m.dim(s) }

func (m ColorMode) dim(s string) string {
	if m == NoColor {
		return s
	}
	return "\x1b[2m" + s + "\x1b[0m"
}

// Orange paints s in brand orange.
func (m ColorMode) Orange(s string) string {
	if m == NoColor {
		return s
	}
	return m.fg(orange) + s + m.reset()
}

// Rainbow paints s with a hue sweep across its characters.
func (m ColorMode) Rainbow(s string) string {
	if m == NoColor {
		return s
	}
	runes := []rune(s)
	var b strings.Builder
	for i, r := range runes {
		if r == ' ' {
			b.WriteRune(r)
			continue
		}
		b.WriteString(m.fg(hue(float64(i) / math.Max(1, float64(len(runes)-1)))))
		b.WriteRune(r)
	}
	b.WriteString(m.reset())
	return b.String()
}

// Bar is a progress bar of the given width, its filled part in a rainbow.
func (m ColorMode) Bar(fraction float64, width int) string {
	fraction = math.Max(0, math.Min(1, fraction))
	filled := int(math.Round(fraction * float64(width)))
	if m == NoColor {
		return "[" + strings.Repeat("#", filled) + strings.Repeat("-", width-filled) + "]"
	}
	var b strings.Builder
	for i := 0; i < filled; i++ {
		b.WriteString(m.fg(hue(float64(i) / float64(width))))
		b.WriteString("█")
	}
	b.WriteString(m.reset())
	b.WriteString(m.dim(strings.Repeat("░", width-filled)))
	return b.String()
}

// hue maps 0..1 to a rainbow colour (red through violet).
func hue(t float64) rgb {
	h := t * 300 // stop at violet rather than wrapping back to red
	c := 1.0
	x := c * (1 - math.Abs(math.Mod(h/60, 2)-1))
	var r, g, b float64
	switch {
	case h < 60:
		r, g = c, x
	case h < 120:
		r, g = x, c
	case h < 180:
		g, b = c, x
	case h < 240:
		g, b = x, c
	default:
		r, b = x, c
	}
	return rgb{uint8(r * 255), uint8(g * 255), uint8(b * 255)}
}

func to256(c rgb) int {
	q := func(v uint8) int { return int(math.Round(float64(v) / 255 * 5)) }
	return 16 + 36*q(c.r) + 6*q(c.g) + q(c.b)
}

func toBasic(c rgb) int {
	code := 30
	if c.r > 127 {
		code += 1
	}
	if c.g > 127 {
		code += 2
	}
	if c.b > 127 {
		code += 4
	}
	return code
}
