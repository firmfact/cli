package cmd

import (
	"bytes"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/firmfact/cli/internal/config"
)

// typist is a person at a terminal: it waits for each prompt to show
// before it types the answer, as they would.
type typist struct {
	t      *testing.T
	master *os.File
	mu     sync.Mutex
	shown  strings.Builder
	seen   int           // how much of shown the prompts so far have used
	more   chan struct{} // a nudge that more was shown
	closed chan struct{} // the terminal is gone
}

// newTypist opens a terminal 80 columns wide and returns the typist at it
// and the terminal side, for the command's stdin and stdout.
func newTypist(t *testing.T) (*typist, *os.File) {
	t.Helper()
	master, tty := openPTY(t)
	if err := unix.IoctlSetWinsize(int(tty.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: 80}); err != nil {
		t.Fatalf("size the terminal: %v", err)
	}
	ty := &typist{t: t, master: master, more: make(chan struct{}, 1), closed: make(chan struct{})}
	go func() {
		defer close(ty.closed)
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				ty.mu.Lock()
				ty.shown.Write(buf[:n])
				ty.mu.Unlock()
				select {
				case ty.more <- struct{}{}:
				default:
				}
			}
			if err != nil {
				return // EIO once the terminal side is closed
			}
		}
	}()
	return ty, tty
}

// answer waits for prompt to show after the last one, then types line and
// Enter.
func (ty *typist) answer(prompt, line string) {
	ty.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		ty.mu.Lock()
		shown := ty.shown.String()
		i := strings.Index(shown[ty.seen:], prompt)
		if i >= 0 {
			ty.seen += i + len(prompt)
		}
		ty.mu.Unlock()
		if i >= 0 {
			break
		}
		select {
		case <-ty.more:
		case <-deadline:
			ty.t.Fatalf("waited for %q; the terminal showed:\n%s", prompt, shown)
		}
	}
	// Enter sends a carriage return, which the terminal hands on as a
	// newline.
	if _, err := ty.master.Write([]byte(line + "\r")); err != nil {
		ty.t.Fatalf("type %q: %v", line, err)
	}
}

// A person signing up at a terminal is asked for every detail left out:
// defaults taken with Enter, the organisation type by number, the password
// twice, the documents accepted, and a mistyped code typed again.
func TestSignupAtATerminal(t *testing.T) {
	isolate(t)
	t.Setenv("TERM", "dumb")
	t.Setenv("NO_COLOR", "1")
	f := &signupServer{wrongCodes: 1}
	srv := f.start(t)

	ty, tty := newTypist(t)
	var errOut bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- NewRootCommand(Build{Version: "test"}, []string{"--host", srv.URL, "signup", "--no-wait"},
			IOStreams{In: tty, Out: tty, Err: &errOut}).Execute()
	}()

	ty.answer("Work email: ", "jan@your-firm.example")
	ty.answer("Your name: ", "Jan de Vries")
	ty.answer("Organisation (legal name) [Your Firm]: ", "")
	ty.answer("Choose [1]: ", "")
	ty.answer("Base currency [EUR]: ", "usd")
	ty.answer("Interface language [en]: ", "")
	ty.answer("Password (min. 12 characters, upper and lower case, a digit): ", "Correct-Horse-9")
	ty.answer("Repeat password: ", "Correct-Horse-9")
	ty.answer("Do you accept these documents? (y/N): ", "y")
	ty.answer("6-digit code from the email: ", "000000")
	ty.answer("6-digit code from the email: ", "123456")

	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("signup did not finish")
	}
	tty.Close()
	<-ty.closed
	if err != nil {
		t.Fatalf("signup: %v\nterminal:\n%s", err, ty.shown.String())
	}

	user := f.signupBody["user"]
	want := map[string]any{
		"email": "jan@your-firm.example", "name": "Jan de Vries", "legal_entity_name": "Your Firm",
		"organization_type": "financial_services", "base_currency": "USD", "preferred_language": "en",
		"password": "Correct-Horse-9", "accept_terms": true,
	}
	if mustJSON(t, user) != mustJSON(t, want) {
		t.Errorf("signup sent %s, want %s", mustJSON(t, user), mustJSON(t, want))
	}
	if n := f.confirms.Load(); n != 2 || f.confirmBody["user"]["confirmation_code"] != "123456" {
		t.Errorf("%d confirmations, the last %v", n, f.confirmBody)
	}
	if !strings.Contains(errOut.String(), "That code did not work; check the email and try again.") {
		t.Errorf("stderr = %q", errOut.String())
	}
	if shown := ty.shown.String(); !strings.Contains(shown, "  1) Financial services") || !strings.Contains(shown, `You are signed in, with "Demo" as your default workspace.`) {
		t.Errorf("terminal showed:\n%s", ty.shown.String())
	}
	if tok, err := config.LoadToken(srv.URL); err != nil || tok == nil || tok.AccessToken != "at" {
		t.Errorf("stored %+v, %v", tok, err)
	}
}

// Passwords that differ, or documents not accepted, end the signup at a
// terminal before anything is sent.
func TestSignupAtATerminalStopsBeforeSending(t *testing.T) {
	cases := map[string]struct {
		password, again, accept string
		want                    string
	}{
		"passwords differ":       {"Correct-Horse-9", "Correct-Horse-8", "", "the passwords do not match"},
		"documents not accepted": {"", "", "n", "signup cancelled: the documents were not accepted"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			t.Setenv("TERM", "dumb")
			f := &signupServer{}
			srv := f.start(t)
			ty, tty := newTypist(t)
			done := make(chan error, 1)
			go func() {
				done <- NewRootCommand(Build{Version: "test"}, []string{"--host", srv.URL, "signup",
					"--email", "jan@yourfirm.example", "--name", "Jan", "--org", "Bank BV", "--type", "financial_services",
					"--currency", "EUR", "--language", "en"},
					IOStreams{In: tty, Out: tty, Err: &bytes.Buffer{}}).Execute()
			}()
			ty.answer("Password (min. 12 characters, upper and lower case, a digit): ", c.password)
			if c.password != "" {
				ty.answer("Repeat password: ", c.again)
			} else {
				ty.answer("Do you accept these documents? (y/N): ", c.accept)
			}
			var err error
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("signup did not finish")
			}
			tty.Close()
			<-ty.closed
			if err == nil || err.Error() != c.want {
				t.Errorf("got %v, want %q", err, c.want)
			}
			if f.signups.Load() != 0 {
				t.Error("the signup was sent")
			}
		})
	}
}
