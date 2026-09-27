package update

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"github.com/firmfact/cli/internal/httpx"
)

// ReleasesFeedURL is GitHub's Atom feed of the repository's releases, the
// most recent first, pre-releases among them (a variable so tests can
// point it elsewhere). GitHub's latest-release page never names a
// pre-release, so the feed is how the CLI hears of them. Like that page it
// is not GitHub's API, and not held to its limit of 60 requests an hour.
var ReleasesFeedURL = "https://github.com/" + Repo + "/releases.atom"

// ReleasesPage is where a person sees every release, to point them at.
const ReleasesPage = "https://github.com/" + Repo + "/releases"

// GitHub is what a check asks GitHub.
type GitHub int

const (
	// NoGitHub leaves GitHub out; only the server is asked.
	NoGitHub GitHub = iota
	// FinalReleases asks for the latest release, which is never a
	// pre-release.
	FinalReleases
	// WithPreReleases reads the feed of releases, whose newest may be a
	// pre-release: for someone running one, who is offered newer ones.
	WithPreReleases
)

// GitHubFor is what a check for someone running current asks GitHub, when
// it asks at all: the feed for a pre-release, the latest release for
// anything else. A local or snapshot build is not a pre-release here,
// whatever its version says (see IsRelease).
func GitHubFor(current string) GitHub {
	if IsRelease(current) && IsPreRelease(current) {
		return WithPreReleases
	}
	return FinalReleases
}

// Releases is what GitHub said has been released. Either may be "": GitHub
// was not asked for it, or named none.
type Releases struct {
	// Latest is GitHub's latest release, never a pre-release.
	Latest string
	// Newest is the newest release in the feed of releases, pre-releases
	// included.
	Newest string
}

// source is what a check asks GitHub and where, read when the check starts
// (see Start).
type source struct {
	github GitHub
	url    string
}

func sourceOf(github GitHub) source {
	switch github {
	case FinalReleases:
		return source{github, LatestReleaseURL}
	case WithPreReleases:
		return source{github, ReleasesFeedURL}
	}
	return source{}
}

// ask asks GitHub what src says, within limit.
func ask(ctx context.Context, src source, limit time.Duration) (Releases, error) {
	switch src.github {
	case FinalReleases:
		latest, err := latestRelease(ctx, src.url, limit)
		return Releases{Latest: latest}, err
	case WithPreReleases:
		newest, err := newestRelease(ctx, src.url, limit)
		return Releases{Newest: newest}, err
	}
	return Releases{}, nil
}

// Ask asks GitHub what the daily check would, and gives up as soon: for
// doctor, which reports what it hears without keeping anyone waiting on a
// GitHub that does not answer.
func Ask(ctx context.Context, github GitHub) (Releases, error) {
	return ask(ctx, sourceOf(github), checkTimeout)
}

// lookupTimeout bounds each of GitHub's answers to update, which someone
// runs to hear them, so it waits longer than the check beside a command.
const lookupTimeout = 30 * time.Second

// Lookup asks GitHub what update needs to know, and says why when GitHub
// could not tell. For FinalReleases that is the latest release; while
// every release is a pre-release there is none, and GitHub's feed says
// which pre-release is the newest (Newest), or that nothing has been
// released at all (both ""). An error means GitHub could not be asked or
// gave an answer that says nothing, never that it has no release.
func Lookup(ctx context.Context, github GitHub) (Releases, error) {
	found, err := ask(ctx, sourceOf(github), lookupTimeout)
	if github == FinalReleases && errors.Is(err, errNoLatest) {
		found.Newest, err = newestRelease(ctx, ReleasesFeedURL, lookupTimeout)
	}
	return found, err
}

// errNoLatest is GitHub's latest-release page sending to the list of
// releases, as it does while there is no release that is not a
// pre-release.
var errNoLatest = errors.New("GitHub names no latest release: none is published yet, or only pre-releases")

// AnswerError is GitHub answering with something other than what was
// asked for, such as 503 while it is down.
type AnswerError struct {
	URL    string
	Status string // "503 Service Unavailable"
	Code   int
	want   string
}

func (e *AnswerError) Error() string {
	return fmt.Sprintf("%s answered %s, not %s", e.URL, e.Status, e.want)
}

// Busy reports whether the answer says to try again later: GitHub is
// rate-limiting or failing.
func (e *AnswerError) Busy() bool {
	return e.Code == http.StatusTooManyRequests || e.Code >= 500
}

