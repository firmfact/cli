package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/mcp"
	"github.com/firmfact/cli/internal/upload"
)

// fastUploadPolls makes an upload's wait read the states every
// millisecond for the rest of the test.
func fastUploadPolls(t *testing.T) {
	t.Helper()
	prev := uploadPollInterval
	uploadPollInterval = time.Millisecond
	t.Cleanup(func() { uploadPollInterval = prev })
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// uploadDir writes the files named, each holding its own name, below a new
// directory that becomes the working one.
func uploadDir(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content(name)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
	return dir
}

// content is what uploadDir writes into the file of that name.
func content(name string) string { return "%PDF-1.7 " + filepath.Base(name) }

// An invoice goes through the whole of an upload: the plan, one request,
// the wait, and the result read back in full: what was read, the contract
// it matched, the variance and its lines, what to review, the link, and
// that nothing is booked.
func TestUploadAnInvoice(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	s := newUploadServer(t)
	uploadDir(t, "LSEG-2026-09.pdf")

	stdout, stderr, err := run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "LSEG-2026-09.pdf")
	if err != nil {
		t.Fatalf("upload: %v (stderr %q)", err, stderr)
	}
	assertGoldenFile(t, filepath.Join(uploadTestdata, "invoice.golden"), stdout)
	if preflights, uploads := s.counts(); preflights != 1 || uploads != 1 {
		t.Errorf("%d preflights, %d uploads", preflights, uploads)
	}
	if stderr != "" {
		t.Errorf("stderr = %q", stderr)
	}
}

// A clean invoice, a contract that updates one already here and a file
// that could not be read each read as the plan's examples do; the one that
// could not be read makes the exit status 1.
func TestUploadShowsEachKindOfDocument(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	s := newUploadServer(t)
	s.states["scan-0034.pdf"] = []string{"reading", "retrying", "reading"}
	uploadDir(t, "BBG-88123.pdf", "BBG-Anywhere-2026.pdf", "scan-0034.pdf")

	stdout, _, err := run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "BBG-88123.pdf", "BBG-Anywhere-2026.pdf", "scan-0034.pdf")
	if code, _ := Classify(err); code != ExitFailed || err.Error() != "1 document could not be read" {
		t.Errorf("exit %d: %v", code, err)
	}
	assertGoldenFile(t, filepath.Join(uploadTestdata, "kinds.golden"), stdout)
	if got := strings.Join(s.sentFiles(), " "); got != "BBG-88123.pdf BBG-Anywhere-2026.pdf scan-0034.pdf" {
		t.Errorf("sent %s, one request each", got)
	}
}

// A folder with more documents than fit as blocks prints a table and a
// line that sums it up. A file already there is not sent, and neither are
// hidden files or a file whose type firmfact does not read.
func TestUploadAFolder(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	s := newUploadServer(t)
	uploadDir(t, "2026-09/LSEG-2026-09.pdf", "2026-09/BBG-88123.pdf", "2026-09/BBG-Anywhere-2026.pdf",
		"2026-09/q3/FactSet-Q3.pdf", "2026-09/scan-0034.pdf", "2026-09/notes.exe", "2026-09/.DS_Store")
	s.has("BBG-88123.pdf", content("BBG-88123.pdf"))

	stdout, _, err := run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "2026-09", "--recursive")
	if code, _ := Classify(err); code != ExitFailed {
		t.Errorf("exit %d: %v", code, err)
	}
	assertGoldenFile(t, filepath.Join(uploadTestdata, "folder.golden"), stdout)
	if got := strings.Join(s.sentFiles(), " "); got != "BBG-Anywhere-2026.pdf LSEG-2026-09.pdf FactSet-Q3.pdf scan-0034.pdf" {
		t.Errorf("sent %s", got)
	}
}

