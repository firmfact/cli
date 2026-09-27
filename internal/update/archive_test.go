package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// entry is one member of a test archive: a regular file with mode perm
// (0o755 when zero), or a directory or symlink when kind says so.
type entry struct {
	name string
	body string
	perm fs.FileMode
	kind fs.FileMode // 0, fs.ModeDir or fs.ModeSymlink; tar.TypeLink via hardLink
	// hardLink makes a tar hard link to body instead.
	hardLink bool
}

func (e entry) mode() fs.FileMode {
	if e.perm == 0 {
		return e.kind | 0o755
	}
	return e.kind | e.perm
}

// archive packs entries as a release archive of the given format.
func archive(t *testing.T, ext string, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	if ext == "zip" {
		zw := zip.NewWriter(&buf)
		for _, e := range entries {
			h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
			if e.kind == fs.ModeDir {
				h.Name += "/"
			}
			h.SetMode(e.mode())
			w, err := zw.CreateHeader(h)
			if err != nil {
				t.Fatal(err)
			}
			// A zipped symlink holds its target as its content.
			if _, err := w.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: int64(e.mode().Perm()), Typeflag: tar.TypeReg, Size: int64(len(e.body))}
		switch {
		case e.hardLink:
			h.Typeflag, h.Linkname, h.Size = tar.TypeLink, e.body, 0
		case e.kind == fs.ModeDir:
			h.Typeflag, h.Name, h.Size = tar.TypeDir, e.name+"/", 0
		case e.kind == fs.ModeSymlink:
			h.Typeflag, h.Linkname, h.Size = tar.TypeSymlink, e.body, 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var formats = []string{"tar.gz", "zip"}

// The binary is found by its name wherever it sits in the archive, next to
// the README and licence the release packs with it, in either format.
func TestExtractTakesTheBinary(t *testing.T) {
	for _, ext := range formats {
		t.Run(ext, func(t *testing.T) {
			body := archive(t, ext,
				entry{name: "README.md", body: "readme"},
				entry{name: "LICENSE", body: "licence"},
				entry{name: "firmfact_1.2.3/" + binaryName(), body: "binary-bytes"},
			)
			if got, err := extract(body, ext); err != nil || string(got) != "binary-bytes" {
				t.Errorf("extract = %q, %v", got, err)
			}
		})
	}
}

// An archive without the binary, or that is not an archive, installs
// nothing.
func TestExtractWithoutTheBinary(t *testing.T) {
	for _, ext := range formats {
		t.Run(ext, func(t *testing.T) {
			body := archive(t, ext, entry{name: "README.md", body: "readme"}, entry{name: "firmfact-docs", body: "docs"})
			if got, err := extract(body, ext); err == nil || err.Error() != "binary not found in archive" {
				t.Errorf("without the binary: %q, %v", got, err)
			}
			if got, err := extract([]byte("<html>Not Found</html>"), ext); err == nil {
				t.Errorf("not an archive: %q, no error", got)
			}
		})
	}
}

// Only a regular file is the binary. A directory, symlink or hard link by
// its name is passed over: taken, it would install as an empty file, or as
// a file holding the path the link points at.
func TestExtractIgnoresEntriesThatAreNotFiles(t *testing.T) {
	for _, ext := range formats {
		t.Run(ext, func(t *testing.T) {
			decoys := []entry{
				{name: binaryName(), kind: fs.ModeDir},
				{name: "bin/" + binaryName(), kind: fs.ModeSymlink, body: "/bin/sh"},
			}
			if ext == "tar.gz" {
				decoys = append(decoys, entry{name: "linked/" + binaryName(), hardLink: true, body: "README.md"})
			}
			real := entry{name: "firmfact_1.2.3/" + binaryName(), body: "binary-bytes"}

			if got, err := extract(archive(t, ext, append(decoys, real)...), ext); err != nil || string(got) != "binary-bytes" {
				t.Errorf("with the binary after the others: %q, %v", got, err)
			}
			if got, err := extract(archive(t, ext, decoys...), ext); err == nil || err.Error() != "binary not found in archive" {
				t.Errorf("with only the others: %q, %v", got, err)
			}
		})
	}
}

// The installed binary can be run by everyone, as a program on the PATH
// must, whatever mode the archive gave it and whatever mode the old binary
// had.
func TestInstallLeavesARunnableBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows runs a file by its name, not by mode bits")
	}
	_, ext := archiveName("1.2.3")
	release{signer: signingKey(t), archive: archive(t, ext, entry{name: binaryName(), body: "new binary", perm: 0o600})}.serve(t)
	passTrialRun(t)
	target := scratchBinary(t)
	if err := os.Chmod(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := install(context.Background(), target, "1.2.3"); err != nil {
		t.Fatalf("install: %v", err)
	}
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("installed with mode %04o, want 0755", fi.Mode().Perm())
	}
	if entries, _ := os.ReadDir(filepath.Dir(target)); len(entries) != 1 {
		t.Errorf("the target's directory holds %d entries, want 1", len(entries))
	}
}

// A release whose files are not all there installs nothing: a tag without
// a release yet, an archive missing for this platform, or a download host
// in trouble.
func TestInstallNeedsEveryReleaseFile(t *testing.T) {
	archiveFile, _ := archiveName("1.2.3")
	cases := map[string]struct {
		missing string // the path answered with 404; "" for all of them
		status  int
		want    string
	}{
		"no release":       {status: http.StatusNotFound, want: "/v1.2.3/checksums.txt: not found"},
		"no archive":       {missing: "/v1.2.3/" + archiveFile, status: http.StatusNotFound, want: "/v1.2.3/" + archiveFile + ": not found"},
		"a server failure": {status: http.StatusInternalServerError, want: "/v1.2.3/checksums.txt: 500 Internal Server Error"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			release{signer: signingKey(t)}.serve(t)
			// In front of a genuine release, a host that fails to hand
			// over some of its files.
			genuine := DownloadBase
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if c.missing == "" || r.URL.Path == c.missing {
					w.WriteHeader(c.status)
					return
				}
				resp, err := http.Get(genuine + r.URL.Path)
				if err != nil {
					t.Error(err)
					return
				}
				defer resp.Body.Close()
				w.WriteHeader(resp.StatusCode)
				_, _ = io.Copy(w, resp.Body)
			}))
			t.Cleanup(srv.Close)
			DownloadBase = srv.URL
			passTrialRun(t)

			target := scratchBinary(t)
			err := install(context.Background(), target, "1.2.3")
			if err == nil || !strings.HasSuffix(err.Error(), c.want) {
				t.Fatalf("want an error ending %q, got %v", c.want, err)
			}
			unchanged(t, target)
		})
	}
}

