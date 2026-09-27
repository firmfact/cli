// Command relnotes prints the notes of a release: its section of
// CHANGELOG.md, which release.yml hands GoReleaser as the text of the
// GitHub release.
//
//	go run ./internal/relnotes v0.2.0 > notes.md
//
// It fails when CHANGELOG.md is not in the shape its introduction
// describes, when it says nothing under the version, or when Unreleased
// still lists changes the tagged commit would ship without mentioning them,
// so that no release goes out without saying what changed. A pre-release
// (v0.3.0-rc.1) without a section of its own takes Unreleased: that is what
// it is a candidate of.
//
// The notes come from a file people write, rather than from commit
// subjects, so that they can say what a change means for its users, and
// above all what a breaking change asks of them.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

func main() {
	changelog := flag.String("changelog", "CHANGELOG.md", "the changelog to read")
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "usage: relnotes [-changelog CHANGELOG.md] vX.Y.Z")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	notes, err := notesFor(*changelog, flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "relnotes:", err)
		os.Exit(1)
	}
	fmt.Print(notes)
}

// notesFor reads the changelog at path and returns the notes for tag.
func notesFor(path, tag string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	sections, err := parse(f)
	_ = f.Close() // only read
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	return notes(sections, tag)
}

// section is one "## " part of the changelog: Unreleased, or a release.
type section struct {
	version string // "" for Unreleased
	date    string
	body    []string // the lines under the heading, up to the next section
}

func (s section) name() string {
	if s.version == "" {
		return "Unreleased"
	}
	return s.version
}

// hasEntries reports whether the section lists any change: a line that is
// neither blank nor a group heading.
func (s section) hasEntries() bool {
	for _, line := range s.body {
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "### ") {
			return true
		}
	}
	return false
}

// groups are the headings a section's entries go under, in the order the
// changelog's introduction lists them.
var groups = []string{"Breaking", "Added", "Changed", "Deprecated", "Removed", "Fixed", "Security"}

var (
	// releaseHeading is a release's heading: "## 0.2.0 - 2026-10-01".
	releaseHeading = regexp.MustCompile(`^## (\S+) - (\S+)$`)
	// tagPattern is the shape of a release tag, as `firmfact update`
	// accepts it (versionPattern in internal/update).
	tagPattern = regexp.MustCompile(`^v?\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)
)

// validVersion reports whether v, with or without its leading 'v', is a
// version a release can be tagged with.
func validVersion(v string) bool {
	return tagPattern.MatchString(v) && semver.IsValid("v"+strings.TrimPrefix(v, "v"))
}

// parse reads the changelog into its sections, newest first, and checks its
// shape: Unreleased first, then releases, each older than the one above
// it, and entries only under the known group headings, in their order.
// Whatever comes before Unreleased is the introduction, and free.
func parse(r io.Reader) ([]section, error) {
	var sections []section
	group := -1 // the index in groups of the current section's last heading
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimRight(sc.Text(), " \t\r")
		switch {
		case line == "## Unreleased":
			if len(sections) > 0 {
				return nil, fmt.Errorf("line %d: Unreleased must be the first section", n)
			}
			sections = append(sections, section{})
			group = -1
		case strings.HasPrefix(line, "## "):
			m := releaseHeading.FindStringSubmatch(line)
			if m == nil || strings.HasPrefix(m[1], "v") || !validVersion(m[1]) {
				return nil, fmt.Errorf("line %d: %q is neither \"## Unreleased\" nor \"## X.Y.Z - YYYY-MM-DD\"", n, line)
			}
			if _, err := time.Parse(time.DateOnly, m[2]); err != nil {
				return nil, fmt.Errorf("line %d: %q is not a date such as 2026-10-01", n, m[2])
			}
			if len(sections) == 0 {
				return nil, fmt.Errorf("line %d: the first section must be \"## Unreleased\"", n)
			}
			if above := sections[len(sections)-1]; above.version != "" {
				if semver.Compare("v"+m[1], "v"+above.version) >= 0 {
					return nil, fmt.Errorf("line %d: %s comes after %s, but is not older; the newest release comes first", n, m[1], above.version)
				}
				if m[2] > above.date {
					return nil, fmt.Errorf("line %d: %s is dated %s, after %s above it", n, m[1], m[2], above.date)
				}
			}
			sections = append(sections, section{version: m[1], date: m[2]})
			group = -1
		case strings.HasPrefix(line, "### "):
			if len(sections) == 0 {
				return nil, fmt.Errorf("line %d: %q comes before \"## Unreleased\"", n, line)
			}
			i := indexOf(groups, strings.TrimPrefix(line, "### "))
			if i < 0 {
				return nil, fmt.Errorf("line %d: %q is not one of the groups (%s)", n, line, strings.Join(groups, ", "))
			}
			if i <= group {
				return nil, fmt.Errorf("line %d: %q comes after %q; the groups go in the order %s", n, line, groups[group], strings.Join(groups, ", "))
			}
			group = i
			sections[len(sections)-1].body = append(sections[len(sections)-1].body, line)
		case len(sections) > 0:
			sections[len(sections)-1].body = append(sections[len(sections)-1].body, line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(sections) == 0 {
		return nil, errors.New("there is no \"## Unreleased\" section")
	}
	return sections, nil
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

// notes is the section of sections that tag's release publishes, without
// its heading, which the release has for a title already.
func notes(sections []section, tag string) (string, error) {
	if !validVersion(tag) {
		return "", fmt.Errorf("%q is not a release tag such as v1.2.3", tag)
	}
	version := strings.TrimPrefix(tag, "v")
	pick := -1
	for i, s := range sections {
		if s.version == version {
			pick = i
		}
	}
	prerelease := semver.Prerelease("v"+version) != ""
	switch {
	case pick < 0 && prerelease:
		pick = 0 // Unreleased
	case pick < 0:
		return "", fmt.Errorf("CHANGELOG.md has no section for %s; before tagging, move what Unreleased lists under a heading \"## %s - YYYY-MM-DD\" and start a new, empty Unreleased", version, version)
	case sections[0].hasEntries():
		return "", fmt.Errorf("CHANGELOG.md still lists changes under Unreleased, which %s would ship without mentioning; move them under %s", version, version)
	}
	s := sections[pick]
	if !s.hasEntries() {
		return "", fmt.Errorf("CHANGELOG.md lists no changes under %s", s.name())
	}
	return strings.Trim(strings.Join(unwrap(s.body), "\n"), "\n") + "\n", nil
}

// unwrap joins the lines of each paragraph and list item into one. The
// changelog is wrapped to be read in an editor, but GitHub shows every line
// break in a release's notes as one. Code blocks stay as they are.
func unwrap(lines []string) []string {
	var out []string
	fenced := false
	for _, line := range lines {
		text := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(text, "```"):
			fenced = !fenced
		case !fenced && len(out) > 0 && takesMore(out[len(out)-1]) && text != "" && !startsBlock.MatchString(text):
			out[len(out)-1] += " " + text
			continue
		}
		out = append(out, line)
	}
	return out
}

// startsBlock matches a line that starts something of its own rather than
// going on from the line above: a list item, heading, table row, quote or
// code fence.
var startsBlock = regexp.MustCompile(`^([-*+] |\d+[.)] |#|\||>|` + "```)")

// takesMore reports whether the next line can go on from line: it holds
// text, and is not a heading, table row or code fence.
func takesMore(line string) bool {
	text := strings.TrimSpace(line)
	return text != "" && !strings.HasPrefix(text, "#") && !strings.HasPrefix(text, "|") && !strings.HasPrefix(text, "```")
}
