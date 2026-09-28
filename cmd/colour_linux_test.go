package cmd

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runInColour is one invocation with stdout on a terminal that shows
// colour and stdin not a terminal: the command colours its results, while
// what is only for a person at the keyboard (the logo, the live line, the
// next steps) stays out of it. NO_COLOR is the caller's. It returns what
// the terminal showed.
func runInColour(t *testing.T, version string, args ...string) (shown, stderr string, err error) {
	t.Helper()
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("COLORTERM", "")
	tty, show := terminal(t, 120)
	var errOut bytes.Buffer
	err = NewRootCommand(Build{Version: version}, args, IOStreams{In: strings.NewReader(""), Out: tty, Err: &errOut}).Execute()
	return show(), errOut.String(), err
}

// assertNoRainbow fails when shown holds a colour other than the basic
// ones: results never carry the logo's rainbow, nor any hue of its own.
func assertNoRainbow(t *testing.T, where, shown string) {
	t.Helper()
	if strings.Contains(shown, "\x1b[38;") {
		t.Errorf("%s: a 256-colour or truecolor code in %q", where, shown)
	}
}

// Signing up, a workspace that is ready and a name claimed say so in
// green.
func TestSuccessLinesAreGreen(t *testing.T) {
	isolate(t)
	fastPolls(t)
	t.Setenv("NO_COLOR", "")

	f := &signupServer{progress: func(poll int) string {
		if poll < 2 {
			return setupRunningBody
		}
		return `{"percentage":100,"liveness":"complete"}`
	}}
	srv := f.start(t)
	shown, _, err := runInColour(t, "test", signupArgs(srv.URL)...)
	if err != nil {
		t.Fatalf("signup: %v\n%s", err, shown)
	}
	for _, want := range []string{
		painted(green, "Welcome to firmfact.") + ` You are signed in, with "Demo" as your default workspace.`,
		painted(green, "Your Demo workspace is ready.") + " Open ",
	} {
		if !strings.Contains(shown, want) {
			t.Errorf("signup: no %q in %q", want, shown)
		}
	}
	assertNoRainbow(t, "signup", shown)

	host, _ := statusServer(t, func(string, int) (int, string) { return http.StatusOK, setupCompleteBody })
	shown, _, err = runInColour(t, "test", "--host", host, "workspaces", "status", "--wait")
	if err != nil || !strings.HasSuffix(shown, "\n"+painted(green, "Demo is ready.")+"\n") {
		t.Errorf("workspaces status --wait: %v, %q", err, shown)
	}
}

