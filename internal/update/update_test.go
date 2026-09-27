package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/mod/modfile"

	"github.com/firmfact/cli/internal/httpx"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.2.0", "0.1.9", true},
		{"1.0.0", "0.9.9", true},
		{"v0.1.1", "0.1.0", true},
		{"0.1.0", "0.1.0", false},
		{"0.1.0", "0.2.0", false},
		{"dev", "0.1.0", false},
		{"0.1.0", "dev", false},
		{"0.3.0-rc1", "0.2.0", true},
		{"0.10.0", "0.9.0", true},
		// A release is newer than its own pre-releases, which the old
		// comparison, blind to anything after '-', counted as equal.
		{"0.3.0", "0.3.0-rc1", true},
		{"0.3.0-rc1", "0.3.0", false},
		{"0.3.0-rc.2", "0.3.0-rc.1", true},
		{"0.3.0-rc.10", "0.3.0-rc.9", true},
		{"0.3.0-rc.1", "0.3.0-beta.2", true},
		// Build metadata does not count.
		{"0.3.1-dev+b005a6f", "0.3.0", true},
		{"0.3.1-dev+b005a6f", "0.3.1-dev+c0ffee1", false},
		{"0.3.1", "0.3.1-dev+b005a6f", true},
		// Not full semantic versions.
		{"1.2", "1.0.0", false},
		{"2.0.0", "1.2", false},
		{"01.2.3", "1.0.0", false},
		{"1.2.3-rc..1", "1.0.0", false},
		{"1.2.3-01", "1.0.0", false},
	}
	for _, c := range cases {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// Someone on a release candidate is offered the release; someone on a
// release is never offered a release candidate, not even of a later version.
func TestOffer(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"0.3.0", "0.3.0-rc1", true},
		{"0.3.0", "0.3.0-rc.2", true},
		{"0.4.0", "0.3.0-rc1", true},
		{"0.3.0-rc.2", "0.3.0-rc.1", true},
		{"0.4.0-rc1", "0.3.0-rc1", true},
		{"0.3.0-rc1", "0.3.0", false},
		{"0.4.0-rc1", "0.3.0", false},
		{"0.3.0-rc1", "0.2.0", false},
		{"0.4.0", "0.3.0", true},
		{"0.3.0", "0.3.0", false},
		{"0.2.0", "0.3.0-rc1", false},
		{"0.4.0", "dev", false},
		{"nightly", "0.3.0", false},
	}
	for _, c := range cases {
		if got := Offer(c.latest, c.current); got != c.want {
			t.Errorf("Offer(%q, %q) = %v, want %v", c.latest, c.current, got, c.want)
		}
	}
}

// Local and snapshot builds are not releases, so the server's minimum
// version does not gate them: a snapshot numbered 0.0.0-SNAPSHOT-... used
// to be refused by every server.
func TestIsRelease(t *testing.T) {
	for _, v := range []string{"0.1.0", "v1.2.3", "0.3.0-rc1", "0.3.0-rc.1", "0.3.0-beta.2", "1.2.3+build.7"} {
		if !IsRelease(v) {
			t.Errorf("%q should be a release", v)
		}
	}
	for _, v := range []string{
		"dev", "test", "",
		"0.0.0-SNAPSHOT-b005a6f", "0.3.1-dev+b005a6f", "v0.3.1-dev", "0.3.1-snapshot.2", "0.4.0-rc1.dev",
		"9.0.0-x/../y", "1.2", "01.2.3",
		// What Go records for a build from a checkout: a pseudo-version
		// for an untagged commit, and +dirty for changed files.
		"0.0.0-20260927101010-b005a6f00000", "0.3.1-0.20260927101010-b005a6f00000",
		"0.4.0-rc.1.0.20260927101010-b005a6f00000", "0.3.0+dirty", "v0.3.1-0.20260927101010-b005a6f00000+dirty",
	} {
		if IsRelease(v) {
			t.Errorf("%q should not be a release", v)
		}
	}
}

