//go:build !windows

package config

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// openLockFile opens the lock file, creating it. O_NOFOLLOW: a symlink put
// in its place is refused rather than followed to create a file elsewhere.
func openLockFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
}

// tryLock takes an exclusive flock on f without waiting, and reports
// whether it got it. flock locks belong to the open file, so each call to
// lockTokens, in this process or another, has one of its own.
func tryLock(f *os.File) (bool, error) {
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.EWOULDBLOCK):
			return false, nil
		case errors.Is(err, unix.ENOLCK), errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EOPNOTSUPP):
			// A file system without locks (some network mounts): carry on
			// unserialised, as before the lock, rather than refuse to run.
			return true, nil
		default:
			return false, err
		}
	}
}

func unlockFile(f *os.File) { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }
