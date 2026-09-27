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

// A folder holds a link to a file in it, under the link's name, but not a
// link that leads out of it, to a hidden file in it, or to a folder, which
// could lead back; a folder named through a link is read. A pattern takes
// the links it matches, as a shell's would.
func TestExpandLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need extra rights on Windows")
	}
	dir := tree(t, "docs/a.pdf", "docs/sub/c.pdf", "docs/.private/key.txt", "elsewhere/b.pdf")
	links := [][2]string{
		{filepath.Join(dir, "elsewhere", "b.pdf"), filepath.Join(dir, "docs", "b.pdf")},
		{filepath.Join("sub", "c.pdf"), filepath.Join(dir, "docs", "inner.pdf")},
		{filepath.Join(dir, "docs", "a.pdf"), filepath.Join(dir, "docs", "abs.pdf")},
		{filepath.Join(".private", "key.txt"), filepath.Join(dir, "docs", "notes.txt")},
		{filepath.Join(dir, "docs"), filepath.Join(dir, "docs", "loop")},
		{filepath.Join(dir, "missing.pdf"), filepath.Join(dir, "docs", "broken.pdf")},
		{filepath.Join(dir, "docs"), filepath.Join(dir, "via")},
	}
	if runtime.GOOS == "linux" {
		links = append(links, [2]string{"/proc/self/environ", filepath.Join(dir, "docs", "env.txt")})
	}
	for _, link := range links {
		if err := os.Symlink(link[0], link[1]); err != nil {
			t.Fatal(err)
		}
	}
	outside := 1
	if runtime.GOOS == "linux" {
		outside = 2
	}
	f, err := Expand([]string{"docs"}, true)
	if err != nil || !sameList(paths(f), "docs/a.pdf", "docs/abs.pdf", "docs/inner.pdf", "docs/sub/c.pdf") || f.Outside != outside || f.Hidden != 2 {
		t.Errorf("docs = %v, %d outside, %d hidden, %v", paths(f), f.Outside, f.Hidden, err)
	}
	f, err = Expand([]string{"via"}, true)
	if err != nil || !sameList(paths(f), "via/a.pdf", "via/abs.pdf", "via/inner.pdf", "via/sub/c.pdf") || f.Outside != outside {
		t.Errorf("via = %v, %d outside, %v", paths(f), f.Outside, err)
	}
	// A pattern skips a link that leads nowhere.
	f, err = Expand([]string{"docs/*.pdf"}, false)
	if err != nil || !sameList(paths(f), "docs/a.pdf", "docs/abs.pdf", "docs/b.pdf", "docs/inner.pdf") {
		t.Errorf("docs/*.pdf = %v, %v", paths(f), err)
	}
}

// A link a folder held must still lead to the same file when it is read:
// turned to another since, even one with the same bytes, it is refused.
func TestVerifyALinkStillLeadsWhereItDid(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need extra rights on Windows")
	}
	dir := tree(t, "docs/a.pdf", "docs/sub/a.pdf")
	link := filepath.Join(dir, "docs", "link.pdf")
	if err := os.Symlink("a.pdf", link); err != nil {
		t.Fatal(err)
	}
	f, err := Expand([]string{"docs"}, true)
	if err != nil || len(f.Sources) != 3 {
		t.Fatalf("%v, %v", paths(f), err)
	}
	s := f.Sources[1]
	file, err := Hash(s.Path)
	if err != nil || s.Verify(file) != nil {
		t.Fatalf("the link as found: %v, %v", err, s.Verify(file))
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("sub", "a.pdf"), link); err != nil {
		t.Fatal(err)
	}
	file, err = Hash(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(file); err == nil || !strings.Contains(err.Error(), "no longer leads to the file") {
		t.Errorf("a link turned elsewhere: %v", err)
	}
	// A file named, or one a pattern found, is whatever is there.
	if err := (Source{Path: s.Path, Named: true}).Verify(file); err != nil {
		t.Errorf("a named file: %v", err)
	}
}

