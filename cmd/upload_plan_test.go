package cmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/firmfact/cli/internal/upload"
)

// preflighted is every file name the server was asked about.
func (s *uploadServer) preflightedNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, names := range s.preflighted {
		out = append(out, names...)
	}
	return out
}

// A file a folder holds that firmfact does not read is left out before it
// is read: its name, size and checksum never reach the server. Office's
// lock file beside an open workbook is hidden. A file named on the command
// line goes to the server for its verdict all the same.
func TestUploadNeverAsksAboutFilesItDoesNotRead(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	s := newUploadServer(t)
	uploadDir(t, "inbox/LSEG-2026-09.pdf", "inbox/notes.exe", "inbox/id_ed25519", "inbox/Thumbs.db", "inbox/~$Budget.xlsx")

	stdout, stderr, err := run("test", "--host", s.URL(), "--workspace", "Acme", "--json", "upload", "inbox", "-r")
	if err != nil {
		t.Fatalf("%v (stderr %q)", err, stderr)
	}
	if got := s.preflightedNames(); !slices.Equal(got, []string{"LSEG-2026-09.pdf"}) {
		t.Errorf("the server was asked about %v", got)
	}
	var got uploadJSON
	decodeOnly(t, "upload", stdout, &got)
	if len(got.Data.Results) != 4 {
		t.Fatalf("results = %+v", got.Data.Results)
	}
	for _, r := range got.Data.Results[1:] {
		if r.Outcome != outcomeSkipped || r.Code != "unsupported_type" || r.SHA256 != "" || r.Size == 0 {
			t.Errorf("%s: %+v", r.Path, r)
		}
	}
	if !strings.Contains(stdout, `"size"`) || !slices.Contains(got.Notes, "Left out 1 hidden file.") {
		t.Errorf("notes = %q", got.Notes)
	}

	if _, _, err := run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "inbox/notes.exe"); err == nil {
		t.Error("a named file firmfact does not read was not refused")
	}
	if got := s.preflightedNames(); !slices.Contains(got, "notes.exe") {
		t.Errorf("a named file was not asked about: %v", got)
	}
}

// A file larger than firmfact takes is refused before it is read, whether
// named or found, and the rest go.
func TestUploadRefusesATooLargeFileUnread(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	s := newUploadServer(t)
	dir := uploadDir(t, "big/LSEG-2026-09.pdf")
	f, err := os.Create(filepath.Join(dir, "big", "scan-all.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	// Sparse: nothing is written, and nothing is read.
	if err := f.Truncate(60 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()

	stdout, _, err := run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "big", "-r")
	if code, _ := Classify(err); code != ExitFailed || err.Error() != "1 file was not stored" {
		t.Errorf("exit %d: %v", code, err)
	}
	for _, want := range []string{
		"Uploading to Acme: 1 new, 1 refused\n",
		"  scan-all.pdf: files of 50 MB or more are not supported, and this one is 60 MB\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in\n%s", want, stdout)
		}
	}
	if strings.Count(stdout, "scan-all.pdf") != 1 {
		t.Errorf("the refusal is said more than once:\n%s", stdout)
	}
	if got := s.preflightedNames(); !slices.Equal(got, []string{"LSEG-2026-09.pdf"}) {
		t.Errorf("the server was asked about %v", got)
	}
}

// When the CLI settles every file itself, nothing is asked or sent, and
// the plan says why.
func TestUploadWithNothingToAsk(t *testing.T) {
	isolate(t)
	s := newUploadServer(t)
	uploadDir(t, "inbox/notes.exe", "inbox/setup.msi")

	stdout, _, err := run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "inbox", "-r")
	if code, _ := Classify(err); code != ExitFailed || err.Error() != "nothing was uploaded: firmfact does not read any of the files found" {
		t.Errorf("exit %d: %v", code, err)
	}
	want := "Nothing to send to Acme\n  Left out, as firmfact does not read files of their type: notes.exe, setup.msi\n"
	if stdout != want {
		t.Errorf("stdout = %q\nwant     %q", stdout, want)
	}
	if preflights, uploads := s.counts(); preflights != 0 || uploads != 0 {
		t.Errorf("%d preflights, %d uploads", preflights, uploads)
	}
}

// sidSets recognises the files whose names start with sid- as one
// Bloomberg SID report set, as the service recognises such a set.
func sidSets(names []string) []map[string]any {
	var set []string
	for _, name := range names {
		if strings.HasPrefix(name, "sid-") {
			set = append(set, name)
		}
	}
	if len(set) < 2 {
		return nil
	}
	return []map[string]any{{"filenames": set, "kind": "bloomberg_sid_single_account", "bytes": 0, "status": "ok",
		"guidance": map[string]any{"status": "complete", "title": "Bloomberg SID report set", "message": "All parts of the set are here."}}}
}

