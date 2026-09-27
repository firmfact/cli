package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

// saveChildEnv, when set, makes the test binary save one token for that
// host and exit instead of running the tests, so a test can run writers as
// separate processes, the way commands run.
const saveChildEnv = "FIRMFACT_TEST_SAVE_HOST"

func TestMain(m *testing.M) {
	if host := os.Getenv(saveChildEnv); host != "" {
		if err := SaveToken(host, &Token{AccessToken: "at " + host}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// Fifty commands saving a sign-in at once each keep theirs. The file is
// read, changed and written whole, and without the lock and a temporary
// file per write, concurrent runs lost writes.
func TestParallelWritersLoseNothing(t *testing.T) {
	dir := isolate(t)
	const writers = 50
	children := make([]*exec.Cmd, writers)
	outputs := make([]bytes.Buffer, writers)
	for i := range children {
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), fmt.Sprintf("%s=https://w%d.example", saveChildEnv, i))
		child.Stdout, child.Stderr = &outputs[i], &outputs[i]
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		children[i] = child
	}
	for i, child := range children {
		if err := child.Wait(); err != nil {
			t.Errorf("writer %d: %v: %s", i, err, outputs[i].String())
		}
	}

	tokens, err := readFileTokens()
	if err != nil {
		t.Fatal(err)
	}
	for i := range writers {
		host := fmt.Sprintf("https://w%d.example", i)
		if tok := tokens[host]; tok == nil || tok.AccessToken != "at "+host {
			t.Errorf("%s: stored %+v", host, tok)
		}
	}
	if len(tokens) != writers {
		t.Errorf("%d tokens stored, want %d", len(tokens), writers)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".credentials-") {
			t.Errorf("temporary file %s left behind", e.Name())
		}
	}
}

// Both files are replaced whole, with their own modes, and nothing else is
// left in the directory.
func TestFilesAreReplacedWhole(t *testing.T) {
	dir := isolate(t)
	for i := range 2 {
		if err := SaveToken(DefaultHost, &Token{AccessToken: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
		cfg := &Config{CurrentProfile: DefaultProfile, Profiles: map[string]*Profile{DefaultProfile: {Host: DefaultHost}}}
		if err := cfg.Save(); err != nil {
			t.Fatal(err)
		}
	}
	var names []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if got := strings.Join(names, " "); got != "config.json credentials.json credentials.lock" {
		t.Errorf("directory holds %s", got)
	}
	if runtime.GOOS == "windows" {
		return // no Unix modes to check
	}
	for name, want := range map[string]os.FileMode{"credentials.json": 0o600, "config.json": 0o644} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s has mode %04o, want %04o", name, fi.Mode().Perm(), want)
		}
	}
}

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the ownership and mode checks are Unix's; Windows relies on the profile's access control lists")
	}
}

// A symlink planted as credentials.json would send the token wherever it
// points, so every use of the file refuses it.
func TestPlantedSymlinkIsRefused(t *testing.T) {
	skipOnWindows(t)
	dir := isolate(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	captured := filepath.Join(t.TempDir(), "captured.json")
	if err := os.Symlink(captured, filepath.Join(dir, "credentials.json")); err != nil {
		t.Fatal(err)
	}

	for name, op := range map[string]func() error{
		"save":   func() error { return SaveToken(DefaultHost, &Token{AccessToken: "secret"}) },
		"load":   func() error { _, err := LoadToken(DefaultHost); return err },
		"delete": func() error { return DeleteToken(DefaultHost) },
	} {
		if err := op(); err == nil || !strings.Contains(err.Error(), "is a symbolic link") {
			t.Errorf("%s: got %v, want the link refused", name, err)
		}
	}
	if _, err := os.Lstat(captured); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the link's target was written: %v", err)
	}
}

