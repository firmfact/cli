package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/firmfact/cli/cmd"
)

// isolate gives the CLI an empty config and a cache that does hold a tool
// list for the default host, as a maintainer's machine would: none of it
// may reach the pages.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("FIRMFACT_CONFIG_DIR", filepath.Join(t.TempDir(), "config"))
	cache := t.TempDir()
	t.Setenv("FIRMFACT_CACHE_DIR", cache)
	for _, name := range []string{"FIRMFACT_HOST", "FIRMFACT_PROFILE", "FIRMFACT_WORKSPACE", "FIRMFACT_TOKEN"} {
		t.Setenv(name, "")
	}
	const host = "https://firmfact.com"
	sum := sha256.Sum256([]byte(host))
	tools := `{"format":2,"host":"` + host + `","fetched_at":"` + time.Now().Format(time.RFC3339) + `",` +
		`"tools":[{"name":"list_vendors","title":"List vendors","inputSchema":{"type":"object"}}]}`
	if err := os.WriteFile(filepath.Join(cache, "tools-"+hex.EncodeToString(sum[:6])+".json"), []byte(tools), 0o600); err != nil {
		t.Fatal(err)
	}
	// A cache the CLI would not read, such as one of an older format, would
	// leave nothing for the pages to keep out.
	root := cmd.NewRootCommand(cmd.Build{Version: "test"}, []string{}, cmd.IOStreams{})
	if c, _, err := root.Find([]string{"vendors", "list"}); err != nil || c.Name() != "list" {
		t.Fatalf("the CLI does not read the tool cache this test writes: %v", err)
	}
}

