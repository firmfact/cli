package config

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/zalando/go-keyring"
)

// secretStore is the part of the OS keyring the CLI uses; tests put a fake
// in its place so they never touch the real one.
type secretStore interface {
	Get(service, user string) (string, error)
	Set(service, user, secret string) error
	Delete(service, user string) error
}

type systemKeyring struct{}

func (systemKeyring) Get(service, user string) (string, error) {
	return keyring.Get(service, user)
}

func (systemKeyring) Set(service, user, secret string) error {
	return keyring.Set(service, user, secret)
}

func (systemKeyring) Delete(service, user string) error {
	return keyring.Delete(service, user)
}

var (
	secrets secretStore = systemKeyring{}

	// keyringLimit bounds each keyring call. A locked Secret Service or a
	// wedged D-Bus can leave a call waiting for good, and a command that
	// never returns is worse than one that says why it stopped.
	keyringLimit = 5 * time.Second
)

// ErrKeyringTimeout is what a keyring call that took too long matches
// (errors.Is): the keyring is there but did not answer, most often because
// it is locked and waits for its password. Unlike a keyring that is not
// there at all, that says nothing about where the sign-in is, so it is not
// taken for "signed out", and a sign-in is not moved to the token file
// because of it.
var ErrKeyringTimeout = errors.New("the system keyring did not answer")

// keyringState remembers that the keyring timed out, so the rest of the
// process does not wait as long again, and whether the user has heard that
// the token file stands in for it.
var keyringState struct {
	sync.Mutex
	timedOut error
	warned   bool
}

// keyringTimeout is the error of a keyring call that took too long.
type keyringTimeout struct{ after time.Duration }

func (e *keyringTimeout) Error() string {
	return fmt.Sprintf("the system keyring did not answer within %s. If it is locked, unlock it and try again, "+
		"or set FIRMFACT_TOKEN_STORE=file to keep sign-ins in a file instead", waitText(e.after))
}

func (e *keyringTimeout) Is(target error) bool { return target == ErrKeyringTimeout }

// keyringPatience is how much longer than keyringLimit a keyring call may
// take, and the context that ends the wait early; see SetKeyringPatience.
var keyringPatience struct {
	sync.Mutex
	ctx   context.Context
	extra time.Duration
}

// SetKeyringPatience lets a keyring call take extra time beyond its usual
// limit, until ctx ends. The CLI gives it to a command with a person at the
// terminal: a locked Secret Service, GNOME Keyring or KeePassXC among them,
// holds each call until its unlock prompt is answered, and typing a
// password takes longer than the limit. A script has nobody to answer it,
// so extra is 0 and the limit stands.
func SetKeyringPatience(ctx context.Context, extra time.Duration) {
	keyringPatience.Lock()
	defer keyringPatience.Unlock()
	keyringPatience.ctx, keyringPatience.extra = ctx, extra
}

// withKeyring runs op, a keyring call, for at most keyringLimit, and past
// that for the patience SetKeyringPatience gave, saying what it waits for.
// A call that runs out of time is left to finish on its own, and the error
// is a keyringTimeout; the rest of the process then goes without the
// keyring at once.
func withKeyring[T any](op func() (T, error)) (T, error) {
	var zero T
	keyringState.Lock()
	down := keyringState.timedOut
	keyringState.Unlock()
	if down != nil {
		return zero, down
	}

	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := op()
		done <- result{v, err}
	}()
	timer := time.NewTimer(keyringLimit)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.v, r.err
	case <-timer.C:
	}
	keyringPatience.Lock()
	ctx, extra := keyringPatience.ctx, keyringPatience.extra
	keyringPatience.Unlock()
	if extra > 0 {
		if ctx == nil {
			ctx = context.Background()
		}
		notef("the system keyring has not answered for %s; if it asks for its password to unlock it, "+
			"give it there. Waiting up to %s more", waitText(keyringLimit), waitText(extra))
		timer.Reset(extra)
		select {
		case r := <-done:
			return r.v, r.err
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-timer.C:
		}
	}
	err := &keyringTimeout{after: keyringLimit + extra}
	keyringState.Lock()
	keyringState.timedOut = err
	keyringState.Unlock()
	return zero, err
}

// warnFileStandsIn says, once a process, that the token file stands in for
// a keyring that did not answer.
func warnFileStandsIn(err error) {
	keyringState.Lock()
	first := !keyringState.warned
	keyringState.warned = true
	keyringState.Unlock()
	if first {
		warnf("%v; using the copy of your sign-in in the token file for this command", err)
	}
}

// waitText is d as a person reads it: "5s", "2 minutes".
func waitText(d time.Duration) string {
	switch {
	case d == time.Minute:
		return "a minute"
	case d > time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%d minutes", d/time.Minute)
	}
	return d.String()
}

// The keyring calls read secrets here rather than in withKeyring's
// goroutine, which may outlive the command's use of it.

func keyringGet(host string) (string, error) {
	k := secrets
	return withKeyring(func() (string, error) { return k.Get(keyringService, host) })
}

func keyringSet(host, secret string) error {
	k := secrets
	_, err := withKeyring(func() (struct{}, error) { return struct{}{}, k.Set(keyringService, host, secret) })
	return err
}

func keyringDelete(host string) error {
	k := secrets
	_, err := withKeyring(func() (struct{}, error) { return struct{}{}, k.Delete(keyringService, host) })
	return err
}

// A missing Secret Service (no D-Bus session, no keychain) is
// "unavailable", not a failure: fall back to the file store. One that is
// there but does not answer is neither (see ErrKeyringTimeout).
func keyringUnavailable(err error) bool {
	if errors.Is(err, keyring.ErrUnsupportedPlatform) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "dbus") || strings.Contains(msg, "secret service") ||
		strings.Contains(msg, "org.freedesktop") || strings.Contains(msg, "no such interface")
}

// warnings is where the token store says that it fell back to the file. The
// CLI points it at the command's stderr; it is never stdout, which scripts
// read.
var warnings = struct {
	sync.Mutex
	w io.Writer
}{w: os.Stderr}

// SetWarningOutput sends the token store's warnings to w.
func SetWarningOutput(w io.Writer) {
	warnings.Lock()
	defer warnings.Unlock()
	warnings.w = w
}

func warnf(format string, args ...any) {
	warnings.Lock()
	defer warnings.Unlock()
	fmt.Fprintf(warnings.w, "warning: "+format+"\n", args...)
}

func notef(format string, args ...any) {
	warnings.Lock()
	defer warnings.Unlock()
	fmt.Fprintf(warnings.w, "note: "+format+"\n", args...)
}
