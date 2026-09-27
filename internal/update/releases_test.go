package update

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	"time"

	"github.com/firmfact/cli/internal/httpx"
)

// feedOf is GitHub's Atom feed of releases, as it writes it, listing a
// release for each tag in the order given, newest first as GitHub has it.
// Each entry's page is .../releases/tag/<tag>, the tag escaped as GitHub
// escapes it.
func feedOf(tags ...string) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom" xmlns:media="http://search.yahoo.com/mrss/" xml:lang="en-US">
  <id>tag:github.com,2008:https://github.com/firmfact/cli/releases</id>
  <link type="text/html" rel="alternate" href="https://github.com/firmfact/cli/releases"/>
  <link type="application/atom+xml" rel="self" href="https://github.com/firmfact/cli/releases.atom"/>
  <title>Release notes from cli</title>
  <updated>2026-09-27T17:10:31Z</updated>
`)
	for _, tag := range tags {
		fmt.Fprintf(&b, `  <entry>
    <id>tag:github.com,2008:Repository/1390997455/%[1]s</id>
    <updated>2026-09-27T17:39:56Z</updated>
    <link rel="alternate" type="text/html" href="https://github.com/firmfact/cli/releases/tag/%[2]s"/>
    <title>%[1]s</title>
    <content type="html">&lt;h3&gt;Added&lt;/h3&gt;&lt;p&gt;Something&#39;s new.&lt;/p&gt;</content>
    <author>
      <name>github-actions[bot]</name>
    </author>
  </entry>
`, html.EscapeString(tag), url.PathEscape(tag))
	}
	b.WriteString("</feed>\n")
	return []byte(b.String())
}

// mixedFeed lists final releases and pre-releases, the newest by version
// (0.3.0-rc.2) neither first nor last, among entries that are no release
// to offer: a draft, which GitHub would give an untagged page, a tag that
// is no version, versions that are not full or not releases, and a tag
// that would climb out of its URL.
var mixedFeed = feedOf(
	"v0.2.1", // a fix to 0.2, published after 0.3.0-rc.2
	"untagged-4b8e2f0c7d1a9e3b5f6c",
	"v0.3.0-rc.2",
	"nightly",
	"v0.3.0-rc.1",
	"v1.2",
	"v9.9.9-x/../../y",
	"v0.4.0-dev.1",
	"v01.2.3",
	"v0.2.0",
	"0.1.0",
)

// The feed as github.com serves it (testdata/releases.atom, fetched while
// v0.1.0-rc.1 was the only release) reads as that one release.
func TestParseGitHubsFeed(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "releases.atom"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseFeed(body)
	if err != nil || len(got) != 1 || got[0] != "0.1.0-rc.1" {
		t.Errorf("parseFeed = %q, %v; want [0.1.0-rc.1]", got, err)
	}
}

// Only an entry whose page is a release tagged with a release version
// counts; the newest is the newest version, not the first entry.
func TestParseFeed(t *testing.T) {
	got, err := parseFeed(mixedFeed)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"0.2.1", "0.3.0-rc.2", "0.3.0-rc.1", "0.2.0", "0.1.0"}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("parseFeed = %q, want %q", got, want)
	}
	if n := newest(got); n != "0.3.0-rc.2" {
		t.Errorf("newest = %q, want 0.3.0-rc.2", n)
	}

	// An entry's page is its alternate link, which a link without rel is
	// too; any other link, or a page that is not a release's, names none.
	entries := `<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom">
<entry><link href="https://github.com/firmfact/cli/releases/tag/v5.0.0"/></entry>
<entry><link rel="enclosure" href="https://github.com/firmfact/cli/releases/tag/v6.0.0"/></entry>
<entry><link rel="alternate" href="https://github.com/firmfact/cli/tree/v7.0.0"/></entry>
<entry><link rel="alternate" href="https://github.com/firmfact/cli/releases/tag/v8.0.0/assets"/></entry>
<entry><link rel="alternate" href="https://github.com/releases/tag/v9.0.0"/></entry>
<entry><link rel="alternate" href="https://github.com/firmfact/cli/releases/tag/v10.0.0?x=1"/></entry>
<entry><title>v11.0.0</title></entry>
</feed>`
	if got, err := parseFeed([]byte(entries)); err != nil || strings.Join(got, " ") != "5.0.0 10.0.0" {
		t.Errorf("parseFeed = %q, %v; want [5.0.0 10.0.0]", got, err)
	}

	// Anything but an Atom feed is an error, not a feed without releases.
	for name, body := range map[string]string{
		"an HTML page":           "<!DOCTYPE html><html><body><h1>Releases</h1></body></html>",
		"JSON from the API":      `[{"tag_name":"v1.2.3","draft":false,"prerelease":false}]`,
		"an RSS feed":            `<?xml version="1.0"?><rss version="2.0"><channel><item><link>https://github.com/firmfact/cli/releases/tag/v1.2.3</link></item></channel></rss>`,
		"a feed of no namespace": `<feed><entry><link href="https://github.com/firmfact/cli/releases/tag/v1.2.3"/></entry></feed>`,
		"nothing":                "",
		"a feed cut short":       string(mixedFeed[:len(mixedFeed)/2]),
	} {
		if got, err := parseFeed([]byte(body)); err == nil {
			t.Errorf("%s: parseFeed = %q, want an error", name, got)
		}
	}
}