// With --json, stdout holds the envelope alone: a result per file with its
// outcome and the server's document entry as it came, unknown fields and
// all, counts by outcome and state, and the schema. The wait and the error
// go to stderr.
func TestUploadJSON(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	s := newUploadServer(t)
	uploadDir(t, "LSEG-2026-09.pdf", "scan-0034.pdf", "notes.exe")

	stdout, stderr, err := run("test", "--host", s.URL(), "--workspace", "Acme", "--json", "upload", "LSEG-2026-09.pdf", "scan-0034.pdf", "notes.exe")
	if code, _ := Classify(err); code != ExitFailed {
		t.Errorf("exit %d: %v", code, err)
	}
	var got struct {
		Data struct {
			Workspace struct{ ID, Name string }
			Results   []struct {
				Path, Filename, Outcome, Code, Message, SHA256 string
				Size                                           int64
				Document                                       map[string]any
			}
			Summary   map[string]any
			Allowance struct{ Remaining int }
		}
		Meta  struct{ Schema string }
		Notes []string
	}
	decodeOnly(t, "upload --json", stdout, &got)
	if got.Data.Workspace.Name != "Acme" || got.Meta.Schema != "document_result/1" || got.Notes == nil || got.Data.Allowance.Remaining != 21 {
		t.Errorf("envelope = %+v", got)
	}
	if len(got.Data.Results) != 3 {
		t.Fatalf("results = %+v", got.Data.Results)
	}
	lseg, scan, exe := got.Data.Results[0], got.Data.Results[1], got.Data.Results[2]
	if lseg.Path != "LSEG-2026-09.pdf" || lseg.Outcome != "created" || lseg.SHA256 != sha256Hex(content("LSEG-2026-09.pdf")) || lseg.Size != int64(len(content("LSEG-2026-09.pdf"))) {
		t.Errorf("LSEG = %+v", lseg)
	}
	variance, _ := lseg.Document["variance"].(map[string]any)
	if variance["status"] != "variance" || lseg.Document["approvals"] == nil {
		t.Errorf("LSEG's document lost what the server sent: %v", lseg.Document)
	}
	if scan.Outcome != "created" || scan.Document["state"] != "failed" {
		t.Errorf("scan = %+v", scan)
	}
	if exe.Outcome != "refused" || exe.Code != "unsupported_type" || !strings.HasPrefix(exe.Message, "notes.exe: files of this type") || exe.Document != nil {
		t.Errorf("notes.exe = %+v", exe)
	}
	states, _ := got.Data.Summary["states"].(map[string]any)
	if got.Data.Summary["created"] != 2.0 || got.Data.Summary["refused"] != 1.0 || states["ready_for_review"] != 1.0 || states["failed"] != 1.0 {
		t.Errorf("summary = %v", got.Data.Summary)
	}
	if stderr != "Waiting for 2 documents to be read (at most 15m)...\n" {
		t.Errorf("stderr = %q", stderr)
	}
}

// A sign-up's default workspace is Demo, which a rebuild wipes. An upload
// that would land there without naming it is refused off a terminal, with
// exit status 2 and before any file is read, unless --yes says to go
// ahead; naming the workspace, with --workspace or FIRMFACT_WORKSPACE, is
// enough.
func TestUploadDemoGuard(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	s := newUploadServer(t)
	s.workspace = `{"id":"` + demoID + `","name":"Demo","demo":true}`
	uploadDir(t, "LSEG-2026-09.pdf")

	code, msg := exitStatusOf(t.Context(), "test", "--host", s.URL(), "upload", "LSEG-2026-09.pdf")
	if code != ExitUsage || !strings.Contains(msg, "would go to Demo, a Demo workspace") || !strings.Contains(msg, "--yes") {
		t.Errorf("exit %d: %s", code, msg)
	}
	if preflights, _ := s.counts(); preflights != 0 {
		t.Errorf("%d preflights before the refusal", preflights)
	}

	stdout, _, err := run("test", "--host", s.URL(), "upload", "LSEG-2026-09.pdf", "--yes")
	if err != nil || !strings.Contains(stdout, "Demo is a Demo workspace: a rebuild from sample data removes what you upload there.") {
		t.Errorf("--yes: %v\n%s", err, stdout)
	}
	if _, err := os.Stat("LSEG-2026-09.pdf"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--workspace", "Demo"}, {"--workspace", demoID}} {
		if _, _, err := run("test", append([]string{"--host", s.URL(), "upload", "LSEG-2026-09.pdf"}, args...)...); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
	t.Setenv("FIRMFACT_WORKSPACE", "Demo")
	if _, _, err := run("test", "--host", s.URL(), "upload", "LSEG-2026-09.pdf"); err != nil {
		t.Errorf("FIRMFACT_WORKSPACE: %v", err)
	}
}

// An upload with no workspace named goes to the default workspace, which
// need not be a Demo one; a profile's workspace the sign-in no longer
// reaches is not found.
func TestUploadToTheDefaultWorkspace(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	s := newUploadServer(t)
	s.me = `[{"id":"` + acmeID + `","name":"Acme","default":true}]`
	uploadDir(t, "LSEG-2026-09.pdf")
	if _, _, err := run("test", "--host", s.URL(), "upload", "LSEG-2026-09.pdf"); err != nil {
		t.Fatal(err)
	}
	saveProfiles(t, "default", map[string]*config.Profile{"default": {Host: s.URL(), Workspace: "gone-1"}})
	if code, msg := exitStatusOf(t.Context(), "test", "upload", "LSEG-2026-09.pdf"); code != ExitNotFound || !strings.Contains(msg, `no workspace "gone-1"`) {
		t.Errorf("exit %d: %s", code, msg)
	}
	s.me = `[]`
	saveProfiles(t, "default", map[string]*config.Profile{"default": {Host: s.URL()}})
	if code, msg := exitStatusOf(t.Context(), "test", "upload", "LSEG-2026-09.pdf"); code != ExitNotFound || !strings.Contains(msg, "no default workspace") {
		t.Errorf("exit %d: %s", code, msg)
	}
}

// The CLI finds the files a pattern the shell left alone matches, and the
// files in a folder with --recursive; the same bytes under two names go
// once. A folder without --recursive, a file that is not there and a
// pattern that matches nothing are mistakes on the command line.
func TestUploadFindsFiles(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	s := newUploadServer(t)
	dir := uploadDir(t, "a.pdf", "b.pdf", "notes.txt", "2026-09/c.pdf")
	if err := os.WriteFile(filepath.Join(dir, "copy.pdf"), []byte(content("a.pdf")), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "*.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.sentFiles(), " "); got != "a.pdf b.pdf" {
		t.Errorf("sent %s", got)
	}
	if !strings.Contains(stdout, "  Left out copy.pdf: the same as a.pdf\n") {
		t.Errorf("stdout = %q", stdout)
	}

	for _, c := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"2026-09"}, ExitUsage, "2026-09 is a folder; add --recursive to upload the files in it"},
		{[]string{"missing.pdf"}, ExitUsage, "no such file: missing.pdf"},
		{[]string{"a.pdf", "status"}, ExitUsage, "no such file: status; for the status of your uploads, run `firmfact upload status`"},
		{[]string{"*.docx"}, ExitUsage, "no files match *.docx"},
		{[]string{"2026-0?"}, ExitFailed, "nothing to upload: the pattern matched a folder; add --recursive to upload the files in it"},
		{nil, ExitUsage, "name the files to upload"},
		{[]string{"a.pdf", "--wait-timeout", "0s"}, ExitUsage, "--wait-timeout must be more than 0"},
	} {
		code, msg := exitStatusOf(t.Context(), "test", append([]string{"--host", s.URL(), "--workspace", "Acme", "upload"}, c.args...)...)
		if code != c.code || !strings.Contains(msg, c.want) {
			t.Errorf("%v: exit %d, %q; want %d, %q", c.args, code, msg, c.code, c.want)
		}
	}
	// status names the subcommand, so a file called status is ./status.
	if code, msg := exitStatusOf(t.Context(), "test", "--host", s.URL(), "--workspace", "Acme", "upload", "./status"); code != ExitUsage || msg != "no such file: ./status" {
		t.Errorf("./status: exit %d, %q", code, msg)
	}
}