// A tag is three numbers and an optional pre-release. Anything else is not
// a version: never newer, never a release, never part of a URL.
func TestValidVersion(t *testing.T) {
	for _, v := range []string{"1.2.3", "v1.2.3", "0.3.0-rc1", "v0.3.0-rc.1", "0.0.0-SNAPSHOT-b005a6f", "10.20.30"} {
		if !validVersion(v) {
			t.Errorf("%q should be a valid version", v)
		}
	}
	for _, v := range []string{
		"", "dev", "1.2", "1.2.3.4", "v1.2.3-", "1.2.3-../../x", "1.2.3-rc1/x", "1.2.3+build", "1.2.3\n", "vv1.2.3", "1.2.3-rc 1", "١.٢.٣",
		// The right characters, but not semantic versions.
		"01.2.3", "1.02.3", "1.2.3-rc..1", "1.2.3-.rc", "1.2.3-rc.", "1.2.3-01",
	} {
		if validVersion(v) {
			t.Errorf("%q should not be a valid version", v)
		}
	}
	// Before, "9.0.0-x/../y" counted as 9.0.0 and was offered as an update.
	for _, v := range []string{"9.0.0-x/../y", "9.0.0+x/../y", "9.0.0-rc1\n/../y"} {
		if Newer(v, "1.0.0") || Offer(v, "1.0.0") || IsRelease(v) {
			t.Errorf("the malformed tag %q must never be newer, offered or a release", v)
		}
	}
}

// fakeGitHub serves a latest-release page that redirects as GitHub's does,
// to the tag in *tag, or to the list of releases when that is empty, and
// counts the requests for it. The tag page itself answers 500: it is never
// fetched.
func fakeGitHub(t *testing.T, tag *string) (asked *atomic.Int32) {
	t.Helper()
	asked = new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/firmfact/cli/releases/latest":
			asked.Add(1)
			to := "/firmfact/cli/releases"
			if *tag != "" {
				to += "/tag/" + url.PathEscape(*tag)
			}
			http.Redirect(w, r, to, http.StatusFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	pointAt(t, srv.URL+"/firmfact/cli/releases/latest")
	return asked
}

// pointAt makes LatestReleaseURL page for the rest of the test.
func pointAt(t *testing.T, page string) {
	t.Helper()
	prev := LatestReleaseURL
	LatestReleaseURL = page
	t.Cleanup(func() { LatestReleaseURL = prev })
}

// The releases come from the repository the module is named after, at the
// addresses firmfact.com's installers and /cli page publish: moving either
// is a change to the website too.
func TestReleaseCoordinates(t *testing.T) {
	gomod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if got := modfile.ModulePath(gomod); got != "github.com/"+Repo {
		t.Errorf("module %q, releases from %q", got, Repo)
	}
	for got, want := range map[string]string{
		LatestReleaseURL: "https://github.com/firmfact/cli/releases/latest",
		DownloadBase:     "https://github.com/firmfact/cli/releases/download",
	} {
		if got != want {
			t.Errorf("%s, want %s", got, want)
		}
	}
}

// The latest version comes from where GitHub's latest-release page
// redirects, not from its rate-limited API, and a tag that is not a
// version is refused.
func TestLatestReleaseReadsTheRedirect(t *testing.T) {
	tag := "v1.4.0"
	asked := fakeGitHub(t, &tag)

	if got, err := LatestRelease(context.Background()); err != nil || got != "1.4.0" {
		t.Errorf("LatestRelease = %q, %v", got, err)
	}
	if n := asked.Load(); n != 1 {
		t.Errorf("%d requests for the latest release, want 1", n)
	}
	for _, tag = range []string{"v1.4.0/../../evil", "nightly", "v1.4", "v01.4.0", "v1.4.0-rc..1", "v1.4.0+build"} {
		if got, err := LatestRelease(context.Background()); err == nil || got != "" {
			t.Errorf("tag %q: LatestRelease = %q, %v; want an error", tag, got, err)
		}
	}
	// A repository without releases sends to its list of releases.
	tag = ""
	if got, err := LatestRelease(context.Background()); err == nil || !strings.Contains(err.Error(), "names no latest release") {
		t.Errorf("no release: LatestRelease = %q, %v", got, err)
	}
}

// A renamed repository's page redirects to the new one's, which is
// followed over https only; a page that answers without a redirect names
// no release.
func TestLatestReleaseRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/old/releases/latest":
			http.Redirect(w, r, "http://example.invalid/new/releases/latest", http.StatusMovedPermanently)
		default:
			io.WriteString(w, `{"tag_name":"v1.4.0"}`)
		}
	}))
	defer srv.Close()

	pointAt(t, srv.URL+"/old/releases/latest")
	var insecure *httpx.InsecureRedirectError
	if _, err := LatestRelease(context.Background()); !errors.As(err, &insecure) {
		t.Errorf("a redirect to plain http: %v", err)
	}
	pointAt(t, srv.URL+"/repos/x/releases/latest")
	if _, err := LatestRelease(context.Background()); err == nil || !strings.Contains(err.Error(), "200 OK, not a redirect") {
		t.Errorf("an answer without a redirect: %v", err)
	}
}

