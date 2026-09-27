package upload

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// writeFile writes content to a file called name in a new directory and
// hashes it.
func writeFile(t *testing.T, name, content string) File {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := Hash(path)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestHashReadsSizeAndChecksum(t *testing.T) {
	f := writeFile(t, "hello.txt", "hello\n")
	if f.Name != "hello.txt" || f.Size != 6 || f.SHA256 != "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03" {
		t.Errorf("got %+v", f)
	}
	dir := t.TempDir()
	if _, err := Hash(dir); err == nil || err.Error() != dir+" is not a regular file" {
		t.Errorf("a directory: got %v", err)
	}
	if _, err := Hash(filepath.Join(dir, "gone.pdf")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing file: got %v", err)
	}
}

// readAll reads a body from its first byte, as the transport does.
func readAll(t *testing.T, b *body) ([]byte, error) {
	t.Helper()
	rc, err := b.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// The form is what the server's Rack reads: a checksum for each file in
// the order of the files, the flags, then each file as
// application/octet-stream under a name that cannot break out of its
// header, however odd the name on disk. Its length is known up front, and
// every opening gives the same bytes.
func TestFormIsWhatTheServerReads(t *testing.T) {
	invoice := writeFile(t, "LSEG-2026-07.pdf", "%PDF-1.7 an invoice")
	odd := writeFile(t, "odd.csv", "vendor,amount\nLSEG,1180.00\n")
	odd.Name = "50% \"final\"\r\nContent-Type: text/html\\ Überweisung.csv"
	files := []File{invoice, odd}

	b, err := newBody(files, Options{Related: true, NewVersion: true})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := readAll(t, b)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(raw)) != b.Size() {
		t.Errorf("read %d bytes, Size says %d", len(raw), b.Size())
	}
	again, _ := readAll(t, b)
	if !bytes.Equal(raw, again) {
		t.Error("a second opening gives other bytes")
	}
	mediaType, params, err := mime.ParseMediaType(b.ContentType())
	if err != nil || mediaType != "multipart/form-data" {
		t.Fatalf("Content-Type %q: %v", b.ContentType(), err)
	}

	type part struct{ name, filename, contentType, value string }
	var got []part
	mr := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
	for {
		p, err := mr.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		value, _ := io.ReadAll(p)
		_, disposition, _ := mime.ParseMediaType(p.Header.Get("Content-Disposition"))
		got = append(got, part{p.FormName(), disposition["filename"], p.Header.Get("Content-Type"), string(value)})
	}
	want := []part{
		{"sha256[]", "", "", invoice.SHA256},
		{"sha256[]", "", "", odd.SHA256},
		{"group_as_related", "", "", "1"},
		{"new_version", "", "", "1"},
		{"files[]", "LSEG-2026-07.pdf", "application/octet-stream", "%PDF-1.7 an invoice"},
		{"files[]", "50%25 %22final%22%0D%0AContent-Type: text/html%5C Überweisung.csv", "application/octet-stream", "vendor,amount\nLSEG,1180.00\n"},
	}
	if len(got) != len(want) {
		t.Fatalf("parts %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("part %d is %q, want %q", i, got[i], want[i])
		}
	}
	// Rack decodes the name, since each percent sign starts an escape.
	if name, err := url.PathUnescape(got[5].filename); err != nil || name != odd.Name {
		t.Errorf("the name decodes to %q (%v), want %q", name, err, odd.Name)
	}

	// The debug log's account of the form names the files, not their
	// bytes.
	if s := b.String(); !strings.Contains(s, `files[] "LSEG-2026-07.pdf" (19 bytes, sha256 `+invoice.SHA256+`)`) || strings.Contains(s, "%PDF") || !strings.HasSuffix(s, "group_as_related=1, new_version=1") {
		t.Errorf("String() = %q", s)
	}
}

func TestFormWithoutFilesIsAnError(t *testing.T) {
	if _, err := newBody(nil, Options{}); err == nil {
		t.Error("want an error")
	}
}

// A file that changed after it was hashed stops the form before its end,
// so the server gets an incomplete request and stores nothing.
func TestChangedFileStopsTheForm(t *testing.T) {
	cases := map[string]func(path string) error{
		"grew": func(path string) error {
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = f.WriteString(" and a page more")
			return err
		},
		"shrank": func(path string) error { return os.Truncate(path, 4) },
		"changed": func(path string) error {
			return os.WriteFile(path, []byte("%PDF-1.7 an INVOICE"), 0o600)
		},
		"went away": os.Remove,
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := writeFile(t, "LSEG-2026-07.pdf", "%PDF-1.7 an invoice")
			b, err := newBody([]File{f}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if err := change(f.Path); err != nil {
				t.Fatal(err)
			}
			raw, err := readAll(t, b)
			var changed *ChangedError
			switch {
			case name == "went away":
				if !errors.Is(err, os.ErrNotExist) {
					t.Errorf("got %v, want the file to be missing", err)
				}
			case !errors.As(err, &changed) || changed.Path != f.Path:
				t.Errorf("got %v, want a *ChangedError", err)
			case err.Error() != f.Path+" changed while it was being uploaded, so nothing was stored; upload it again once it is complete":
				t.Errorf("message %q", err)
			}
			if bytes.Contains(raw, []byte(b.boundary+"--")) {
				t.Error("the form was finished all the same")
			}
		})
	}
}