// files reads every file under dir, by its path relative to dir.
func files(t *testing.T, dir string) map[string]string {
	t.Helper()
	got := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		rel, _ := filepath.Rel(dir, path)
		got[filepath.ToSlash(rel)] = string(raw)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

var commitDate = time.Date(2026, time.September, 27, 7, 19, 53, 0, time.UTC)

// The scripts and pages the archives carry: every script, a page per
// command and help topic, for the CLI's own commands under the name
// firmfact, dated by the commit. The same commit gives the same bytes.
func TestGenerate(t *testing.T) {
	isolate(t)
	first, second := t.TempDir(), t.TempDir()
	// A page left from an earlier run, for a command since removed.
	if err := os.MkdirAll(filepath.Join(first, "manpages"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first, "manpages", "firmfact-gone.1"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := generate(first, commitDate); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TZ", "Pacific/Kiritimati")
	if err := generate(second, commitDate); err != nil {
		t.Fatal(err)
	}
	got := files(t, first)
	if again := files(t, second); len(again) != len(got) {
		t.Errorf("second run wrote %d files, first %d", len(again), len(got))
	} else {
		for name, content := range got {
			if again[name] != content {
				t.Errorf("%s differs between runs", name)
			}
		}
	}

	for _, name := range []string{
		"completions/firmfact.bash", "completions/_firmfact", "completions/firmfact.fish", "completions/firmfact.ps1",
		"manpages/firmfact.1", "manpages/firmfact-login.1", "manpages/firmfact-workspaces-use.1",
		"manpages/firmfact-config-profiles-rename.1", "manpages/firmfact-environment.1",
	} {
		if got[name] == "" {
			t.Errorf("no %s", name)
		}
	}
	for name, content := range got {
		switch {
		case name == "manpages/firmfact-gone.1":
			t.Error("a page from an earlier run is still there")
		case strings.Contains(name, "vendors"):
			t.Errorf("%s: a workspace command from the build machine's cache", name)
		case strings.Contains(content, "Auto generated"), strings.Contains(content, "gendocs"):
			t.Errorf("%s names the tool that made it", name)
		}
	}

	root := got["manpages/firmfact.1"]
	if !strings.Contains(root, `.TH "FIRMFACT" "1" "Sep 2026" "firmfact" "Firmfact manual"`) {
		t.Errorf("firmfact.1 header:\n%.200s", root)
	}
	for _, want := range []string{"firmfact vendors list", "firmfact-environment(1)", "--version", "firmfact-workspaces(1)"} {
		if !strings.Contains(root, want) {
			t.Errorf("firmfact.1 does not mention %s", want)
		}
	}
	// Placeholders survive Markdown, which would take <id or name> for HTML.
	if use := got["manpages/firmfact-workspaces-use.1"]; !strings.Contains(use, `\fBfirmfact workspaces use <id or name> [flags]\fP`) {
		t.Errorf("firmfact-workspaces-use.1 synopsis:\n%s", use)
	}
	if !strings.Contains(root, "localhost:<port> for local development") {
		t.Error("firmfact.1 lost the <port> in --host")
	}
	// A topic is text to read: its lists keep their layout, and it has no
	// options.
	env := got["manpages/firmfact-environment.1"]
	if !strings.Contains(env, ".EX\n  FIRMFACT_HOST             the firmfact host") || strings.Contains(env, "OPTIONS") {
		t.Errorf("firmfact-environment.1:\n%s", env)
	}
	if !strings.Contains(env, "\\fBfirmfact environment\\fP") {
		t.Errorf("firmfact-environment.1 synopsis:\n%.400s", env)
	}

	for name, want := range map[string]string{
		"completions/firmfact.bash": "complete -o default -F __start_firmfact firmfact",
		"completions/_firmfact":     "#compdef firmfact",
		"completions/firmfact.fish": "complete -c firmfact",
		"completions/firmfact.ps1":  "Register-ArgumentCompleter -CommandName 'firmfact'",
	} {
		if !strings.Contains(got[name], want) {
			t.Errorf("%s does not contain %q", name, want)
		}
	}
}

func TestSourceDate(t *testing.T) {
	commit := func() (string, error) { return "1790493593\n", nil }
	noGit := func() (string, error) { return "", errors.New("not a git repository") }

	if got, err := sourceDate("1790493593", noGit); err != nil || !got.Equal(commitDate) || got.Location() != time.UTC {
		t.Errorf("SOURCE_DATE_EPOCH: %v, %v", got, err)
	}
	if got, err := sourceDate("", commit); err != nil || !got.Equal(commitDate) {
		t.Errorf("commit time: %v, %v", got, err)
	}
	if _, err := sourceDate("", noGit); err == nil || !strings.Contains(err.Error(), "no SOURCE_DATE_EPOCH") {
		t.Errorf("no date at all: %v", err)
	}
	if _, err := sourceDate("yesterday", noGit); err == nil || !strings.Contains(err.Error(), `"yesterday"`) {
		t.Errorf("a date that is not a number: %v", err)
	}
}

// The tests run in a checkout, so git knows when HEAD was committed.
func TestCommitTime(t *testing.T) {
	out, err := commitTime()
	if err != nil {
		t.Skipf("no git here: %v", err)
	}
	if _, err := sourceDate("", func() (string, error) { return out, nil }); err != nil {
		t.Errorf("commit time %q: %v", out, err)
	}
}

func TestMarkdown(t *testing.T) {
	cases := map[string]string{
		"use <id or name>":                     `use \<id or name>`,
		"run `firmfact <command> --help` now":  "run `firmfact <command> --help` now",
		"a <b> `c <d>` e <f>":                  "a \\<b> `c <d>` e \\<f>",
		"1 < 2, and 3 > 2":                     `1 \< 2, and 3 > 2`,
		"plain text, nothing to escape at all": "plain text, nothing to escape at all",
	}
	for in, want := range cases {
		if got := markdownText(in); got != want {
			t.Errorf("markdownText(%q) = %q, want %q", in, got, want)
		}
	}
	long := "Text with <name>.\n\n  KEY    value <x>\n\tsource <(firmfact completion bash)"
	want := "Text with \\<name>.\n\n      KEY    value <x>\n\tsource <(firmfact completion bash)"
	if got := markdownLong(long); got != want {
		t.Errorf("markdownLong = %q, want %q", got, want)
	}
}
