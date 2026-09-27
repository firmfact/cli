package upload

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
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
	// Hidden is how many hidden files and folders (their names start with
	// a dot, such as .DS_Store or .git) a pattern or a folder held, which
	// are left out as a shell leaves them out of *.
	Hidden int
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
// the pattern asks for them (.*), and a folder does not follow a link to
// another folder, which could lead back to itself. A link to a file is
// the file.
func Expand(args []string, recursive bool) (*Found, error) {
	f := &Found{}
	seen := map[string]bool{}
	add := func(path string, named bool) {
		key := path
		if path != Stdin {
			if abs, err := filepath.Abs(path); err == nil {
				key = abs
			}
		}
		if !seen[key] {
			seen[key] = true
			f.Sources = append(f.Sources, Source{Path: path, Named: named})
		}
	}
	for _, arg := range args {
		if arg == Stdin {
			add(arg, true)
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
			add(arg, true)
		case errors.Is(err, fs.ErrNotExist) && hasMeta(arg):
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

func (f *Found) glob(pattern string, recursive bool, add func(string, bool)) error {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("%s is not a pattern the CLI can read: %w", pattern, err)
	}
	// Go's * matches a leading dot; a shell's does not, and a user who
	// types *.pdf expects the shell's answer.
	dots := strings.HasPrefix(filepath.Base(pattern), ".")
	n := 0
	for _, m := range matches {
		if !dots && hidden(m) {
			f.Hidden++
			continue
		}
		info, err := os.Stat(m)
		switch {
		case err != nil:
			// A link to nothing, or a file gone since the pattern found it.
			continue
		case info.IsDir() && recursive:
			if err := f.walk(m, add); err != nil {
				return err
			}
		case info.IsDir():
			f.Folders = append(f.Folders, m)
		case info.Mode().IsRegular():
			add(m, false)
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

// walk adds the regular files in root and the folders below it.
func (f *Found) walk(root string, add func(string, bool)) error {
	// With a separator at its end, a root that is a link to a folder is
	// read as the folder; WalkDir would otherwise stop at the link.
	start := root
	if !strings.HasSuffix(start, string(filepath.Separator)) && !strings.HasSuffix(start, "/") {
		start += string(filepath.Separator)
	}
	return filepath.WalkDir(start, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == start {
			return nil
		}
		if hidden(path) {
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
			add(path, false)
		case d.Type()&fs.ModeSymlink != 0:
			if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
				add(path, false)
			}
		}
		return nil
	})
}

func hidden(path string) bool {
	return strings.HasPrefix(filepath.Base(path), ".")
}

// MaxFileBytes is the most one file may hold: firmfact's limit, which each
// preflight states as Limits.MaxFileBytes. Standard input is read up to it,
// since the CLI keeps a copy of what it reads there.
const MaxFileBytes = 50<<20 - 1

// TooLargeError is standard input that held more than MaxFileBytes.
type TooLargeError struct{ Limit int64 }

func (e *TooLargeError) Error() string {
	return fmt.Sprintf("standard input holds more than %d bytes, the most one file may be", e.Limit)
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