// - reads the file from standard input, which --name names; the copy the
// CLI keeps while it uploads is gone afterwards.
func TestUploadFromStandardInput(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	s := newUploadServer(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	uploadDir(t)

	stdout, _, err := runWithInput(content("scan-0034.pdf"), "test", "--host", s.URL(), "--workspace", "Acme", "upload", "-", "--name", "scan-0034.pdf")
	if code, _ := Classify(err); code != ExitFailed || !strings.Contains(stdout, "scan-0034.pdf: could not be read") {
		t.Errorf("exit %d: %v\n%s", code, err, stdout)
	}
	if got := strings.Join(s.sentFiles(), " "); got != "scan-0034.pdf" {
		t.Errorf("sent %s", got)
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Errorf("left behind: %v", entries)
	}

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-"}, "- needs --name"},
		{[]string{"-", "-", "--name", "a.pdf"}, "name it once"},
		{[]string{"a.pdf", "--name", "b.pdf"}, "--name names the file read from standard input"},
	} {
		code, msg := exitStatusOf(t.Context(), "test", append([]string{"--host", s.URL(), "--workspace", "Acme", "upload"}, c.args...)...)
		if code != ExitUsage || !strings.Contains(msg, c.want) {
			t.Errorf("%v: exit %d, %q", c.args, code, msg)
		}
	}
}

// Uploading the same files again sends nothing: the plan says they are
// already there, their results are shown as before, and the exit status is
// 0, so a script can run over a folder as often as it likes.
func TestUploadTwiceIsSafe(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	s := newUploadServer(t)
	uploadDir(t, "LSEG-2026-09.pdf")
	args := []string{"--host", s.URL(), "--workspace", "Acme", "upload", "LSEG-2026-09.pdf"}
	if _, _, err := run("test", args...); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := run("test", args...)
	if err != nil {
		t.Fatal(err)
	}
	if _, uploads := s.counts(); uploads != 1 {
		t.Errorf("%d uploads", uploads)
	}
	for _, want := range []string{"Nothing to send to Acme: 1 already in firmfact\n", "LSEG-2026-09.pdf: invoice, ready for review (already in firmfact)\n", "  Variance    EUR 1,550.00 (14.2%) above the contract (preview)\n"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in\n%s", want, stdout)
		}
	}
}

// dropAnswer reads a request whole, stores it when store is set, and
// closes the connection without an answer.
func dropAnswer(s *uploadServer, w http.ResponseWriter, r *http.Request, store bool) {
	if store {
		s.accept(r)
	} else {
		_, _ = io.Copy(io.Discard, r.Body)
	}
	if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
		conn.Close()
	}
}