// A body the transport closes stops reading.
func TestClosedFormReadsNoMore(t *testing.T) {
	f := writeFile(t, "a.pdf", "%PDF-1.7")
	b, _ := newBody([]File{f}, Options{})
	rc, _ := b.Open()
	buf := make([]byte, 8)
	if _, err := rc.Read(buf); err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if _, err := rc.Read(buf); !errors.Is(err, os.ErrClosed) {
		t.Errorf("a read after Close: %v", err)
	}
}

// A file over the limit is refused before it is read: by its size, or, for
// a file whose size says nothing (such as /proc/self/pagemap, which says
// 0), once the limit has been read.
func TestHashStopsAtTheLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "huge.pdf")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// Sparse: no disk space, and nothing to read if the size is checked.
	if err := f.Truncate(MaxFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	var tooLarge *TooLargeError
	if _, err := Hash(path); !errors.As(err, &tooLarge) || tooLarge.Path != path || err.Error() != path+" holds more than 52428799 bytes, the most one file may be" {
		t.Errorf("a file over the limit: %v", err)
	}
	if runtime.GOOS != "linux" {
		return
	}
	start := time.Now()
	if _, err := Hash("/proc/self/pagemap"); !errors.As(err, &tooLarge) {
		t.Errorf("/proc/self/pagemap: %v", err)
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Errorf("/proc/self/pagemap took %s", took)
	}
}

// A file put in the place of the one hashed, even with the same bytes, is
// not sent: not a byte of it goes.
func TestSwappedFileStopsTheForm(t *testing.T) {
	f := writeFile(t, "LSEG-2026-07.pdf", "%PDF-1.7 an invoice")
	b, err := newBody([]File{f}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(filepath.Dir(f.Path), "other.pdf")
	if err := os.WriteFile(other, []byte("%PDF-1.7 an invoice"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(other, f.Path); err != nil {
		t.Fatal(err)
	}
	raw, err := readAll(t, b)
	var changed *ChangedError
	if !errors.As(err, &changed) || changed.Path != f.Path {
		t.Errorf("got %v, want a *ChangedError", err)
	}
	if bytes.Contains(raw, []byte("an invoice")) {
		t.Error("the other file's bytes were sent")
	}
}

// A request's size is the files' and the form's around them, as sent.
func TestRequestSize(t *testing.T) {
	f := writeFile(t, "a.pdf", "%PDF-1.7")
	b, _ := newBody([]File{f, f}, Options{Related: true})
	if got := RequestSize([]File{f, f}, Options{Related: true}); got != b.Size() || got <= 2*f.Size {
		t.Errorf("RequestSize = %d, the body is %d", got, b.Size())
	}
	if RequestSize(nil, Options{}) != 0 {
		t.Error("no files, no request")
	}
}