// fakeServer answers the version endpoint with minimum and counts the
// requests for it.
func fakeServer(t *testing.T, minimum string) (host string, asked *atomic.Int32) {
	t.Helper()
	asked = new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/cli/version" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		asked.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"minimum_version": minimum})
	}))
	t.Cleanup(srv.Close)
	return srv.URL, asked
}

// Check asks GitHub only when told to; the server always.
func TestCheckAsksGitHubOnlyWhenTold(t *testing.T) {
	tag := "v1.4.0"
	github := fakeGitHub(t, &tag)
	host, server := fakeServer(t, "0.5.0")
	dir := t.TempDir()

	s := Check(context.Background(), host, dir, false)
	if github.Load() != 0 || server.Load() != 1 {
		t.Errorf("without GitHub: %d GitHub and %d server requests, want 0 and 1", github.Load(), server.Load())
	}
	if s.Latest != "" || s.Minimum != "0.5.0" {
		t.Errorf("without GitHub: %+v", s)
	}
	if _, due := Cached(host, dir, false); due {
		t.Error("a check without GitHub is not the day's check for the next one without GitHub")
	}
	// The first command of the day wrote to a pipe; the next, on a
	// terminal, still asks GitHub.
	if _, due := Cached(host, dir, true); !due {
		t.Error("a check without GitHub counts as the day's question to GitHub")
	}

	s = Check(context.Background(), host, dir, true)
	if github.Load() != 1 || s.Latest != "1.4.0" || s.Minimum != "0.5.0" {
		t.Errorf("with GitHub: %d GitHub requests, %+v", github.Load(), s)
	}
	if got, due := Cached(host, dir, true); due || got.Latest != "1.4.0" {
		t.Errorf("cached %+v, due %v", got, due)
	}
	// A later check without GitHub keeps the day's question to GitHub.
	Check(context.Background(), host, dir, false)
	if got, due := Cached(host, dir, true); due || got.Latest != "1.4.0" {
		t.Errorf("after a check without GitHub: cached %+v, due %v", got, due)
	}
	if _, due := Cached("https://other.example", dir, false); !due {
		t.Error("another host's check counts for this one")
	}
}

// A GitHub that never answers does not hold up the command a check runs
// beside: stopping the check returns at once. The server's answer, which
// came in, is kept, but the check stays due, so the next run asks again.
func TestStopDoesNotWaitForGitHub(t *testing.T) {
	arrived := make(chan struct{}, 1)
	blackhole := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-r.Context().Done()
	}))
	defer blackhole.Close()
	pointAt(t, blackhole.URL)
	host, _ := fakeServer(t, "0.5.0")
	dir := t.TempDir()

	stop := Start(context.Background(), host, dir, true)
	<-arrived
	waitFor(t, func() bool { s, _ := Cached(host, dir, true); return s.Minimum == "0.5.0" })
	start := time.Now()
	stop()
	if took := time.Since(start); took > time.Second {
		t.Errorf("stop took %s", took)
	}
	if s, due := Cached(host, dir, false); !due || s.Minimum != "0.5.0" {
		t.Errorf("cached %+v, due %v; want the minimum kept and the check due", s, due)
	}
}

// A check stopped before any source answered learned nothing, so it writes
// nothing: what the last check found, here for another host, stays.
func TestAStoppedCheckWritesNothing(t *testing.T) {
	arrived := make(chan struct{}, 2)
	blackhole := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-r.Context().Done()
	}))
	defer blackhole.Close()
	pointAt(t, blackhole.URL+"/releases/latest")
	dir := t.TempDir()
	Record(context.Background(), "https://a.example", dir, "1.0.0", "0.5.0", true)
	before, err := os.ReadFile(filepath.Join(dir, statusFile))
	if err != nil {
		t.Fatal(err)
	}

	stop := Start(context.Background(), blackhole.URL, dir, true)
	<-arrived
	<-arrived
	stop()
	if after, _ := os.ReadFile(filepath.Join(dir, statusFile)); !bytes.Equal(after, before) {
		t.Errorf("the status went from %s to %s", before, after)
	}
}