// A wildcard in a folder's part of a pattern does not look into hidden
// folders, as a shell's does not; one that starts with a dot does.
func TestExpandLeavesHiddenFoldersOutOfPatterns(t *testing.T) {
	tree(t, "public/invoice.pdf", ".private/salary.pdf", "public/~$Budget.xlsx", "public/Budget.xlsx")
	f, err := Expand([]string{"*/*.pdf"}, false)
	if err != nil || !sameList(paths(f), "public/invoice.pdf") || f.Hidden != 1 {
		t.Errorf("*/*.pdf = %v, hidden %d, %v", paths(f), f.Hidden, err)
	}
	f, err = Expand([]string{".*/*.pdf"}, false)
	if err != nil || !sameList(paths(f), ".private/salary.pdf") {
		t.Errorf(".*/*.pdf = %v, %v", paths(f), err)
	}
	// Office's lock file beside an open workbook is hidden too, in a
	// pattern and in a folder, unless the pattern asks for it.
	f, err = Expand([]string{"public/*.xlsx"}, false)
	if err != nil || !sameList(paths(f), "public/Budget.xlsx") || f.Hidden != 1 {
		t.Errorf("public/*.xlsx = %v, hidden %d, %v", paths(f), f.Hidden, err)
	}
	f, err = Expand([]string{"public"}, true)
	if err != nil || !sameList(paths(f), "public/Budget.xlsx", "public/invoice.pdf") || f.Hidden != 1 {
		t.Errorf("public = %v, hidden %d, %v", paths(f), f.Hidden, err)
	}
	f, err = Expand([]string{"public/~$*"}, false)
	if err != nil || !sameList(paths(f), "public/~$Budget.xlsx") {
		t.Errorf("public/~$* = %v, %v", paths(f), err)
	}
}

// Where patterns ignore case, as on Windows, *.pdf finds SCAN001.PDF, under
// the name it has.
func TestExpandFoldsCase(t *testing.T) {
	tree(t, "caps/SCAN001.PDF", "caps/scan002.pdf", "CAPS2/a.Pdf")
	f, err := Expand([]string{"caps/*.pdf"}, false)
	if err != nil || !sameList(paths(f), "caps/scan002.pdf") {
		t.Errorf("case counts: %v, %v", paths(f), err)
	}
	prev := foldCase
	foldCase = true
	t.Cleanup(func() { foldCase = prev })
	f, err = Expand([]string{"caps/*.pdf", "CAPS2/*.PDF", "c[A-Z]ps/scan00[12].pdf"}, false)
	if err != nil || !sameList(paths(f), "caps/SCAN001.PDF", "caps/scan002.pdf", "CAPS2/a.Pdf") {
		t.Errorf("case folded: %v, %v", paths(f), err)
	}
	if f, err := Expand([]string{"*/*.pdf"}, false); err != nil || !sameList(paths(f), "CAPS2/a.Pdf", "caps/SCAN001.PDF", "caps/scan002.pdf") {
		t.Errorf("*/*.pdf folded: %v, %v", paths(f), err)
	}
}

// A pattern is tried whenever no file has that name: on Windows the name
// is not even a valid one, which is not "not there". A folder the CLI may
// not read is an error, not a pattern that matched nothing.
func TestExpandTriesAPatternForAnyNameThatIsNoFile(t *testing.T) {
	dir := tree(t, "a.pdf")
	var none *NoMatchError
	// Under a file rather than a folder: not "not there" either.
	if _, err := Expand([]string{"a.pdf/*.pdf"}, false); !errors.As(err, &none) {
		t.Errorf("a pattern below a file: %v", err)
	}
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		return
	}
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o750) })
	if _, err := Expand([]string{"locked/*.pdf"}, false); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("a folder that cannot be read: %v", err)
	}
}

// A pattern matches from the root, or from a folder named as it is.
func TestExpandPatternFolders(t *testing.T) {
	dir := tree(t, "a/b/c.pdf")
	abs := filepath.Join(dir, "a", "*", "*.pdf")
	f, err := Expand([]string{abs}, false)
	if err != nil || len(f.Sources) != 1 || f.Sources[0].Path != filepath.Join(dir, "a", "b", "c.pdf") {
		t.Errorf("%s = %v, %v", abs, paths(f), err)
	}
	for dir, want := range map[string]string{"": ".", "a/": "a", string(filepath.Separator): string(filepath.Separator)} {
		if got := globDir(dir, 0); got != want {
			t.Errorf("globDir(%q) = %q, want %q", dir, got, want)
		}
	}
	if got := globDir("C:", 2); got != "C:." {
		t.Errorf("globDir(C:) = %q", got)
	}
	if got := globDir("C:/", 2); got != "C:/" {
		t.Errorf("globDir(C:/) = %q", got)
	}
}

// Firmfact's types, by extension and whatever its case.
func TestReadable(t *testing.T) {
	for name, want := range map[string]bool{
		"a.pdf": true, "SCAN.PDF": true, "report.xlsx": true, "mail.eml": true,
		"notes.exe": false, "Thumbs.db": false, "id_ed25519": false, "vault.kdbx": false, "a.pdf.gpg": false,
	} {
		if Readable(name) != want {
			t.Errorf("Readable(%q) = %v", name, !want)
		}
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