// An upload whose answer is lost is looked up again: a file the server
// has now was stored, and one it does not have is sent again, once.
func TestUploadAfterALostAnswer(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	uploadDir(t, "LSEG-2026-09.pdf", "BBG-88123.pdf")

	t.Run("sent again", func(t *testing.T) {
		s := newUploadServer(t)
		s.uploadHook = func(n int, w http.ResponseWriter, r *http.Request) bool {
			if n == 1 {
				dropAnswer(s, w, r, false)
				return true
			}
			return false
		}
		if _, stderr, err := run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "LSEG-2026-09.pdf"); err != nil {
			t.Fatalf("%v (stderr %q)", err, stderr)
		}
		if preflights, uploads := s.counts(); preflights != 2 || uploads != 2 {
			t.Errorf("%d preflights, %d uploads", preflights, uploads)
		}
	})
	t.Run("stored before it was lost", func(t *testing.T) {
		s := newUploadServer(t)
		s.uploadHook = func(n int, w http.ResponseWriter, r *http.Request) bool {
			if n == 1 {
				dropAnswer(s, w, r, true)
				return true
			}
			return false
		}
		stdout, _, err := run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "LSEG-2026-09.pdf")
		if err != nil || !strings.Contains(stdout, "LSEG-2026-09.pdf: invoice, ready for review\n") {
			t.Fatalf("%v\n%s", err, stdout)
		}
		if preflights, uploads := s.counts(); preflights != 2 || uploads != 1 {
			t.Errorf("%d preflights, %d uploads", preflights, uploads)
		}
	})
	for _, status := range []int{http.StatusBadGateway, http.StatusInternalServerError} {
		t.Run(fmt.Sprintf("a %d after storing", status), func(t *testing.T) {
			s := newUploadServer(t)
			s.uploadHook = func(n int, w http.ResponseWriter, r *http.Request) bool {
				if n == 1 {
					s.accept(r)
					w.WriteHeader(status)
					return true
				}
				return false
			}
			if _, _, err := run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "LSEG-2026-09.pdf"); err != nil {
				t.Fatal(err)
			}
			if _, uploads := s.counts(); uploads != 1 {
				t.Errorf("%d uploads", uploads)
			}
		})
	}
	t.Run("lost twice", func(t *testing.T) {
		s := newUploadServer(t)
		s.uploadHook = func(n int, w http.ResponseWriter, r *http.Request) bool {
			dropAnswer(s, w, r, false)
			return true
		}
		stdout, stderr, err := run("test", "--host", s.URL(), "--workspace", "Acme", "--json", "upload", "LSEG-2026-09.pdf", "BBG-88123.pdf")
		if code, _ := Classify(err); code != ExitUnavailable {
			t.Errorf("exit %d: %v (stderr %q)", code, err, stderr)
		}
		if _, uploads := s.counts(); uploads != 2 {
			t.Errorf("%d uploads; the second file must wait for a later run", uploads)
		}
		var got uploadJSON
		decodeOnly(t, "lost twice", stdout, &got)
		if r := got.Data.Results; len(r) != 2 || r[0].Outcome != outcomeFailed || r[0].Code != "no_answer" || r[1].Outcome != outcomeNotSent || r[1].Code != "no_answer" {
			t.Errorf("results = %+v", r)
		}
	})
}

