package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

// mockKeyring puts go-keyring's own in-memory store behind the system
// keyring the store calls, so these tests go through systemKeyring as the
// CLI does. With fail set, every keyring call fails with it. It returns the
// store's warnings. go-keyring has no way back to the real keyring, which
// suits the tests: none of them may reach it.
func mockKeyring(t *testing.T, fail error) *bytes.Buffer {
	t.Helper()
	if fail != nil {
		keyring.MockInitWithError(fail)
	} else {
		keyring.MockInit()
	}
	prev := secrets
	secrets = systemKeyring{}
	resetKeyringState()
	var warned bytes.Buffer
	SetWarningOutput(&warned)
	t.Setenv("FIRMFACT_TOKEN_STORE", "")
	t.Cleanup(func() {
		keyring.MockInit()
		secrets = prev
		resetKeyringState()
		SetWarningOutput(os.Stderr)
	})
	return &warned
}

const localHost = "http://127.0.0.1:5000"

// Each host's sign-in is a keyring entry of its own: saved, read and
// removed without touching the other's, and without a token file.
func TestSystemKeyringKeepsHostsApart(t *testing.T) {
	dir := isolate(t)
	warned := mockKeyring(t, nil)
	hosts := map[string]string{DefaultHost: "prod", localHost: "local"}
	for host, access := range hosts {
		if err := SaveToken(host, &Token{AccessToken: access, RefreshToken: "rt-" + access}); err != nil {
			t.Fatal(err)
		}
	}
	for host, access := range hosts {
		if tok, err := LoadToken(host); err != nil || tok == nil || tok.AccessToken != access || tok.RefreshToken != "rt-"+access {
			t.Errorf("%s: loaded %+v, %v", host, tok, err)
		}
	}
	if secret, err := keyring.Get(keyringService, DefaultHost); err != nil || !strings.Contains(secret, `"access_token":"prod"`) {
		t.Errorf("keyring entry = %q, %v", secret, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "credentials.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a token file was written: %v", err)
	}
	if where, err := DescribeStore(localHost); err != nil || where != "system keyring" {
		t.Errorf("store = %q, %v", where, err)
	}

	if err := DeleteToken(DefaultHost); err != nil {
		t.Fatal(err)
	}
	if tok, err := LoadToken(DefaultHost); err != nil || tok != nil {
		t.Errorf("after logout: %+v, %v", tok, err)
	}
	if tok, err := LoadToken(localHost); err != nil || tok == nil || tok.AccessToken != "local" {
		t.Errorf("the other host after logout: %+v, %v", tok, err)
	}
	// Signing out twice is not an error.
	if err := DeleteToken(DefaultHost); err != nil {
		t.Errorf("second logout: %v", err)
	}
	if warned.Len() != 0 {
		t.Errorf("warned: %s", warned.String())
	}
}

// A keyring that is not there, the way D-Bus and the Secret Service say
// so, sends the sign-in to the file, and says so once.
func TestUnavailableKeyringFallsBackToTheFile(t *testing.T) {
	for name, fail := range map[string]error{
		"no secret service":    errors.New("The name org.freedesktop.secrets was not provided by any .service files"),
		"no session bus":       errors.New("dbus: couldn't determine address of session bus"),
		"no such interface":    errors.New("No such interface \"org.freedesktop.Secret.Collection\" on object at path /org/freedesktop/secrets/collection/login"),
		"unsupported platform": keyring.ErrUnsupportedPlatform,
	} {
		t.Run(name, func(t *testing.T) {
			dir := isolate(t)
			warned := mockKeyring(t, fail)
			path := filepath.Join(dir, "credentials.json")

			for range 2 {
				if err := SaveToken(DefaultHost, &Token{AccessToken: "at"}); err != nil {
					t.Fatalf("save: %v", err)
				}
			}
			tokens, err := readFileTokens()
			if err != nil || tokens[DefaultHost] == nil || tokens[DefaultHost].AccessToken != "at" {
				t.Fatalf("file holds %v, %v", tokens, err)
			}
			if got := warned.String(); strings.Count(got, "warning: ") != 1 || !strings.Contains(got, "the system keyring is not available") {
				t.Errorf("warnings = %q", got)
			}
			if tok, err := LoadToken(DefaultHost); err != nil || tok == nil || tok.AccessToken != "at" {
				t.Errorf("loaded %+v, %v", tok, err)
			}
			if where, err := DescribeStore(DefaultHost); err != nil || !strings.HasPrefix(where, path+", as the system keyring is not available") {
				t.Errorf("store = %q, %v", where, err)
			}
			if err := DeleteToken(DefaultHost); err != nil {
				t.Fatalf("delete: %v", err)
			}
			if tok, err := LoadToken(DefaultHost); err != nil || tok != nil {
				t.Errorf("after delete: %+v, %v", tok, err)
			}
		})
	}
}

