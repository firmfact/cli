package cmd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/firmfact/cli/internal/api"
	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/update"
)

// releases fakes GitHub for update: the latest release is latest ("" for
// a GitHub that is failing, which answers 502), and every download is
// missing. It returns the number of downloads asked for.
func releases(t *testing.T, latest string) func() int32 {
	t.Helper()
	var downloads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/latest" && latest != "":
			redirectToTag(w, r, latest)
		case r.URL.Path == "/latest":
			w.WriteHeader(http.StatusBadGateway)
		default:
			downloads.Add(1)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	prevURL, prevBase, prevVersion := update.LatestReleaseURL, update.DownloadBase, api.Version
	update.LatestReleaseURL, update.DownloadBase = srv.URL+"/latest", srv.URL+"/download"
	t.Cleanup(func() { update.LatestReleaseURL, update.DownloadBase, api.Version = prevURL, prevBase, prevVersion })
	return downloads.Load
}

// directDownload makes the test binary count as a direct download,
// however it was built, or skips the test.
func directDownload(t *testing.T) {
	t.Helper()
	for _, key := range []string{"HOMEBREW_PREFIX", "SCOOP", "SCOOP_GLOBAL"} {
		t.Setenv(key, "")
	}
	if m := update.InstallMethod(); m != update.Direct {
		t.Skipf("the test binary counts as installed with %s", m)
	}
}

// update says when there is nothing newer, fails as unavailable when
// GitHub cannot say what the latest release is, and otherwise downloads
// it; here the download is missing, which fails the command and leaves the
// running binary as it was.
func TestUpdateCommand(t *testing.T) {
	isolate(t)
	directDownload(t)
	host := meServer(t).URL

	releases(t, "")
	code, msg := exitStatusOf(t.Context(), "0.1.0", "--host", host, "update")
	if code != ExitUnavailable || !strings.HasPrefix(msg, "GitHub cannot say which releases there are just now") || !strings.HasSuffix(msg, "; try again later") {
		t.Errorf("GitHub failing: exit %d (%s)", code, msg)
	}
	// Only a GitHub that gives no answer is one that could not be reached.
	prev := update.LatestReleaseURL
	update.LatestReleaseURL = closedHost(t) + "/latest"
	code, msg = exitStatusOf(t.Context(), "0.1.0", "--host", host, "update")
	update.LatestReleaseURL = prev
	if code != ExitUnavailable || !strings.HasPrefix(msg, "could not reach GitHub: ") || !strings.HasSuffix(msg, "; check your connection or https://github.com/firmfact/cli/releases") {
		t.Errorf("GitHub unreachable: exit %d (%s)", code, msg)
	}

	isolate(t) // forget what the last check learned
	releases(t, "v0.1.0")
	if stdout, _, err := run("0.1.0", "--host", host, "update"); err != nil || stdout != "You have the latest version (0.1.0).\n" {
		t.Errorf("up to date: %q, %v", stdout, err)
	}

	isolate(t)
	downloads := releases(t, "v0.2.0")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(self)
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := run("0.1.0", "--host", host, "--json", "update")
	if err == nil || !strings.HasSuffix(err.Error(), "/download/v0.2.0/checksums.txt was not found") {
		t.Fatalf("got %v, want the missing download", err)
	}
	if stdout != "" || stderr != "Updating firmfact 0.1.0 to 0.2.0...\n" {
		t.Errorf("--json: stdout %q, stderr %q", stdout, stderr)
	}
	if downloads() != 1 {
		t.Errorf("%d downloads, want 1", downloads())
	}
	if after, err := os.Stat(self); err != nil || !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Errorf("the running binary changed: %v", err)
	}
}

// update runs whatever FIRMFACT_PROFILE names, as it runs with a config
// file that cannot be read: a profile that does not exist must not stand
// between a user and a fixed release. The host whose check it adds what it
// learns to is then the default one, as for a new profile; a command that
// uses the profile gets no host for it.
func TestUpdateRunsWithoutTheProfile(t *testing.T) {
	isolate(t)
	directDownload(t)
	host := meServer(t).URL
	releases(t, "v0.1.0")
	t.Setenv("FIRMFACT_PROFILE", "gone")
	t.Setenv("FIRMFACT_HOST", host)
	if stdout, _, err := run("0.1.0", "update"); err != nil || stdout != "You have the latest version (0.1.0).\n" {
		t.Errorf("update: %q, %v", stdout, err)
	}

	t.Setenv("FIRMFACT_HOST", "")
	cfg := &config.Config{CurrentProfile: config.DefaultProfile, Profiles: map[string]*config.Profile{}}
	app := &App{Name: "firmfact", Config: cfg, ProfileName: "gone"}
	if _, err := app.Host(); err == nil {
		t.Error("a command that uses the profile got a host for one that does not exist")
	}
	app.missingProfileOK = true
	if got, err := app.Host(); err != nil || got != config.DefaultHost {
		t.Errorf("Host() = %q, %v; want %s", got, err, config.DefaultHost)
	}
}

