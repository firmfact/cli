package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// The token lock is an advisory file lock that serialises, across
// processes, every change to a stored token and every renewal. The file
// store is read, changed and written back whole, so two commands saving at
// once used to lose one of the writes. And the server rotates refresh
// tokens: two commands renewing with the same one leave the slower holding
// a token the server no longer accepts. Under the lock the second finds the
// first one's result and uses it instead.

const lockName = "credentials.lock"

// lockLimit is how long a command waits for another to let go. The longest
// hold is a renewal, one request to the server.
var lockLimit = 2 * time.Minute

// lockTokens takes the token lock, waiting while another process holds it,
// until ctx ends or lockLimit passes. The lock goes when the returned
// function is called, or with the process.
func lockTokens(ctx context.Context) (unlock func(), err error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, lockName)
	f, err := openLockFile(path)
	if err != nil {
		return nil, fmt.Errorf("opening the token lock: %w", err)
	}
	deadline := time.Now().Add(lockLimit)
	wait := time.Millisecond
	for {
		ok, err := tryLock(f)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("locking %s: %w", path, err)
		}
		if ok {
			return func() {
				unlockFile(f)
				_ = f.Close()
			}, nil
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("another firmfact command has held %s for over %s; try again once it has finished", path, lockLimit)
		}
		// Polling rather than a blocking lock, so a Ctrl-C still ends the
		// wait. Holds are short; the interval stays small.
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			_ = f.Close()
			return nil, ctx.Err()
		case <-t.C:
		}
		wait = min(2*wait, 50*time.Millisecond)
	}
}
