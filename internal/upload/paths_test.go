package upload

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"
)

// tree makes the files named, each holding its own name, below a new
// directory, and makes that directory the working one for the test.
func tree(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
	return dir
}

// paths are the sources' paths, with forward slashes, and each marked * when
// named.
func paths(f *Found) []string {
	var out []string
	for _, s := range f.Sources {
		p := filepath.ToSlash(s.Path)
		if s.Named {
			p += "*"
		}
		out = append(out, p)
	}
	return out
}

func sameList(got []string, want ...string) bool {
	return strings.Join(got, " ") == strings.Join(want, " ")
}

// Files come in the order the arguments give them, each once; a pattern's
// matches and a folder's files in lexical order, marked as found rather
// than named; hidden files are left out of both, and counted.
func TestExpand(t *testing.T) {
	tree(t, "b.pdf", "a.pdf", "notes.txt", ".DS_Store",
		"2026-09/LSEG.pdf", "2026-09/BBG.pdf", "2026-09/.git/config", "2026-09/q3/FactSet.pdf")

	f, err := Expand([]string{"notes.txt", "*.pdf", "a.pdf", "-", "2026-09"}, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"notes.txt*", "a.pdf", "b.pdf", "-*", "2026-09/BBG.pdf", "2026-09/LSEG.pdf", "2026-09/q3/FactSet.pdf"}
	if !sameList(paths(f), want...) {
		t.Errorf("sources = %v\nwant      %v", paths(f), want)
	}
	if f.Hidden != 1 || len(f.Folders) != 0 {
		t.Errorf("hidden %d, folders %v", f.Hidden, f.Folders)
	}

	// Without recursion, a folder a pattern matches is left out and named;
	// one named itself is an error.
	f, err = Expand([]string{"*"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !sameList(paths(f), "a.pdf", "b.pdf", "notes.txt") || !sameList(f.Folders, "2026-09") || f.Hidden != 1 {
		t.Errorf("* = %v, folders %v, hidden %d", paths(f), f.Folders, f.Hidden)
	}
	var folder *FolderError
	if _, err := Expand([]string{"a.pdf", "2026-09"}, false); !errors.As(err, &folder) || folder.Path != "2026-09" || err.Error() != "2026-09 is a folder" {
		t.Errorf("a folder without recursion: %v", err)
	}

	// A pattern that asks for hidden files gets them.
	f, err = Expand([]string{".*"}, false)
	if err != nil || !sameList(paths(f), ".DS_Store") {
		t.Errorf(".* = %v, %v", paths(f), err)
	}
}

// A path that is not there, a pattern that matches nothing and one that is
// not a pattern are errors, before anything is read.
func TestExpandRefuses(t *testing.T) {
	tree(t, "a.pdf", "folder/b.pdf", ".hidden")

	if _, err := Expand([]string{"a.pdf", "missing.pdf"}, false); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing file: %v", err)
	}
	var none *NoMatchError
	if _, err := Expand([]string{"*.docx"}, false); !errors.As(err, &none) || err.Error() != "no files match *.docx" {
		t.Errorf("no match: %v", err)
	}
	// A pattern that matches only hidden files matches none the user sees.
	if _, err := Expand([]string{"[.]*"}, false); !errors.As(err, &none) {
		t.Errorf("no visible match: %v", err)
	}
	if _, err := Expand([]string{"[unclosed"}, false); err == nil || !strings.Contains(err.Error(), "not a pattern") {
		t.Errorf("a bad pattern: %v", err)
	}
}

// A file whose name holds a pattern's characters is that file, not a
// pattern.
func TestExpandNameWithPatternCharacters(t *testing.T) {
	tree(t, "[draft] invoice.pdf", "d invoice.pdf")
	f, err := Expand([]string{"[draft] invoice.pdf"}, false)
	if err != nil || !sameList(paths(f), "[draft] invoice.pdf*") {
		t.Errorf("got %v, %v", paths(f), err)
	}
}

// A folder holds the files that links in it point at, but a link to a
// folder is not followed, since it could lead back; a folder named through
// a link is read.
func TestExpandLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need extra rights on Windows")
	}
	dir := tree(t, "docs/a.pdf", "elsewhere/b.pdf")
	for _, link := range [][2]string{
		{filepath.Join(dir, "elsewhere", "b.pdf"), filepath.Join(dir, "docs", "b.pdf")},
		{filepath.Join(dir, "docs"), filepath.Join(dir, "docs", "loop")},
		{filepath.Join(dir, "missing.pdf"), filepath.Join(dir, "docs", "broken.pdf")},
		{filepath.Join(dir, "docs"), filepath.Join(dir, "via")},
	} {
		if err := os.Symlink(link[0], link[1]); err != nil {
			t.Fatal(err)
		}
	}
	f, err := Expand([]string{"docs"}, true)
	if err != nil || !sameList(paths(f), "docs/a.pdf", "docs/b.pdf") {
		t.Errorf("docs = %v, %v", paths(f), err)
	}
	f, err = Expand([]string{"via"}, true)
	if err != nil || !sameList(paths(f), "via/a.pdf", "via/b.pdf") {
		t.Errorf("via = %v, %v", paths(f), err)
	}
	// A pattern skips a link that leads nowhere.
	f, err = Expand([]string{"docs/*.pdf"}, false)
	if err != nil || !sameList(paths(f), "docs/a.pdf", "docs/b.pdf") {
		t.Errorf("docs/*.pdf = %v, %v", paths(f), err)
	}
}

// A folder that cannot be read stops the search, rather than uploading
// only some of what the user asked for.
func TestExpandUnreadableFolder(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a folder the test cannot read")
	}
	dir := tree(t, "docs/a.pdf", "docs/locked/b.pdf")
	locked := filepath.Join(dir, "docs", "locked")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o750) })
	if _, err := Expand([]string{"docs"}, true); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("got %v", err)
	}
}

// Standard input is copied to a file of its own, up to the limit; more is
// refused and nothing is kept.
func TestSpool(t *testing.T) {
	dir := t.TempDir()
	s, err := Spool(strings.NewReader("%PDF-1.7 piped"), dir, 20)
	if err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(s.Path); err != nil || string(raw) != "%PDF-1.7 piped" {
		t.Errorf("copy = %q, %v", raw, err)
	}
	s.Remove()
	if _, err := os.Stat(s.Path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the copy is still there: %v", err)
	}

	var tooLarge *TooLargeError
	if _, err := Spool(strings.NewReader(strings.Repeat("x", 21)), dir, 20); !errors.As(err, &tooLarge) || tooLarge.Limit != 20 {
		t.Errorf("over the limit: %v", err)
	}
	if _, err := Spool(iotest.ErrReader(errors.New("pipe broke")), dir, 20); err == nil || !strings.Contains(err.Error(), "pipe broke") {
		t.Errorf("a read error: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("left behind: %v", entries)
	}
	if _, err := Spool(strings.NewReader("x"), filepath.Join(dir, "missing"), 20); err == nil {
		t.Error("a directory that is not there made a copy")
	}
}
