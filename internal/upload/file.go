// Package upload sends documents to a firmfact workspace for its intake to
// read, and reads back what became of them (firmfact upload).
//
// An upload goes in three steps. Hash reads each file once for its size
// and SHA-256. Preflight sends those, and no bytes, for the server to say
// which files are new, which are already in the workspace, how they group
// and how much of the monthly allowance is left. Upload then sends one file,
// or one group of related files, a request, streamed from disk and checked
// against the hash on the way; Documents, Recent and Document read the
// results back in document_result/1 (see Document).
package upload

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// File is one file to upload, as Hash found it.
type File struct {
	// Path is where the file is, for messages.
	Path string
	// Name is what the server is told the file is called: the last
	// element of Path, unless the caller names it otherwise (standard
	// input has no name of its own). The server cleans it up further.
	Name string
	// Size is the file's length in bytes.
	Size int64
	// SHA256 is the file's SHA-256 in lower-case hex, which the server
	// finds duplicates by and checks the bytes it gets against.
	SHA256 string

	// info is the file Hash read, which each attempt at sending it must
	// find at Path again (see same).
	info os.FileInfo
}

// Hash reads the file at path once, a piece at a time, for its size and
// SHA-256. Only a regular file will do: one the upload can read again, from
// the start, for each attempt, and find unchanged. A file of more than
// MaxFileBytes is a *TooLargeError, and is not read beyond that: firmfact
// would refuse it, and a file such as /proc/self/pagemap, whose size says
// 0, would otherwise be read for ever.
func Hash(path string) (File, error) {
	f, info, err := openRegular(path)
	if err != nil {
		return File{}, err
	}
	// Closing a file that was only read loses nothing.
	defer func() { _ = f.Close() }()
	if info.Size() > MaxFileBytes {
		return File{}, &TooLargeError{Path: path, Limit: MaxFileBytes}
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, MaxFileBytes+1))
	switch {
	case err != nil:
		return File{}, fmt.Errorf("reading %s: %w", path, err)
	case n > MaxFileBytes:
		return File{}, &TooLargeError{Path: path, Limit: MaxFileBytes}
	}
	return File{Path: path, Name: filepath.Base(path), Size: n, SHA256: hex.EncodeToString(h.Sum(nil)), info: info}, nil
}

// openRegular opens the file at path for reading, when it is a regular
// file, and says what it found. The type comes from the file opened, not
// from the path beforehand, which could be swapped in between; and the
// open does not wait (openFlags), which it would on a pipe with no writer.
func openRegular(path string) (*os.File, os.FileInfo, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|openFlags, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", path)
	}
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return f, info, nil
}

// same reports whether opened is still the file Hash read: the same file
// (not another one moved or linked into its place since), of the same size
// and last changed at the same time. A File that Hash did not make has
// nothing to compare, and passes.
func (f *File) same(opened *os.File) bool {
	if f.info == nil {
		return true
	}
	info, err := opened.Stat()
	return err == nil && os.SameFile(info, f.info) && info.Size() == f.info.Size() && info.ModTime().Equal(f.info.ModTime())
}

// ChangedError is a file that is no longer what Hash found: it grew,
// shrank or changed while it was being uploaded. The upload stops before
// the request is complete, so the server stores nothing of it.
type ChangedError struct {
	Path string
}

func (e *ChangedError) Error() string {
	return e.Path + " changed while it was being uploaded, so nothing was stored; upload it again once it is complete"
}