// The fixed temporary name the store once wrote through is just another
// file name now: a link planted there receives nothing.
func TestOldTemporaryNameCapturesNothing(t *testing.T) {
	skipOnWindows(t)
	dir := isolate(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	captured := filepath.Join(t.TempDir(), "captured.json")
	if err := os.Symlink(captured, filepath.Join(dir, "credentials.json.tmp")); err != nil {
		t.Fatal(err)
	}
	if err := SaveToken(DefaultHost, &Token{AccessToken: "secret"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(captured); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the token went through the link: %v", err)
	}
	if tok, err := LoadToken(DefaultHost); err != nil || tok == nil || tok.AccessToken != "secret" {
		t.Errorf("stored %+v, %v", tok, err)
	}
}

// A token file others can read is refused, with the command that fixes it.
func TestFileOthersCanReadIsRefused(t *testing.T) {
	skipOnWindows(t)
	dir := isolate(t)
	if err := SaveToken(DefaultHost, &Token{AccessToken: "secret"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "credentials.json")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	want := "other users can read or change " + path + " (mode 0644); fix it with: chmod 600 " + path
	if _, err := LoadToken(DefaultHost); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("load: got %v, want %q", err, want)
	}
	if err := SaveToken(DefaultHost, &Token{AccessToken: "next"}); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("save: got %v, want %q", err, want)
	}
}

// A config directory others can write to is refused: they could swap the
// token file for one of their own.
func TestDirectoryOthersCanChangeIsRefused(t *testing.T) {
	skipOnWindows(t)
	dir := isolate(t)
	if err := SaveToken(DefaultHost, &Token{AccessToken: "secret"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	want := "other users can change " + dir + " (mode 0770)"
	if _, err := LoadToken(DefaultHost); err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "chmod go-w "+dir) {
		t.Errorf("load: got %v, want %q", err, want)
	}
	if err := os.Remove(filepath.Join(dir, "credentials.json")); err != nil {
		t.Fatal(err)
	}
	if err := SaveToken(DefaultHost, &Token{AccessToken: "next"}); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("save: got %v, want %q", err, want)
	}
}

// fakeKeyring stands in for the system keyring, which tests never touch.
type fakeKeyring struct {
	mu      sync.Mutex
	secrets map[string]string
	err     error         // every call fails with it, when set
	block   chan struct{} // every call waits for it to close, when set
	calls   int
}

func (k *fakeKeyring) begin() error {
	k.mu.Lock()
	k.calls++
	block, err := k.block, k.err
	k.mu.Unlock()
	if block != nil {
		<-block
	}
	return err
}

func (k *fakeKeyring) Get(service, user string) (string, error) {
	if err := k.begin(); err != nil {
		return "", err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	secret, ok := k.secrets[service+" "+user]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return secret, nil
}

func (k *fakeKeyring) Set(service, user, secret string) error {
	if err := k.begin(); err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.secrets[service+" "+user] = secret
	return nil
}

func (k *fakeKeyring) Delete(service, user string) error {
	if err := k.begin(); err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.secrets[service+" "+user]; !ok {
		return keyring.ErrNotFound
	}
	delete(k.secrets, service+" "+user)
	return nil
}

func (k *fakeKeyring) secret(host string) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.secrets[keyringService+" "+host]
}

func (k *fakeKeyring) callCount() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.calls
}

// useKeyring puts a fake in the system keyring's place, lets the store use
// it, and collects the store's warnings.
func useKeyring(t *testing.T) (*fakeKeyring, *bytes.Buffer) {
	t.Helper()
	k := &fakeKeyring{secrets: map[string]string{}}
	prev := secrets
	secrets = k
	resetKeyringState()
	var warned bytes.Buffer
	SetWarningOutput(&warned)
	t.Setenv("FIRMFACT_TOKEN_STORE", "")
	t.Cleanup(func() {
		secrets = prev
		resetKeyringState()
		SetWarningOutput(os.Stderr)
	})
	return k, &warned
}