// SelfUpdate checks the version before it looks for the release, so a
// malformed one leaves the running binary alone and asks nothing of the
// download host; the path it reports is the running binary's own.
func TestSelfUpdateRefusesAMalformedVersion(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()
	prev := DownloadBase
	DownloadBase = srv.URL
	defer func() { DownloadBase = prev }()

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(self)
	if err != nil {
		t.Fatal(err)
	}
	path, err := SelfUpdate(context.Background(), "latest")
	if err == nil || !strings.Contains(err.Error(), `"latest" is not a release version; not installing`) {
		t.Fatalf("SelfUpdate = %v, want a refusal", err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	if path != self {
		t.Errorf("path = %q, want %q", path, self)
	}
	if after, err := os.Stat(self); err != nil || !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Errorf("the running binary changed: %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the download host got %d request(s), want none", n)
	}
}

// A binary in a directory the user cannot write to, as a copy put in
// /usr/local/bin by hand is, cannot be updated in place. The error says to
// download the new version instead, and nothing is left behind.
func TestInstallIntoADirectoryItCannotWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no directory modes to take write access away with")
	}
	if os.Geteuid() == 0 {
		t.Skip("root writes to any directory")
	}
	release{signer: signingKey(t)}.serve(t)
	passTrialRun(t)
	target := scratchBinary(t)
	dir := filepath.Dir(target)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	err := install(context.Background(), target, "1.2.3")
	if err == nil || !strings.HasPrefix(err.Error(), "cannot write next to "+target+" (") || !strings.HasSuffix(err.Error(), "); download the new version by hand") {
		t.Fatalf("got %v, want the directory refused", err)
	}
	unchanged(t, target)
}
