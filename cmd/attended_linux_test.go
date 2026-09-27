package cmd

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/firmfact/cli/internal/ui"
)

// terminal is a new pseudo-terminal of the given width for stdin and
// stdout, as a person at one has them; shown closes it and returns what it
// showed.
func terminal(t *testing.T, columns int) (tty *os.File, shown func() string) {
	t.Helper()
	master, tty := openPTY(t)
	if err := unix.IoctlSetWinsize(int(tty.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: uint16(columns)}); err != nil {
		t.Fatalf("size the terminal: %v", err)
	}
	read := make(chan string, 1)
	go func() {
		raw, err := io.ReadAll(master)
		if err != nil && !errors.Is(err, syscall.EIO) {
			t.Errorf("read terminal: %v", err)
		}
		read <- strings.ReplaceAll(string(raw), "\r\n", "\n")
	}()
	return tty, func() string {
		tty.Close()
		return <-read
	}
}

// runAttended is one invocation by a person at a terminal of the given
// width: stdin and stdout are the terminal. It returns what it showed.
func runAttended(t *testing.T, columns int, args ...string) string {
	t.Helper()
	tty, shown := terminal(t, columns)
	err := NewRootCommand(Build{Version: "test"}, args, IOStreams{In: tty, Out: tty, Err: io.Discard}).Execute()
	out := shown()
	if err != nil {
		t.Fatalf("%v: %v; terminal showed %q", args, err, out)
	}
	return out
}

// NO_COLOR takes the colour away, not the person: on a terminal the next
// step is still shown, in plain text. --json on the same terminal gets
// none.
func TestNoColorTerminalStillGetsTheNextStep(t *testing.T) {
	isolate(t)
	t.Setenv("TERM", "xterm-256color")
	srv := (&mcpServer{result: vendorsResult}).start(t)
	signedInWithTools(t, srv.URL, listVendors, analyzeCostTrends)
	const hint = "Next step: firmfact analyze cost-trends --entity-type vendor  (see the spend by vendor and how it moves)\n"

	t.Setenv("NO_COLOR", "1")
	shown := runAttended(t, 80, "--host", srv.URL, "vendors", "list")
	if !strings.HasSuffix(shown, "\n"+hint) || strings.Contains(shown, "\x1b") {
		t.Errorf("with NO_COLOR the terminal showed %q, want the hint in plain text", shown)
	}
	if shown := runAttended(t, 80, "--host", srv.URL, "--json", "vendors", "list"); strings.Contains(shown, "Next step") {
		t.Errorf("with --json the terminal showed %q", shown)
	}

	t.Setenv("NO_COLOR", "")
	shown = runAttended(t, 80, "--host", srv.URL, "vendors", "list")
	if !strings.Contains(shown, "Next step:") || !strings.Contains(shown, "\x1b[") {
		t.Errorf("in colour the terminal showed %q", shown)
	}
}

// The logo is for a person at a terminal wide enough for it: plain with
// NO_COLOR, none on a narrow terminal (nor its animation), none with
// --json or with stdin not a terminal.
func TestBannerOnATerminal(t *testing.T) {
	isolate(t)
	t.Setenv("TERM", "xterm-256color")
	cases := []struct {
		name    string
		columns int
		noColor bool
		json    bool
		piped   bool
		want    string // "" for no logo at all
	}{
		{name: "plain", columns: 80, noColor: true, want: "█▀▀ █ █▀█"},
		{name: "colour", columns: 80, want: "\x1b["},
		{name: "narrow", columns: 30},
		{name: "narrow plain", columns: 30, noColor: true},
		{name: "json", columns: 80, json: true},
		{name: "stdin piped", columns: 80, piped: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("NO_COLOR", "")
			if c.noColor {
				t.Setenv("NO_COLOR", "1")
			}
			tty, shown := terminal(t, c.columns)
			app := &App{Name: "firmfact", In: tty, Out: tty, Err: io.Discard, JSONOutput: c.json}
			if c.piped {
				app.In = strings.NewReader("")
			}
			up := app.Banner()
			// The rainbow takes a second; on a terminal that shows a logo,
			// a cancelled one skips it.
			ctx, cancel := context.WithCancel(context.Background())
			if c.want != "" {
				cancel()
			}
			app.Celebrate(ctx, up)
			cancel()
			out := shown()
			if c.want == "" {
				if up != 0 || out != "" {
					t.Errorf("Banner = %d, terminal showed %q; want nothing", up, out)
				}
				return
			}
			if up == 0 || !strings.Contains(out, c.want) || !strings.Contains(out, ui.Tagline) {
				t.Errorf("Banner = %d, terminal showed %q; want a logo with %q", up, out, c.want)
			}
			if c.noColor && strings.Contains(out, "\x1b") {
				t.Errorf("escape codes with NO_COLOR: %q", out)
			}
		})
	}
}

// With NO_COLOR a person still watches the setup on one line redrawn in
// place, without colour, and cut to fit a narrow terminal so that no
// redraw leaves a wrapped row behind. A terminal that knows no escapes
// (TERM=dumb) gets a line per change.
func TestSetupProgressOnANoColorTerminal(t *testing.T) {
	isolate(t)
	fastPolls(t)
	t.Setenv("NO_COLOR", "1")
	const activity = "Importing contracts, vendors and the allocations between them"
	// Each run's first check finds the setup under way, its second done.
	host, _ := statusServer(t, func(_ string, poll int) (int, string) {
		if poll%2 == 1 {
			return http.StatusOK, `{"percentage":40,"current_activity":"` + activity + `","liveness":"running"}`
		}
		return http.StatusOK, setupCompleteBody
	})

	t.Setenv("TERM", "xterm-256color")
	shown := runAttended(t, 30, "--host", host, "workspaces", "status", "--wait")
	draws := strings.Split(shown, "\r\x1b[K")
	if len(draws) != 3 {
		t.Fatalf("terminal showed %q, want two draws of the live line", shown)
	}
	for _, d := range draws[1:] {
		line, _, _ := strings.Cut(d, "\n")
		if n := ui.Columns(line); n > 29 {
			t.Errorf("a live line of %d columns on a 30-column terminal: %q", n, line)
		}
		if strings.Contains(line, "\x1b") {
			t.Errorf("escape codes in the live line with NO_COLOR: %q", line)
		}
	}
	if !strings.Contains(draws[1], "40%  Import") || !strings.HasSuffix(draws[1], "…") {
		t.Errorf("first draw = %q, want the activity cut to fit", draws[1])
	}
	if !strings.HasSuffix(shown, "100%  Ready\nDemo is ready.\n") {
		t.Errorf("terminal showed %q", shown)
	}

	t.Setenv("TERM", "dumb")
	shown = runAttended(t, 80, "--host", host, "workspaces", "status", "--wait")
	if strings.Contains(shown, "\x1b") || !strings.Contains(shown, "\n   40%  "+activity+"\n") {
		t.Errorf("on a dumb terminal it showed %q, want a line per change", shown)
	}
}
