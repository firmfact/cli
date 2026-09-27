package cmd

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/update"
)

// runBuild is run for a binary whose build stamped b.
func runBuild(b Build, args ...string) (stdout, stderr string, err error) {
	var out, errOut bytes.Buffer
	err = NewRootCommand(b, args, IOStreams{In: strings.NewReader(""), Out: &out, Err: &errOut}).Execute()
	return out.String(), errOut.String(), err
}

// stamped is a release build as .goreleaser.yaml stamps it.
var stamped = Build{Version: "0.4.0", Commit: "0123456789abcdef0123456789abcdef01234567", Date: "2026-09-27T08:01:00Z"}

// knowLatest records latest as the newest release a check heard of, in a
// check for another host than any the test uses: the latest release does
// not depend on the host.
func knowLatest(t *testing.T, latest string) {
	t.Helper()
	cacheDir, err := config.CacheDir()
	if err != nil {
		t.Fatal(err)
	}
	update.Record(context.Background(), "https://elsewhere.example", cacheDir, latest, "", true)
}

// version prints what the build stamped, what the binary was built with
// and for, how it was installed and the latest release the last check
// found, in text and in JSON, without asking any server.
func TestVersionCommand(t *testing.T) {
	isolate(t)
	knowLatest(t, "0.4.1")
	method := update.InstallMethod()
	hint := update.UpgradeHint(method, "firmfact")

	stdout, stderr, err := runBuild(stamped, "--host", "localhost:1", "version")
	if err != nil || stderr != "" {
		t.Fatalf("version: %v, stderr %q", err, stderr)
	}
	for _, want := range []string{
		"firmfact 0.4.0\n",
		"  commit    0123456789abcdef0123456789abcdef01234567\n",
		"  committed 27 Sep 2026 08:01 UTC\n",
		"  go        " + runtime.Version() + "\n",
		"  platform  " + runtime.GOOS + "/" + runtime.GOARCH + "\n",
		"  install   " + string(method) + "\n",
		"  latest    0.4.1 is available; upgrade with: " + hint + "\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("version lacks %q:\n%s", want, stdout)
		}
	}

	stdout, _, err = runBuild(stamped, "version", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got versionReport
	decodeOnly(t, "version", stdout, &got)
	want := versionReport{
		buildReport: buildReport{
			Version: "0.4.0", Commit: stamped.Commit, Date: stamped.Date,
			GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH, InstallMethod: method,
		},
		Latest: "0.4.1", UpdateAvailable: true, Upgrade: hint,
	}
	if got != want {
		t.Errorf("version --json = %+v\nwant %+v", got, want)
	}
}

// Without a stamp or anything Go recorded, the commit and its date are
// unknown; before any check the latest release is too, and the text says
// where to look it up. The release in use is not offered to itself.
func TestVersionCommandUnknowns(t *testing.T) {
	isolate(t)
	stdout, _, err := runBuild(Build{Version: "0.4.0"}, "version")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"  commit    unknown\n",
		"  committed unknown\n",
		"  latest    not known yet; `firmfact doctor` looks it up\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("version lacks %q:\n%s", want, stdout)
		}
	}

	knowLatest(t, "0.4.0")
	modified := stamped
	modified.Modified = true
	stdout, _, err = runBuild(modified, "version")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"  commit    0123456789abcdef0123456789abcdef01234567, with uncommitted changes\n",
		"  latest    0.4.0 (this version)\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("version lacks %q:\n%s", want, stdout)
		}
	}
	stdout, _, err = runBuild(modified, "--json", "version")
	if err != nil {
		t.Fatal(err)
	}
	var got versionReport
	decodeOnly(t, "version", stdout, &got)
	if !got.Modified || got.Latest != "0.4.0" || got.UpdateAvailable || got.Upgrade != "" {
		t.Errorf("version --json = %+v", got)
	}
}

// version needs no profile, so a config file that cannot be used does not
// stop it, and it says which version runs when it is below the server's
// minimum (TestOutdatedVersionCanStillSignOutAndUndo).
func TestVersionWithBrokenConfig(t *testing.T) {
	isolate(t)
	path := writeConfigFile(t, brokenConfig)
	stdout, _, err := runBuild(stamped, "version")
	if err != nil || !strings.HasPrefix(stdout, "firmfact 0.4.0\n") {
		t.Errorf("version = %q, %v", stdout, err)
	}
	assertFileHolds(t, path, brokenConfig)
}

// doctor reports the build the way version does, and every request to the
// server names the build's commit in its User-Agent.
func TestDoctorShowsTheBuild(t *testing.T) {
	isolate(t)
	fakeGitHub(t)
	var mu sync.Mutex
	agents := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		agents[r.UserAgent()] = true
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/cli/me":
			io.WriteString(w, `{"data":{"user":{"id":"u1","name":"Jan","email":"jan@yourfirm.example"},"workspaces":[]}}`)
		case "/api/v1/cli/version":
			io.WriteString(w, `{"minimum_version":"0.0.1"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	storedToken(t, srv.URL, time.Hour)

	stdout, _, _ := runBuild(stamped, "--host", srv.URL, "--json", "doctor")
	var report doctorReport
	decodeOnly(t, "doctor", stdout, &report)
	want := buildReport{
		Version: "0.4.0", Commit: stamped.Commit, Date: stamped.Date,
		GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH, InstallMethod: update.InstallMethod(),
	}
	if report.Build != want {
		t.Errorf("doctor --json build = %+v\nwant %+v", report.Build, want)
	}

	stdout, _, _ = runBuild(stamped, "--host", srv.URL, "doctor")
	if !strings.HasPrefix(stdout, "firmfact 0.4.0 on "+srv.URL+"\n  commit    "+stamped.Commit+"\n  committed 27 Sep 2026 08:01 UTC\n") {
		t.Errorf("doctor does not start with the build:\n%s", stdout)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(agents) != 1 || !agents["firmfact-cli/0.4.0 (commit 0123456789ab)"] {
		t.Errorf("User-Agents = %v; want the version and the commit", agents)
	}
}
