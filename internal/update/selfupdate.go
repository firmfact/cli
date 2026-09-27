package update

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/firmfact/cli/internal/httpx"
)

// DownloadBase is where release assets live, one directory per tag (a
// variable so tests can serve releases from a fake server).
var DownloadBase = "https://github.com/" + Repo + "/releases/download"

// SelfUpdate replaces the running binary with release `version` from GitHub.
// Before anything is written, the release's checksums.txt must carry a
// valid signature by a trusted release key and the archive must match its
// checksum there, and before the swap the new binary must run and report
// that version. The binary is swapped with a rename, so an interrupted
// update leaves the old one in place.
func SelfUpdate(ctx context.Context, version string) (string, error) {
	exe, err := executable()
	if err != nil {
		return "", err
	}
	return exe, install(ctx, exe, version)
}

// executable is the running binary's path, symlinks resolved: the file an
// update replaces.
func executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

// RemoveOldBinary deletes the binary an earlier update set aside on
// Windows, where a running .exe can be renamed but not deleted, so the
// update leaves it as <exe>.old for the next run to remove. It is best
// effort: while that old binary still runs, or when the directory is
// read-only, it stays for a later run.
func RemoveOldBinary() {
	if !moveAside {
		return
	}
	if exe, err := executable(); err == nil {
		_ = os.Remove(oldPath(exe))
	}
}

// oldPath is where the swap sets aside the binary at exe.
func oldPath(exe string) string { return exe + ".old" }

// archiveName is the release asset holding this platform's binary.
func archiveName(version string) (name, ext string) {
	ext = "tar.gz"
	if runtime.GOOS == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("firmfact_%s_%s_%s.%s", version, runtime.GOOS, runtime.GOARCH, ext), ext
}

// install puts release `version` at exe; SelfUpdate passes the running
// binary, tests a scratch file.
func install(ctx context.Context, exe, version string) error {
	// The version names URLs and files below, so it must look like one.
	if !validVersion(version) {
		return fmt.Errorf("%q is not a release version; not installing", version)
	}
	version = strings.TrimPrefix(version, "v")
	archive, ext := archiveName(version)
	base := fmt.Sprintf("%s/v%s/", strings.TrimSuffix(DownloadBase, "/"), version)

	// A release asset lives on GitHub's download host, one redirect away.
	// Every hop must be https; the signature is what makes it trustworthy.
	client := httpx.New(httpx.Options{Timeout: 5 * time.Minute, FollowHTTPS: true})
	sums, err := download(ctx, client, base+checksumsFile)
	if err != nil {
		return err
	}
	sig, err := download(ctx, client, base+signatureFile)
	if errors.Is(err, errNotFound) {
		return fmt.Errorf("release v%s has no %s, so it cannot be verified; not installing", version, signatureFile)
	}
	if err != nil {
		return err
	}
	if err := VerifyChecksums(sums, sig); err != nil {
		return fmt.Errorf("release v%s: %w; not installing", version, err)
	}
	want, err := checksumFor(sums, archive)
	if err != nil {
		return fmt.Errorf("%w; not installing", err)
	}
	body, err := download(ctx, client, base+archive)
	if err != nil {
		return err
	}
	got := sha256.Sum256(body)
	if hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("checksum mismatch for %s; not installing", archive)
	}

	binary, err := extract(body, ext)
	if err != nil {
		return err
	}
	return replace(ctx, exe, binary, version)
}

// errNotFound is a release asset that does not exist.
var errNotFound = errors.New("not found")

func download(ctx context.Context, client *httpx.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("download %s: %w", url, errNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
}

func checksumFor(sums []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && fields[1] == name {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("no checksum listed for %s", name)
}

func binaryName() string {
	if runtime.GOOS == "windows" {
		return "firmfact.exe"
	}
	return "firmfact"
}

func extract(body []byte, ext string) ([]byte, error) {
	want := binaryName()
	if ext == "zip" {
		zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			// As in a tarball, only a regular file is the binary: a
			// directory or symlink by its name would install as an empty
			// file, or as one holding the path the link points at.
			if filepath.Base(f.Name) == want && f.Mode().IsRegular() {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(rc)
			}
		}
		return nil, errors.New("binary not found in archive")
	}
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, errors.New("binary not found in archive")
		}
		if err != nil {
			return nil, err
		}
		if filepath.Base(h.Name) == want && h.Typeflag == tar.TypeReg {
			return io.ReadAll(tr)
		}
	}
}

// replace puts binary at exe, once a trial run shows it works here and is
// release `version`.
func replace(ctx context.Context, exe string, binary []byte, version string) error {
	dir := filepath.Dir(exe)
	// Windows runs only files named .exe, and this one is run before the
	// swap.
	tmp, err := os.CreateTemp(dir, ".firmfact-update-*"+exeSuffix())
	if err != nil {
		return fmt.Errorf("cannot write next to %s (%w); download the new version by hand", exe, err)
	}
	// Once the rename below has succeeded there is nothing left to remove.
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(binary); err != nil {
		_ = tmp.Close() // the write error is the one worth reporting
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// A program on the PATH is readable and runnable by everyone.
	if err := os.Chmod(tmp.Name(), 0o755); err != nil { //nolint:gosec // G302, see above
		return err
	}
	if err := trialRun(ctx, tmp.Name(), version); err != nil {
		return fmt.Errorf("%w; not installing", err)
	}
	return swap(tmp.Name(), exe)
}

// exeSuffix is the extension Windows needs to run a file.
func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// trialTimeout bounds the new binary's trial run. The first run of a new
// file can take seconds on Windows while antivirus software scans it.
var trialTimeout = 10 * time.Second

// trialRun is runsAs; the install tests, whose binaries are text, replace
// it.
var trialRun = runsAs

// runsAs runs the binary at path with --version and requires it to report
// `version`. It stops an update whose binary does not start on this machine
// (another architecture, a file damaged on the way to disk) or is not the
// release it was downloaded as, before it takes the place of one that
// works. Every release must keep `--version` printing its version as the
// last word of its output.
func runsAs(ctx context.Context, path, version string) error {
	ctx, cancel := context.WithTimeout(ctx, trialTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version") //nolint:gosec // G204: the release binary whose signed checksum install just checked
	// --version runs no command, but a daily check must not run ahead of it.
	cmd.Env = append(os.Environ(), "FIRMFACT_NO_UPDATE_CHECK=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// Should the binary leave a child holding its output open, stop
	// waiting for it soon after the binary itself has gone.
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			return fmt.Errorf("the new version did not answer --version within %s", trialTimeout)
		case ctx.Err() != nil:
			return ctx.Err() // interrupted
		}
		if msg := brief(stderr.String()); msg != "" {
			return fmt.Errorf("the new version does not run here: %w (%q)", err, msg)
		}
		return fmt.Errorf("the new version does not run here: %w", err)
	}
	fields := strings.Fields(stdout.String())
	if len(fields) == 0 {
		return errors.New("the new version printed nothing for --version")
	}
	if got := strings.TrimPrefix(fields[len(fields)-1], "v"); got != version {
		return fmt.Errorf("the new binary reports version %q, not %s", brief(got), version)
	}
	return nil
}

// brief is the first line of s, trimmed and cut to 200 bytes, for an
// error message.
func brief(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// rename is os.Rename; tests make it fail.
var rename = os.Rename

// moveAside is whether the binary in place must be renamed away before the
// new one can take its name: a running .exe cannot be overwritten, but it
// can be renamed. Elsewhere a rename replaces it in one step.
var moveAside = runtime.GOOS == "windows"

// swap moves the binary at next to exe. Should the old binary have been set
// aside and the new one then fail to take its place, the old one is put
// back, so exe is not left without a binary.
func swap(next, exe string) error {
	if !moveAside {
		return rename(next, exe)
	}
	old := oldPath(exe)
	_ = os.Remove(old)
	if err := rename(exe, old); err != nil {
		return fmt.Errorf("cannot move %s aside: %w", exe, err)
	}
	err := rename(next, exe)
	if err == nil {
		return nil
	}
	if restoreErr := rename(old, exe); restoreErr != nil {
		return fmt.Errorf("cannot put the new version in place (%w), nor the old one back (%w); rename %s to %s to use it again", err, restoreErr, old, exe)
	}
	return fmt.Errorf("cannot put the new version in place (%w); the old one is back", err)
}