// waitFor polls until done, for up to ten seconds.
func waitFor(t *testing.T, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !done(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("gave up waiting")
		}
	}
}

// Every embedded release key decodes (the package would not even load
// otherwise), and a key the CLI does not know signs nothing it accepts.
func TestReleaseKeys(t *testing.T) {
	if len(trustedKeys) == 0 || len(trustedKeys) != len(releaseKeys) {
		t.Fatalf("%d trusted keys from %d embedded", len(trustedKeys), len(releaseKeys))
	}
	_, stranger, _ := ed25519.GenerateKey(rand.Reader)
	sums := []byte("abc123  firmfact_1.2.3_linux_amd64.tar.gz\n")
	if err := VerifyChecksums(sums, ed25519.Sign(stranger, sums)); err == nil {
		t.Error("a stranger's signature verified")
	}
}

func TestChecksumFor(t *testing.T) {
	sums := []byte("abc123  firmfact_0.1.0_linux_amd64.tar.gz\ndef456  firmfact_0.1.0_darwin_arm64.tar.gz\n")
	got, err := checksumFor(sums, "firmfact_0.1.0_darwin_arm64.tar.gz")
	if err != nil || got != "def456" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := checksumFor(sums, "firmfact_0.1.0_windows_amd64.zip"); err == nil {
		t.Error("missing entry must be an error")
	}
}

func TestUpgradeHint(t *testing.T) {
	if UpgradeHint(Homebrew, "ff") != "brew upgrade firmfact" || UpgradeHint(Scoop, "ff") != "scoop update firmfact" ||
		UpgradeHint(Winget, "ff") != "winget upgrade --exact --id Firmfact.CLI" || UpgradeHint(Direct, "ff") != "ff update" {
		t.Error("unexpected hints")
	}
}

