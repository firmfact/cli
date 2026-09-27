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
}

// Hash reads the file at path once, a piece at a time, for its size and
// SHA-256. Only a regular file will do: one the upload can read again, from
// the start, for each attempt, and find unchanged.
func Hash(path string) (File, error) {
	info, err := os.Stat(path)
	if err != nil {
		return File{}, err
	}
	if !info.Mode().IsRegular() {
		return File{}, fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return File{}, err
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	// Closing a file that was only read loses nothing.
	_ = f.Close()
	if err != nil {
		return File{}, fmt.Errorf("reading %s: %w", path, err)
	}
	return File{Path: path, Name: filepath.Base(path), Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
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
