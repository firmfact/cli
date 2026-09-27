package update

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"golang.org/x/mod/semver"
)

// The fuzz targets here cover what reaches the CLI from the network before
// anything has vouched for it: a release's version, from GitHub's redirect
// and the service's minimum, and a release's checksums.txt and archive.
// `go test` runs their seeds and every input in testdata/fuzz; CI fuzzes
// each for 30 seconds on a pull request and for longer every night (see
// .github/workflows/fuzz.yml). To fuzz one here:
//
//	go test -run '^$' -fuzz '^FuzzVersions$' -fuzztime 1m ./internal/update

// FuzzVersions holds the version rules to what the update check and
// self-update rely on: a version that names a download cannot climb out of
// its URL or file name, and versions are ordered consistently, so no pair
// is each newer than the other and nothing that does not parse is newer or
// older than anything.
func FuzzVersions(f *testing.F) {
	for _, seed := range [][3]string{
		{"0.3.0", "v0.3.0-rc.1", "0.2.9"},
		{"0.3.1-dev+b005a6f", "0.3.1-dev+c0ffee1", "0.3.1"},
		{"0.3.1-0.20260927101010-b005a6f00000", "0.3.0+dirty", "0.0.0-SNAPSHOT-b005a6f"},
		{"1.2.3-x/../../y", "1.2.3-rc..1", "01.2.3"},
		{"dev", "", "v1.2"},
		{" 1.2.3\n", "vv1.2.3", "1.2.3-01"},
	} {
		f.Add(seed[0], seed[1], seed[2])
	}
	f.Fuzz(func(t *testing.T, a, b, c string) {
		if validVersion(a) {
			if strings.ContainsAny(a, "/\\?#%&= \t\r\n") || strings.Contains(a, "..") {
				t.Errorf("validVersion(%q): a tag that could leave its path", a)
			}
			if _, ok := canonical(a); !ok {
				t.Errorf("validVersion(%q), but canonical says it is no version", a)
			}
		}
		ca, okA := canonical(a)
		cb, okB := canonical(b)
		if !okA && (Newer(a, b) || Newer(b, a)) {
			t.Errorf("%q is not a version, yet compares with %q", a, b)
		}
		if Newer(a, a) {
			t.Errorf("Newer(%q, %q): a version newer than itself", a, a)
		}
		if Newer(a, b) && Newer(b, a) {
			t.Errorf("%q and %q are each newer than the other", a, b)
		}
		if Newer(a, b) && Newer(b, c) && !Newer(a, c) {
			t.Errorf("%q > %q > %q, but not %q > %q", a, b, c, a, c)
		}
		// Two versions neither newer than the other are the same version,
		// build metadata aside.
		if okA && okB && !Newer(a, b) && !Newer(b, a) && semver.Canonical(ca) != semver.Canonical(cb) {
			t.Errorf("%q and %q differ, yet neither is newer", a, b)
		}
		if Offer(a, b) {
			if !Newer(a, b) {
				t.Errorf("Offer(%q, %q) offers what is not newer", a, b)
			}
			if semver.Prerelease(cb) == "" && semver.Prerelease(ca) != "" {
				t.Errorf("Offer(%q, %q) offers a pre-release to someone on a release", a, b)
			}
		}
		if IsRelease(a) && !okA {
			t.Errorf("IsRelease(%q), but it is no version", a)
		}
		// The leading 'v' is optional and changes nothing.
		if !strings.HasPrefix(a, "v") && a == strings.TrimSpace(a) {
			cv, okV := canonical("v" + a)
			if okV != okA || cv != ca {
				t.Errorf("canonical(%q) = %q, %v but canonical(%q) = %q, %v", a, ca, okA, "v"+a, cv, okV)
			}
		}
	})
}

