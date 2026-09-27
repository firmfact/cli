package upload

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// Stdin is the argument that names standard input as a file to upload.
const Stdin = "-"

// Source is a file an upload names, before it is read.
type Source struct {
	// Path is the file as the command line gave it, or as a pattern or a
	// folder found it; Stdin for standard input.
	Path string
	// Named is a file the command line names itself, rather than one a
	// pattern matched or a folder held. The CLI leaves a found file out
	// when firmfact does not read its type, and reports a named one as
	// refused: the user asked for that file in particular.
	Named bool

	// target is, for a link a folder held, the file it led to when the
	// folder was read (see Verify).
	target os.FileInfo
}

// Verify reports whether file, which Hash read at s.Path, is the file the
// search found there. A link in a folder counts only for a file in that
// folder (see Expand); turned to another file by the time it is read, it
// would take that one along under an innocent name.
func (s Source) Verify(file File) error {
	if s.target != nil && (file.info == nil || !os.SameFile(s.target, file.info)) {
		return fmt.Errorf("%s no longer leads to the file it did when the folder was read", s.Path)
	}
	return nil
}

// Found is what the arguments of an upload name.
type Found struct {
	// Sources are the files, in the order the arguments give them, each
	// once however many arguments name it. A folder's files come in
	// lexical order, as do a pattern's matches.
	Sources []Source
	// Folders are the folders a pattern matched, left out because the
	// upload is not recursive.
	Folders []string
	// Hidden is how many hidden files and folders a pattern or a folder
	// held, which are left out as a shell leaves them out of *: those
	// whose names start with a dot, such as .DS_Store or .git, and the
	// lock files Word and Excel keep beside an open document, such as
	// ~$Budget.xlsx.
	Hidden int
	// Outside is how many links in the folders named lead out of them,
	// which are left out: a folder's files are the files in it.
	Outside int
}

// FolderError is a folder named without asking for its files.
type FolderError struct{ Path string }

func (e *FolderError) Error() string { return e.Path + " is a folder" }

// NoMatchError is a pattern that matches no file.
type NoMatchError struct{ Pattern string }

func (e *NoMatchError) Error() string { return "no files match " + e.Pattern }

// Expand finds the files args name: a file, a folder (whose files, and its
// folders' files, count with recursive; without, a folder named is a
// *FolderError), a pattern such as *.pdf that the shell left for the CLI
// (Windows shells expand none), or Stdin. A path that does not exist is an
// error that satisfies errors.Is(err, fs.ErrNotExist).
//
// Hidden files and folders are left out of patterns and folders, unless
// the pattern asks for them (.*). A folder does not follow a link to
// another folder, which could lead back to itself, and holds a link to a
// file only when that file is in the folder too. A link named on the
// command line, or matched by a pattern as the shell would have matched
// it, is the file.
func Expand(args []string, recursive bool) (*Found, error) {
	f := &Found{}
	seen := map[string]bool{}
	add := func(s Source) {
		key := s.Path
		if s.Path != Stdin {
			if abs, err := filepath.Abs(s.Path); err == nil {
				key = abs
			}
		}
		if !seen[key] {
			seen[key] = true
			f.Sources = append(f.Sources, s)
		}
	}
	for _, arg := range args {
		if arg == Stdin {
			add(Source{Path: arg, Named: true})
			continue
		}
		info, err := os.Stat(arg)
		switch {
		case err == nil && info.IsDir():
			if !recursive {
				return nil, &FolderError{Path: arg}
			}
			if err := f.walk(arg, add); err != nil {
				return nil, err
			}
		case err == nil:
			add(Source{Path: arg, Named: true})
		case hasMeta(arg) && !errors.Is(err, fs.ErrPermission):
			// No file of that name, so a pattern. Windows says the name is
			// not a valid one rather than not there, as * and ? cannot be
			// in a name.
			if err := f.glob(arg, recursive, add); err != nil {
				return nil, err
			}
		default:
			return nil, err
		}
	}
	return f, nil
}

// hasMeta reports whether s holds a pattern's special characters. A name
// that holds them and exists, such as "[draft] invoice.pdf", is the file.
func hasMeta(s string) bool { return strings.ContainsAny(s, "*?[") }

// foldCase makes a pattern match names whatever their case, as the command
// prompt's own patterns do on Windows, where the CLI is the only one to
// expand them and scanners write SCAN0001.PDF. Elsewhere a pattern matches
// as the shells there match it.
var foldCase = runtime.GOOS == "windows"

