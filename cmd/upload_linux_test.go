package cmd

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/firmfact/cli/internal/upload"
)

// At a terminal, an upload that would go to a Demo workspace nobody named
// asks first, after the plan: no uploads nothing, yes uploads.
func TestUploadAsksBeforeDemo(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	t.Setenv("TERM", "dumb")
	t.Setenv("NO_COLOR", "1")
	s := newUploadServer(t)
	s.workspace = `{"id":"` + demoID + `","name":"Demo","demo":true}`
	uploadDir(t, "LSEG-2026-09.pdf")
	const question = "Demo is a Demo workspace, and a rebuild removes what you upload there. Upload to Demo all the same? (y/N): "

	for _, c := range []struct {
		answer, want string
		uploads      int
	}{
		{"n", "Nothing was uploaded.", 0},
		{"y", "LSEG-2026-09.pdf: invoice, ready for review", 1},
	} {
		ty, tty := newTypist(t)
		var errOut bytes.Buffer
		done := make(chan error, 1)
		go func() {
			done <- NewRootCommand(Build{Version: "test"}, []string{"--host", s.URL(), "upload", "LSEG-2026-09.pdf"},
				IOStreams{In: tty, Out: tty, Err: &errOut}).Execute()
		}()
		ty.answer(question, c.answer)
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("answer %s: %v (%s)", c.answer, err, errOut.String())
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the upload did not finish")
		}
		tty.Close()
		<-ty.closed
		ty.mu.Lock()
		shown := ty.shown.String()
		ty.mu.Unlock()
		if !strings.Contains(shown, c.want) {
			t.Errorf("answer %s: the terminal showed\n%s", c.answer, shown)
		}
		if _, uploads := s.counts(); uploads != c.uploads {
			t.Errorf("answer %s: %d uploads", c.answer, uploads)
		}
	}
}

// The question comes before the first file is sent, also when the files
// of the first preflight are all in the workspace already and the first
// one to send is in the next.
func TestUploadAsksBeforeDemoPastAFullFirstPreflight(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	t.Setenv("TERM", "dumb")
	t.Setenv("NO_COLOR", "1")
	s := newUploadServer(t)
	s.workspace = `{"id":"` + demoID + `","name":"Demo","demo":true}`
	s.remaining = 200
	var names []string
	for i := range upload.MaxPreflightFiles + 1 {
		names = append(names, fmt.Sprintf("invoices/%03d.pdf", i))
	}
	uploadDir(t, names...)
	for _, name := range names[:upload.MaxPreflightFiles] {
		s.has(filepath.Base(name), content(name))
	}

	ty, tty := newTypist(t)
	var errOut bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- NewRootCommand(Build{Version: "test"}, []string{"--host", s.URL(), "upload", "invoices", "-r"},
			IOStreams{In: tty, Out: tty, Err: &errOut}).Execute()
	}()
	ty.answer("Upload to Demo all the same? (y/N): ", "n")
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("%v (%s)", err, errOut.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the upload did not finish")
	}
	tty.Close()
	<-ty.closed
	if preflights, uploads := s.counts(); preflights != 2 || uploads != 0 {
		t.Errorf("%d preflights, %d uploads; want 2 and none", preflights, uploads)
	}
}

// A person at a terminal sees what the upload is doing on a line redrawn
// in place, which is gone before the results are printed.
func TestUploadShowsProgressOnATerminal(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "1")
	s := newUploadServer(t)
	uploadDir(t, "LSEG-2026-09.pdf", "BBG-88123.pdf")

	shown := runAttended(t, 60, "--host", s.URL(), "--workspace", "Acme", "upload", "LSEG-2026-09.pdf", "BBG-88123.pdf")
	for _, want := range []string{
		"\r\x1b[KReading 2 of 2 files: BBG-88123.pdf",
		"\r\x1b[KSending 1 of 2: LSEG-2026-09.pdf (",
		"\r\x1b[KWaiting for 2 of 2 documents to be read (0s)",
		"\r\x1b[K\nLSEG-2026-09.pdf: invoice, ready for review\n",
	} {
		if !strings.Contains(shown, want) {
			t.Errorf("missing %q in %q", want, shown)
		}
	}
	if strings.Contains(shown, "Waiting for 2 documents to be read (at most") {
		t.Errorf("the line for logs was shown on the terminal: %q", shown)
	}
}

// The live line is gone before whatever follows it, also when nothing is
// waited for (--no-wait): the results start on a line of their own.
func TestUploadClearsTheLiveLineWithoutAWait(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "1")
	s := newUploadServer(t)
	uploadDir(t, "LSEG-2026-09.pdf", "BBG-88123.pdf")

	shown := runAttended(t, 80, "--host", s.URL(), "--workspace", "Acme", "upload", "LSEG-2026-09.pdf", "BBG-88123.pdf", "--no-wait")
	i := strings.LastIndex(shown, "Sending 2 of 2: BBG-88123.pdf (")
	if i < 0 {
		t.Fatalf("no live line in %q", shown)
	}
	line, _, _ := strings.Cut(shown[i:], "\n")
	if !strings.Contains(line, ")\r\x1b[K") {
		t.Errorf("the live line stayed: %q", line)
	}
}

// - with a terminal for standard input is a mistake, and the hint that
// says what to do works in the shell at hand: a redirect, or on Windows,
// where PowerShell has none, naming the file.
func TestUploadStandardInputFromATerminal(t *testing.T) {
	isolate(t)
	s := newUploadServer(t)
	for goos, want := range map[string]string{
		"linux":   "- reads the file from a pipe or a redirect, not from the terminal: such as `firmfact upload - --name scan.pdf < scan.pdf`",
		"windows": "- reads the file from a pipe, not from the terminal; to upload a file, name it: such as `firmfact upload scan.pdf`",
	} {
		prev := runtimeOS
		runtimeOS = goos
		tty, shown := terminal(t, 80)
		err := NewRootCommand(Build{Version: "test"}, []string{"--host", s.URL(), "--workspace", "Acme", "upload", "-", "--name", "scan.pdf"},
			IOStreams{In: tty, Out: tty, Err: io.Discard}).Execute()
		shown()
		runtimeOS = prev
		if code, _ := Classify(err); code != ExitUsage || err.Error() != want {
			t.Errorf("%s: exit %d, %v", goos, code, err)
		}
	}
}