// withinLimit runs a request to page with ctx bounded by limit, and says
// when the limit, rather than the caller, ended it: a GitHub too slow to
// answer is one that could not be reached.
func withinLimit[T any](ctx context.Context, page string, limit time.Duration, do func(context.Context) (T, error)) (T, error) {
	bounded, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	v, err := do(bounded)
	if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		host := page
		if u, perr := url.Parse(page); perr == nil {
			host = u.Host
		}
		err = httpx.TooSlow(host, limit, err)
	}
	return v, err
}

// maxFeed is the most of the feed that is read. GitHub's lists ten
// releases with their notes, a few kilobytes each.
const maxFeed = 4 << 20

// newestRelease reads the newest release, pre-releases included, from the
// Atom feed at page: "" when the feed lists none. Redirects are followed
// over https only, as a renamed repository's feed sends to the new one's.
func newestRelease(ctx context.Context, page string, limit time.Duration) (string, error) {
	return withinLimit(ctx, page, limit, func(ctx context.Context) (string, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, page, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Accept", "application/atom+xml")
		resp, err := httpx.New(httpx.Options{Timeout: limit, FollowHTTPS: true}).Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", &AnswerError{URL: page, Status: resp.Status, Code: resp.StatusCode, want: "the list of releases"}
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxFeed+1))
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", page, err)
		}
		if len(body) > maxFeed {
			return "", fmt.Errorf("%s is larger than %d MB, which no list of releases is", page, maxFeed>>20)
		}
		versions, err := parseFeed(body)
		if err != nil {
			return "", fmt.Errorf("%s: %w", page, err)
		}
		return newest(versions), nil
	})
}

// The parts of an Atom feed the CLI reads: each entry's links.
type atomFeed struct {
	XMLName xml.Name    `xml:"http://www.w3.org/2005/Atom feed"`
	Entries []atomEntry `xml:"http://www.w3.org/2005/Atom entry"`
}

type atomEntry struct {
	Links []atomLink `xml:"http://www.w3.org/2005/Atom link"`
}

type atomLink struct {
	Rel  string `xml:"rel,attr"`
	Href string `xml:"href,attr"`
}

// parseFeed lists the versions of the releases an Atom feed of releases
// names, in its order. What is not an Atom feed is an error. An entry
// counts only when its page is a release's, /<owner>/<repo>/releases/tag/
// <tag>, and the tag is a release version (see validVersion and
// IsRelease); any other is left out. So is a draft, which GitHub keeps out
// of the feed, and whose page would be .../releases/tag/untagged-<id>. A
// version reached here is only a name to ask for: installing it still
// takes its release's signature and checksum.
func parseFeed(body []byte) ([]string, error) {
	var feed atomFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("not a feed of releases: %w", err)
	}
	var versions []string
	for _, e := range feed.Entries {
		if v, ok := entryVersion(e); ok {
			versions = append(versions, v)
		}
	}
	return versions, nil
}

// entryVersion is the release version an entry's page names.
func entryVersion(e atomEntry) (string, bool) {
	for _, l := range e.Links {
		// An Atom link without rel is the entry's page too.
		if l.Rel != "" && l.Rel != "alternate" {
			continue
		}
		u, err := url.Parse(l.Href)
		if err != nil {
			return "", false
		}
		parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
		if len(parts) != 5 || parts[0] == "" || parts[1] == "" || parts[2] != "releases" || parts[3] != "tag" {
			return "", false
		}
		tag := parts[4]
		if !validVersion(tag) || !IsRelease(tag) {
			return "", false
		}
		return strings.TrimPrefix(tag, "v"), true
	}
	return "", false
}

// newest is the newest of versions by semantic-version precedence, not by
// the feed's order, which is when each was published: a fix to an older
// line can come after a pre-release of the next. "" for none.
func newest(versions []string) string {
	best := ""
	for _, v := range versions {
		if best == "" || Newer(v, best) {
			best = v
		}
	}
	return best
}

// IsPreRelease reports whether v is a version with a pre-release part,
// such as 0.3.0-rc.1.
func IsPreRelease(v string) bool {
	cv, ok := canonical(v)
	return ok && semver.Prerelease(cv) != ""
}

// ReleaseVersion is the version a release is tagged with, as someone may
// type it (0.2.0 or v0.2.0), without its 'v'; false when s is no release
// version.
func ReleaseVersion(s string) (string, bool) {
	if !validVersion(s) {
		return "", false
	}
	return strings.TrimPrefix(s, "v"), true
}
