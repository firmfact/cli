package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const changelog = `# Changelog

Free text before Unreleased, such as this, is the introduction.

## Unreleased

## 0.3.0-rc.1 - 2026-11-02

### Fixed

- The release candidate's own fix.

## 0.2.0 - 2026-10-01

### Breaking

- ` + "`--old`" + ` is gone; use ` + "`--new`" + `.

### Added

- A new command.

## 0.1.0 - 2026-09-30

### Added

- The first release.
`

func parsed(t *testing.T, text string) []section {
	t.Helper()
	sections, err := parse(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	return sections
}

// A release's notes are its section without the heading: the groups and
// their entries, as written.
func TestNotes(t *testing.T) {
	sections := parsed(t, changelog)
	got, err := notes(sections, "v0.2.0")
	want := "### Breaking\n\n- `--old` is gone; use `--new`.\n\n### Added\n\n- A new command.\n"
	if err != nil || got != want {
		t.Errorf("notes(v0.2.0) = %q, %v\nwant %q", got, err, want)
	}
	if got, err := notes(sections, "0.1.0"); err != nil || got != "### Added\n\n- The first release.\n" {
		t.Errorf("notes(0.1.0) = %q, %v", got, err)
	}
	// A pre-release with a section of its own gets it.
	if got, err := notes(sections, "v0.3.0-rc.1"); err != nil || !strings.Contains(got, "own fix") {
		t.Errorf("notes(v0.3.0-rc.1) = %q, %v", got, err)
	}
}

// The notes join each list item and paragraph into one line, as GitHub
// shows every line break in a release's notes, but keep code blocks,
// nested lists and blank lines as they are.
func TestNotesUnwrap(t *testing.T) {
	text := "## Unreleased\n\n## 0.2.0 - 2026-10-01\n\n### Added\n\n" +
		"- `ask` puts a question\n  to a workspace.\n" +
		"  - A nested item\n    wrapped.\n" +
		"- Another item.\n\n" +
		"A paragraph that\nwraps.\n\n" +
		"```bash\nfirmfact ask \"why\"\n  --continue\n```\n" +
		"After the code.\n"
	want := "### Added\n\n" +
		"- `ask` puts a question to a workspace.\n" +
		"  - A nested item wrapped.\n" +
		"- Another item.\n\n" +
		"A paragraph that wraps.\n\n" +
		"```bash\nfirmfact ask \"why\"\n  --continue\n```\n" +
		"After the code.\n"
	if got, err := notes(parsed(t, text), "v0.2.0"); err != nil || got != want {
		t.Errorf("notes = %q, %v\nwant %q", got, err, want)
	}
}

// A pre-release without a section of its own takes Unreleased, which must
// say something.
func TestPrereleaseTakesUnreleased(t *testing.T) {
	text := strings.Replace(changelog, "## Unreleased\n", "## Unreleased\n\n### Fixed\n\n- Not released yet.\n", 1)
	got, err := notes(parsed(t, text), "v0.3.0-rc.2")
	if err != nil || got != "### Fixed\n\n- Not released yet.\n" {
		t.Errorf("notes = %q, %v", got, err)
	}
	if _, err := notes(parsed(t, changelog), "v0.3.0-rc.2"); err == nil || !strings.Contains(err.Error(), "no changes under Unreleased") {
		t.Errorf("an empty Unreleased gave notes: %v", err)
	}
}

// A release without notes is refused before anything is built: no section,
// an empty one, Unreleased entries the release would leave out, or a tag
// that is no version.
func TestNotesRefuses(t *testing.T) {
	sections := parsed(t, changelog)
	cases := map[string]struct {
		text, tag, want string
	}{
		"no section":         {changelog, "v0.4.0", "no section for 0.4.0"},
		"not a tag":          {changelog, "0.2", "not a release tag"},
		"a path":             {changelog, "v1.2.3-x/../y", "not a release tag"},
		"build metadata":     {changelog, "v0.2.0+dirty", "not a release tag"},
		"empty section":      {strings.Replace(changelog, "### Added\n\n- The first release.\n", "", 1), "v0.1.0", "no changes under 0.1.0"},
		"unreleased entries": {strings.Replace(changelog, "## Unreleased\n", "## Unreleased\n\n- Forgotten.\n", 1), "v0.2.0", "still lists changes under Unreleased"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := sections
			if c.text != changelog {
				s = parsed(t, c.text)
			}
			if got, err := notes(s, c.tag); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("notes(%s) = %q, %v; want an error saying %q", c.tag, got, err, c.want)
			}
		})
	}
}