// Once the keyring holds a sign-in, its copy in the file goes: it would be
// read again, stale, whenever the keyring was not available.
func TestKeyringSaveDropsTheFileCopy(t *testing.T) {
	dir := isolate(t)
	other := "https://other.example"
	for host, access := range map[string]string{DefaultHost: "from-file", other: "other"} {
		if err := SaveToken(host, &Token{AccessToken: access}); err != nil {
			t.Fatal(err)
		}
	}
	k, warned := useKeyring(t)

	// Stored while there was no keyring: read from the file until saved.
	if where, err := DescribeStore(DefaultHost); err != nil || !strings.Contains(where, "stored while the system keyring was not available") {
		t.Errorf("before: store = %q, %v", where, err)
	}
	if err := SaveToken(DefaultHost, &Token{AccessToken: "renewed"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(k.secret(DefaultHost), `"renewed"`) {
		t.Errorf("keyring holds %q", k.secret(DefaultHost))
	}
	tokens, err := readFileTokens()
	if err != nil {
		t.Fatal(err)
	}
	if tokens[DefaultHost] != nil || tokens[other] == nil {
		t.Errorf("file holds %v; want only %s", tokens, other)
	}
	if tok, err := LoadToken(DefaultHost); err != nil || tok == nil || tok.AccessToken != "renewed" {
		t.Errorf("loaded %+v, %v", tok, err)
	}
	if where, err := DescribeStore(DefaultHost); err != nil || where != "system keyring" {
		t.Errorf("after: store = %q, %v", where, err)
	}
	if warned.Len() != 0 {
		t.Errorf("warned: %s", warned.String())
	}

	if err := DeleteToken(DefaultHost); err != nil {
		t.Fatal(err)
	}
	if k.secret(DefaultHost) != "" {
		t.Error("logout left the keyring entry")
	}
	if tok, err := LoadToken(DefaultHost); err != nil || tok != nil {
		t.Errorf("after delete: %+v, %v", tok, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "credentials.json")); err != nil {
		t.Error("the other host's sign-in went with it")
	}
}

// Without a keyring the sign-in goes to the file, and the user hears that
// once, when it first lands there, not at every renewal.
func TestNoKeyringFallsBackWithOneNotice(t *testing.T) {
	dir := isolate(t)
	k, warned := useKeyring(t)
	k.err = keyring.ErrUnsupportedPlatform

	for i := range 3 {
		if err := SaveToken(DefaultHost, &Token{AccessToken: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "credentials.json")
	if n := strings.Count(warned.String(), "warning: "); n != 1 {
		t.Errorf("%d warnings, want 1:\n%s", n, warned.String())
	}
	if !strings.Contains(warned.String(), "the system keyring is not available") || !strings.Contains(warned.String(), path) {
		t.Errorf("warning = %q", warned.String())
	}
	if tok, err := LoadToken(DefaultHost); err != nil || tok == nil || tok.AccessToken != "2" {
		t.Errorf("loaded %+v, %v", tok, err)
	}
	if where, err := DescribeStore(DefaultHost); err != nil || !strings.HasPrefix(where, path+", as the system keyring is not available") {
		t.Errorf("store = %q, %v", where, err)
	}
	if err := DeleteToken(DefaultHost); err != nil {
		t.Fatal(err)
	}
	if tok, err := LoadToken(DefaultHost); err != nil || tok != nil {
		t.Errorf("after delete: %+v, %v", tok, err)
	}
}

// resetKeyringState forgets a timeout, the warning about it and the
// patience a test gave.
func resetKeyringState() {
	keyringState.Lock()
	keyringState.timedOut, keyringState.warned = nil, false
	keyringState.Unlock()
	SetKeyringPatience(context.Background(), 0)
}

// hangingKeyring is a fake keyring whose calls wait until release is
// called, with a limit of 50ms.
func hangingKeyring(t *testing.T) (k *fakeKeyring, warned *bytes.Buffer, release func()) {
	t.Helper()
	k, warned = useKeyring(t)
	k.block = make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(k.block) }) }
	t.Cleanup(release)
	prev := keyringLimit
	keyringLimit = 50 * time.Millisecond
	t.Cleanup(func() { keyringLimit = prev })
	return k, warned, release
}

// A keyring that does not answer, most often a locked one waiting for its
// password, may hold the sign-in. So it is no "not signed in": reading,
// saving and removing the sign-in fail, as unavailable (ErrKeyringTimeout)
// and saying what to do, and nothing goes to a token file the user never
// chose. The wait is paid once a process.
func TestKeyringThatHangsIsNoSignOut(t *testing.T) {
	dir := isolate(t)
	k, warned, _ := hangingKeyring(t)
	const advice = "the system keyring did not answer within 50ms. If it is locked, unlock it and try again, or set FIRMFACT_TOKEN_STORE=file"

	tok, err := LoadToken(DefaultHost)
	if tok != nil || !errors.Is(err, ErrKeyringTimeout) || !strings.Contains(err.Error(), "could not read your sign-in: "+advice) {
		t.Errorf("load: %+v, %v", tok, err)
	}
	if err := SaveToken(DefaultHost, &Token{AccessToken: "at", RefreshToken: "rt"}); !errors.Is(err, ErrKeyringTimeout) || !strings.Contains(err.Error(), "could not save your sign-in: "+advice) {
		t.Errorf("save: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "credentials.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a token file was written: %v", err)
	}
	if err := DeleteToken(DefaultHost); !errors.Is(err, ErrKeyringTimeout) || !strings.Contains(err.Error(), "could not remove your sign-in from the system keyring: ") {
		t.Errorf("delete: %v", err)
	}
	if _, err := DescribeStore(DefaultHost); !errors.Is(err, ErrKeyringTimeout) {
		t.Errorf("describe: %v", err)
	}
	if n := k.callCount(); n != 1 {
		t.Errorf("%d keyring calls, want the one that timed out", n)
	}
	if warned.Len() != 0 {
		t.Errorf("warned: %s", warned.String())
	}
}

// A sign-in stored in the file while there was no keyring serves the
// command when the keyring does not answer, with one warning, and a renewal
// goes back there, as the file is where it came from. Logout removes that
// copy and still says the keyring may hold the sign-in.
func TestKeyringThatHangsFallsBackToAFileCopy(t *testing.T) {
	isolate(t)
	if err := SaveToken(DefaultHost, &Token{AccessToken: "from file", RefreshToken: "rt-1"}); err != nil {
		t.Fatal(err)
	}
	_, warned, _ := hangingKeyring(t)

	if tok, err := LoadToken(DefaultHost); err != nil || tok == nil || tok.AccessToken != "from file" {
		t.Errorf("load: %+v, %v", tok, err)
	}
	if err := SaveToken(DefaultHost, &Token{AccessToken: "renewed", RefreshToken: "rt-2"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if tok, err := LoadToken(DefaultHost); err != nil || tok == nil || tok.AccessToken != "renewed" {
		t.Errorf("load after the renewal: %+v, %v", tok, err)
	}
	if got := warned.String(); strings.Count(got, "warning: ") != 1 || !strings.Contains(got, "did not answer within 50ms") || !strings.Contains(got, "using the copy of your sign-in in the token file for this command") {
		t.Errorf("warnings = %q", got)
	}

	if err := DeleteToken(DefaultHost); !errors.Is(err, ErrKeyringTimeout) {
		t.Errorf("delete: %v", err)
	}
	if tokens, err := readFileTokens(); err != nil || tokens[DefaultHost] != nil {
		t.Errorf("file after delete: %v, %v", tokens, err)
	}
}

// With a person at the terminal (SetKeyringPatience), a keyring that has
// not answered within its limit gets longer, as its unlock prompt waits for
// a password, and the person hears what the CLI waits for.
func TestKeyringWaitsForItsUnlockPrompt(t *testing.T) {
	isolate(t)
	k, warned, release := hangingKeyring(t)
	k.secrets[keyringService+" "+DefaultHost] = `{"access_token":"in the keyring"}`
	SetKeyringPatience(context.Background(), 10*time.Second)
	time.AfterFunc(150*time.Millisecond, release)

	if tok, err := LoadToken(DefaultHost); err != nil || tok == nil || tok.AccessToken != "in the keyring" {
		t.Errorf("load: %+v, %v", tok, err)
	}
	if want := "note: the system keyring has not answered for 50ms; if it asks for its password to unlock it, give it there. Waiting up to 10s more\n"; warned.String() != want {
		t.Errorf("stderr = %q, want %q", warned.String(), want)
	}
}

// Ctrl-C ends that wait: the command stops as interrupted, not as a
// keyring that timed out.
func TestKeyringWaitEndsWithTheCommand(t *testing.T) {
	isolate(t)
	hangingKeyring(t)
	ctx, cancel := context.WithCancel(context.Background())
	SetKeyringPatience(ctx, time.Minute)
	time.AfterFunc(100*time.Millisecond, cancel)

	start := time.Now()
	if _, err := LoadToken(DefaultHost); !errors.Is(err, context.Canceled) {
		t.Errorf("load: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the wait went on for %s after the interrupt", took)
	}
}

// How long a wait reads.
func TestWaitText(t *testing.T) {
	for d, want := range map[time.Duration]string{5 * time.Second: "5s", time.Minute: "a minute", 2 * time.Minute: "2 minutes", 90 * time.Second: "1m30s"} {
		if got := waitText(d); got != want {
			t.Errorf("waitText(%s) = %q, want %q", d, got, want)
		}
	}
}

// Where the sign-in is kept, as doctor shows it.
func TestDescribeStore(t *testing.T) {
	dir := isolate(t)
	path := filepath.Join(dir, "credentials.json")
	if where, err := DescribeStore(DefaultHost); err != nil || where != path+" (FIRMFACT_TOKEN_STORE=file)" {
		t.Errorf("forced file: %q, %v", where, err)
	}
	t.Setenv("FIRMFACT_TOKEN", "env")
	if where, err := DescribeStore(DefaultHost); err != nil || !strings.HasPrefix(where, "FIRMFACT_TOKEN") {
		t.Errorf("environment: %q, %v", where, err)
	}
	t.Setenv("FIRMFACT_TOKEN", "")

	k, _ := useKeyring(t)
	if where, err := DescribeStore(DefaultHost); err != nil || where != "system keyring" {
		t.Errorf("empty keyring: %q, %v", where, err)
	}
	k.err = errors.New("access denied")
	if _, err := DescribeStore(DefaultHost); err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Errorf("failing keyring: %v", err)
	}
	if _, err := LoadToken(DefaultHost); err == nil {
		t.Error("a keyring failure that is not unavailability must not read as signed out")
	}
}

// Commands renewing at once renew once: the second finds the first one's
// token under the lock and uses it.
func TestRenewTokenRenewsOnceBetweenCommands(t *testing.T) {
	isolate(t)
	held := &Token{AccessToken: "old", RefreshToken: "rt-1", ExpiresAt: time.Now().Add(-time.Minute)}
	if err := SaveToken(DefaultHost, held); err != nil {
		t.Fatal(err)
	}
	var renewals atomic.Int32
	renew := func(stored *Token) (*Token, error) {
		renewals.Add(1)
		if stored.RefreshToken != "rt-1" {
			t.Errorf("renewing with %q", stored.RefreshToken)
		}
		time.Sleep(100 * time.Millisecond) // the other is waiting by now
		return &Token{AccessToken: "new", RefreshToken: "rt-2", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			tok, err := RenewToken(context.Background(), DefaultHost, held, renew)
			if err != nil || tok == nil || tok.AccessToken != "new" {
				t.Errorf("renewed to %+v, %v", tok, err)
			}
		})
	}
	wg.Wait()
	if n := renewals.Load(); n != 1 {
		t.Errorf("%d renewals, want 1", n)
	}
	if tok, _ := LoadToken(DefaultHost); tok == nil || tok.RefreshToken != "rt-2" {
		t.Errorf("stored %+v", tok)
	}
}

// A renewal the server refused (invalid_grant) because a command without
// the lock renewed first ends with that command's token, not the refusal.
func TestRenewTokenAfterRefusalUsesTheNewerToken(t *testing.T) {
	isolate(t)
	held := &Token{AccessToken: "old", RefreshToken: "rt-1"}
	if err := SaveToken(DefaultHost, held); err != nil {
		t.Fatal(err)
	}
	refused := errors.New("invalid_grant")
	tok, err := RenewToken(context.Background(), DefaultHost, held, func(*Token) (*Token, error) {
		// An older CLI, which takes no lock, renews and stores meanwhile.
		if err := writeFileTokens(map[string]*Token{DefaultHost: {AccessToken: "theirs", RefreshToken: "rt-2"}}); err != nil {
			t.Error(err)
		}
		return nil, refused
	})
	if err != nil || tok == nil || tok.AccessToken != "theirs" {
		t.Fatalf("got %+v, %v; want their token", tok, err)
	}

	// Nothing newer stored: the refusal stands.
	if _, err := RenewToken(context.Background(), DefaultHost, tok, func(*Token) (*Token, error) { return nil, refused }); !errors.Is(err, refused) {
		t.Errorf("got %v, want the refusal", err)
	}
}

// A sign-in removed meanwhile (logout in another terminal) is gone, not
// renewed.
func TestRenewTokenAfterLogout(t *testing.T) {
	isolate(t)
	tok, err := RenewToken(context.Background(), DefaultHost, &Token{AccessToken: "old", RefreshToken: "rt"}, func(*Token) (*Token, error) {
		t.Error("renewed a sign-in that is gone")
		return nil, nil
	})
	if err != nil || tok != nil {
		t.Errorf("got %+v, %v; want nothing", tok, err)
	}
}

// A wait for the lock ends with a Ctrl-C, or after lockLimit with an error
// that says why.
func TestLockWaitEnds(t *testing.T) {
	isolate(t)
	unlock, err := lockTokens(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := RenewToken(ctx, DefaultHost, &Token{}, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
	prev := lockLimit
	lockLimit = 30 * time.Millisecond
	t.Cleanup(func() { lockLimit = prev })
	if err := SaveToken(DefaultHost, &Token{AccessToken: "at"}); err == nil || !strings.Contains(err.Error(), "another firmfact command has held") {
		t.Errorf("held: %v", err)
	}
}