// Each way an upload can end has its exit status.
func TestUploadExitStatuses(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	uploadDir(t, "LSEG-2026-09.pdf", "BBG-88123.pdf", "notes.exe")
	both := []string{"LSEG-2026-09.pdf", "BBG-88123.pdf"}
	refuse := func(status int, body string, header ...string) func(*uploadServer) {
		return func(s *uploadServer) {
			s.uploadHook = func(n int, w http.ResponseWriter, r *http.Request) bool {
				for _, h := range header {
					k, v, _ := strings.Cut(h, ": ")
					w.Header().Set(k, v)
				}
				_, _ = io.Copy(io.Discard, r.Body)
				answerJSON(w, status, body)
				return true
			}
		}
	}
	cases := []struct {
		name  string
		setup func(*uploadServer)
		args  []string
		code  int
		want  string
		sent  int
	}{
		{"a named file firmfact does not read", nil, []string{"notes.exe"}, ExitFailed, "1 file was not stored", 0},
		{"the workspace does not exist", nil, []string{"--workspace", "Nope", "LSEG-2026-09.pdf"}, ExitNotFound, "Workspace not found", 0},
		{"the wait runs out", func(s *uploadServer) {
			s.states["LSEG-2026-09.pdf"] = make([]string, 10000)
			for i := range s.states["LSEG-2026-09.pdf"] {
				s.states["LSEG-2026-09.pdf"][i] = "reading"
			}
		}, []string{"LSEG-2026-09.pdf", "--wait-timeout", "30ms"}, ExitUnavailable,
			"1 document was still being read after 30ms; check with `firmfact upload status --workspace " + acmeID + " 423a2262-85dd-4cf1-9b51-60c7bbf2ff7d`", 1},
		{"rate-limited", refuse(http.StatusTooManyRequests, `{"error":"Rate limit exceeded","code":"RATE_LIMITED"}`), both, ExitUnavailable,
			"2 files could not be sent now", 1},
		{"over the allowance", refuse(http.StatusForbidden,
			`{"error":"This upload needs 1 of this month's documents and 0 are left, so nothing was stored.","code":"QUOTA_EXCEEDED","details":{"needed":1,"allowance":{"limit":25,"used":25,"pending":0,"remaining":0,"resets_on":"2026-10-01"}}}`),
			both, ExitFailed, "2 files were not stored", 1},
		{"a file refused as a whole", refuse(http.StatusUnprocessableEntity,
			`{"error":"LSEG-2026-09.pdf: the contents do not match the .pdf extension, so it was not stored.","code":"FILE_REFUSED","details":{"results":[{"index":0,"filename":"LSEG-2026-09.pdf","outcome":"type_mismatch","message":"LSEG-2026-09.pdf: the contents do not match the .pdf extension, so it was not stored."}]}}`),
			both, ExitFailed, "2 files were not stored", 2},
		{"the virus scanner is busy", refuse(http.StatusServiceUnavailable,
			`{"error":"The virus scanner is busy.","code":"SCANNER_BUSY","details":{"results":[{"index":0,"filename":"LSEG-2026-09.pdf","outcome":"scanner_busy","message":"The virus scanner is busy; try again in a minute."}]}}`,
			"Retry-After: 3600"), both, ExitUnavailable, "2 files could not be sent now", 1},
		{"the role may not upload", refuse(http.StatusForbidden, `{"error":"Only admins and editors can upload documents.","code":"FORBIDDEN"}`),
			both, ExitFailed, "Only admins and editors can upload documents.", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newUploadServer(t)
			if c.setup != nil {
				c.setup(s)
			}
			args := append([]string{"--host", s.URL(), "--workspace", "Acme", "upload"}, c.args...)
			code, msg := exitStatusOf(t.Context(), "test", args...)
			if code != c.code || !strings.Contains(msg, c.want) {
				t.Errorf("exit %d, %q; want %d, %q", code, msg, c.code, c.want)
			}
			if _, uploads := s.counts(); uploads != c.sent {
				t.Errorf("%d upload requests, want %d", uploads, c.sent)
			}
		})
	}

	t.Run("not signed in", func(t *testing.T) {
		s := newUploadServer(t)
		if err := config.DeleteToken(s.URL()); err != nil {
			t.Fatal(err)
		}
		if code, msg := exitStatusOf(t.Context(), "test", "--host", s.URL(), "--workspace", "Acme", "upload", "LSEG-2026-09.pdf"); code != ExitSignedOut {
			t.Errorf("exit %d: %s", code, msg)
		}
	})
	t.Run("a host without uploads", func(t *testing.T) {
		host := answering(t, http.StatusNotFound, "text/html", "<html>Not found</html>")
		storedToken(t, host, time.Hour)
		code, msg := exitStatusOf(t.Context(), "test", "--host", host, "--workspace", "Acme", "upload", "LSEG-2026-09.pdf")
		if code != ExitUnsupported || !strings.Contains(msg, "does not offer uploads yet") {
			t.Errorf("exit %d: %s", code, msg)
		}
	})
	t.Run("interrupted", func(t *testing.T) {
		s := newUploadServer(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		// Ctrl-C as the second file goes: the first is in firmfact.
		s.uploadHook = func(n int, w http.ResponseWriter, r *http.Request) bool {
			if n == 1 {
				return false
			}
			// The server sees the connection close only once it has read
			// the body.
			_, _ = io.Copy(io.Discard, r.Body)
			cancel()
			<-r.Context().Done()
			return true
		}
		stdout, _, err := runContext(ctx, "--host", s.URL(), "--workspace", "Acme", "upload", "LSEG-2026-09.pdf", "BBG-88123.pdf")
		if code, _ := Classify(err); code != ExitInterrupted {
			t.Errorf("exit %d: %v", code, err)
		}
		// What was sent is read all the same, and the rest waits for the
		// next run.
		for _, want := range []string{
			"Stopped. 1 file was uploaded before that, and firmfact reads it all the same; see what it read with `firmfact upload status --workspace " + acmeID + " 423a2262-85dd-4cf1-9b51-60c7bbf2ff7d`.\n",
			"Run the same command again to send the rest: files already there are skipped.\n",
		} {
			if !strings.Contains(stdout, want) {
				t.Errorf("missing %q in\n%s", want, stdout)
			}
		}
	})
	t.Run("interrupted while waiting", func(t *testing.T) {
		s := newUploadServer(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		s.anyHook = func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Query().Get("view") != "state" {
				return false
			}
			cancel()
			<-r.Context().Done()
			return true
		}
		stdout, _, err := runContext(ctx, "--host", s.URL(), "--workspace", "Acme", "upload", "LSEG-2026-09.pdf")
		if code, _ := Classify(err); code != ExitInterrupted {
			t.Errorf("exit %d: %v", code, err)
		}
		want := "Stopped waiting. Firmfact reads the documents all the same; see what it read with `firmfact upload status --workspace " + acmeID + " 423a2262-85dd-4cf1-9b51-60c7bbf2ff7d`.\n"
		if !strings.HasSuffix(stdout, want) {
			t.Errorf("stdout = %q", stdout)
		}
	})
}

