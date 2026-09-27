// Package update keeps the CLI current: a daily check for a newer release
// and for the minimum version the server supports, and a self-update for
// binaries that were downloaded directly (package-manager installs are left
// to their package manager).
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/httpx"
)

const (
	Repo       = "firmfact/cli"
	checkEvery = 24 * time.Hour
	// checkTimeout bounds each source of a check: the server's answer, and
	// GitHub's with any redirects on the way.
	checkTimeout = 3 * time.Second
	// maxHops is how many renames of the repository the latest-release
	// lookup follows before it gives up.
	maxHops = 5
)

// LatestReleaseURL is GitHub's page for the newest release, which redirects
// to that release's tag (a variable so tests can point it elsewhere). The
// page is used rather than GitHub's API, which allows 60 unauthenticated
// requests an hour per address: an office whose traffic leaves through one
// address, as a bank's often does, soon uses those up.
var LatestReleaseURL = "https://github.com/" + Repo + "/releases/latest"

// Status is what the last check found. CheckedAt is when the day's check
// last completed; GitHubCheckedAt is when one of them last asked GitHub,
// which a check does only when told to (see Check). Latest is GitHub's
// latest release, never a pre-release; Newest the newest in its feed of
// releases, pre-releases included, which a check reads only for someone
// running a pre-release (see GitHubFor).
type Status struct {
	CheckedAt       time.Time `json:"checked_at"`
	GitHubCheckedAt time.Time `json:"github_checked_at,omitzero"`
	Host            string    `json:"host"`
	Latest          string    `json:"latest"`
	Newest          string    `json:"newest,omitempty"`
	Minimum         string    `json:"minimum"`
}

// NewestFor is the newest release the check knows of that someone running
// current may be offered: the latest release, or for someone on a
// pre-release, the newest in the feed when that is newer. "" when the
// check knows of none. Whether it is worth offering is Offer's to say.
func (s Status) NewestFor(current string) string {
	if GitHubFor(current) == WithPreReleases && (s.Latest == "" || Newer(s.Newest, s.Latest)) {
		return s.Newest
	}
	return s.Latest
}

// Cached is what the last check found for host, and whether the next check
// is due: a day after the last one, or at once when that was for another
// host, whose minimum version says nothing about this one. For a check
// that would ask GitHub (github), it is also due a day after GitHub was
// last asked, so that a day whose first command wrote to a pipe, which
// leaves GitHub out, does not keep a later one on a terminal from hearing
// of a new release.
func Cached(host, cacheDir string, github bool) (Status, bool) {
	s := load(cacheDir)
	if s.Host != host {
		return Status{}, true
	}
	due := time.Since(s.CheckedAt) >= checkEvery || github && time.Since(s.GitHubCheckedAt) >= checkEvery
	return s, due
}

// Check asks the server for the oldest CLI version it supports and GitHub
// what github says (nothing, with NoGitHub), both at once, and records
// what they said. A source that cannot be reached leaves its field as the
// last check found it; it never fails a command.
func Check(ctx context.Context, host, cacheDir string, github GitHub) Status {
	return check(ctx, host, cacheDir, sourceOf(github))
}

// Start is Check in the background, so that neither source adds to the
// time of the command it runs beside; what it learns applies from the next
// run. stop cancels whatever is still being waited for and returns once
// the answers that did come in are recorded. A check stopped before every
// source it asked had answered does not count as the day's check, so the
// next run asks again.
func Start(ctx context.Context, host, cacheDir string, github GitHub) (stop func()) {
	// Read here, not in the background: a test points it elsewhere and
	// back again around the command.
	src := sourceOf(github)
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		check(ctx, host, cacheDir, src)
	}()
	return func() {
		cancel()
		<-done
	}
}

// answer is what one source of a check said. stopped means the check was
// stopped before the source answered, so its silence says nothing.
type answer struct {
	fromGitHub bool
	releases   Releases
	minimum    string
	stopped    bool
}

// check asks the server, and GitHub what src says, and records each answer
// as it comes: a server that answers at once is kept even when the command
// ends while GitHub is still being waited for. An answer cut short says
// nothing, so it is not written down.
func check(ctx context.Context, host, cacheDir string, src source) Status {
	answers := make(chan answer, 2)
	asked := 1
	go func() {
		minimum, err := serverMinimum(ctx, host)
		answers <- answer{minimum: minimum, stopped: err != nil && ctx.Err() != nil}
	}()
	github := src.github != NoGitHub
	if github {
		asked++
		go func() {
			found, err := ask(ctx, src, checkTimeout)
			answers <- answer{fromGitHub: true, releases: found, stopped: err != nil && ctx.Err() != nil}
		}()
	}
	var found Releases
	var minimum string
	complete := true
	s, _ := Cached(host, cacheDir, false)
	for i := range asked {
		a := <-answers
		if a.stopped {
			complete = false
			continue
		}
		if a.fromGitHub {
			found = a.releases
		} else {
			minimum = a.minimum
		}
		s = record(host, cacheDir, found, minimum, complete && i == asked-1, github)
	}
	return s
}