// fakeRepo is GitHub as the CLI sees firmfact's releases: the
// latest-release page, the feed of releases, and each release's files.
type fakeRepo struct {
	// latest is the tag the latest-release page sends to; "" sends to the
	// list of releases, as GitHub does while there is no release that is
	// not a pre-release. setLatest changes it once the fake serves.
	latest string
	// feed is the feed of releases; feedStatus, when set, is its answer
	// instead.
	feed       []byte
	feedStatus int
	// files are the releases with files to download, by version, and
	// whether their checksums.txt is signed by signer, a key the test
	// trusts. A release not here has none.
	files  map[string]bool
	signer ed25519.PrivateKey

	archives map[string][]byte // each release's archive, made by serve
	mu       sync.Mutex
	hits     map[string]int
}

// serve starts the fake and points the CLI at it for the rest of the test.
func (r *fakeRepo) serve(t *testing.T) {
	t.Helper()
	r.hits, r.archives = map[string]int{}, map[string][]byte{}
	for version := range r.files {
		_, ext := archiveName(version)
		r.archives[version] = releaseArchive(t, ext, []byte("binary "+version))
	}
	srv := httptest.NewServer(http.HandlerFunc(r.handle))
	t.Cleanup(srv.Close)
	base := srv.URL + "/firmfact/cli/releases"
	prevLatest, prevFeed, prevBase := LatestReleaseURL, ReleasesFeedURL, DownloadBase
	LatestReleaseURL, ReleasesFeedURL, DownloadBase = base+"/latest", base+".atom", base+"/download"
	t.Cleanup(func() { LatestReleaseURL, ReleasesFeedURL, DownloadBase = prevLatest, prevFeed, prevBase })
}

func (r *fakeRepo) setLatest(tag string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.latest = tag
}

func (r *fakeRepo) handle(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.hits[req.URL.Path]++
	latest := r.latest
	r.mu.Unlock()
	switch path := req.URL.Path; {
	case path == "/firmfact/cli/releases/latest" && latest == "":
		http.Redirect(w, req, "/firmfact/cli/releases", http.StatusFound)
	case path == "/firmfact/cli/releases/latest":
		http.Redirect(w, req, "/firmfact/cli/releases/tag/"+url.PathEscape(latest), http.StatusFound)
	case path == "/firmfact/cli/releases.atom" && r.feedStatus != 0:
		w.WriteHeader(r.feedStatus)
	case path == "/firmfact/cli/releases.atom":
		w.Header().Set("Content-Type", "application/atom+xml; charset=utf-8")
		_, _ = w.Write(r.feed)
	case strings.HasPrefix(path, "/firmfact/cli/releases/download/v"):
		version, file, _ := strings.Cut(strings.TrimPrefix(path, "/firmfact/cli/releases/download/v"), "/")
		body, ok := r.archives[version]
		if !ok {
			http.NotFound(w, req)
			return
		}
		archive, _ := archiveName(version)
		sum := sha256.Sum256(body)
		sums := []byte(hex.EncodeToString(sum[:]) + "  " + archive + "\n")
		switch {
		case file == archive:
			_, _ = w.Write(body)
		case file == checksumsFile:
			_, _ = w.Write(sums)
		case file == signatureFile && r.files[version]:
			_, _ = w.Write(ed25519.Sign(r.signer, sums))
		default:
			http.NotFound(w, req)
		}
	default:
		http.NotFound(w, req)
	}
}

// asked is how often path was requested.
func (r *fakeRepo) asked(path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits[path]
}

// trialRunsAs stands in for the new binary's trial run, which a text file
// posing as a binary cannot pass: the file must hold the binary of the
// version it is installed as.
func trialRunsAs(t *testing.T) {
	t.Helper()
	prev := trialRun
	trialRun = func(_ context.Context, path, version string) error {
		if got, _ := os.ReadFile(path); string(got) != "binary "+version {
			return fmt.Errorf("the new binary reports version %q, not %s", got, version)
		}
		return nil
	}
	t.Cleanup(func() { trialRun = prev })
}