// hostileEntry is a finished invoice whose every string tries to drive the
// terminal: a clipboard write, a colour, a cursor move, and a
// right-to-left override that would turn text around.
const hostileEntry = `{
	"id": "9f4ca05a-7b6e-4a2f-9cb3-5e6f708192a3",
	"state": "ready_for_review",
	"url": "https://app.firmfact.example/accounts/x/documents/9f4c\u001b]8;;https://evil.example\u0007",
	"own": true, "filename": "evil\u001b[2J.pdf", "type": "invoice\u001b[31m",
	"read": {"type": "invoice",
		"vendor": {"name": "Evil\u001b]52;c;cm0gLXJmIH4=\u0007 Corp", "status": "linked", "linked_to": "Evil \u202eproC"},
		"number": "INV\u009b31m-1", "dates": {"document": "2026-09-01\u001b[1A"}, "currency": "EUR\u001b[31m",
		"amounts": {"total": "100.00"},
		"checks": [{"code": "totals", "status": "fail", "message": "Totals\u001b[8m hidden"}]},
	"contract_match": {"status": "linked", "how": "automatic", "score": "1",
		"contract": {"number": "C-1\u001b[0m", "name": "Contract \u200b\u2066name"}},
	"variance": {"preview": true, "status": "variance", "summary": "EUR 1.00 above\u001b[K",
		"lines": [{"line": 1, "message": "Unit price\r\nerror: forged"}]},
	"review": {"items": [{"message": "Line 1\u001b[5m: choose"}], "more_items": 0},
	"booked": false, "notes": ["Order forms\u001b[31m cannot be published yet.", "Nothing is booked."]
}`

// Every string the server sends reaches the terminal as visible escapes,
// never as sequences it acts on, and never across a line of its own.
func TestUploadEscapesServerText(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	s := newUploadServer(t)
	s.fixtures["evil.pdf"] = hostileEntry
	s.workspace = `{"id":"` + acmeID + `","name":"Acme\u001b]0;pwned\u0007","demo":false}`
	uploadDir(t, "evil.pdf", "odd.exe")
	s.preflightHook = func(n int, w http.ResponseWriter, r *http.Request) bool {
		rec := &recorder{header: http.Header{}}
		s.preflight(rec, r)
		body := strings.Replace(rec.body.String(), "odd.exe: files of this type", `odd\u001b[2K.exe: files of this type`, 1)
		answerJSON(w, rec.status, body)
		return true
	}

	stdout, _, _ := run("test", "--host", s.URL(), "--workspace", acmeID, "upload", "evil.pdf", "odd.exe")
	if strings.ContainsAny(stdout, "\x1b\x07\u009b\u202e\u200b\u2066\r") {
		t.Errorf("control characters reached the terminal:\n%q", stdout)
	}
	for _, want := range []string{
		`Evil\u001b]52;c;cm0gLXJmIH4=\u0007 Corp, linked to Evil \u202eproC`,
		`Uploading to Acme\u001b]0;pwned\u0007: 1 new, 1 refused`,
		`odd\u001b[2K.exe: files of this type`,
		`Unit price  error: forged`,
		`Contract \u200b\u2066name (C-1\u001b[0m)`,
		`Order forms\u001b[31m cannot be published yet.`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %s in\n%s", want, stdout)
		}
	}
}

// recorder is enough of an http.ResponseWriter to capture an answer.
type recorder struct {
	header http.Header
	status int
	body   strings.Builder
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *recorder) WriteHeader(status int)      { r.status = status }

// --related sends the files in one request, as one group; a file of the
// group firmfact refuses, or a group over the limits for one request,
// sends none of them: a group stored without one of its files would be
// read, and counted, as if it were whole.
func TestUploadRelated(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	names := []string{"invoice.pdf", "usage.xlsx"}
	for i := range 10 {
		names = append(names, "part-"+string(rune('a'+i))+".pdf")
	}
	uploadDir(t, append(names, "notes.exe")...)

	s := newUploadServer(t)
	stdout, _, err := run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "invoice.pdf", "usage.xlsx", "--related")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.sentFiles(); len(got) != 1 || got[0] != "invoice.pdf+usage.xlsx" || !s.related[0] {
		t.Errorf("sent %v (related %v)", got, s.related)
	}
	if !strings.Contains(stdout, "  Sent together as related documents, counted once against the allowance: invoice.pdf, usage.xlsx\n") {
		t.Errorf("stdout = %s", stdout)
	}

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"invoice.pdf", "notes.exe"}, "  invoice.pdf: not sent, as notes.exe was refused and --related sends the files together or not at all\n"},
		{names[1:], "You selected 11 files. Upload at most 10 at a time."},
	} {
		s := newUploadServer(t)
		stdout, _, err := run("test", append([]string{"--host", s.URL(), "--workspace", "Acme", "upload", "--related"}, c.args...)...)
		if code, _ := Classify(err); code != ExitFailed || !strings.Contains(stdout, c.want) {
			t.Errorf("%v: exit %d (%v)\n%s", c.args, code, err, stdout)
		}
		if _, uploads := s.counts(); uploads != 0 {
			t.Errorf("%v: %d uploads", c.args, uploads)
		}
	}
}