// Any other keyring failure, such as a refused access, is the command's
// error. Falling back would put the token in a file the user never chose,
// and reading the file instead could sign in with a stale copy.
func TestKeyringFailureIsReported(t *testing.T) {
	dir := isolate(t)
	// A sign-in stored while there was no keyring.
	if err := SaveToken(DefaultHost, &Token{AccessToken: "file copy"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	mockKeyring(t, errors.New("access denied"))

	for name, op := range map[string]func() error{
		"save":     func() error { return SaveToken(localHost, &Token{AccessToken: "at"}) },
		"load":     func() error { _, err := LoadToken(DefaultHost); return err },
		"delete":   func() error { return DeleteToken(DefaultHost) },
		"describe": func() error { _, err := DescribeStore(DefaultHost); return err },
	} {
		if err := op(); err == nil || !strings.Contains(err.Error(), "access denied") {
			t.Errorf("%s: got %v, want the keyring's error", name, err)
		}
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "credentials.json")); !bytes.Equal(after, before) {
		t.Errorf("the token file changed to %s", after)
	}
}

// A sign-in kept in the keyring with an old copy left in the file (from
// before the keyring could be reached) is gone from both after logout; the
// copy would otherwise sign the user in again the next time the keyring
// could not be reached.
func TestDeleteClearsBothStores(t *testing.T) {
	isolate(t)
	for host, access := range map[string]string{DefaultHost: "old copy", localHost: "local"} {
		if err := SaveToken(host, &Token{AccessToken: access}); err != nil {
			t.Fatal(err)
		}
	}
	mockKeyring(t, nil)
	if err := keyring.Set(keyringService, DefaultHost, `{"access_token":"current"}`); err != nil {
		t.Fatal(err)
	}
	if tok, err := LoadToken(DefaultHost); err != nil || tok == nil || tok.AccessToken != "current" {
		t.Fatalf("loaded %+v, %v; want the keyring's token", tok, err)
	}

	if err := DeleteToken(DefaultHost); err != nil {
		t.Fatal(err)
	}
	if _, err := keyring.Get(keyringService, DefaultHost); !errors.Is(err, keyring.ErrNotFound) {
		t.Errorf("keyring after logout: %v", err)
	}
	tokens, err := readFileTokens()
	if err != nil || tokens[DefaultHost] != nil || tokens[localHost] == nil {
		t.Errorf("file after logout holds %v, %v; want only %s", tokens, err, localHost)
	}
	if tok, err := LoadToken(DefaultHost); err != nil || tok != nil {
		t.Errorf("after logout: %+v, %v", tok, err)
	}
}

// A token file that does not parse is reported with its path and the place
// it broke, and nothing writes over it: it may hold other hosts' sign-ins.
// With a keyring, a new sign-in goes there and the file stays as it is.
func TestCorruptTokenFileIsLeftAlone(t *testing.T) {
	dir := isolate(t)
	if err := SaveToken(DefaultHost, &Token{AccessToken: "at"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "credentials.json")
	corrupt := []byte("{\n  \"https://firmfact.com\": {\"access_token\": \"at\"},\n}\n")
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}

	want := "token file " + path + ": line 3, column 1: invalid character '}' looking for beginning of object key string; remove it and sign in again"
	if _, err := LoadToken(DefaultHost); err == nil || err.Error() != want {
		t.Errorf("load: got %v, want %q", err, want)
	}
	for name, op := range map[string]func() error{
		"save":   func() error { return SaveToken(localHost, &Token{AccessToken: "at"}) },
		"delete": func() error { return DeleteToken(DefaultHost) },
	} {
		if err := op(); err == nil || !strings.HasPrefix(err.Error(), "token file "+path+": ") {
			t.Errorf("%s: got %v, want the broken file named", name, err)
		}
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, corrupt) {
		t.Fatalf("the file was rewritten: %q", got)
	}

	warned := mockKeyring(t, nil)
	if err := SaveToken(DefaultHost, &Token{AccessToken: "new"}); err != nil {
		t.Fatalf("save with a keyring: %v", err)
	}
	if !strings.Contains(warned.String(), "its old copy in the token file could not be removed") {
		t.Errorf("warnings = %q", warned.String())
	}
	if tok, err := LoadToken(DefaultHost); err != nil || tok == nil || tok.AccessToken != "new" {
		t.Errorf("loaded %+v, %v", tok, err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, corrupt) {
		t.Errorf("the file was rewritten: %q", got)
	}
}

// FIRMFACT_TOKEN takes the place of the stored sign-in for its host while
// it is set, and leaves that sign-in as it was.
func TestEnvTokenWinsOverTheStoredOne(t *testing.T) {
	isolate(t)
	if err := SaveToken(DefaultHost, &Token{AccessToken: "stored", RefreshToken: "rt"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FIRMFACT_TOKEN", "from-env")
	if tok, err := LoadToken(DefaultHost); err != nil || tok == nil || *tok != (Token{AccessToken: "from-env"}) {
		t.Errorf("with FIRMFACT_TOKEN: %+v, %v", tok, err)
	}
	if !TokenFromEnv() {
		t.Error("TokenFromEnv = false")
	}
	t.Setenv("FIRMFACT_TOKEN", "")
	if tok, err := LoadToken(DefaultHost); err != nil || tok == nil || tok.AccessToken != "stored" || tok.RefreshToken != "rt" {
		t.Errorf("without: %+v, %v", tok, err)
	}
}

// What counts as "no keyring here" (use the file) rather than a failure. A
// keyring that is there but did not answer is not: see
// TestKeyringThatHangsIsNoSignOut.
func TestKeyringUnavailable(t *testing.T) {
	cases := map[error]bool{
		keyring.ErrUnsupportedPlatform:                                                  true,
		fmt.Errorf("keyring: %w", keyring.ErrUnsupportedPlatform):                       true,
		&keyringTimeout{after: keyringLimit}:                                            false,
		errors.New("dbus: couldn't determine address of session bus"):                   true,
		errors.New("The name org.freedesktop.secrets was not provided by any .service"): true,
		errors.New("failed to talk to the Secret Service"):                              true,
		errors.New("No such interface \"org.freedesktop.DBus.Properties\""):             true,
		keyring.ErrNotFound:         false,
		keyring.ErrSetDataTooBig:    false,
		errors.New("access denied"): false,
	}
	for err, want := range cases {
		if got := keyringUnavailable(err); got != want {
			t.Errorf("keyringUnavailable(%q) = %v, want %v", err, got, want)
		}
	}
}