// update finds the latest release where GitHub's latest-release page
// sends, without the feed. While there is none, the feed says whether
// there are pre-releases, and with --pre it gives the newest release,
// pre-releases included.
func TestLookup(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		repo   *fakeRepo
		github GitHub
		want   Releases
		feed   int // requests for the feed
	}{
		{"a release", &fakeRepo{latest: "v0.2.1", feed: mixedFeed}, FinalReleases, Releases{Latest: "0.2.1"}, 0},
		{"pre-releases only", &fakeRepo{feed: feedOf("v0.1.0-rc.2", "untagged-4b8e2f0c7d1a", "v0.1.0-rc.1")}, FinalReleases, Releases{Newest: "0.1.0-rc.2"}, 1},
		{"no release at all", &fakeRepo{feed: feedOf()}, FinalReleases, Releases{}, 1},
		{"a draft only", &fakeRepo{feed: feedOf("untagged-4b8e2f0c7d1a")}, FinalReleases, Releases{}, 1},
		{"pre-releases wanted", &fakeRepo{latest: "v0.2.1", feed: mixedFeed}, WithPreReleases, Releases{Newest: "0.3.0-rc.2"}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.repo.serve(t)
			got, err := Lookup(ctx, c.github)
			if err != nil || got != c.want {
				t.Errorf("Lookup = %+v, %v; want %+v", got, err, c.want)
			}
			if n := c.repo.asked("/firmfact/cli/releases.atom"); n != c.feed {
				t.Errorf("the feed was asked for %d times, want %d", n, c.feed)
			}
			if c.github == WithPreReleases && c.repo.asked("/firmfact/cli/releases/latest") != 0 {
				t.Error("the latest-release page was asked for pre-releases")
			}
		})
	}
}

// A GitHub that cannot be reached, one that is busy, and one that answers
// with something that is not a feed of releases each fail the lookup in a
// way of their own; none of them reads as a repository without releases.
func TestLookupFailures(t *testing.T) {
	ctx := context.Background()
	closed := func() string {
		srv := httptest.NewServer(http.NotFoundHandler())
		srv.Close()
		return srv.URL
	}

	t.Run("no connection", func(t *testing.T) {
		(&fakeRepo{}).serve(t)
		LatestReleaseURL, ReleasesFeedURL = closed()+"/latest", closed()+"/releases.atom"
		var netErr *httpx.Error
		for _, github := range []GitHub{FinalReleases, WithPreReleases} {
			if _, err := Lookup(ctx, github); !errors.As(err, &netErr) || netErr.Kind != httpx.Refused {
				t.Errorf("%d: %v, want a refused connection", github, err)
			}
		}
	})
	for name, c := range map[string]struct {
		repo *fakeRepo
		busy bool
	}{
		"GitHub failing":   {&fakeRepo{feedStatus: http.StatusServiceUnavailable}, true},
		"rate-limited":     {&fakeRepo{feedStatus: http.StatusTooManyRequests}, true},
		"no feed":          {&fakeRepo{feedStatus: http.StatusNotFound}, false},
		"after no release": {&fakeRepo{feedStatus: http.StatusBadGateway}, true},
	} {
		t.Run(name, func(t *testing.T) {
			c.repo.serve(t)
			github := WithPreReleases
			if name == "after no release" {
				github = FinalReleases
			}
			var answer *AnswerError
			if _, err := Lookup(ctx, github); !errors.As(err, &answer) || answer.Busy() != c.busy {
				t.Errorf("%v; want an *AnswerError, busy %v", err, c.busy)
			}
		})
	}
	t.Run("not a feed", func(t *testing.T) {
		(&fakeRepo{feed: []byte("<!DOCTYPE html><html><body>Sign in</body></html>")}).serve(t)
		if got, err := Lookup(ctx, WithPreReleases); err == nil || !strings.Contains(err.Error(), "not a feed of releases") {
			t.Errorf("Lookup = %+v, %v", got, err)
		}
	})
	t.Run("too large", func(t *testing.T) {
		huge := append(feedOf("v0.3.0-rc.1"), make([]byte, maxFeed)...)
		(&fakeRepo{feed: huge}).serve(t)
		if got, err := Lookup(ctx, WithPreReleases); err == nil || !strings.Contains(err.Error(), "larger than 4 MB") {
			t.Errorf("Lookup = %+v, %v", got, err)
		}
	})
	// A GitHub that takes longer than the limit is one that could not be
	// reached, said as such, rather than the context's deadline.
	t.Run("too slow", func(t *testing.T) {
		slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}))
		defer slow.Close()
		for name, lookup := range map[string]func() error{
			"feed": func() error { _, err := newestRelease(ctx, slow.URL+"/releases.atom", 50*time.Millisecond); return err },
			"latest": func() error {
				_, err := latestRelease(ctx, slow.URL+"/releases/latest", 50*time.Millisecond)
				return err
			},
		} {
			var netErr *httpx.Error
			if err := lookup(); !errors.As(err, &netErr) || !strings.HasSuffix(err.Error(), "did not finish answering within 50ms") {
				t.Errorf("%s: %v", name, err)
			}
		}
		// Stopped by the caller, it is the caller's doing.
		stopped, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := newestRelease(stopped, slow.URL, time.Minute); !errors.Is(err, context.Canceled) {
			t.Errorf("stopped: %v", err)
		}
	})
}

