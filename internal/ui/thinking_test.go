package ui

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// The line counts up once a tick and is blanked out again when the answer
// comes, with spaces, so the next output starts on a clean line.
func TestThinkingCountsAndClears(t *testing.T) {
	var buf lockedBuffer
	stop := thinking(context.Background(), &buf, 10*time.Millisecond)
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(buf.String(), "2s"); {
		if time.Now().After(deadline) {
			t.Fatalf("no second count; output = %q", buf.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	stop() // a second stop is harmless

	out := buf.String()
	if !strings.HasPrefix(out, "\rThinking... 1s\rThinking... 2s") {
		t.Errorf("output = %q, want a count from 1s", out)
	}
	last := strings.LastIndex(out, "\rThinking... ")
	line := strings.TrimPrefix(out[last:], "\r")
	line = line[:strings.Index(line, "\r")]
	if want := "\r" + strings.Repeat(" ", len(line)) + "\r"; !strings.HasSuffix(out, want) {
		t.Errorf("output = %q, want it to end by blanking %q", out, line)
	}
}

// An answer within the first tick draws nothing at all.
func TestThinkingQuickAnswerDrawsNothing(t *testing.T) {
	var buf bytes.Buffer
	stop := thinking(context.Background(), &buf, time.Hour)
	stop()
	if buf.Len() != 0 {
		t.Errorf("output = %q, want none", buf.String())
	}
}

// lockedBuffer can be read while the ticker writes to it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// After Ctrl-C the line is neither redrawn over the echoed ^C nor blanked.
func TestThinkingAfterInterrupt(t *testing.T) {
	var buf lockedBuffer
	ctx, cancel := context.WithCancel(context.Background())
	stop := thinking(ctx, &buf, 10*time.Millisecond)
	time.Sleep(25 * time.Millisecond)
	cancel()
	time.Sleep(15 * time.Millisecond) // a draw under way when cancel came
	drawn := buf.String()
	time.Sleep(30 * time.Millisecond)
	stop()
	if out := buf.String(); out != drawn || !strings.HasSuffix(out, "s") {
		t.Errorf("output = %q, want it to stay at %q", out, drawn)
	}
}

// Anything but a terminal gets no ticker.
func TestThinkingNeedsATerminal(t *testing.T) {
	var buf bytes.Buffer
	stop := Thinking(context.Background(), &buf)
	stop()
	if buf.Len() != 0 {
		t.Errorf("output = %q", buf.String())
	}
}

// Where escape sequences do not work (TERM=dumb), an interjected line
// starts over the thinking count with a carriage return and is wide enough
// to cover it; the count goes on below.
func TestInterjectCoversTheCount(t *testing.T) {
	t.Setenv("TERM", "dumb")
	var buf bytes.Buffer
	Interject(&buf, "Busy")
	if want := "\rBusy" + strings.Repeat(" ", countWidth-len("Busy")) + "\n"; buf.String() != want {
		t.Errorf("output = %q, want %q", buf.String(), want)
	}
	if count := "Thinking... 300000s"; len(count) > countWidth {
		t.Errorf("a count of %q is wider than the %d columns an interjection covers", count, countWidth)
	}
	buf.Reset()
	long := strings.Repeat("x", countWidth+5)
	Interject(&buf, long)
	if buf.String() != "\r"+long+"\n" {
		t.Errorf("output = %q, want the long line as it is", buf.String())
	}
}