// claim says the name is the user's in green.
func TestClaimSaysSoInGreen(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash in /usr/bin or /bin")
	}
	isolate(t)
	t.Setenv("NO_COLOR", "")
	t.Setenv("FIRMFACT_NO_UPDATE_CHECK", "1")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZDOTDIR", "")
	rc := filepath.Join(os.Getenv("HOME"), ".bashrc")
	if err := os.WriteFile(rc, []byte("export KEEP=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	shown, _, err := runInColour(t, "0.1.0", "claim", "ff", "--yes", "--shell", "bash")
	if err != nil {
		t.Fatalf("claim: %v\n%s", err, shown)
	}
	if want := painted(green, "ff is yours.") + " Open a new terminal"; !strings.Contains(shown, want) {
		t.Errorf("no %q in %q", want, shown)
	}
}

// doctor marks a check that passed with a green ok and one that failed
// with a red FAIL, in a column of their own either way.
func TestDoctorMarksInColour(t *testing.T) {
	isolate(t)
	fakeGitHub(t)
	srv := meServer(t)
	t.Setenv("NO_COLOR", "")
	// Not signed in: the sign-in and the commands fail, the rest pass.
	shown, _, err := runInColour(t, "test", "--host", srv.URL, "doctor")
	if err == nil {
		t.Errorf("doctor passed when not signed in:\n%s", shown)
	}
	for _, want := range []string{
		"\n  " + painted(green, "ok") + "    version ",
		"\n  " + painted(red, "FAIL") + "  sign-in ",
		"\n  " + painted(red, "FAIL") + "  commands ",
	} {
		if !strings.Contains(shown, want) {
			t.Errorf("no %q in\n%q", want, shown)
		}
	}
	checks := 0
	for _, line := range strings.Split(withoutColour(shown), "\n") {
		if strings.HasPrefix(line, "  ok    ") || strings.HasPrefix(line, "  FAIL  ") {
			checks++
		}
	}
	if checks < 6 {
		t.Errorf("%d check lines with the mark in its column:\n%s", checks, withoutColour(shown))
	}
	assertNoRainbow(t, "doctor", shown)
}

// The "error:" line's prefix is red when stderr itself is a terminal that
// shows colour, and plain with NO_COLOR, on a terminal without escapes,
// or as JSON.
func TestErrorPrefixIsRedOnATerminal(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	report := func(asJSON bool) string {
		tty, shown := terminal(t, 80)
		ReportError(tty, errors.New("the host did not answer"), asJSON)
		return shown()
	}
	t.Setenv("NO_COLOR", "")
	if got, want := report(false), painted(red, "error:")+" the host did not answer\n"; got != want {
		t.Errorf("in colour: %q, want %q", got, want)
	}
	if got := report(true); strings.Contains(got, "\x1b") {
		t.Errorf("as JSON: %q", got)
	}
	t.Setenv("NO_COLOR", "1")
	if got := report(false); got != "error: the host did not answer\n" {
		t.Errorf("with NO_COLOR: %q", got)
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	if got := report(false); got != "error: the host did not answer\n" {
		t.Errorf("with TERM=dumb: %q", got)
	}
}

// update's last line says it is done in green on a terminal that shows
// colour.
func TestUpdatedLineOnATerminal(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "")
	tty, shown := terminal(t, 80)
	got := updatedLine(&App{Out: tty}, "/usr/local/bin/firmfact")
	shown()
	if want := painted(green, "Done.") + " Updated /usr/local/bin/firmfact."; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// upload status's list of recent uploads paints the state of a document
// that could not be read, or was skipped over the allowance, red, in the
// columns the plain list has.
func TestRecentUploadsInColour(t *testing.T) {
	isolate(t)
	inUTC(t)
	s := newUploadServer(t)
	s.fixtures["BBG-88123.pdf"] = `{"id":"a1b2c3d4-0000-4000-8000-000000000003","state":"skipped","reason":"over_quota","own":true}`
	for _, name := range []string{"scan-0034.pdf", "LSEG-2026-09.pdf", "BBG-88123.pdf"} {
		s.has(name, content(name))
	}
	plain, _, err := run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "status")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("NO_COLOR", "")
	shown, _, err := runInColour(t, "test", "--host", s.URL(), "--workspace", "Acme", "upload", "status")
	if err != nil {
		t.Fatal(err)
	}
	if withoutColour(shown) != plain {
		t.Errorf("colour aside, the list differs:\n%s\nwant\n%s", withoutColour(shown), plain)
	}
	for _, want := range []string{
		"  BBG-88123.pdf     " + painted(red, "skipped, as this month's allowance of documents was used up") + "  a1b2c3d4-",
		"  LSEG-2026-09.pdf  ready for review  ",
		"  scan-0034.pdf     " + painted(red, "could not be read") + "  ",
	} {
		if !strings.Contains(shown, want) {
			t.Errorf("no %q in\n%q", want, shown)
		}
	}
	assertNoRainbow(t, "upload status", shown)
}

// An upload's results on a colour terminal read as the golden ones do,
// colour aside: none and nothing flagged in green, a variance above the
// contract and a document that could not be read in red, and the table's
// columns where they were. With NO_COLOR the terminal shows the golden
// output exactly.
func TestUploadResultsInColour(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	uploadDir(t, "2026-09/LSEG-2026-09.pdf", "2026-09/BBG-88123.pdf", "2026-09/BBG-Anywhere-2026.pdf",
		"2026-09/q3/FactSet-Q3.pdf", "2026-09/scan-0034.pdf", "2026-09/notes.exe", "2026-09/.DS_Store")
	golden := strings.ReplaceAll(readFixtureFile(t, filepath.Join(uploadTestdata, "folder.golden")), "\r\n", "\n")
	// Each run gets a workspace of its own, as the first would leave its
	// files in the one they share.
	upload := func() string {
		s := newUploadServer(t)
		s.has("BBG-88123.pdf", content("BBG-88123.pdf"))
		shown, _, _ := runInColour(t, "test", "--host", s.URL(), "--workspace", "Acme", "upload", "2026-09", "--recursive")
		return shown
	}

	t.Setenv("NO_COLOR", "")
	shown := upload()
	if withoutColour(shown) != golden {
		t.Errorf("colour aside, the table differs from folder.golden:\n%s", withoutColour(shown))
	}
	for _, want := range []string{
		painted(green, "none") + "       " + painted(green, "nothing flagged") + "\n",
		painted(red, "+1,550.00") + "  2 items\n",
		"-                  -          " + painted(red, "could not be read") + "\n",
		"4 ready for review, " + painted(red, "1 could not be read") + ".\n",
	} {
		if !strings.Contains(shown, want) {
			t.Errorf("no %q in\n%q", want, shown)
		}
	}
	assertNoRainbow(t, "upload", shown)

	t.Setenv("NO_COLOR", "1")
	if shown := upload(); shown != golden {
		t.Errorf("with NO_COLOR the terminal showed\n%q\nwant folder.golden", shown)
	}
}

// An invoice's block on a colour terminal: its variance above the
// contract in red.
func TestUploadedInvoiceInColour(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	s := newUploadServer(t)
	uploadDir(t, "LSEG-2026-09.pdf")
	t.Setenv("NO_COLOR", "")
	shown, _, err := runInColour(t, "test", "--host", s.URL(), "--workspace", "Acme", "upload", "LSEG-2026-09.pdf")
	if err != nil {
		t.Fatal(err)
	}
	golden := strings.ReplaceAll(readFixtureFile(t, filepath.Join(uploadTestdata, "invoice.golden")), "\r\n", "\n")
	if withoutColour(shown) != golden {
		t.Errorf("colour aside, the block differs from invoice.golden:\n%s", withoutColour(shown))
	}
	if want := "  Variance    " + painted(red, "EUR 1,550.00 (14.2%) above the contract") + " (preview)\n"; !strings.Contains(shown, want) {
		t.Errorf("no %q in\n%q", want, shown)
	}
	assertNoRainbow(t, "upload", shown)
}
