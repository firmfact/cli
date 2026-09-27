package cmd

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/firmfact/cli/internal/api"
	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/update"
)

// github is GitHub for update and the daily check: the latest-release
// page, the feed of releases, and the files of each release.
type github struct {
	// latest is the tag the latest-release page sends to; "" sends to the
	// list of releases, as while there is no release that is not a
	// pre-release.
	latest string
	// feed lists a release for each tag, newest first.
	feed []string
	// files are the releases with files to download, by version, and
	// whether their checksums.txt has a signature. The signature is never
	// by a key the CLI trusts, so update refuses every one of them; the
	// release that installs is internal/update's to test.
	files map[string]bool

	mu   sync.Mutex
	hits map[string]int
}

// serve starts the fake and points the CLI at it for the rest of the test.
func (g *github) serve(t *testing.T) {
	t.Helper()
	g.hits = map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(g.handle))
	t.Cleanup(srv.Close)
	base := srv.URL + "/firmfact/cli/releases"
	prevLatest, prevFeed, prevBase, prevVersion := update.LatestReleaseURL, update.ReleasesFeedURL, update.DownloadBase, api.Version
	update.LatestReleaseURL, update.ReleasesFeedURL, update.DownloadBase = base+"/latest", base+".atom", base+"/download"
	t.Cleanup(func() {
		update.LatestReleaseURL, update.ReleasesFeedURL, update.DownloadBase, api.Version = prevLatest, prevFeed, prevBase, prevVersion
	})
}

