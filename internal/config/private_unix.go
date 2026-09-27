//go:build !windows

package config

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// checkPrivateDir refuses a config directory that is not the user's own, or
// that other users can write to: whoever can write there can swap
// credentials.json for a symlink or for a file of their own.
func checkPrivateDir(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if !ownedByUser(fi) {
		return fmt.Errorf("%s belongs to another user, so the CLI will not keep your sign-in there; use a directory of your own (set FIRMFACT_CONFIG_DIR)", dir)
	}
	if perm := fi.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("other users can change %s (mode %04o), so the CLI will not keep your sign-in there; fix it with: chmod go-w %s", dir, perm, dir)
	}
	return nil
}

// openPrivate opens credentials.json for reading. It must be a plain file
// of the user's own that nobody else can read or write: a symlink can point
// anywhere, and a file others can read may already have been copied.
func openPrivate(path string) (*os.File, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, symlinkError(path)
	}
	// O_NOFOLLOW refuses a symlink swapped in since the Lstat too.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, symlinkError(path)
		}
		return nil, err
	}
	if fi, err = f.Stat(); err == nil {
		err = checkPrivateFile(path, fi)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func checkPrivateFile(path string, fi os.FileInfo) error {
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file; remove it and sign in again", path)
	}
	if !ownedByUser(fi) {
		return fmt.Errorf("%s belongs to another user; remove it and sign in again", path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("other users can read or change %s (mode %04o); fix it with: chmod 600 %s, then sign out and in again in case the token in it was copied", path, perm, path)
	}
	return nil
}

func symlinkError(path string) error {
	return fmt.Errorf("%s is a symbolic link; the CLI keeps your sign-in only in a plain file of its own. Remove the link and sign in again", path)
}

func ownedByUser(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return !ok || int64(st.Uid) == int64(os.Geteuid())
}

// syncDir makes a rename in dir durable. Some file systems cannot sync a
// directory; the rename has happened either way, so that is no error.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
