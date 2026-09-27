package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The token file and the directory it is in are the user's alone: the file
// 0600 and the directory closed to everyone else, whatever the umask lets
// through.
func TestTokenFileIsPrivate(t *testing.T) {
	skipOnWindows(t)
	dir := isolate(t)
	if err := SaveToken(DefaultHost, &Token{AccessToken: "at"}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, "credentials.json"): 0o600} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got&0o077 != 0 || got&want != want {
			t.Errorf("%s has mode %04o, want %04o", path, got, want)
		}
	}
}

// Something other than a file where the token file belongs is refused,
// with what to do about it, rather than read or replaced.
func TestTokenFileThatIsNotAFileIsRefused(t *testing.T) {
	skipOnWindows(t)
	dir := isolate(t)
	path := filepath.Join(dir, "credentials.json")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	want := path + " is not a regular file; remove it and sign in again"
	if _, err := LoadToken(DefaultHost); err == nil || err.Error() != want {
		t.Errorf("load: got %v, want %q", err, want)
	}
	if err := SaveToken(DefaultHost, &Token{AccessToken: "at"}); err == nil || err.Error() != want {
		t.Errorf("save: got %v, want %q", err, want)
	}
	if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
		t.Errorf("the directory was replaced: %v", err)
	}
	if where, err := DescribeStore(DefaultHost); err != nil || !strings.HasSuffix(where, "(FIRMFACT_TOKEN_STORE=file)") {
		t.Errorf("store = %q, %v", where, err)
	}
}