func (f *Found) glob(pattern string, recursive bool, add func(Source)) error {
	matches, err := f.match(pattern)
	if err != nil {
		return fmt.Errorf("%s is not a pattern the CLI can read: %w", pattern, err)
	}
	n := 0
	for _, m := range matches {
		if m.hidden {
			f.Hidden++
			continue
		}
		info, err := os.Stat(m.path)
		switch {
		case err != nil:
			// A link to nothing, or a file gone since the pattern found it.
			continue
		case info.IsDir() && recursive:
			if err := f.walk(m.path, add); err != nil {
				return err
			}
		case info.IsDir():
			f.Folders = append(f.Folders, m.path)
		case info.Mode().IsRegular():
			add(Source{Path: m.path})
		default:
			// A device, a socket or a pipe: nothing to upload.
			continue
		}
		n++
	}
	if n == 0 {
		return &NoMatchError{Pattern: pattern}
	}
	return nil
}

// globMatch is a path a pattern matches. hidden is set when a wildcard in
// it matched a hidden name, which a shell's does not.
type globMatch struct {
	path   string
	hidden bool
}

// match finds the paths pattern matches, as filepath.Glob does, except
// that a hidden name (see hiddenName) matched by a part of the pattern
// that does not start the same way is marked hidden, in any part of the
// path: a shell's */*.pdf never looks into .private. With foldCase, case
// does not count.
func (f *Found) match(pattern string) ([]globMatch, error) {
	if _, err := filepath.Match(pattern, ""); err != nil {
		return nil, err
	}
	if !hasMeta(pattern) {
		if _, err := os.Lstat(pattern); err != nil {
			return nil, nil
		}
		return []globMatch{{path: pattern}}, nil
	}
	dir, file := filepath.Split(pattern)
	volume := len(filepath.VolumeName(dir))
	dir = globDir(dir, volume)
	dirs := []globMatch{{path: dir}}
	if hasMeta(dir[volume:]) {
		if dir == pattern {
			return nil, filepath.ErrBadPattern
		}
		var err error
		if dirs, err = f.match(dir); err != nil {
			return nil, err
		}
	}
	var matches []globMatch
	for _, d := range dirs {
		var err error
		if matches, err = matchIn(d, file, matches); err != nil {
			return nil, err
		}
	}
	return matches, nil
}

// globDir is the folder part of a pattern, as filepath.Glob cleans it: the
// working folder for none, and without its last separator unless that is
// the root (/, C:\).
func globDir(dir string, volume int) string {
	switch {
	case dir == "":
		return "."
	case volume+1 == len(dir) && os.IsPathSeparator(dir[len(dir)-1]):
		return dir
	case volume == len(dir) && volume == 2:
		// C: is the working folder of drive C.
		return dir + "."
	}
	return dir[:len(dir)-1]
}

// matchIn adds to out the names in dir that pattern, a pattern for one
// name, matches, in lexical order. A folder that cannot be read matches
// nothing, as for filepath.Glob.
func matchIn(dir globMatch, pattern string, out []globMatch) ([]globMatch, error) {
	d, err := os.Open(dir.path)
	if err != nil {
		return out, nil
	}
	names, _ := d.Readdirnames(-1)
	_ = d.Close()
	slices.Sort(names)
	folded := pattern
	if foldCase {
		folded = strings.ToLower(pattern)
	}
	for _, name := range names {
		candidate := name
		if foldCase {
			candidate = strings.ToLower(name)
		}
		ok, err := filepath.Match(folded, candidate)
		if err != nil {
			return out, err
		}
		if ok {
			hidden := dir.hidden || (hiddenName(name) && !hiddenName(pattern))
			out = append(out, globMatch{path: filepath.Join(dir.path, name), hidden: hidden})
		}
	}
	return out, nil
}

