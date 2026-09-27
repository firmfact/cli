package upload

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime/multipart"
	"net/textproto"
	"os"
	"strings"
	"sync"

	"github.com/firmfact/cli/internal/ui"
)

// body is the multipart form of one upload request, streamed from the
// files rather than held in memory. It is an api.Body: the client opens it
// again for each attempt.
//
// The parts, in order: a sha256[] field for each file, group_as_related
// and new_version when set, then a files[] part for each file, in the same
// order as their checksums. Every file part says application/octet-stream,
// whatever the file: the server works out the type from the bytes and
// refuses a file whose bytes do not match its extension, so a type the CLI
// guessed from the name would only be something to disagree with.
type body struct {
	boundary string
	segments []segment
	size     int64
	summary  string
}

// segment is a stretch of the body: the multipart framing and small
// fields, or the bytes of one file.
type segment struct {
	head []byte
	file *File
}

// newBody lays out the form for files. The framing is built once, and its
// length with the files' sizes is the body's, which the request declares
// up front.
func newBody(files []File, opts Options) (*body, error) {
	if len(files) == 0 {
		return nil, errors.New("no files to upload")
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	b := &body{boundary: mw.Boundary()}
	for _, f := range files {
		if err := mw.WriteField("sha256[]", f.SHA256); err != nil {
			return nil, err
		}
	}
	if opts.Related {
		if err := mw.WriteField("group_as_related", "1"); err != nil {
			return nil, err
		}
	}
	if opts.NewVersion {
		if err := mw.WriteField("new_version", "1"); err != nil {
			return nil, err
		}
	}
	for i := range files {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="files[]"; filename="`+partFilename(files[i].Name)+`"`)
		h.Set("Content-Type", "application/octet-stream")
		if _, err := mw.CreatePart(h); err != nil {
			return nil, err
		}
		b.add(segment{head: bytes.Clone(buf.Bytes())})
		buf.Reset()
		b.add(segment{file: &files[i]})
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	b.add(segment{head: bytes.Clone(buf.Bytes())})
	b.summary = summary(files, opts)
	return b, nil
}

func (b *body) add(s segment) {
	b.segments = append(b.segments, s)
	if s.file != nil {
		b.size += s.file.Size
	} else {
		b.size += int64(len(s.head))
	}
}

// Size is the length of the whole form.
func (b *body) Size() int64 { return b.size }

// NearRequestLimit is a request's size from which it may be too large for
// the network in front of firmfact, which the web app's uploads keep under
// by sending larger ones in pieces. The CLI sends each request whole.
const NearRequestLimit = 90 << 20

// RequestSize is how many bytes one request that uploads files sends: the
// files and the form around them.
func RequestSize(files []File, opts Options) int64 {
	b, err := newBody(files, opts)
	if err != nil {
		return 0
	}
	return b.Size()
}

// ContentType is the form's media type, with its boundary.
func (b *body) ContentType() string { return "multipart/form-data; boundary=" + b.boundary }

// String describes the form for the debug log, which never shows a
// file's bytes.
func (b *body) String() string { return b.summary }

// Open returns the form from its first byte. Each file is opened when the
// form reaches it, found to be the file Hash read (File.same), read in
// pieces, and checked on the way against what Hash found (see fileReader).
func (b *body) Open() (io.ReadCloser, error) {
	return &bodyReader{segments: b.segments}, nil
}

func summary(files []File, opts Options) string {
	parts := make([]string, 0, len(files)+2)
	for _, f := range files {
		parts = append(parts, fmt.Sprintf("files[] %q (%s, sha256 %s)", f.Name, ui.Bytes(f.Size), f.SHA256))
	}
	if opts.Related {
		parts = append(parts, "group_as_related=1")
	}
	if opts.NewVersion {
		parts = append(parts, "new_version=1")
	}
	return "multipart form: " + strings.Join(parts, ", ")
}

// partFilename is name as it goes in the quoted filename of a part's
// Content-Disposition. A name on disk may hold a quote, a backslash or a
// line break, which would end the header or the part early, so those, any
// other control character and the percent sign itself are percent-encoded,
// as browsers encode them. The server (Rack) decodes a filename whose
// percent signs all start such an escape, which encoding every one of them
// makes sure of; anything else, UTF-8 included, goes as it is.
func partFilename(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c < 0x20 || c == 0x7f || c == '"' || c == '\\' || c == '%' {
			fmt.Fprintf(&b, "%%%02X", c)
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// bodyReader reads a body's segments in turn. The transport may close it
// from another goroutine while a Read is under way, so the two take turns.
type bodyReader struct {
	mu       sync.Mutex
	segments []segment
	current  io.Reader
	file     *fileReader // the file being read, if any
	closed   bool
}

func (r *bodyReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		if r.closed {
			return 0, os.ErrClosed
		}
		if r.current == nil {
			if len(r.segments) == 0 {
				return 0, io.EOF
			}
			s := r.segments[0]
			r.segments = r.segments[1:]
			if s.file == nil {
				r.current = bytes.NewReader(s.head)
				continue
			}
			f, _, err := openRegular(s.file.Path)
			if err != nil {
				return 0, err
			}
			if !s.file.same(f) {
				// Another file in its place (a link turned elsewhere, say),
				// or this one changed: not a byte of it goes.
				_ = f.Close()
				return 0, &ChangedError{Path: s.file.Path}
			}
			r.file = &fileReader{f: f, want: s.file, left: s.file.Size, h: sha256.New()}
			r.current = r.file
		}
		n, err := r.current.Read(p)
		if errors.Is(err, io.EOF) {
			r.closeFile()
			r.current = nil
			if n == 0 {
				continue
			}
			err = nil
		}
		return n, err
	}
}

func (r *bodyReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.closeFile()
	return nil
}

func (r *bodyReader) closeFile() {
	if r.file != nil {
		// Closing a file that was only read loses nothing.
		_ = r.file.f.Close()
		r.file = nil
	}
}

// fileReader reads exactly want.Size bytes of a file, hashing them on the
// way, and ends with a *ChangedError instead of io.EOF when the file is not
// what Hash found: shorter, longer, or other bytes. The error comes before
// the rest of the form is sent, so the server gets an incomplete request
// and stores nothing, rather than bytes that no longer match the hash the
// form declares.
type fileReader struct {
	f    *os.File
	want *File
	left int64
	h    hash.Hash
}

func (r *fileReader) Read(p []byte) (int, error) {
	if r.left == 0 {
		return 0, r.check()
	}
	if int64(len(p)) > r.left {
		p = p[:r.left]
	}
	n, err := r.f.Read(p)
	r.h.Write(p[:n])
	r.left -= int64(n)
	switch {
	case errors.Is(err, io.EOF) && r.left > 0:
		return n, &ChangedError{Path: r.want.Path}
	case errors.Is(err, io.EOF):
		err = nil // check() says whether this is the end
	}
	return n, err
}

// check is the end of a file whose declared size has been read: io.EOF when
// nothing follows and the bytes hash as they did.
func (r *fileReader) check() error {
	var one [1]byte
	if n, _ := io.ReadFull(r.f, one[:]); n > 0 {
		return &ChangedError{Path: r.want.Path}
	}
	if hex.EncodeToString(r.h.Sum(nil)) != r.want.SHA256 {
		return &ChangedError{Path: r.want.Path}
	}
	return io.EOF
}