// FuzzChecksumFor reads checksums.txt as sha256sum and GoReleaser write it:
// the first line that is a checksum and exactly the archive's name gives the
// checksum, and nothing else does. testdata/fuzz holds a real one, from a
// snapshot build.
func FuzzChecksumFor(f *testing.F) {
	f.Add([]byte("abc123  firmfact_0.1.0_linux_amd64.tar.gz\ndef456  firmfact_0.1.0_darwin_arm64.tar.gz\n"), "firmfact_0.1.0_darwin_arm64.tar.gz")
	f.Add([]byte("abc123  firmfact_0.1.0_linux_amd64.tar.gz\r\nabc124  firmfact_0.1.0_linux_amd64.tar.gz\r\n"), "firmfact_0.1.0_linux_amd64.tar.gz")
	f.Add([]byte("abc123 *firmfact_0.1.0_windows_amd64.zip\nabc123  firmfact_0.1.0_windows_amd64.zip extra\n"), "firmfact_0.1.0_windows_amd64.zip")
	f.Add([]byte("  \n\n"), "")
	f.Fuzz(func(t *testing.T, sums []byte, name string) {
		got, err := checksumFor(sums, name)
		if err == nil && (got == "" || strings.ContainsFunc(got, unicode.IsSpace)) {
			t.Fatalf("checksumFor(%q) = %q, not a checksum", name, got)
		}
		if err == nil && (name == "" || strings.ContainsFunc(name, unicode.IsSpace)) {
			t.Fatalf("checksumFor(%q) = %q: no listed name is empty or has a space", name, got)
		}
		// The same answer as reading the lines one by one, where each fits
		// the scanner; a longer line stops it, which lists nothing.
		want, found := "", false
		for line := range strings.SplitSeq(string(sums), "\n") {
			if len(line) >= bufio.MaxScanTokenSize {
				return
			}
			if fields := strings.Fields(line); !found && len(fields) == 2 && fields[1] == name {
				want, found = fields[0], true
			}
		}
		if found != (err == nil) || got != want {
			t.Errorf("checksumFor(%q) = %q, %v; the lines list %q (found: %v)", name, got, err, want, found)
		}
	})
}

// FuzzExtract opens release archives. Self-update opens one only once its
// checksum matches the signed checksums.txt, but a reader of archives should
// not fall over on a damaged one whatever vouched for it: any input gives
// the binary or an error, the same each time. testdata/fuzz holds two real
// release archives, a tar.gz and a zip from a GoReleaser snapshot build with
// every file cut to its first 32 bytes, so the fuzzer starts from the
// headers GoReleaser writes.
func FuzzExtract(f *testing.F) {
	f.Add(releaseArchive(f, "tar.gz", []byte("new binary")), false)
	f.Add(releaseArchive(f, "zip", []byte("new binary")), true)
	f.Add([]byte{}, false)
	f.Add([]byte("PK\x05\x06\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"), true)
	f.Fuzz(func(t *testing.T, body []byte, zipped bool) {
		ext := "tar.gz"
		if zipped {
			ext = "zip"
		}
		got, err := extract(body, ext)
		if err == nil && got == nil {
			t.Fatal("no error, and no binary")
		}
		again, err2 := extract(body, ext)
		if (err == nil) != (err2 == nil) || !bytes.Equal(got, again) {
			t.Errorf("two reads of one archive differ: %q, %v and %q, %v", brief(string(got)), err, brief(string(again)), err2)
		}
	})
}

// The real archives in testdata/fuzz give their binary: the Linux one on
// Linux and macOS, where the binary is firmfact, and the Windows one on
// Windows, where it is firmfact.exe.
func TestExtractReleaseArchives(t *testing.T) {
	name, zipped, magic := "release_linux_amd64", false, "\x7fELF"
	if runtime.GOOS == "windows" {
		name, zipped, magic = "release_windows_amd64", true, "MZ"
	}
	body, isZip := corpusArchive(t, filepath.Join("testdata", "fuzz", "FuzzExtract", name))
	if isZip != zipped {
		t.Fatalf("%s: zipped = %v", name, isZip)
	}
	ext := "tar.gz"
	if zipped {
		ext = "zip"
	}
	got, err := extract(body, ext)
	if err != nil || !bytes.HasPrefix(got, []byte(magic)) {
		t.Fatalf("extract(%s) = %q, %v", name, got, err)
	}
}

// corpusArchive reads the archive and its zipped flag from a FuzzExtract
// corpus file, whose lines after the first are []byte("...") and
// bool(...), the Go syntax `go test` writes them in.
func corpusArchive(t *testing.T, path string) ([]byte, bool) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(string(raw), "\r\n", "\n")), "\n")
	if len(lines) != 3 || lines[0] != "go test fuzz v1" {
		t.Fatalf("%s is not a FuzzExtract corpus file", path)
	}
	quoted, ok := strings.CutPrefix(lines[1], "[]byte(")
	if !ok {
		t.Fatalf("%s: %.40q is not a []byte", path, lines[1])
	}
	body, err := strconv.Unquote(strings.TrimSuffix(quoted, ")"))
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	zipped, err := strconv.ParseBool(strings.TrimSuffix(strings.TrimPrefix(lines[2], "bool("), ")"))
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return []byte(body), zipped
}
