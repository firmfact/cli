package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, dir, raw string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// A file that does not parse is an error that says where, in the line and
// column an editor shows; one that is empty is the defaults.
func TestLoadSaysWhereTheFileIsWrong(t *testing.T) {
	dir := isolate(t)
	cases := []struct{ raw, want string }{
		{"{\n  \"profiles\": {\n    \"ci\": {\"host\": \"x\"},\n  }\n}\n", "line 4, column 3: invalid character '}' looking for beginning of object key string"},
		{"{\"profiles\": {\"ci\": {\"host\": 5}}}", "line 1, column 30: profiles.ci.host should be a string, not a number"},
		{"{\"profiles\": []}", "line 1, column 14: profiles should be an object, not an array"},
		{"{\"profiles\": {\"ci\": {\"insecure_http\": \"yes\"}}}", "profiles.ci.insecure_http should be true or false, not a string"},
		{"{\"profiles\": {\"ci\": null}}", `profile "ci" is null`},
		{"{} x", "line 1, column 4: invalid character 'x' after top-level value"},
	}
	for _, c := range cases {
		path := writeConfig(t, dir, c.raw)
		cfg, err := Load()
		var fileErr *FileError
		if !errors.As(err, &fileErr) || fileErr.Path != path || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: got %+v, %v; want a FileError with %q", c.raw, cfg, err, c.want)
		}
	}

	for _, raw := range []string{"", " \n", "null"} {
		writeConfig(t, dir, raw)
		cfg, err := Load()
		if err != nil || cfg.CurrentProfile != DefaultProfile || cfg.Profiles == nil {
			t.Errorf("%q: %+v, %v", raw, cfg, err)
		}
	}
}

// Save never replaces a file that does not parse, even one that broke
// after it was read, and a placeholder is never saved at all.
func TestSaveLeavesABrokenFileAlone(t *testing.T) {
	dir := isolate(t)
	cfg := &Config{CurrentProfile: "ci", Profiles: map[string]*Profile{"ci": {Host: "https://ci.example"}}}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	const broken = `{"current_profile": "ci",}`
	path := writeConfig(t, dir, broken)
	loaded.CurrentProfile = DefaultProfile
	var fileErr *FileError
	if err := loaded.Save(); !errors.As(err, &fileErr) {
		t.Errorf("Save over a broken file: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != broken {
		t.Errorf("file = %q", got)
	}

	cause := &FileError{Path: path, Err: errors.New("bad")}
	if err := Placeholder(cause).Save(); !errors.Is(err, cause) {
		t.Errorf("Placeholder Save = %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := Placeholder(cause).Save(); !errors.Is(err, cause) {
		t.Errorf("Placeholder Save without a file = %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a placeholder was written: %v", err)
	}
}

// Reset moves the file to a copy named after the time, never over an
// earlier copy.
func TestResetMovesTheFileAside(t *testing.T) {
	dir := isolate(t)
	if backup, err := Reset(time.Now()); err != nil || backup != "" {
		t.Fatalf("without a file: %q, %v", backup, err)
	}
	now := time.Date(2026, 9, 27, 15, 30, 12, 0, time.UTC)
	var backups []string
	for _, raw := range []string{"one", "two"} {
		path := writeConfig(t, dir, raw)
		backup, err := Reset(now)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(backup); string(got) != raw {
			t.Errorf("%s holds %q, want %q", backup, got, raw)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("config file still there: %v", err)
		}
		backups = append(backups, filepath.Base(backup))
	}
	want := []string{"config.json.20260927-153012.bak", "config.json.20260927-153012-2.bak"}
	if !reflect.DeepEqual(backups, want) {
		t.Errorf("backups = %v, want %v", backups, want)
	}
}

// The default profile and the current one exist without being in the
// file; any other name has to be made.
func TestProfilesThatExist(t *testing.T) {
	cfg := &Config{CurrentProfile: "ci", Profiles: map[string]*Profile{"staging": {Host: "https://staging.example"}}}
	for name, want := range map[string]bool{"default": true, "ci": true, "staging": true, "stagin": false} {
		if _, ok := cfg.Lookup(name); ok != want {
			t.Errorf("Lookup(%q) = %v", name, ok)
		}
	}
	if got := cfg.Names(); !reflect.DeepEqual(got, []string{"ci", "default", "staging"}) {
		t.Errorf("Names = %v", got)
	}
	if p := cfg.Peek("stagin"); p.Host != DefaultHost || len(cfg.Profiles) != 1 {
		t.Errorf("Peek = %+v, profiles %v", p, cfg.Profiles)
	}

	cfg.Rename("ci", "build")
	if cfg.CurrentProfile != "build" || cfg.Profiles["build"].Host != DefaultHost {
		t.Errorf("after renaming the current profile: %+v", cfg)
	}
	cfg.Rename("staging", "stage")
	if cfg.Profiles["stage"].Host != "https://staging.example" || cfg.Profiles["staging"] != nil {
		t.Errorf("after rename: %+v", cfg.Profiles)
	}

	for _, name := range []string{"ci", "prod-eu", "a.b_c", "9"} {
		if err := CheckProfileName(name); err != nil {
			t.Errorf("%q: %v", name, err)
		}
	}
	for _, name := range []string{"", "-x", ".x", "a b", "a/b", "é", strings.Repeat("a", 65)} {
		if err := CheckProfileName(name); err == nil {
			t.Errorf("%q passed", name)
		}
	}
}