// serverMinimum asks host for the oldest CLI version it supports. The
// server is asked as the API is: its redirects are not followed.
func serverMinimum(ctx context.Context, host string) (string, error) {
	var server struct {
		MinimumVersion string `json:"minimum_version"`
	}
	err := getJSON(ctx, httpx.New(httpx.Options{Timeout: checkTimeout}), host+"/api/v1/cli/version", &server)
	return server.MinimumVersion, err
}

// latestRelease reads the latest release's version from where page, GitHub's
// latest-release page, redirects (.../releases/tag/v1.2.3), within limit.
// The release page itself is never fetched. The only redirect followed is
// to another latest-release page, as a renamed repository sends, and over
// https only. A redirect to the list of releases, which GitHub sends while
// no release is published or every one is a pre-release, is errNoLatest.
func latestRelease(ctx context.Context, page string, limit time.Duration) (string, error) {
	return withinLimit(ctx, page, limit, func(ctx context.Context) (string, error) {
		client := httpx.New(httpx.Options{Timeout: limit})
		for hops := 0; ; hops++ {
			to, err := redirectOf(ctx, client, page)
			if err != nil {
				return "", err
			}
			if _, tag, ok := strings.Cut(to.Path, "/releases/tag/"); ok {
				if !validVersion(tag) {
					return "", fmt.Errorf("the latest release is tagged %q, which is not a version", tag)
				}
				return strings.TrimPrefix(tag, "v"), nil
			}
			if strings.HasSuffix(to.Path, "/releases") {
				return "", errNoLatest
			}
			// Anywhere else is no answer GitHub is known to give.
			if !strings.HasSuffix(to.Path, "/releases/latest") {
				return "", fmt.Errorf("%s names no latest release", page)
			}
			if to.Scheme != "https" {
				return "", &httpx.InsecureRedirectError{To: to.Redacted()}
			}
			if hops == maxHops {
				return "", fmt.Errorf("stopped after %d redirects", maxHops)
			}
			page = to.String()
		}
	})
}

// redirectOf is where page redirects to. A HEAD request is enough: the
// answer's Location says it all.
func redirectOf(ctx context.Context, client *httpx.Client, page string) (*url.URL, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, page, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	to, err := resp.Location()
	if resp.StatusCode/100 != 3 || err != nil {
		return nil, &AnswerError{URL: page, Status: resp.Status, Code: resp.StatusCode, want: "a redirect to the latest release"}
	}
	return to, nil
}

