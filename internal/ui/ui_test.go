package ui

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Without colour (NO_COLOR, TERM=dumb, a pipe) nothing carries an escape
// code. The logo is drawn plain: whether to draw it at all, which scripts
// and pipes never get, is the caller's decision.
func TestNoColorIsPlain(t *testing.T) {
	var buf bytes.Buffer
	up := Banner(&buf, NoColor, 200)
	out := buf.String()
	if strings.Contains(out, "\x1b") || !strings.Contains(out, "▄███▄") || !strings.Contains(out, Tagline) {
		t.Errorf("plain banner = %q", out)
	}
	if up != len(bigDelta)+3 {
		t.Errorf("Banner = %d, want %d", up, len(bigDelta)+3)
	}
	if got := NoColor.Rainbow("hi there"); got != "hi there" {
		t.Errorf("Rainbow = %q", got)
	}
	if got := NoColor.Bar(0.5, 10); got != "[#####-----]" {
		t.Errorf("Bar = %q", got)
	}
}

func TestBannerPicksSizeByWidth(t *testing.T) {
	var wide, narrow bytes.Buffer
	Banner(&wide, TrueColor, 140)
	Banner(&narrow, TrueColor, 80)
	if strings.Count(wide.String(), "\n") <= strings.Count(narrow.String(), "\n") {
		t.Error("wide terminal should get the big banner")
	}
	for _, out := range []string{wide.String(), narrow.String()} {
		if !strings.Contains(out, "\x1b[38;2;255;140;0m") {
			t.Error("delta must be brand orange")
		}
		if !strings.Contains(out, Tagline) {
			t.Error("tagline missing")
		}
	}
}

// A terminal narrower than the small logo gets none, and no animation: its
// rows would wrap into noise.
func TestBannerNeedsTheWidth(t *testing.T) {
	sweepFrame = 0
	if w := smallArt.width(); w != 38 {
		t.Errorf("small logo is %d columns wide, want 38", w)
	}
	for _, width := range []int{30, 39} {
		var out bytes.Buffer
		if up := Banner(&out, TrueColor, width); up != 0 || out.Len() != 0 {
			t.Errorf("width %d: Banner = %d, printed %q; want nothing", width, up, out.String())
		}
		if AnimateBanner(context.Background(), &out, TrueColor, width, 50, 5) || out.Len() != 0 {
			t.Errorf("width %d: animated %q; want nothing", width, out.String())
		}
	}
	var out bytes.Buffer
	if up := Banner(&out, NoColor, 40); up != len(smallDelta)+3 {
		t.Errorf("width 40: Banner = %d, want the small logo", up)
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if n := Columns(line); n > 38 {
			t.Errorf("a line of %d columns at width 40: %q", n, line)
		}
	}
}

func TestColumnsLeavesOutEscapes(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"plain", 5},
		{"\x1b[38;2;255;140;0m▄▀▄\x1b[0m ok", 6},
		{TrueColor.Bar(0.5, 10), 10},
		{Color256.Bar(0.3, 10), 10},
		{NoColor.Bar(0.5, 10), 12},
		{"Zürich", 6},
	}
	for _, c := range cases {
		if got := Columns(c.in); got != c.want {
			t.Errorf("Columns(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestColourModesEncode(t *testing.T) {
	if got := Color256.Orange("x"); !strings.HasPrefix(got, "\x1b[38;5;") {
		t.Errorf("256: %q", got)
	}
	if got := Basic.Orange("x"); !strings.HasPrefix(got, "\x1b[33m") {
		t.Errorf("basic: %q", got)
	}
}

func TestAnimateBanner(t *testing.T) {
	sweepFrame = 0
	var plain bytes.Buffer
	if AnimateBanner(context.Background(), &plain, NoColor, 140, 50, 12) || plain.Len() != 0 {
		t.Error("no animation without colour")
	}

	var out bytes.Buffer
	below := Banner(&out, TrueColor, 140)
	if AnimateBanner(context.Background(), &out, TrueColor, 140, below-1, below) {
		t.Error("a logo taller than the terminal cannot be reached")
	}
	out.Reset()
	if !AnimateBanner(context.Background(), &out, TrueColor, 140, 50, below) {
		t.Fatal("expected an animation")
	}
	s := out.String()
	if !strings.HasPrefix(s, "\x1b[?25l") || !strings.HasSuffix(s, "\x1b[?25h") {
		t.Error("cursor must be hidden during and shown after")
	}
	if strings.Count(s, fmt.Sprintf("\x1b[%dA", below)) != sweepFrames+1 {
		t.Errorf("expected %d frames", sweepFrames+1)
	}
	lastFrame := s[strings.LastIndex(s, fmt.Sprintf("\x1b[%dA", below)):]
	if !strings.Contains(lastFrame, "\x1b[38;2;255;140;0m") || strings.Contains(lastFrame, "\x1b[38;2;255;0;0m") {
		t.Error("the last frame must be the brand colours, not the rainbow")
	}
}

// Ctrl-C during the rainbow skips to the last frame: the logo is left in its
// brand colours and the cursor is shown again.
func TestAnimateBannerStopsWhenCancelled(t *testing.T) {
	sweepFrame = time.Hour // only a cancel can end the wait
	defer func() { sweepFrame = 0 }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var out bytes.Buffer
	below := Banner(&out, TrueColor, 140)
	out.Reset()
	if !AnimateBanner(ctx, &out, TrueColor, 140, 50, below) {
		t.Fatal("expected an animation")
	}
	s := out.String()
	up := fmt.Sprintf("\x1b[%dA", below)
	if n := strings.Count(s, up); n != 2 {
		t.Errorf("drew %d frames, want the first and the last", n)
	}
	lastFrame := s[strings.LastIndex(s, up):]
	if !strings.Contains(lastFrame, "\x1b[38;2;255;140;0m") || strings.Contains(lastFrame, "\x1b[38;2;255;0;0m") {
		t.Error("the last frame must be the brand colours, not the rainbow")
	}
	if !strings.HasSuffix(s, "\x1b[?25h") {
		t.Error("the cursor must be shown again")
	}
}

func TestRowCounterCountsWrappedLines(t *testing.T) {
	var sink bytes.Buffer
	c := &RowCounter{W: &sink, Width: 10}
	fmt.Fprint(c, "short\n")                    // 1 row
	fmt.Fprint(c, strings.Repeat("x", 25)+"\n") // 25 chars at width 10: 3 rows
	fmt.Fprint(c, "\n")                         // 1 row
	if c.Rows() != 5 {
		t.Errorf("rows = %d, want 5", c.Rows())
	}
	if !strings.HasPrefix(sink.String(), "short\n") {
		t.Error("output must pass through unchanged")
	}
}