// --no-wait sends the files and leaves: nothing is read back but what the
// upload answered, and the output says how to look later.
func TestUploadNoWait(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	s := newUploadServer(t)
	uploadDir(t, "LSEG-2026-09.pdf")
	stdout, _, err := run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "LSEG-2026-09.pdf", "--no-wait")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"LSEG-2026-09.pdf: waiting to be read\n",
		"Firmfact reads them meanwhile; see what it read with `firmfact upload status --workspace " + acmeID + " 423a2262-85dd-4cf1-9b51-60c7bbf2ff7d`.\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in\n%s", want, stdout)
		}
	}
	for _, req := range s.requests {
		if strings.Contains(req, "view=state") || strings.HasPrefix(req, "GET") {
			t.Errorf("read back after --no-wait: %s", req)
		}
	}
}

// upload is the CLI's own command, so no workspace tool may take its name.
func TestUploadIsReservedFromTools(t *testing.T) {
	plans := planToolCommands([]mcp.Tool{{Name: "upload"}}, commandNames(NewReferenceCommand("test")))
	if plans[0].skipped != skipReserved {
		t.Errorf("a tool named upload: %+v", plans[0])
	}
}

// Tab completes files for upload, as the shell would, and waits for
// --wait-timeout.
func TestUploadCompletion(t *testing.T) {
	isolate(t)
	candidates, directive := complete(t, "test", "upload", "")
	if !strings.Contains(strings.Join(candidates, " "), "status") || directive != ":0" {
		t.Errorf("upload <Tab> = %v %s", candidates, directive)
	}
	candidates, _ = complete(t, "test", "upload", "--wait-timeout", "")
	if strings.Join(candidates, " ") != "5m 15m 30m 1h" {
		t.Errorf("--wait-timeout <Tab> = %v", candidates)
	}
	if _, directive := complete(t, "test", "upload", "--name", ""); directive != ":4" {
		t.Errorf("--name <Tab>: directive %s", directive)
	}
}

// The results of a run are a function of the documents alone.
func TestUploadJSONOfAnEmptyRun(t *testing.T) {
	r := &uploadRun{found: &upload.Found{}}
	raw, err := json.Marshal(r.json())
	if err != nil || !strings.Contains(string(raw), `"results":[]`) || !strings.Contains(string(raw), `"notes":[]`) {
		t.Errorf("%s, %v", raw, err)
	}
}

// A file that changes between being hashed and being sent is not stored:
// its request stops short, and the next file goes as usual.
func TestUploadChangedFile(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	s := newUploadServer(t)
	dir := uploadDir(t, "LSEG-2026-09.pdf", "BBG-88123.pdf")
	s.preflightHook = func(n int, w http.ResponseWriter, r *http.Request) bool {
		// The same size, other bytes.
		changed := strings.ToUpper(content("LSEG-2026-09.pdf"))
		if err := os.WriteFile(filepath.Join(dir, "LSEG-2026-09.pdf"), []byte(changed), 0o600); err != nil {
			t.Error(err)
		}
		return false
	}
	stdout, _, err := run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "LSEG-2026-09.pdf", "BBG-88123.pdf")
	if code, _ := Classify(err); code != ExitFailed || err.Error() != "1 file was not stored" {
		t.Errorf("exit %d: %v", code, err)
	}
	for _, want := range []string{
		"LSEG-2026-09.pdf: not sent\n  LSEG-2026-09.pdf changed while it was being uploaded, so nothing was\n  stored; upload it again once it is complete\n",
		"BBG-88123.pdf: invoice, ready for review\n",
		"1 uploaded, 1 not sent: 1 ready for review.\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in\n%s", want, stdout)
		}
	}
	if got := strings.Join(s.sentFiles(), " "); got != "BBG-88123.pdf" {
		t.Errorf("stored %s", got)
	}
}

// A file gone since it was hashed is that file's failure; the others go
// as usual.
func TestUploadFileGoneBeforeItIsSent(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	s := newUploadServer(t)
	dir := uploadDir(t, "LSEG-2026-09.pdf", "BBG-88123.pdf")
	s.preflightHook = func(n int, w http.ResponseWriter, r *http.Request) bool {
		if err := os.Remove(filepath.Join(dir, "LSEG-2026-09.pdf")); err != nil {
			t.Error(err)
		}
		return false
	}
	stdout, _, err := run("test", "--host", s.URL(), "--workspace", "Acme", "--json", "upload", "LSEG-2026-09.pdf", "BBG-88123.pdf")
	if code, _ := Classify(err); code != ExitFailed {
		t.Errorf("exit %d: %v", code, err)
	}
	var got uploadJSON
	decodeOnly(t, "gone", stdout, &got)
	if r := got.Data.Results; len(r) != 2 || r[0].Outcome != outcomeFailed || r[0].Code != "unreadable" || !strings.HasPrefix(r[0].Message, "could not read LSEG-2026-09.pdf: ") || r[1].Outcome != outcomeCreated {
		t.Errorf("results = %+v", r)
	}
}