// A set the server recognises by name goes as one request, and the plan
// calls it by its title. A set at the end of a preflight's worth is seen
// whole by the next one, rather than split between two requests.
func TestUploadSendsARecognisedSetWhole(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)

	t.Run("named", func(t *testing.T) {
		s := newUploadServer(t)
		s.sets = sidSets
		uploadDir(t, "sid-a.csv", "sid-b.csv", "other.pdf")
		stdout, _, err := run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "sid-a.csv", "sid-b.csv", "other.pdf", "--no-wait")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stdout, "  Sent together as one Bloomberg SID report set: sid-a.csv, sid-b.csv\n    All parts of the set are here.\n") {
			t.Errorf("stdout = %s", stdout)
		}
		if got := strings.Join(s.sentFiles(), " "); got != "sid-a.csv+sid-b.csv other.pdf" {
			t.Errorf("sent %s", got)
		}
	})
	t.Run("across a preflight's worth", func(t *testing.T) {
		s := newUploadServer(t)
		s.sets = sidSets
		s.remaining = 200
		var names []string
		for i := range 98 {
			names = append(names, fmt.Sprintf("inbox/a%03d.pdf", i))
		}
		names = append(names, "inbox/sid-1.csv", "inbox/sid-2.csv", "inbox/sid-3.csv", "inbox/sid-4.csv", "inbox/z1.pdf", "inbox/z2.pdf", "inbox/z3.pdf")
		uploadDir(t, names...)
		if _, _, err := run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "inbox", "-r", "--no-wait"); err != nil {
			t.Fatal(err)
		}
		if got := s.sentFiles(); !slices.Contains(got, "sid-1.csv+sid-2.csv+sid-3.csv+sid-4.csv") {
			t.Errorf("the set went split: %v", got[95:])
		}
		if preflights, uploads := s.counts(); preflights != 2 || uploads != len(names)-3 {
			t.Errorf("%d preflights, %d uploads", preflights, uploads)
		}
	})
}

// A found file the server refuses by its name alone, such as a Data
// License delivery that only the web app takes, is left out with the
// server's reason, not as a type firmfact does not read.
func TestUploadSaysWhyTheServerLeftAFileOut(t *testing.T) {
	isolate(t)
	fastUploadPolls(t)
	s := newUploadServer(t)
	s.byName = func(name string) (string, string) {
		if strings.HasPrefix(name, "DL_") {
			return "unsupported_type", name + ": files named as a Data License delivery or a SQL Server backup can only be uploaded in the web app, so it was not stored."
		}
		return "", ""
	}
	uploadDir(t, "inbox/DL_usage.csv", "inbox/LSEG-2026-09.pdf")
	stdout, _, err := run("test", "--host", s.URL(), "--workspace", "Acme", "upload", "inbox", "-r")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "  DL_usage.csv: files named as a Data License delivery or a SQL Server backup can only be uploaded in the web app, so it was not stored.\n") ||
		strings.Contains(stdout, "does not read files of their type") {
		t.Errorf("stdout = %s", stdout)
	}
}

// Files refused for one reason take one line, however many there are:
// the server's sentences start with each file's name, which the line lists
// in its place.
func TestRefusalsTakeALinePerReason(t *testing.T) {
	var files []*uploadFile
	for i := range 7 {
		name := fmt.Sprintf("%02d.pdf", i)
		files = append(files, &uploadFile{name: name, outcome: outcomeRefused, code: "quota_exceeded",
			message: name + ": this month's document limit would be exceeded, so it would not be stored. The limit resets on October 01, 2026."})
	}
	for _, name := range []string{"a.pdf", "b.pdf"} {
		files = append(files, &uploadFile{name: name, outcome: outcomeRefused, code: "too_many_files",
			message: "You can upload at most 10 files at a time, and 11 are selected. Upload the rest separately."})
	}
	files = append(files, &uploadFile{name: "odd.pdf", outcome: outcomeRefused, code: "odd_code"},
		&uploadFile{name: "one.pdf", outcome: outcomeRefused, message: "one.pdf: the file is empty, so it was not stored."})
	var out bytes.Buffer
	printRefusals(&out, files)
	want := "  00.pdf, 01.pdf, 02.pdf, 03.pdf, 04.pdf and 2 more: this month's document limit would be exceeded, so it would not be stored. The limit resets on October 01, 2026.\n" +
		"  a.pdf, b.pdf: You can upload at most 10 files at a time, and 11 are selected. Upload the rest separately.\n" +
		"  odd.pdf: refused (odd_code)\n" +
		"  one.pdf: the file is empty, so it was not stored.\n"
	if out.String() != want {
		t.Errorf("got\n%s\nwant\n%s", out.String(), want)
	}
	for _, f := range files {
		if !f.listed {
			t.Errorf("%s is not marked as listed", f.name)
		}
	}
}

// A group close to the most one request can carry is warned of in the
// plan, since the CLI sends each request whole.
func TestPlanWarnsOfAGroupNearTheRequestLimit(t *testing.T) {
	r := &uploadRun{flags: uploadFlags{related: true}}
	big := sendGroup{related: true, members: []*uploadFile{
		{file: upload.File{Name: "invoice.pdf", Size: 46 << 20}},
		{file: upload.File{Name: "usage.xlsx", Size: 46 << 20}},
	}}
	var out bytes.Buffer
	r.printGroup(&out, big)
	if !strings.Contains(out.String(), "    Together 92 MB, close to the most one request can carry; if it is refused as too large, upload fewer of these files at a time.\n") {
		t.Errorf("got %q", out.String())
	}
	small := sendGroup{members: []*uploadFile{{file: upload.File{Name: "a.csv", Size: 10}}, {file: upload.File{Name: "b.csv", Size: 10}}}}
	out.Reset()
	r.printGroup(&out, small)
	if out.String() != "  Sent together as one set of related reports: a.csv, b.csv\n" {
		t.Errorf("got %q", out.String())
	}
}