// claim shows its plan and changes nothing without --yes off a terminal;
// with it, it links the name and puts ~/.local/bin on the PATH, says so,
// and has nothing to do a second time; --undo takes it all back. A name
// that was free has no previous definition to keep, and the output does
// not pretend it has one.
func TestClaimCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows gets a .cmd shim and no rc block; TestWindowsShim covers it")
	}
	// A PATH without the user's own directories, where no other ff hides
	// the one claim makes.
	t.Setenv("PATH", "/usr/bin:/bin")
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash in /usr/bin or /bin")
	}
	isolate(t)
	t.Setenv("FIRMFACT_NO_UPDATE_CHECK", "1")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ZDOTDIR", "")
	home := os.Getenv("HOME")
	rc := filepath.Join(home, ".bashrc")
	link := filepath.Join(home, ".local", "bin", "ff")
	const original = "export KEEP=1\n"
	if err := os.WriteFile(rc, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := run("0.1.0", "claim", "--shell", "bash")
	if code, _ := Classify(err); code != ExitUsage || !strings.Contains(err.Error(), "run again with --yes") {
		t.Fatalf("without --yes: exit %d, %v", code, err)
	}
	for _, want := range []string{"To make ff run firmfact:", "1. link " + link + " -> ", "2. add this block to the end of " + rc + ":", "export PATH="} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the plan lacks %q:\n%s", want, stdout)
		}
	}
	if _, err := os.Lstat(link); err == nil {
		t.Error("the link was made without --yes")
	}
	if got, _ := os.ReadFile(rc); string(got) != original {
		t.Errorf(".bashrc changed without --yes: %q", got)
	}

	stdout, _, err = run("0.1.0", "claim", "ff", "--yes", "--shell", "bash")
	if err != nil {
		t.Fatalf("claim --yes: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout, "ff is yours. Open a new terminal (or run: source "+rc+"), then try: ff whoami") ||
		!strings.Contains(stdout, "The previous "+rc+" is saved as ") {
		t.Errorf("claim --yes said:\n%s", stdout)
	}
	if strings.Contains(stdout, "your previous ff") || !strings.HasSuffix(stdout, "\nUndo everything with: firmfact claim ff --undo\n") {
		t.Errorf("claim --yes speaks of a previous ff that never was, or not of undo:\n%s", stdout)
	}
	if _, err := os.Readlink(link); err != nil {
		t.Errorf("no link: %v", err)
	}

	if stdout, _, err = run("0.1.0", "claim", "ff", "--shell", "bash"); err != nil || stdout != "ff already runs firmfact.\n" {
		t.Errorf("claiming again: %q, %v", stdout, err)
	}

	stdout, _, err = run("0.1.0", "claim", "ff", "--undo", "--shell", "bash")
	if err != nil || !strings.HasSuffix(stdout, "Done. Open a new terminal for ff to go back to what it was.\n") {
		t.Errorf("--undo: %q, %v", stdout, err)
	}
	if _, err := os.Lstat(link); err == nil {
		t.Error("--undo left the link")
	}
	if got, _ := os.ReadFile(rc); string(got) != original {
		t.Errorf(".bashrc after --undo = %q", got)
	}
	if stdout, _, err = run("0.1.0", "claim", "ff", "--undo", "--shell", "bash"); err != nil || stdout != "Nothing to undo for ff.\n" {
		t.Errorf("--undo again: %q, %v", stdout, err)
	}
}

// doctor's write probe is a file of its own. At a fixed name, a symlink
// that someone who can write to the config directory planted there would
// have doctor empty the file it points at, a ~/.bashrc say.
func TestDoctorProbeDoesNotWriteThroughALink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("making a symbolic link takes a privilege on Windows")
	}
	isolate(t)
	fakeGitHub(t)
	srv := meServer(t)
	signedInWithTools(t, srv.URL, listVendors)
	dir := os.Getenv("FIRMFACT_CONFIG_DIR")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), ".bashrc")
	if err := os.WriteFile(victim, []byte("export KEEP=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, ".doctor")); err != nil {
		t.Fatal(err)
	}

	if stdout, _, err := run("test", "--host", srv.URL, "doctor"); err != nil {
		t.Fatalf("doctor: %v\n%s", err, stdout)
	}
	if got, err := os.ReadFile(victim); err != nil || string(got) != "export KEEP=1\n" {
		t.Errorf("the link's target now holds %q, %v", got, err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".doctor-*")); len(left) > 0 {
		t.Errorf("the probe was left behind: %v", left)
	}
}