// walk adds the regular files in root and the folders below it.
func (f *Found) walk(root string, add func(Source)) error {
	// With a separator at its end, a root that is a link to a folder is
	// read as the folder; WalkDir would otherwise stop at the link.
	start := root
	if !strings.HasSuffix(start, string(filepath.Separator)) && !strings.HasSuffix(start, "/") {
		start += string(filepath.Separator)
	}
	// Where the folder really is, for the links in it to be inside; found
	// once there is a link to check.
	base, resolved := "", false
	return filepath.WalkDir(start, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == start {
			return nil
		}
		if hiddenName(d.Name()) {
			f.Hidden++
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		switch {
		case d.IsDir():
			return nil
		case d.Type().IsRegular():
			add(Source{Path: path})
		case d.Type()&fs.ModeSymlink != 0:
			if !resolved {
				// Should the folder's own path not resolve, no link in it
				// can be shown to stay inside, and link leaves each out.
				base, _ = realPath(root)
				resolved = true
			}
			f.link(base, path, add)
		}
		return nil
	})
}

// realPath is path from the root, with every link on the way resolved:
// two such paths compare, whether each was given relative or absolute, and
// whatever links lead to them (on macOS, /var is /private/var).
func realPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// link adds the link at path, found in the folder base, when it leads to
// a regular file in that folder that is not hidden. A folder from an
// archive, a checkout or a share can hold links of anyone's making, and
// one to a private file elsewhere would take it along under an innocent
// name, into a workspace its members can read. A link to a folder is not
// followed, as it could lead back.
func (f *Found) link(base, path string, add func(Source)) {
	target, err := realPath(path)
	if err != nil {
		// A link to nothing.
		return
	}
	info, err := os.Stat(target)
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	rel, err := filepath.Rel(base, target)
	switch {
	case base == "" || err != nil || !filepath.IsLocal(rel):
		f.Outside++
	case slices.ContainsFunc(strings.Split(rel, string(filepath.Separator)), hiddenName):
		f.Hidden++
	default:
		add(Source{Path: path, target: info})
	}
}

// hiddenName reports whether a file or folder of this name is hidden: its
// name starts with a dot, or it is a lock file Word or Excel keeps beside
// an open document (~$Budget.xlsx), which is hidden on Windows and holds
// no document.
func hiddenName(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "~$")
}

// extensions are the types of file firmfact reads, by extension, as the
// service lists them (a preflight's limits.extensions).
var extensions = []string{".pdf", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".docx", ".xlsx", ".xls", ".csv", ".tsv", ".txt", ".eml", ".zip"}

// Readable reports whether firmfact reads a file of this name, as far as
// its extension says. A file a folder or a pattern found that it does not
// read is left out before it is read, so that its name, size and checksum
// never leave the machine. The service has the last word on the rest.
//
// The list is the service's (the preflight's limits.extensions); a type it
// adds reaches found files once the CLI lists it too, and a named file
// goes to the service whatever its type.
func Readable(name string) bool {
	return slices.Contains(extensions, strings.ToLower(filepath.Ext(name)))
}

// MaxFileBytes is the most one file may hold: firmfact's limit, which each
// preflight states as Limits.MaxFileBytes. Standard input is read up to it,
// since the CLI keeps a copy of what it reads there.
const MaxFileBytes = 50<<20 - 1

// TooLargeError is a file, or standard input, that holds more than Limit
// bytes.
type TooLargeError struct {
	// Path is the file, or empty for standard input.
	Path  string
	Limit int64
}

func (e *TooLargeError) Error() string {
	what := "standard input"
	if e.Path != "" {
		what = e.Path
	}
	return fmt.Sprintf("%s holds more than %d bytes, the most one file may be", what, e.Limit)
}

// Spooled is standard input, copied to a file of its own: an upload reads
// its file more than once (to hash it, then for each attempt at sending
// it), and a pipe can be read only once.
type Spooled struct {
	// Path is the copy.
	Path string
}

// Spool copies r, up to limit bytes, to a new file in dir (the system's
// temporary directory when dir is empty). More than limit is a
// *TooLargeError, and nothing is kept. Remove deletes the copy.
func Spool(r io.Reader, dir string, limit int64) (*Spooled, error) {
	tmp, err := os.CreateTemp(dir, "firmfact-upload-*")
	if err != nil {
		return nil, err
	}
	s := &Spooled{Path: tmp.Name()}
	n, err := io.Copy(tmp, io.LimitReader(r, limit+1))
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	switch {
	case err != nil:
		s.Remove()
		return nil, fmt.Errorf("reading standard input: %w", err)
	case n > limit:
		s.Remove()
		return nil, &TooLargeError{Limit: limit}
	}
	return s, nil
}

// Remove deletes the copy. It is gone either way once the upload is over,
// so an error has no one to go to.
func (s *Spooled) Remove() { _ = os.Remove(s.Path) }
