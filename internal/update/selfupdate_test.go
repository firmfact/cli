package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/firmfact/cli/internal/httpx"
)

// releaseArchive packs a binary the way the release does for this platform.
func releaseArchive(t testing.TB, ext string, binary []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if ext == "zip" {
		zw := zip.NewWriter(&buf)
		w, err := zw.Create(binaryName())
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(binary)
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: binaryName(), Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(binary)
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// signingKey makes the test trust a fresh release key, instead of the real
// one, and returns its private half.
func signingKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	prev := trustedKeys
	trustedKeys = []ed25519.PublicKey{pub}
	t.Cleanup(func() { trustedKeys = prev })
	return priv
}

// release is what the fake release server serves as v1.2.3. With only
// signer set, the trusted key from signingKey, it is a genuine release: the
// archive, a correct checksums.txt and the signer's signature over it.
type release struct {
	archive []byte // default: releaseArchive of "new binary"
	sums    []byte // default: the archive's checksum
	sig     []byte // default: signer's signature over sums
	signer  ed25519.PrivateKey
	noSig   bool // serve no checksums.txt.sig at all
}

// serve starts the fake release server, points DownloadBase at it and
// returns the number of requests it has had so far.
func (r release) serve(t *testing.T) func() int32 {
	t.Helper()
	archive, ext := archiveName("1.2.3")
	if r.archive == nil {
		r.archive = releaseArchive(t, ext, []byte("new binary"))
	}
	if r.sums == nil {
		sum := sha256.Sum256(r.archive)
		r.sums = []byte(hex.EncodeToString(sum[:]) + "  " + archive + "\n")
	}
	if r.sig == nil && r.signer != nil {
		r.sig = ed25519.Sign(r.signer, r.sums)
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hits.Add(1)
		switch req.URL.Path {
		case "/v1.2.3/checksums.txt":
			_, _ = w.Write(r.sums)
		case "/v1.2.3/checksums.txt.sig":
			if r.noSig {
				http.NotFound(w, req)
				return
			}
			_, _ = w.Write(r.sig)
		case "/v1.2.3/" + archive:
			_, _ = w.Write(r.archive)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	prev := DownloadBase
	DownloadBase = srv.URL
	t.Cleanup(func() { DownloadBase = prev })
	return hits.Load
}

func scratchBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), binaryName())
	if err := os.WriteFile(path, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// unchanged fails the test unless target still holds exactly the old
// binary, alone in its directory (no temporary file left behind either).
func unchanged(t *testing.T, target string) {
	t.Helper()
	if got, _ := os.ReadFile(target); !bytes.Equal(got, []byte("old binary")) {
		t.Errorf("target changed to %q", got)
	}
	if entries, _ := os.ReadDir(filepath.Dir(target)); len(entries) != 1 {
		t.Errorf("the target's directory holds %d entries, want 1", len(entries))
	}
}

// passTrialRun stands in for the new binary's trial run, which a text file
// posing as a binary cannot pass, and checks it is asked for version 1.2.3.
func passTrialRun(t *testing.T) {
	t.Helper()
	prev := trialRun
	trialRun = func(_ context.Context, path, version string) error {
		if got, _ := os.ReadFile(path); string(got) != "new binary" {
			t.Errorf("trial run of %q", got)
		}
		if version != "1.2.3" {
			t.Errorf("trial run asked for version %q, want 1.2.3", version)
		}
		return nil
	}
	t.Cleanup(func() { trialRun = prev })
}

func TestInstallReplacesTheBinary(t *testing.T) {
	release{signer: signingKey(t)}.serve(t)
	passTrialRun(t)
	target := scratchBinary(t)
	if err := install(context.Background(), target, "1.2.3"); err != nil {
		t.Fatalf("install: %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "new binary" {
		t.Errorf("target = %q", got)
	}
}

// Rotation: while two keys are trusted, a release signed with either one
// installs.
func TestInstallAcceptsAnyTrustedKey(t *testing.T) {
	first := signingKey(t)
	passTrialRun(t)
	pub, second, _ := ed25519.GenerateKey(rand.Reader)
	trustedKeys = append(trustedKeys, pub)
	for name, key := range map[string]ed25519.PrivateKey{"first": first, "second": second} {
		t.Run(name, func(t *testing.T) {
			release{signer: key}.serve(t)
			target := scratchBinary(t)
			if err := install(context.Background(), target, "v1.2.3"); err != nil {
				t.Fatalf("install: %v", err)
			}
			if got, _ := os.ReadFile(target); string(got) != "new binary" {
				t.Errorf("target = %q", got)
			}
		})
	}
}

// Nothing is installed unless checksums.txt carries a trusted signature and
// the archive matches it; every refusal leaves the binary byte-identical.
func TestInstallRefusesAnUnverifiedRelease(t *testing.T) {
	key := signingKey(t)
	_, stranger, _ := ed25519.GenerateKey(rand.Reader)
	archive, ext := archiveName("1.2.3")
	genuine := releaseArchive(t, ext, []byte("new binary"))
	evil := releaseArchive(t, ext, []byte("evil binary"))
	listing := func(body []byte) []byte {
		sum := sha256.Sum256(body)
		return []byte(hex.EncodeToString(sum[:]) + "  " + archive + "\n")
	}
	cases := map[string]struct {
		release release
		want    string
	}{
		// Whoever swaps the archive and rewrites checksums.txt to match
		// still holds only the genuine signature.
		"tampered checksums.txt":    {release{archive: evil, sums: listing(evil), sig: ed25519.Sign(key, listing(genuine))}, "does not match"},
		"signed with the wrong key": {release{archive: evil, sums: listing(evil), signer: stranger}, "does not match"},
		"no signature":              {release{noSig: true, signer: key}, "no checksums.txt.sig"},
		"a truncated signature":     {release{sig: []byte("short"), signer: key}, "5 bytes"},
		// A genuine checksums.txt, but the archive is not the one it lists.
		"tampered archive":   {release{archive: evil, sums: listing(genuine), signer: key}, "checksum mismatch"},
		"archive not listed": {release{sums: []byte(strings.Repeat("0", 64) + "  firmfact_1.2.3_plan9_mips.tar.gz\n"), signer: key}, "no checksum listed"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			c.release.serve(t)
			target := scratchBinary(t)
			err := install(context.Background(), target, "1.2.3")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error containing %q, got %v", c.want, err)
			}
			if !strings.Contains(err.Error(), "not installing") {
				t.Errorf("the error does not say nothing was installed: %v", err)
			}
			unchanged(t, target)
		})
	}
}

// A malformed version is refused before any URL is built from it: the fake
// server hears nothing and the binary stays.
func TestInstallRefusesAMalformedVersion(t *testing.T) {
	requests := release{signer: signingKey(t)}.serve(t)
	for _, v := range []string{"", "1.2", "1.2.3/../../../evil", "1.2.3-rc1/x", "1.2.3?x=1", "1.2.3-rc1\n", " 1.2.3", "latest", "v1.2.3+build"} {
		t.Run(v, func(t *testing.T) {
			target := scratchBinary(t)
			err := install(context.Background(), target, v)
			if err == nil || !strings.Contains(err.Error(), "not a release version") {
				t.Fatalf("install(%q) = %v, want a refusal", v, err)
			}
			unchanged(t, target)
		})
	}
	if n := requests(); n != 0 {
		t.Errorf("the release server got %d request(s), want none", n)
	}
}

// A release download follows redirects (GitHub hands assets over from
// another host) only while they stay on https: a redirect to plain http is
// refused before anything is fetched from it, and the binary stays.
func TestInstallRefusesAPlainHTTPRedirect(t *testing.T) {
	var hits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer plain.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+r.URL.Path, http.StatusFound)
	}))
	defer srv.Close()
	prev := DownloadBase
	DownloadBase = srv.URL
	defer func() { DownloadBase = prev }()

	target := scratchBinary(t)
	err := install(context.Background(), target, "1.2.3")
	var insecure *httpx.InsecureRedirectError
	if !errors.As(err, &insecure) {
		t.Fatalf("want an *httpx.InsecureRedirectError, got %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the http host got %d request(s), want none", n)
	}
	if got, _ := os.ReadFile(target); string(got) != "old binary" {
		t.Errorf("target changed to %q", got)
	}
}