func (g *github) handle(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.hits[r.URL.Path]++
	g.mu.Unlock()
	switch path := r.URL.Path; {
	case path == "/firmfact/cli/releases/latest" && g.latest == "":
		http.Redirect(w, r, "/firmfact/cli/releases", http.StatusFound)
	case path == "/firmfact/cli/releases/latest":
		redirectToTag(w, r, g.latest)
	case path == "/firmfact/cli/releases.atom":
		w.Header().Set("Content-Type", "application/atom+xml; charset=utf-8")
		fmt.Fprint(w, releasesFeed(g.feed...))
	case strings.HasPrefix(path, "/firmfact/cli/releases/download/v"):
		version, file, _ := strings.Cut(strings.TrimPrefix(path, "/firmfact/cli/releases/download/v"), "/")
		signed, ok := g.files[version]
		switch {
		case ok && file == "checksums.txt":
			fmt.Fprintf(w, "%s  firmfact_%s_linux_amd64.tar.gz\n", strings.Repeat("0", 64), version)
		case ok && signed && file == "checksums.txt.sig":
			sig := make([]byte, 64)
			_, _ = rand.Read(sig)
			_, _ = w.Write(sig)
		default:
			http.NotFound(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

// asked is how often path was requested; downloads how many of the
// release files were.
func (g *github) asked(path string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hits[path]
}

// total is how many requests the fake has had.
func (g *github) total() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, hits := range g.hits {
		n += hits
	}
	return n
}

func (g *github) downloads() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for path, hits := range g.hits {
		if strings.Contains(path, "/releases/download/") {
			n += hits
		}
	}
	return n
}

// releasesFeed is GitHub's Atom feed of releases as it writes it, an entry
// for each tag.
func releasesFeed(tags ...string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom" xmlns:media="http://search.yahoo.com/mrss/" xml:lang="en-US">
  <id>tag:github.com,2008:https://github.com/firmfact/cli/releases</id>
  <link type="text/html" rel="alternate" href="https://github.com/firmfact/cli/releases"/>
  <title>Release notes from cli</title>
`)
	for _, tag := range tags {
		fmt.Fprintf(&b, `  <entry>
    <id>tag:github.com,2008:Repository/1390997455/%[1]s</id>
    <link rel="alternate" type="text/html" href="https://github.com/firmfact/cli/releases/tag/%[2]s"/>
    <title>%[1]s</title>
    <content type="html">&lt;p&gt;Notes&lt;/p&gt;</content>
  </entry>
`, html.EscapeString(tag), url.PathEscape(tag))
	}
	b.WriteString("</feed>\n")
	return b.String()
}

// mixedReleases is a feed of final releases and pre-releases, whose newest
// by version (0.3.0-rc.2) is not its first entry, among entries that are
// no release to offer: a draft, which GitHub would give an untagged page,
// and tags that are no version.
var mixedReleases = []string{"v0.2.1", "untagged-4b8e2f0c7d1a", "v0.3.0-rc.2", "nightly", "v0.3.0-rc.1", "v1.2", "v0.2.0"}

// selfUnchanged fails the test unless the running binary is as it was
// when the test began.
func selfUnchanged(t *testing.T) func() {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(self)
	if err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		if after, err := os.Stat(self); err != nil || !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
			t.Errorf("the running binary changed: %v", err)
		}
	}
}

// While every release is a pre-release, GitHub names no latest release.
// update says so, and how to take the newest pre-release, rather than
// blaming the connection; nothing is wrong, so it exits 0.
func TestUpdateWithPreReleasesOnly(t *testing.T) {
	isolate(t)
	directDownload(t)
	host := meServer(t).URL
	g := &github{feed: []string{"v0.1.0-rc.1", "untagged-4b8e2f0c7d1a"}}
	g.serve(t)

	stdout, _, err := runAs("ff", "--host", host, "update")
	if err != nil || stdout != "No release is published yet, only pre-releases. To take the newest pre-release: ff update --pre\n" {
		t.Errorf("update: %q, %v", stdout, err)
	}
	stdout, _, err = run("0.0.9", "--host", host, "--json", "update")
	var got updateResult
	decodeOnly(t, "update", stdout, &got)
	if err != nil || got != (updateResult{Version: "0.0.9", Newest: "0.1.0-rc.1"}) {
		t.Errorf("update --json: %+v, %v", got, err)
	}
	if g.downloads() != 0 {
		t.Errorf("%d downloads, want none", g.downloads())
	}
	// What it learned is there for version to show.
	cacheDir, err := config.CacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := update.Cached(host, cacheDir, false); s.Newest != "0.1.0-rc.1" {
		t.Errorf("cached %+v", s)
	}

	g = &github{}
	g.serve(t)
	if stdout, _, err := run("0.0.9", "--host", host, "update"); err != nil || stdout != "No release is published yet.\n" {
		t.Errorf("no release at all: %q, %v", stdout, err)
	}
	if stdout, _, err := run("0.0.9", "--host", host, "update", "--pre"); err != nil || stdout != "No release is published yet.\n" {
		t.Errorf("--pre, no release at all: %q, %v", stdout, err)
	}

	// A feed that is not one says nothing of the releases: that is a
	// failure, not a repository without any.
	update.ReleasesFeedURL = answering(t, http.StatusOK, "text/html", "<!DOCTYPE html><html><body>Sign in</body></html>")
	code, msg := exitStatusOf(t.Context(), "0.0.9", "--host", host, "update")
	if code != ExitFailed || !strings.HasPrefix(msg, "could not tell which releases there are: ") || !strings.Contains(msg, "not a feed of releases") {
		t.Errorf("not a feed: exit %d (%s)", code, msg)
	}
}

// --pre takes the newest release, pre-releases included, by version rather
// than by its place in the feed, and holds it to the same checks: here it
// has no signature, and the running binary stays. Without --pre, update
// takes the latest release, and tells someone on a newer pre-release
// where the newer ones are.
func TestUpdatePre(t *testing.T) {
	isolate(t)
	directDownload(t)
	host := meServer(t).URL
	g := &github{latest: "v0.2.1", feed: mixedReleases, files: map[string]bool{"0.3.0-rc.2": false, "0.2.1": true}}
	g.serve(t)
	unchanged := selfUnchanged(t)

	stdout, stderr, err := run("0.2.0", "--host", host, "--json", "update", "--pre")
	if err == nil || err.Error() != "release v0.3.0-rc.2 has no checksums.txt.sig, so it cannot be verified; not installing" {
		t.Errorf("--pre: %v", err)
	}
	if stdout != "" || stderr != "Updating firmfact 0.2.0 to 0.3.0-rc.2...\n" {
		t.Errorf("--pre --json: stdout %q, stderr %q", stdout, stderr)
	}
	if g.asked("/firmfact/cli/releases/latest") != 0 || g.asked("/firmfact/cli/releases.atom") != 1 {
		t.Error("--pre did not read the feed alone")
	}

	if stdout, _, err := run("0.3.0-rc.2", "--host", host, "update", "--pre"); err != nil || stdout != "You have the newest version, pre-releases included (0.3.0-rc.2).\n" {
		t.Errorf("--pre on the newest: %q, %v", stdout, err)
	}

	// The latest release is signed, but not with firmfact's key.
	_, _, err = run("0.2.0", "--host", host, "update")
	if code, _ := Classify(err); code != ExitFailed || err.Error() != "release v0.2.1: the signature does not match firmfact's release key; not installing" {
		t.Errorf("update: exit %d, %v", code, err)
	}
	stdout, _, err = run("0.3.0-rc.1", "--host", host, "update")
	if err != nil || stdout != "You have 0.3.0-rc.1, newer than the latest release (0.2.1). To take newer pre-releases too: firmfact update --pre\n" {
		t.Errorf("update on a newer pre-release: %q, %v", stdout, err)
	}
	unchanged()
}

// --version installs the release it names, with or without its v, after
// the same checks. One older than this version needs --yes off a terminal;
// the same version needs nothing; a version never released is not found.
// --pre and --version together, or a --version that is no version, are
// usage errors that ask nothing of GitHub.
func TestUpdateVersion(t *testing.T) {
	isolate(t)
	directDownload(t)
	host := meServer(t).URL
	g := &github{latest: "v0.2.1", feed: mixedReleases, files: map[string]bool{"0.1.0": false, "0.3.0-rc.2": false}}
	g.serve(t)
	unchanged := selfUnchanged(t)

	for _, args := range [][]string{{"--pre", "--version", "0.2.1"}, {"--version", "1.2"}, {"--version", "latest"}, {"--version", ""}} {
		code, msg := exitStatusOf(t.Context(), "0.2.0", append([]string{"--host", host, "update"}, args...)...)
		if code != ExitUsage {
			t.Errorf("%v: exit %d (%s), want %d", args, code, msg, ExitUsage)
		}
	}
	if _, msg := exitStatusOf(t.Context(), "0.2.0", "--host", host, "update", "--version", "1.2"); msg != `--version takes a release version such as 0.2.0 or 0.3.0-rc.1, not "1.2"` {
		t.Errorf("--version 1.2: %s", msg)
	}

	code, msg := exitStatusOf(t.Context(), "0.2.0", "--host", host, "update", "--version", "v0.1.0")
	if code != ExitUsage || msg != "0.1.0 is older than the 0.2.0 you have; nothing changed. Add --yes to install it all the same" {
		t.Errorf("an older version without --yes: exit %d (%s)", code, msg)
	}
	if n := g.total(); n != 0 {
		t.Errorf("GitHub was asked %d time(s) before anything was agreed", n)
	}

	stdout, stderr, err := run("0.2.0", "--host", host, "--json", "update", "--version", "v0.1.0", "--yes")
	if err == nil || err.Error() != "release v0.1.0 has no checksums.txt.sig, so it cannot be verified; not installing" {
		t.Errorf("an older version with --yes: %v", err)
	}
	if stdout != "" || stderr != "Installing firmfact 0.1.0 in place of 0.2.0...\n" {
		t.Errorf("--json: stdout %q, stderr %q", stdout, stderr)
	}
	// A newer pre-release needs no --yes, nor --pre.
	if _, _, err := run("0.2.0", "--host", host, "update", "--version", "0.3.0-rc.2"); err == nil || !strings.Contains(err.Error(), "release v0.3.0-rc.2 has no checksums.txt.sig") {
		t.Errorf("a newer pre-release: %v", err)
	}

	code, msg = exitStatusOf(t.Context(), "0.2.0", "--host", host, "update", "--version", "0.9.9")
	if code != ExitNotFound || !strings.HasPrefix(msg, "there is no release 0.9.9 (") ||
		!strings.HasSuffix(msg, "/download/v0.9.9/checksums.txt was not found); the releases are at https://github.com/firmfact/cli/releases") {
		t.Errorf("a version never released: exit %d (%s)", code, msg)
	}

	before := g.downloads()
	if stdout, _, err := run("0.2.0", "--host", host, "update", "--version", "0.2.0"); err != nil || stdout != "You have 0.2.0 already.\n" {
		t.Errorf("the same version: %q, %v", stdout, err)
	}
	if g.downloads() != before {
		t.Error("the same version was downloaded")
	}
	if g.asked("/firmfact/cli/releases/latest") != 0 || g.asked("/firmfact/cli/releases.atom") != 0 {
		t.Error("--version looked for the latest release")
	}
	unchanged()
}

// A copy that Homebrew installed is Homebrew's to upgrade, with --pre and
// --version too; update says so, and asks nothing of GitHub.
func TestUpdateLeavesHomebrewItsOwn(t *testing.T) {
	isolate(t)
	host := meServer(t).URL
	g := &github{latest: "v0.2.1", feed: mixedReleases}
	g.serve(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	t.Setenv("HOMEBREW_PREFIX", filepath.Dir(filepath.Dir(exe)))
	if update.InstallMethod() != update.Homebrew {
		t.Skipf("the test binary at %s does not pass for a Homebrew install", exe)
	}

	stdout, _, err := run("0.2.0", "--host", host, "update", "--version", "0.2.1")
	want := "This copy was installed with homebrew, which installs the latest release only; upgrade with:\n\n  brew upgrade firmfact\n\nThere is no way to keep homebrew from upgrading it.\n"
	if err != nil || stdout != want {
		t.Errorf("--version: %q, %v", stdout, err)
	}
	stdout, _, err = run("0.2.0", "--host", host, "--json", "update", "--pre")
	var got updateResult
	decodeOnly(t, "update", stdout, &got)
	if err != nil || got != (updateResult{Version: "0.2.0", Upgrade: "brew upgrade firmfact"}) {
		t.Errorf("--pre --json: %+v, %v", got, err)
	}
	if n := g.total(); n != 0 {
		t.Errorf("GitHub was asked %d time(s)", n)
	}
}

// Scoop keeps a version with hold, winget with a pin, and winget alone
// installs a version it is asked for; neither has pre-releases.
func TestUpdateLeavesScoopAndWingetTheirOwn(t *testing.T) {
	for _, c := range []struct {
		method  update.Method
		version string
		pre     bool
		want    string
	}{
		{update.Scoop, "", true, "This copy was installed with scoop, which has releases only, not pre-releases; upgrade with:\n\n  scoop update firmfact\n\nTo keep scoop from upgrading it: scoop hold firmfact\n"},
		{update.Scoop, "0.2.0", false, "This copy was installed with scoop, which installs the latest release only; upgrade with:\n\n  scoop update firmfact\n\nTo keep scoop from upgrading it: scoop hold firmfact\n"},
		{update.Winget, "0.2.0", false, "This copy was installed with winget; upgrade with:\n\n  winget upgrade --exact --id Firmfact.CLI\n\nor install 0.2.0 with:\n\n  winget install --exact --id Firmfact.CLI --version 0.2.0\n\nTo keep winget from upgrading it: winget pin add --exact --id Firmfact.CLI\n"},
		{update.Winget, "0.3.0-rc.1", false, "This copy was installed with winget, which installs the latest release only; upgrade with:\n\n  winget upgrade --exact --id Firmfact.CLI\n\nTo keep winget from upgrading it: winget pin add --exact --id Firmfact.CLI\n"},
	} {
		var out bytes.Buffer
		app := &App{Name: "firmfact", Out: &out}
		if err := leaveToPackageManager(app, c.method, c.version, c.pre); err != nil || out.String() != c.want {
			t.Errorf("%s %q (pre %v):\n%s\nwant:\n%s", c.method, c.version, c.pre, out.String(), c.want)
		}
	}
	var out bytes.Buffer
	app := &App{Name: "firmfact", Out: &out, JSONOutput: true}
	if err := leaveToPackageManager(app, update.Winget, "0.2.0", false); err != nil {
		t.Fatal(err)
	}
	var got updateResult
	decodeOnly(t, "update", out.String(), &got)
	want := updateResult{Version: api.Version, Target: "0.2.0", Upgrade: "winget upgrade --exact --id Firmfact.CLI",
		Install: "winget install --exact --id Firmfact.CLI --version 0.2.0", Pin: "winget pin add --exact --id Firmfact.CLI"}
	if got != want {
		t.Errorf("--json: %+v, want %+v", got, want)
	}
}

// Someone on a pre-release hears of newer pre-releases: version shows the
// newest the last check found, and how to take it, and so does doctor,
// which reads GitHub's feed of releases for them. Someone on a release is
// offered only the release.
func TestPreReleasesAreOfferedToPreReleases(t *testing.T) {
	isolate(t)
	t.Setenv("FIRMFACT_NO_UPDATE_CHECK", "")
	g := &github{latest: "v0.1.0", feed: []string{"v0.2.0-rc.1", "v0.1.0", "v0.1.0-rc.2"}}
	g.serve(t)
	knowLatest(t, "0.1.0")
	cacheDir, err := config.CacheDir()
	if err != nil {
		t.Fatal(err)
	}
	update.Remember("https://elsewhere.example", cacheDir, update.Releases{Newest: "0.2.0-rc.1"})
	hint := upgradeTo(update.InstallMethod(), "firmfact", "0.2.0-rc.1")

	for version, want := range map[string]versionReport{
		"0.2.0-rc.0": {Latest: "0.2.0-rc.1", UpdateAvailable: true, Upgrade: hint},
		"0.1.0-rc.2": {Latest: "0.2.0-rc.1", UpdateAvailable: true, Upgrade: hint},
		"0.0.9":      {Latest: "0.1.0", UpdateAvailable: true, Upgrade: update.UpgradeHint(update.InstallMethod(), "firmfact")},
		"0.1.0":      {Latest: "0.1.0"},
	} {
		stdout, _, err := run(version, "version", "--json")
		var got versionReport
		decodeOnly(t, "version", stdout, &got)
		if err != nil || got.Latest != want.Latest || got.UpdateAvailable != want.UpdateAvailable || got.Upgrade != want.Upgrade {
			t.Errorf("%s: version says latest %q, available %v, upgrade %q; want %+v", version, got.Latest, got.UpdateAvailable, got.Upgrade, want)
		}
	}

	srv := meServer(t)
	stdout, _, _ := run("0.1.0-rc.2", "--host", srv.URL, "--json", "doctor")
	if c := versionCheck(t, stdout); !c.OK || c.Detail != "0.1.0-rc.2 works; 0.2.0-rc.1 is available ("+hint+")" {
		t.Errorf("doctor on a pre-release: %+v", c)
	}
	if g.asked("/firmfact/cli/releases.atom") != 1 || g.asked("/firmfact/cli/releases/latest") != 0 {
		t.Error("doctor on a pre-release did not read the feed alone")
	}
	stdout, _, _ = run("0.1.0", "--host", srv.URL, "--json", "doctor")
	if c := versionCheck(t, stdout); !c.OK || c.Detail != "0.1.0 is the latest" {
		t.Errorf("doctor on a release: %+v", c)
	}
}
