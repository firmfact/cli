// Package config keeps the CLI's profiles (which firmfact host, which default
// workspace) and its OAuth tokens.
//
// Tokens live in the OS keyring when one is available and fall back to a
// 0600 file next to the config otherwise (headless Linux, CI), which is
// refused when others could have read or replaced it. Changes to it and
// token renewals take a lock shared by every process (see lockTokens).
// FIRMFACT_TOKEN overrides both with a bare access token for scripts, for
// one host only (see envToken).
package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/zalando/go-keyring"
)

const (
	DefaultHost    = "https://firmfact.com"
	DefaultProfile = "default"
	keyringService = "firmfact-cli"
)

type Profile struct {
	Host string `json:"host"`
	// InsecureHTTP records that the host was set with --insecure-http: it
	// is plain http to another machine, which ParseHost refuses otherwise.
	InsecureHTTP bool `json:"insecure_http,omitempty"`
	// Workspace is the default workspace id for commands that take one.
	Workspace string `json:"workspace,omitempty"`
}

type Config struct {
	CurrentProfile string              `json:"current_profile"`
	Profiles       map[string]*Profile `json:"profiles"`
	// unusable is why this config stands in for a file that could not be
	// used (see Placeholder); Save refuses to write it.
	unusable error
}

type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	ClientID     string    `json:"client_id,omitempty"`
}

// Expired reports whether the access token is expired or about to be.
func (t *Token) Expired() bool {
	return !t.ExpiresAt.IsZero() && time.Now().Add(30*time.Second).After(t.ExpiresAt)
}

func Dir() (string, error) {
	if dir := os.Getenv("FIRMFACT_CONFIG_DIR"); dir != "" {
		return dir, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "firmfact"), nil
}

func CacheDir() (string, error) {
	if dir := os.Getenv("FIRMFACT_CACHE_DIR"); dir != "" {
		return dir, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "firmfact"), nil
}

// Path is where the config file is kept.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// FileError is a config file that is there but cannot be used: it does
// not parse, or cannot be read. The CLI never saves over one (see Save);
// the user repairs it, or sets it aside with `config reset` (see Reset).
// Carrying on with the defaults instead once let the next change of
// profile replace a file with a trailing comma, and every profile in it.
type FileError struct {
	Path string
	Err  error
}

func (e *FileError) Error() string { return "config file " + e.Path + ": " + e.Err.Error() }
func (e *FileError) Unwrap() error { return e.Err }

// Load reads the config file; without one, it is the defaults. A file that
// cannot be used is a *FileError.
func Load() (*Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	return read(path)
}

func read(path string) (*Config, error) {
	cfg := &Config{CurrentProfile: DefaultProfile, Profiles: map[string]*Profile{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, &FileError{Path: path, Err: err}
	}
	// An empty file holds nothing to lose, so it is the defaults rather
	// than a file to repair.
	if len(bytes.TrimSpace(raw)) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, &FileError{Path: path, Err: jsonProblem(raw, err)}
	}
	for name, p := range cfg.Profiles {
		if p == nil {
			return nil, &FileError{Path: path, Err: fmt.Errorf("profile %q is null; give it a host or remove it", name)}
		}
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]*Profile{}
	}
	if cfg.CurrentProfile == "" {
		cfg.CurrentProfile = DefaultProfile
	}
	return cfg, nil
}

// jsonProblem says where in raw the problem err is, as the line and column
// an editor shows (encoding/json gives a byte offset), and a value of the
// wrong type in words rather than Go's.
func jsonProblem(raw []byte, err error) error {
	var (
		syntax  *json.SyntaxError
		typeErr *json.UnmarshalTypeError
		offset  int64
		what    = err.Error()
	)
	switch {
	case errors.As(err, &syntax):
		offset = syntax.Offset
	case errors.As(err, &typeErr):
		offset = typeErr.Offset
		what = fmt.Sprintf("%s should be %s, not %s", typeErr.Field, jsonKind(typeErr.Type), article(typeErr.Value))
	default:
		return err
	}
	// The offset counts the offending byte.
	if offset < 1 || offset > int64(len(raw)) {
		return errors.New(what)
	}
	before := raw[:offset-1]
	line := bytes.Count(before, []byte("\n")) + 1
	column := len(before) - bytes.LastIndexByte(before, '\n')
	return fmt.Errorf("line %d, column %d: %s", line, column, what)
}

func jsonKind(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "true or false"
	case reflect.Map, reflect.Struct, reflect.Pointer:
		return "an object"
	}
	return article(t.String())
}

func article(noun string) string {
	if noun != "" && strings.ContainsRune("aeiou", rune(noun[0])) {
		return "an " + noun
	}
	return "a " + noun
}