// versionPattern is the shape of a release tag: three numbers and an
// optional pre-release. The tag reaches us unsigned in GitHub's redirect and
// becomes part of download URLs and file names, so nothing else passes
// ("1.2.3-x/../../y" must not become a path).
var versionPattern = regexp.MustCompile(`^v?\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

// buildPattern is the shape of a version as a binary reports it: a tag's
// shape, plus the build metadata a snapshot carries after a '+'
// ("0.3.1-dev+b005a6f"; see snapshot in .goreleaser.yaml). It never names
// a download.
var buildPattern = regexp.MustCompile(`^v?\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// validVersion reports whether v has the shape of a release tag and is a
// semantic version (no leading zeros, no empty pre-release parts).
func validVersion(v string) bool {
	return versionPattern.MatchString(v) && semver.IsValid(withV(v))
}

// canonical is v as golang.org/x/mod/semver compares it ("v0.3.0-rc.1"),
// or false when v is not a full semantic version. The pattern comes first
// because semver alone reads "v1.2" as v1.2.0.
func canonical(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if !buildPattern.MatchString(v) {
		return "", false
	}
	v = withV(v)
	return v, semver.IsValid(v)
}

// withV is v with the leading 'v' semver requires, whether or not it had
// one.
func withV(v string) string {
	return "v" + strings.TrimPrefix(v, "v")
}

// Record stores what a check learned about host, the releases on GitHub
// and the server's minimum version, and returns the status. Any of them
// may be empty when its source could not be reached or was not asked; the
// cached value stands in. github says whether GitHub was asked. doctor
// asks the server itself, so it records its findings here.
func Record(ctx context.Context, host, cacheDir string, found Releases, minimum string, github bool) Status {
	return record(host, cacheDir, found, minimum, ctx.Err() == nil, github)
}

// Remember stores what update learned of the releases on GitHub, as a
// check for host would. It is not the day's check, which asks the server
// too, so that stays due when it was.
func Remember(host, cacheDir string, found Releases) {
	record(host, cacheDir, found, "", false, false)
}

// record is Record for a check that may not have heard from every source.
// Only a complete one counts as the day's check, and as the day's question
// to GitHub when it asked GitHub; an incomplete one keeps what it learned
// but leaves the check due, so the next run asks again.
func record(host, cacheDir string, found Releases, minimum string, complete, github bool) Status {
	cached := load(cacheDir)
	now := time.Now()
	s := Status{CheckedAt: now, Host: host, Latest: found.Latest, Newest: found.Newest, Minimum: minimum}
	if github {
		s.GitHubCheckedAt = now
	}
	// Keep what we knew when this round could not reach a source, or did
	// not ask it.
	if cached.Host == host {
		if s.Latest == "" {
			s.Latest = cached.Latest
		}
		if s.Newest == "" {
			s.Newest = cached.Newest
		}
		if s.Minimum == "" {
			s.Minimum = cached.Minimum
		}
		if !github {
			s.GitHubCheckedAt = cached.GitHubCheckedAt
		}
	}
	if !complete {
		s.CheckedAt, s.GitHubCheckedAt = time.Time{}, time.Time{}
		if cached.Host == host {
			s.CheckedAt, s.GitHubCheckedAt = cached.CheckedAt, cached.GitHubCheckedAt
		}
	}
	// Best effort, like the check. Through a file of its own that is renamed
	// into place, never written through a symlink at the name.
	_ = config.WriteJSON(filepath.Join(cacheDir, statusFile), s, 0o600)
	return s
}

// LatestKnown is the newest release the last recorded check knew of that
// someone running current may be offered (see Status.NewestFor), whichever
// host that check was for, as GitHub's answer does not depend on the host;
// "" when it knew of none.
func LatestKnown(cacheDir, current string) string { return load(cacheDir).NewestFor(current) }

const statusFile = "version-check.json"

// load is the last check's status, or the zero Status.
func load(cacheDir string) Status {
	var s Status
	raw, err := os.ReadFile(filepath.Join(cacheDir, statusFile))
	if err != nil || json.Unmarshal(raw, &s) != nil {
		return Status{}
	}
	return s
}

func getJSON(ctx context.Context, client *httpx.Client, page string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, page, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", page, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(out)
}

// Newer reports whether version a is newer than b by semantic-version
// precedence: "0.2.0" > "0.1.9", "0.3.0-rc.2" > "0.3.0-rc.1", and a release
// is newer than its own pre-releases ("0.3.0" > "0.3.0-rc1"). So someone
// on a release candidate is offered the release, and a minimum of 0.3.0
// turns 0.3.0-rc1 away. Build metadata does not count. Versions that do not
// parse (such as "dev") are never newer or older.
func Newer(a, b string) bool {
	va, okA := canonical(a)
	vb, okB := canonical(b)
	return okA && okB && semver.Compare(va, vb) > 0
}

// Offer reports whether release latest is worth offering to someone running
// current: it must be newer, and someone on a release is never offered a
// pre-release, since they did not choose to test one. GitHub's latest
// release is never a pre-release, but the feed a check reads for someone
// on a pre-release lists them; this keeps the promise either way.
func Offer(latest, current string) bool {
	if !Newer(latest, current) {
		return false
	}
	lv, _ := canonical(latest)
	cv, _ := canonical(current)
	return semver.Prerelease(lv) == "" || semver.Prerelease(cv) != ""
}

// IsRelease reports whether v is a released version rather than a local,
// test or snapshot build ("dev", "test", "0.3.1-dev+b005a6f",
// "0.0.0-SNAPSHOT-b005a6f"). Only a release takes part in the checks; any
// other build is exempt from the server's minimum version, which a
// snapshot numbered from 0.0.0 would never meet. A build from a checkout
// reports the version Go gives it: a pseudo-version for an untagged commit
// ("0.3.1-0.20260927101010-b005a6f00000"), and "+dirty" when the files had
// changed. Neither is a release.
func IsRelease(v string) bool {
	cv, ok := canonical(v)
	if !ok || module.IsPseudoVersion(cv) {
		return false
	}
	pre := strings.ToLower(semver.Prerelease(cv))
	build := strings.ToLower(semver.Build(cv))
	return !strings.Contains(pre, "snapshot") && !strings.Contains(pre, "dev") && !strings.Contains(build, "dirty")
}

// Method is how this binary was installed, which decides how to upgrade it.
type Method string

const (
	Homebrew Method = "homebrew"
	Scoop    Method = "scoop"
	Winget   Method = "winget"
	Direct   Method = "direct"
)

// InstallMethod is how the running binary was installed.
func InstallMethod() Method {
	exe, err := os.Executable()
	if err != nil {
		return Direct
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return classify(exe, os.Getenv)
}

// classify decides how the binary at exe (symlinks already resolved) was
// installed. Getting this wrong towards Direct is the costly mistake: the
// self-update then rewrites a file its package manager owns. getenv is
// passed in so tests can describe any machine.
func classify(exe string, getenv func(string) string) Method {
	p := normalise(exe)
	switch {
	// Formulae live in the Cellar and casks in the Caskroom, wherever the
	// prefix is (/usr/local on Intel Macs, /opt/homebrew on Apple silicon).
	case strings.Contains(p, "/cellar/") || strings.Contains(p, "/caskroom/") ||
		strings.Contains(p, "/homebrew/") || strings.Contains(p, "/linuxbrew/"):
		return Homebrew
	case strings.Contains(p, "/scoop/"):
		return Scoop
	// winget keeps a portable command, which firmfact is to it, in its
	// Packages directory, per user (%LOCALAPPDATA%\Microsoft\WinGet) or
	// for the machine (%ProgramFiles%\WinGet), and links it from Links
	// beside that, which a link that cannot be resolved still shows.
	case strings.Contains(p, "/winget/packages/") || strings.Contains(p, "/winget/links/"):
		return Winget
	}
	// A custom Homebrew prefix. Its bin directory is the exception: brew
	// only ever links into it, so after resolving symlinks a file there was
	// put there by hand. That matters on Intel Macs, where the prefix is
	// /usr/local and /usr/local/bin is where a direct download often goes.
	if prefix := normalise(getenv("HOMEBREW_PREFIX")); prefix != "" && within(p, prefix) {
		if dir := path.Dir(p); dir != prefix+"/bin" && dir != prefix+"/sbin" {
			return Homebrew
		}
	}
	// A custom Scoop root (a user or a global one). Scoop installs every
	// app under its apps directory, so a root such as D:/tools does not
	// claim everything else kept there.
	for _, key := range []string{"SCOOP", "SCOOP_GLOBAL"} {
		if root := normalise(getenv(key)); root != "" && within(p, root+"/apps") {
			return Scoop
		}
	}
	return Direct
}

// normalise makes a path comparable whichever system wrote it: forward
// slashes, no trailing slash, and lower case, because macOS and Windows
// match paths regardless of case.
func normalise(p string) string {
	if strings.TrimSpace(p) == "" {
		return ""
	}
	return strings.ToLower(path.Clean(strings.ReplaceAll(p, `\`, "/")))
}

// within reports whether p lies below dir (both normalised).
func within(p, dir string) bool {
	return strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

// UpgradeHint is the command that upgrades this installation. name is how
// the CLI was invoked, which is what runs its own update; Homebrew and
// Scoop know the package as firmfact whatever the user calls it. winget
// gets the package's identifier, exactly: a name it matches against every
// package's name and moniker, from any publisher.
func UpgradeHint(m Method, name string) string {
	switch m {
	case Homebrew:
		return "brew upgrade firmfact"
	case Scoop:
		return "scoop update firmfact"
	case Winget:
		return "winget upgrade --exact --id Firmfact.CLI"
	}
	return name + " update"
}

// PinHint is the command that keeps the package manager m from upgrading
// firmfact past the version it has, or "" when m cannot: Homebrew pins
// formulae only, and firmfact is a cask.
func PinHint(m Method) string {
	switch m {
	case Scoop:
		return "scoop hold firmfact"
	case Winget:
		return "winget pin add --exact --id Firmfact.CLI"
	}
	return ""
}

// InstallHint is the command with which the package manager m installs
// release version in place of the one it has, or "" when m cannot. Only
// winget keeps every version it was given; Homebrew's cask and Scoop's
// manifest name the latest release alone. None of them is given a
// pre-release (see skip_upload in .goreleaser.yaml).
func InstallHint(m Method, version string) string {
	if m != Winget || IsPreRelease(version) {
		return ""
	}
	return "winget install --exact --id Firmfact.CLI --version " + version
}