// A package manager's copy must never be classified Direct, or `firmfact
// update` rewrites a file that manager owns; a download kept by hand must
// stay Direct, or the CLI points at an upgrade command that cannot work.
func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		exe  string
		env  map[string]string
		want Method
	}{
		{"cask on Apple silicon", "/opt/homebrew/Caskroom/firmfact/0.2.0/firmfact", nil, Homebrew},
		{"cask on an Intel Mac", "/usr/local/Caskroom/firmfact/0.2.0/firmfact", nil, Homebrew},
		{"cask on Linux", "/home/linuxbrew/.linuxbrew/Caskroom/firmfact/0.2.0/firmfact", nil, Homebrew},
		{"formula in the Cellar", "/usr/local/Cellar/firmfact/0.2.0/bin/firmfact", nil, Homebrew},
		{"custom Homebrew prefix", "/srv/brew/opt/firmfact/firmfact", map[string]string{"HOMEBREW_PREFIX": "/srv/brew/"}, Homebrew},
		{"download in the Intel Homebrew bin", "/usr/local/bin/firmfact", map[string]string{"HOMEBREW_PREFIX": "/usr/local"}, Direct},
		{"scoop, default root", "C:/Users/x/scoop/apps/firmfact/current/firmfact.exe", nil, Scoop},
		{"scoop, backslashes", `C:\Users\x\scoop\apps\firmfact\current\firmfact.exe`, nil, Scoop},
		{"scoop, custom root", "D:/tools/apps/firmfact/current/firmfact.exe", map[string]string{"SCOOP": "D:/tools"}, Scoop},
		{"scoop, custom root in other case", `d:\TOOLS\apps\firmfact\0.2.0\firmfact.exe`, map[string]string{"SCOOP": `D:\tools\`}, Scoop},
		{"scoop, custom global root", "E:/shared/apps/firmfact/current/firmfact.exe", map[string]string{"SCOOP_GLOBAL": "E:/shared"}, Scoop},
		{"download beside a custom scoop root", "D:/tools/firmfact.exe", map[string]string{"SCOOP": "D:/tools"}, Direct},
		{"download sharing a scoop root's name", "D:/tools-old/apps/firmfact.exe", map[string]string{"SCOOP": "D:/tools"}, Direct},
		{"winget, per user", `C:\Users\x\AppData\Local\Microsoft\WinGet\Packages\Firmfact.CLI_Microsoft.Winget.Source_8wekyb3d8bbwe\firmfact.exe`, nil, Winget},
		{"winget, for the machine", `C:\Program Files\WinGet\Packages\Firmfact.CLI_Microsoft.Winget.Source_8wekyb3d8bbwe\firmfact.exe`, nil, Winget},
		{"winget link left unresolved", `C:\Users\x\AppData\Local\Microsoft\WinGet\Links\firmfact.exe`, nil, Winget},
		{"download beside winget's folder", `C:\Users\x\AppData\Local\Microsoft\WinGet\firmfact.exe`, nil, Direct},
		{"download in ~/.local/bin", "/home/x/.local/bin/firmfact", map[string]string{"HOMEBREW_PREFIX": "/home/linuxbrew/.linuxbrew"}, Direct},
		{"go install into $GOPATH/bin", "/home/x/go/bin/firmfact", map[string]string{"GOPATH": "/home/x/go"}, Direct},
		{"download on Windows", `C:\Users\x\AppData\Local\Programs\firmfact\firmfact.exe`, nil, Direct},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			getenv := func(key string) string { return c.env[key] }
			if got := classify(c.exe, getenv); got != c.want {
				t.Errorf("classify(%q) = %s, want %s", c.exe, got, c.want)
			}
		})
	}
}

func TestExtractFindsTheBinary(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{"README.md": "readme", binaryName(): "binary-bytes"} {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()

	got, err := extract(buf.Bytes(), "tar.gz")
	if err != nil || string(got) != "binary-bytes" {
		t.Fatalf("extract = %q, %v", got, err)
	}
}

// A source that could not be reached leaves its field empty; the value the
// last check learned for the same host stands in, and not one learned for
// another host.
func TestRecordKeepsWhatWasKnown(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	Record(ctx, "https://a.example", dir, "1.0.0", "0.5.0", true)

	if s := Record(ctx, "https://a.example", dir, "", "", true); s.Latest != "1.0.0" || s.Minimum != "0.5.0" {
		t.Errorf("same host: %+v", s)
	}
	if s := Record(ctx, "https://b.example", dir, "", "0.7.0", true); s.Latest != "" || s.Minimum != "0.7.0" {
		t.Errorf("other host: %+v", s)
	}
	if s := load(dir); s.Host != "https://b.example" || s.Minimum != "0.7.0" {
		t.Errorf("stored: %+v", s)
	}
}

// The status goes to a file of its own that is renamed into place: a
// symlink planted at its name is replaced, and the file it points at is
// left as it was.
func TestRecordDoesNotWriteThroughALink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("making a symbolic link takes a privilege on Windows")
	}
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), ".bashrc")
	if err := os.WriteFile(victim, []byte("export KEEP=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, statusFile)); err != nil {
		t.Fatal(err)
	}
	Record(context.Background(), "https://a.example", dir, "1.0.0", "0.5.0", true)
	if got, err := os.ReadFile(victim); err != nil || string(got) != "export KEEP=1\n" {
		t.Errorf("the link's target now holds %q, %v", got, err)
	}
	if info, err := os.Lstat(filepath.Join(dir, statusFile)); err != nil || !info.Mode().IsRegular() {
		t.Errorf("status file: %v, %v", info, err)
	}
	if s := load(dir); s.Latest != "1.0.0" || s.Minimum != "0.5.0" {
		t.Errorf("stored %+v", s)
	}
}

// The latest release the last check heard of is known whichever host that
// check was for, though the host's minimum is not.
func TestLatestKnown(t *testing.T) {
	dir := t.TempDir()
	if got := LatestKnown(dir); got != "" {
		t.Errorf("before any check: %q", got)
	}
	Record(context.Background(), "https://a.example", dir, "1.0.0", "0.5.0", true)
	if got := LatestKnown(dir); got != "1.0.0" {
		t.Errorf("LatestKnown = %q, want 1.0.0", got)
	}
	if s, _ := Cached("https://b.example", dir, true); s.Latest != "" || s.Minimum != "" {
		t.Errorf("another host's cached status: %+v", s)
	}
}