// --pre end to end, below the command: the newest release in a feed of
// every kind is a pre-release without a signature, which is refused; the
// signed pre-release before it installs, and so does an older release,
// which is the command's to ask about. A version with no release has
// nothing to download.
func TestInstallFromTheFeed(t *testing.T) {
	key := signingKey(t)
	repo := &fakeRepo{
		feed:   mixedFeed,
		files:  map[string]bool{"0.3.0-rc.2": false, "0.3.0-rc.1": true, "0.2.1": true, "0.2.0": true},
		signer: key,
	}
	repo.serve(t)
	trialRunsAs(t)
	ctx := context.Background()

	found, err := Lookup(ctx, WithPreReleases)
	if err != nil || found.Newest != "0.3.0-rc.2" {
		t.Fatalf("Lookup = %+v, %v", found, err)
	}
	target := scratchBinary(t)
	if err := install(ctx, target, found.Newest); err == nil || !strings.Contains(err.Error(), "release v0.3.0-rc.2 has no checksums.txt.sig, so it cannot be verified; not installing") {
		t.Errorf("the unsigned pre-release: %v", err)
	}
	unchanged(t, target)

	for _, v := range []string{"0.3.0-rc.1", "v0.2.0"} {
		target := scratchBinary(t)
		if err := install(ctx, target, v); err != nil {
			t.Fatalf("install %s: %v", v, err)
		}
		if got, _ := os.ReadFile(target); string(got) != "binary "+strings.TrimPrefix(v, "v") {
			t.Errorf("install %s: the target holds %q", v, got)
		}
	}

	target = scratchBinary(t)
	var missing *NotPublishedError
	if err := install(ctx, target, "0.2.5"); !errors.As(err, &missing) || missing.Version != "0.2.5" || !strings.HasSuffix(missing.URL, "/download/v0.2.5/checksums.txt") {
		t.Errorf("a version never released: %v", err)
	}
	unchanged(t, target)
}

// Someone on a pre-release hears of newer pre-releases from the daily
// check, which reads the feed for them instead of the latest-release page;
// someone on a release is never offered one, whatever the check knows.
func TestTheCheckForAPreRelease(t *testing.T) {
	repo := &fakeRepo{feed: feedOf("v0.1.0-rc.2", "untagged-4b8e2f0c7d1a", "v0.1.0-rc.1")}
	repo.serve(t)
	host, _ := fakeServer(t, "0.1.0-rc.1")
	dir := t.TempDir()
	ctx := context.Background()

	s := Check(ctx, host, dir, GitHubFor("0.1.0-rc.1"))
	if s.Newest != "0.1.0-rc.2" || s.Latest != "" || s.Minimum != "0.1.0-rc.1" {
		t.Errorf("status %+v", s)
	}
	if repo.asked("/firmfact/cli/releases.atom") != 1 || repo.asked("/firmfact/cli/releases/latest") != 0 {
		t.Error("the check did not read the feed alone")
	}
	if _, due := Cached(host, dir, true); due {
		t.Error("the check that read the feed is not the day's")
	}
	if got := LatestKnown(dir, "0.1.0-rc.1"); got != "0.1.0-rc.2" || !Offer(got, "0.1.0-rc.1") {
		t.Errorf("for 0.1.0-rc.1 the newest known is %q", got)
	}
	if got := LatestKnown(dir, "0.0.9"); got != "" {
		t.Errorf("for 0.0.9 the newest known is %q; a release is never offered a pre-release", got)
	}

	// Once there is a release, a check for someone on it asks the
	// latest-release page, and what the feed said stays for the next
	// check that reads it.
	repo.setLatest("v0.1.0")
	s = Check(ctx, host, dir, GitHubFor("0.0.9"))
	if s.Latest != "0.1.0" || s.Newest != "0.1.0-rc.2" || repo.asked("/firmfact/cli/releases.atom") != 1 {
		t.Errorf("status %+v after a check for a release", s)
	}
	if got := s.NewestFor("0.1.0-rc.2"); got != "0.1.0" {
		t.Errorf("for 0.1.0-rc.2 the newest known is %q, want the release", got)
	}
}

func TestNewestFor(t *testing.T) {
	for _, c := range []struct {
		latest, newest, current, want string
	}{
		{"0.2.0", "0.3.0-rc.1", "0.3.0-rc.0", "0.3.0-rc.1"},
		{"0.2.0", "0.3.0-rc.1", "0.2.0", "0.2.0"},
		{"0.3.0", "0.3.0-rc.2", "0.3.0-rc.1", "0.3.0"},
		{"", "0.1.0-rc.2", "0.1.0-rc.1", "0.1.0-rc.2"},
		{"", "0.1.0-rc.2", "0.1.0", ""},
		{"0.2.0", "", "0.2.0-rc.1", "0.2.0"},
		// A local or snapshot build hears of no pre-release.
		{"0.2.0", "0.3.0-rc.1", "0.3.1-dev+b005a6f", "0.2.0"},
		{"0.2.0", "0.3.0-rc.1", "dev", "0.2.0"},
	} {
		s := Status{Latest: c.latest, Newest: c.newest}
		if got := s.NewestFor(c.current); got != c.want {
			t.Errorf("%+v.NewestFor(%q) = %q, want %q", s, c.current, got, c.want)
		}
	}
}

func TestGitHubFor(t *testing.T) {
	for v, want := range map[string]GitHub{
		"0.1.0-rc.1": WithPreReleases, "v0.3.0-beta.2": WithPreReleases,
		"0.1.0": FinalReleases, "dev": FinalReleases, "0.3.1-dev+b005a6f": FinalReleases, "0.0.0-SNAPSHOT-b005a6f": FinalReleases,
	} {
		if got := GitHubFor(v); got != want {
			t.Errorf("GitHubFor(%q) = %d, want %d", v, got, want)
		}
	}
}

// What update learns is kept for the next check to build on, but is not
// the day's check, which asks the server too.
func TestRememberIsNotTheDaysCheck(t *testing.T) {
	dir := t.TempDir()
	Remember("https://a.example", dir, Releases{Newest: "0.1.0-rc.2"})
	if s, due := Cached("https://a.example", dir, false); !due || s.Newest != "0.1.0-rc.2" {
		t.Errorf("cached %+v, due %v; want the release kept and the check due", s, due)
	}

	Record(context.Background(), "https://a.example", dir, Releases{Latest: "0.1.0"}, "0.0.9", true)
	Remember("https://a.example", dir, Releases{Newest: "0.2.0-rc.1"})
	s, due := Cached("https://a.example", dir, true)
	if due || s.Latest != "0.1.0" || s.Newest != "0.2.0-rc.1" || s.Minimum != "0.0.9" {
		t.Errorf("cached %+v, due %v", s, due)
	}
}

// A package manager is told how to keep firmfact at a version where it
// can, and how to install one where it can; it never has pre-releases.
func TestPackageManagerHints(t *testing.T) {
	if PinHint(Homebrew) != "" || PinHint(Scoop) != "scoop hold firmfact" ||
		PinHint(Winget) != "winget pin add --exact --id Firmfact.CLI" || PinHint(Direct) != "" {
		t.Error("unexpected pin hints")
	}
	if InstallHint(Homebrew, "0.2.0") != "" || InstallHint(Scoop, "0.2.0") != "" ||
		InstallHint(Winget, "0.2.0") != "winget install --exact --id Firmfact.CLI --version 0.2.0" ||
		InstallHint(Winget, "0.3.0-rc.1") != "" {
		t.Error("unexpected install hints")
	}
}

func TestReleaseVersion(t *testing.T) {
	for in, want := range map[string]string{"0.2.0": "0.2.0", "v0.3.0-rc.1": "0.3.0-rc.1", "10.20.30": "10.20.30"} {
		if got, ok := ReleaseVersion(in); !ok || got != want {
			t.Errorf("ReleaseVersion(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "latest", "1.2", "v1.2.3+build", " 1.2.3", "1.2.3/../x", "01.2.3"} {
		if got, ok := ReleaseVersion(in); ok {
			t.Errorf("ReleaseVersion(%q) = %q; want no version", in, got)
		}
	}
}