// What the preflight says of each file decides what is sent: a file being
// uploaded by someone else right now is not (exit status 5, as it is
// worth running again), a file over the allowance is not, and one read
// and skipped for the allowance could not be read.
func TestUploadPreflightVerdicts(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	uploadDir(t, "LSEG-2026-09.pdf", "BBG-88123.pdf")

	s := newUploadServer(t)
	s.preflightHook = func(n int, w http.ResponseWriter, r *http.Request) bool {
		rec := &recorder{header: http.Header{}}
		s.preflight(rec, r)
		answerJSON(w, rec.status, strings.Replace(rec.body.String(), `"status":"ok"`, `"status":"in_progress"`, 1))
		return true
	}
	stdout, _, err := run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "LSEG-2026-09.pdf", "BBG-88123.pdf")
	if code, _ := Classify(err); code != ExitUnavailable {
		t.Errorf("in progress: exit %d: %v", code, err)
	}
	if !strings.Contains(stdout, "Uploading to Acme: 1 new, 1 being uploaded by someone else\n  LSEG-2026-09.pdf: someone is uploading the same file right now; run the command again in a minute to see it\n") {
		t.Errorf("stdout = %s", stdout)
	}

	s = newUploadServer(t)
	s.remaining = 1
	stdout, _, err = run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "LSEG-2026-09.pdf", "BBG-88123.pdf")
	if code, _ := Classify(err); code != ExitFailed || !strings.Contains(stdout, "  BBG-88123.pdf: this month's allowance of documents is used up") {
		t.Errorf("over the allowance: exit %d: %v\n%s", code, err, stdout)
	}

	s = newUploadServer(t)
	s.fixtures["LSEG-2026-09.pdf"] = `{"id":"a1b2c3d4-0000-4000-8000-000000000003","state":"skipped","reason":"over_quota","own":true,
		"error":{"code":"usage_limit_exceeded","message":"This document was not processed because your workspace has reached its monthly document limit."}}`
	_, _, err = run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "LSEG-2026-09.pdf")
	if code, _ := Classify(err); code != ExitFailed || !strings.Contains(err.Error(), "1 document was skipped, as this month's allowance of documents was used up") {
		t.Errorf("skipped: exit %d: %v", code, err)
	}
}

// More files than one preflight takes go a preflight's worth at a time,
// each with its plan; a later preflight that fails leaves the rest for the
// next run.
func TestUploadManyFiles(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	var names []string
	for i := range upload.MaxPreflightFiles + 2 {
		names = append(names, "invoices/"+strings.Repeat("0", 3-len(itoa(i)))+itoa(i)+".pdf")
	}
	uploadDir(t, names...)
	s := newUploadServer(t)
	s.remaining = 200
	stdout, _, err := run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "invoices", "-r", "--no-wait")
	if err != nil {
		t.Fatal(err)
	}
	if preflights, uploads := s.counts(); preflights != 2 || uploads != len(names) {
		t.Errorf("%d preflights, %d uploads", preflights, uploads)
	}
	for _, want := range []string{"Uploading to Acme (files 1 to 100 of 102): 100 new\n", "Uploading to Acme (files 101 to 102 of 102): 2 new\n", "102 uploaded: 102 still being read.\n"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q", want)
		}
	}

	s = newUploadServer(t)
	s.remaining = 200
	s.preflightHook = func(n int, w http.ResponseWriter, r *http.Request) bool {
		if n == 2 {
			answerJSON(w, http.StatusServiceUnavailable, `{"error":"Service unavailable"}`)
			return true
		}
		return false
	}
	stdout, _, err = run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "invoices", "-r", "--no-wait")
	if code, _ := Classify(err); code != ExitUnavailable || !strings.Contains(stdout, "2 files were not sent: Service unavailable.\n") {
		t.Errorf("exit %d: %v\n%s", code, err, stdout)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

// A refusal of one request over the limits (a proxy's, or the service's
// own) is that request's; the next goes as usual.
func TestUploadRequestOverTheLimits(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	uploadDir(t, "LSEG-2026-09.pdf", "BBG-88123.pdf")
	s := newUploadServer(t)
	s.uploadHook = func(n int, w http.ResponseWriter, r *http.Request) bool {
		if n == 1 {
			_, _ = io.Copy(io.Discard, r.Body)
			answerJSON(w, http.StatusRequestEntityTooLarge, `<html>413 Request Entity Too Large</html>`)
			return true
		}
		return false
	}
	stdout, _, err := run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "LSEG-2026-09.pdf", "BBG-88123.pdf")
	if code, _ := Classify(err); code != ExitFailed {
		t.Errorf("exit %d: %v", code, err)
	}
	if !strings.Contains(stdout, "LSEG-2026-09.pdf: not stored\n  the upload (") || !strings.Contains(stdout, "BBG-88123.pdf: invoice, ready for review") {
		t.Errorf("stdout = %s", stdout)
	}
}
