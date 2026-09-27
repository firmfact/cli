package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// isolate points the config at scratch space and the tokens at the file,
// and returns the config directory. The CLI creates that directory itself,
// as it would for a user, so its mode does not depend on the umask.
func isolate(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "config")
	t.Setenv("FIRMFACT_CONFIG_DIR", dir)
	t.Setenv("FIRMFACT_TOKEN_STORE", "file")
	t.Setenv("FIRMFACT_TOKEN", "")
	t.Setenv("FIRMFACT_HOST", "")
	return dir
}

// FIRMFACT_TOKEN goes to the default host, or to FIRMFACT_HOST when that is
// set, and to no other host.
func TestEnvTokenIsForOneHost(t *testing.T) {
	isolate(t)
	t.Setenv("FIRMFACT_TOKEN", "env-token")

	if tok, err := LoadToken(DefaultHost); err != nil || tok == nil || tok.AccessToken != "env-token" {
		t.Fatalf("default host: %+v, %v", tok, err)
	}
	tok, err := LoadToken("http://127.0.0.1:5000")
	if err == nil || tok != nil {
		t.Fatalf("another host got %+v, %v", tok, err)
	}
	if !strings.Contains(err.Error(), "set FIRMFACT_HOST=http://127.0.0.1:5000") {
		t.Errorf("error = %v", err)
	}

	t.Setenv("FIRMFACT_HOST", "127.0.0.1:5000")
	if tok, err := LoadToken("http://127.0.0.1:5000"); err != nil || tok == nil || tok.AccessToken != "env-token" {
		t.Fatalf("FIRMFACT_HOST: %+v, %v", tok, err)
	}
	if tok, err := LoadToken(DefaultHost); err == nil || tok != nil {
		t.Fatalf("with FIRMFACT_HOST set, the default host got %+v, %v", tok, err)
	}

	t.Setenv("FIRMFACT_HOST", "ftp://x")
	if _, err := LoadToken("ftp://x"); err == nil || !strings.Contains(err.Error(), "FIRMFACT_HOST") {
		t.Errorf("invalid FIRMFACT_HOST: %v", err)
	}
}

// A mismatch is an error, not a quiet switch to the stored sign-in.
func TestEnvTokenMismatchDoesNotFallBackToStoredToken(t *testing.T) {
	isolate(t)
	if err := SaveToken("http://127.0.0.1:5000", &Token{AccessToken: "stored"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FIRMFACT_TOKEN", "env-token")
	if tok, err := LoadToken("http://127.0.0.1:5000"); err == nil {
		t.Fatalf("got %+v, want an error", tok)
	}
}