// Placeholder stands in for a config file that cannot be used, cause
// saying why: the defaults, which Save refuses to write, so that the file
// stays as it is for the user to repair or reset.
func Placeholder(cause error) *Config {
	return &Config{CurrentProfile: DefaultProfile, Profiles: map[string]*Profile{}, unusable: cause}
}

// Save writes the config. It refuses to replace a file that does not
// parse, even one that broke after Load read it: someone may be editing
// it by hand, and what it holds is theirs to repair.
func (c *Config) Save() error {
	if c.unusable != nil {
		return c.unusable
	}
	path, err := Path()
	if err != nil {
		return err
	}
	if _, err := read(path); err != nil {
		return err
	}
	return writeJSON(path, c, 0o644)
}

// Reset moves the config file aside, to a copy named after the time, and
// returns the copy's path, or "" when there is no file. Commands then start
// again from the defaults; the copy keeps what the file held.
func Reset(now time.Time) (string, error) {
	path, err := Path()
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	stamp := now.Format("20060102-150405")
	for i := 1; i <= 100; i++ {
		backup := fmt.Sprintf("%s.%s.bak", path, stamp)
		if i > 1 {
			backup = fmt.Sprintf("%s.%s-%d.bak", path, stamp, i)
		}
		// A rename would replace a copy made in the same second.
		_, err := os.Lstat(backup)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Rename(path, backup); err != nil {
				return "", err
			}
			return backup, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("too many copies of %s from %s", path, stamp)
}

// Profile returns the named profile, creating it with the default host.
func (c *Config) Profile(name string) *Profile {
	if name == "" {
		name = c.CurrentProfile
	}
	p, ok := c.Profiles[name]
	if !ok {
		p = &Profile{Host: DefaultHost}
		c.Profiles[name] = p
	}
	return p
}

// ParsedHost is the profile's stored host, checked with ParseHost (or
// ParseHostAllowHTTP when the profile or this invocation allows plain http).
// A stored host that fails is an error, never a quiet switch to the default:
// the host decides where the token is sent.
func (p *Profile) ParsedHost(allowHTTP bool) (string, error) {
	parse := ParseHost
	if allowHTTP || p.InsecureHTTP {
		parse = ParseHostAllowHTTP
	}
	return parse(p.Host)
}

// LoadToken returns the stored token for host, or nil when there is none.
func LoadToken(host string) (*Token, error) {
	if env := os.Getenv("FIRMFACT_TOKEN"); env != "" {
		return envToken(env, host)
	}
	return store{}.load(host)
}

// TokenFromEnv reports whether FIRMFACT_TOKEN supplies the token. Such a
// token is never stored, so logout has nothing of it to forget.
func TokenFromEnv() bool { return os.Getenv("FIRMFACT_TOKEN") != "" }

// envToken hands out FIRMFACT_TOKEN for one host only: FIRMFACT_HOST when
// set, else the default. A token in the environment is easily forgotten
// about, and --host or a profile pointing elsewhere must not receive it. A
// mismatch fails rather than falling back to the stored sign-in, so a script
// never runs as someone it did not mean to.
func envToken(token, host string) (*Token, error) {
	tokenHost, source := DefaultHost, "the default host "+DefaultHost
	if env := os.Getenv("FIRMFACT_HOST"); env != "" {
		parsed, err := ParseHostAllowHTTP(env)
		if err != nil {
			return nil, fmt.Errorf("FIRMFACT_HOST: %w", err)
		}
		tokenHost, source = parsed, "FIRMFACT_HOST ("+parsed+")"
	}
	if host != tokenHost {
		return nil, fmt.Errorf("FIRMFACT_TOKEN is only sent to %s, not to %s; set FIRMFACT_HOST=%s if the token is for that host, or unset FIRMFACT_TOKEN to use your stored sign-in",
			source, host, host)
	}
	return &Token{AccessToken: token}, nil
}

func SaveToken(host string, t *Token) error { return store{}.save(host, t) }

func DeleteToken(host string) error { return store{}.delete(host) }

// RenewToken renews host's token under the token lock, so that commands
// running at once renew it once between them. held is the token the caller
// has been using. Once the lock is taken the stored token is read again:
// when another command has replaced held meanwhile, that token is returned
// and renew is not called. Otherwise renew gets the stored token and what
// it returns is stored and returned. A nil token means the sign-in is gone.
//
// When renew fails, the stored token is read once more. A token stored
// meanwhile, by a command that takes no lock (an older version of the CLI,
// say), is why the server refused (invalid_grant) the one renew sent, and
// it is used instead of the error.
func RenewToken(ctx context.Context, host string, held *Token, renew func(stored *Token) (*Token, error)) (*Token, error) {
	unlock, err := lockTokens(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	s := store{held: true}
	stored, err := s.load(host)
	if err != nil {
		return nil, err
	}
	// A replacement that has itself expired is renewed rather than used.
	if stored == nil || (!sameToken(stored, held) && !stored.Expired()) {
		return stored, nil
	}
	fresh, err := renew(stored)
	if err != nil {
		if ctx.Err() == nil {
			if again, lerr := s.load(host); lerr == nil && again != nil && !sameToken(again, stored) {
				return again, nil
			}
		}
		return nil, err
	}
	if err := s.save(host, fresh); err != nil {
		return nil, err
	}
	return fresh, nil
}

func sameToken(a, b *Token) bool {
	return a != nil && b != nil && a.AccessToken == b.AccessToken && a.RefreshToken == b.RefreshToken
}

// DescribeStore says where host's token is kept, for doctor: in the
// environment, in the system keyring, or in the file and why.
func DescribeStore(host string) (string, error) {
	if TokenFromEnv() {
		return "FIRMFACT_TOKEN from the environment (not stored)", nil
	}
	path, err := credentialsPath()
	if err != nil {
		return "", err
	}
	if fileStoreForced() {
		return path + " (FIRMFACT_TOKEN_STORE=file)", nil
	}
	_, err = keyringGet(host)
	switch {
	case err == nil:
		return "system keyring", nil
	case keyringUnavailable(err):
		return fmt.Sprintf("%s, as the system keyring is not available (%v)", path, err), nil
	case errors.Is(err, ErrKeyringTimeout):
		return "", err
	case !errors.Is(err, keyring.ErrNotFound):
		return "", fmt.Errorf("system keyring: %w", err)
	}
	// Not in the keyring: a sign-in stored while the keyring was not
	// available is still read from the file until it is next saved.
	if t, err := (store{}).fileToken(host); err != nil {
		return "", err
	} else if t != nil {
		return path + " (stored while the system keyring was not available)", nil
	}
	return "system keyring", nil
}

// store reads and writes tokens: in the OS keyring when there is one, and
// otherwise in credentials.json, a file only the user can read. Every use
// of the file happens under the token lock; held says the caller already
// holds it (RenewToken), so it is not taken twice.
type store struct{ held bool }

func (s store) locked(fn func() error) error {
	if s.held {
		return fn()
	}
	unlock, err := lockTokens(context.Background())
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

func (s store) load(host string) (*Token, error) {
	if !fileStoreForced() {
		secret, err := keyringGet(host)
		switch {
		case err == nil:
			var t Token
			if err := json.Unmarshal([]byte(secret), &t); err != nil {
				return nil, err
			}
			return &t, nil
		case errors.Is(err, ErrKeyringTimeout):
			// The keyring may well hold the sign-in and not have
			// answered. A copy in the file, stored while there was no
			// keyring, serves this command; without one, the sign-in is
			// not known to be gone, so this is no "not signed in".
			t, ferr := s.fileToken(host)
			if ferr != nil {
				return nil, ferr
			}
			if t == nil {
				return nil, fmt.Errorf("could not read your sign-in: %w", err)
			}
			warnFileStandsIn(err)
			return t, nil
		case !errors.Is(err, keyring.ErrNotFound) && !keyringUnavailable(err):
			return nil, err
		}
	}
	return s.fileToken(host)
}

// fileToken is host's token in the file, or nil.
func (s store) fileToken(host string) (*Token, error) {
	if !credentialsExist() {
		return nil, nil
	}
	var t *Token
	err := s.locked(func() error {
		tokens, err := readFileTokens()
		t = tokens[host]
		return err
	})
	return t, err
}

func (s store) save(host string, t *Token) error {
	// G117: storing the token is the point; it goes to the keyring, or to a
	// file only the user can read.
	raw, err := json.Marshal(t) //nolint:gosec // see above
	if err != nil {
		return err
	}
	var keyringErr error
	if !fileStoreForced() {
		keyringErr = keyringSet(host, string(raw))
		switch {
		case keyringErr == nil:
			s.dropFileCopy(host)
			return nil
		case errors.Is(keyringErr, ErrKeyringTimeout):
			// A keyring that is there but did not answer keeps the old
			// sign-in, and would hand it out again once unlocked. Only a
			// sign-in this command read from the file, which the file
			// then serves, goes back there; any other is not moved to a
			// file the user never chose.
			t, err := s.fileToken(host)
			if err != nil {
				return err
			}
			if t == nil {
				return fmt.Errorf("could not save your sign-in: %w", keyringErr)
			}
			warnFileStandsIn(keyringErr)
		case !keyringUnavailable(keyringErr):
			return keyringErr
		}
	}
	isNew := false
	err = s.editFile(func(tokens map[string]*Token) bool {
		_, had := tokens[host]
		isNew = !had
		tokens[host] = t
		return true
	})
	// Said once, when a sign-in first lands in the file, rather than at
	// every renewal: a machine without a keyring would hear it hourly. A
	// keyring that timed out has said so already.
	if err == nil && isNew && keyringErr != nil && !errors.Is(keyringErr, ErrKeyringTimeout) {
		path, _ := credentialsPath()
		warnf("the system keyring is not available (%v), so your sign-in is kept in %s, which only you can read. Set FIRMFACT_TOKEN_STORE=file to use the file without trying the keyring", keyringErr, path)
	}
	return err
}

// dropFileCopy removes host's entry from the file once the keyring holds
// its token. A copy left behind from a time without a keyring would be read
// again, stale, the next time the keyring is not available.
func (s store) dropFileCopy(host string) {
	if !credentialsExist() {
		return
	}
	err := s.editFile(func(tokens map[string]*Token) bool {
		_, had := tokens[host]
		delete(tokens, host)
		return had
	})
	if err != nil {
		warnf("your sign-in is now in the system keyring, but its old copy in the token file could not be removed: %v", err)
	}
}

// delete removes host's sign-in from the keyring and the file. A keyring
// that did not answer may still hold it: the copy in the file goes all the
// same, and the error says so, so that logout never reports a sign-out
// that did not happen.
func (s store) delete(host string) error {
	var keyringErr error
	if !fileStoreForced() {
		err := keyringDelete(host)
		switch {
		case errors.Is(err, ErrKeyringTimeout):
			keyringErr = fmt.Errorf("could not remove your sign-in from the system keyring: %w", err)
		case err != nil && !errors.Is(err, keyring.ErrNotFound) && !keyringUnavailable(err):
			return err
		}
	}
	if credentialsExist() {
		err := s.editFile(func(tokens map[string]*Token) bool {
			_, had := tokens[host]
			delete(tokens, host)
			return had
		})
		if err != nil {
			return err
		}
	}
	return keyringErr
}

// editFile reads the file, lets change edit its tokens, and writes them back
// when change reports a change, all under the token lock, so that no other
// command's write falls between the read and the write.
func (s store) editFile(change func(map[string]*Token) bool) error {
	return s.locked(func() error {
		tokens, err := readFileTokens()
		if err != nil {
			return err
		}
		if !change(tokens) {
			return nil
		}
		return writeFileTokens(tokens)
	})
}

func fileStoreForced() bool {
	return os.Getenv("FIRMFACT_TOKEN_STORE") == "file"
}

func credentialsPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "credentials.json"), nil
}

// credentialsExist reports whether there is anything at the file's path
// (a symlink counts, to be refused when read). Without one, reading needs
// no lock.
func credentialsExist() bool {
	path, err := credentialsPath()
	if err != nil {
		return true // let the read report it
	}
	_, err = os.Lstat(path)
	return !errors.Is(err, os.ErrNotExist)
}

// readFileTokens reads the file, refusing one that others could have
// written or read (see checkPrivateDir and openPrivate).
func readFileTokens() (map[string]*Token, error) {
	path, err := credentialsPath()
	if err != nil {
		return nil, err
	}
	tokens := map[string]*Token{}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return tokens, nil
	}
	if err := checkPrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	f, err := openPrivate(path)
	if errors.Is(err, os.ErrNotExist) {
		return tokens, nil
	}
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(f)
	_ = f.Close() // opened to read: the read has said whatever went wrong
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &tokens); err != nil {
		// Nothing writes over it (see editFile): it may hold the sign-ins
		// of other hosts. The error says which file, and where it broke.
		return nil, fmt.Errorf("token file %s: %w; remove it and sign in again", path, jsonProblem(raw, err))
	}
	return tokens, nil
}

func writeFileTokens(tokens map[string]*Token) error {
	path, err := credentialsPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := checkPrivateDir(dir); err != nil {
		return err
	}
	return writeJSON(path, tokens, 0o600)
}

// WriteJSON is writeJSON for the CLI's other files, such as its caches: a
// process that ends while writing one, or two that write it at once, leave
// the old file or a whole new one.
func WriteJSON(path string, v any, mode os.FileMode) error { return writeJSON(path, v, mode) }

// writeJSON replaces path with v as indented JSON. The content goes to a
// new temporary file of its own in the same directory, which is renamed
// over path once it is complete and on disk: a reader sees the old file or
// the new one, never part of either, and writers at the same time cannot
// mix their parts. A fixed temporary name allowed both, and anyone who
// planted a symlink under that name received the file. The rename replaces
// a symlink at path rather than writing through it.
func writeJSON(path string, v any, mode os.FileMode) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))+"-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err = tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err = tmp.Write(raw); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}