// A changelog out of shape fails as it is read, with the line to fix.
func TestParseRefuses(t *testing.T) {
	cases := map[string]struct{ text, want string }{
		"no unreleased":          {"# Changelog\n\n## 0.1.0 - 2026-09-30\n", "line 3: the first section must be"},
		"nothing at all":         {"# Changelog\n", "no \"## Unreleased\""},
		"unreleased later":       {"## Unreleased\n## 0.1.0 - 2026-09-30\n## Unreleased\n", "line 3: Unreleased must be the first"},
		"v in the heading":       {"## Unreleased\n## v0.1.0 - 2026-09-30\n", "line 2:"},
		"no date":                {"## Unreleased\n## 0.1.0\n", "line 2:"},
		"not a date":             {"## Unreleased\n## 0.1.0 - 2026-02-30\n", "not a date"},
		"not a version":          {"## Unreleased\n## 0.1 - 2026-09-30\n", "line 2:"},
		"older first":            {"## Unreleased\n## 0.1.0 - 2026-09-30\n## 0.2.0 - 2026-10-01\n", "line 3: 0.2.0 comes after 0.1.0"},
		"the same twice":         {"## Unreleased\n## 0.1.0 - 2026-09-30\n## 0.1.0 - 2026-09-30\n", "line 3:"},
		"dated after":            {"## Unreleased\n## 0.2.0 - 2026-09-30\n## 0.1.0 - 2026-10-01\n", "dated 2026-10-01"},
		"unknown group":          {"## Unreleased\n### Features\n", "not one of the groups"},
		"groups out of order":    {"## Unreleased\n### Fixed\n- a\n### Added\n- b\n", "line 4:"},
		"group twice":            {"## Unreleased\n### Added\n- a\n### Added\n- b\n", "line 4:"},
		"group before a section": {"# Changelog\n### Added\n## Unreleased\n", "line 2:"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parse(strings.NewReader(c.text)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("parse = %v, want an error saying %q", err, c.want)
			}
		})
	}
}

// CHANGELOG.md itself is in shape, so a pull request that breaks it fails
// here rather than on the day of a release. A changelog written on Windows
// reads the same.
func TestChangelog(t *testing.T) {
	sections, err := parse(strings.NewReader(readFile(t, filepath.Join("..", "..", "CHANGELOG.md"))))
	if err != nil {
		t.Fatalf("CHANGELOG.md: %v", err)
	}
	if sections[0].version != "" {
		t.Errorf("CHANGELOG.md starts with %s, not Unreleased", sections[0].name())
	}
	if _, err := parse(strings.NewReader(strings.ReplaceAll(changelog, "\n", "\r\n"))); err != nil {
		t.Errorf("with CRLF line endings: %v", err)
	}
}

// notesFor reads the file it is given.
func TestNotesFor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "CHANGELOG.md")
	if err := os.WriteFile(path, []byte(changelog), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := notesFor(path, "v0.1.0"); err != nil || got != "### Added\n\n- The first release.\n" {
		t.Errorf("notesFor = %q, %v", got, err)
	}
	if err := os.WriteFile(path, []byte("## 0.1.0 - 2026-09-30\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := notesFor(path, "v0.1.0"); err == nil || !strings.Contains(err.Error(), path+": line 1") {
		t.Errorf("a changelog out of shape: %v", err)
	}
	if _, err := notesFor(filepath.Join(t.TempDir(), "missing.md"), "v0.1.0"); err == nil {
		t.Error("a missing changelog gave notes")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
