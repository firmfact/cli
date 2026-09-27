package claim

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// manualEdit is claim declining to change an rc file itself; the user gets
// the lines to add or remove by hand instead.
type manualEdit struct{ path, reason string }

func (e *manualEdit) Error() string { return e.path + ": " + e.reason }

// rcTarget returns the file an edit of the rc file at path lands in. It
// follows links, so the link a dotfile manager (stow, chezmoi, home-manager)
// put in the home directory stays a link and the file it points at gets the
// change. manual is set, with the reason, when claim must leave the file to
// the user: a link to a file that does not exist, or a file that belongs to
// someone else, which a rename would take over.
func rcTarget(path string) (target, manual string, err error) {
	target, err = filepath.EvalSymlinks(path)
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		if dest, lerr := os.Readlink(path); lerr == nil {
			return "", fmt.Sprintf("it links to %s, which does not exist", dest), nil
		}
		return path, "", nil // a new file
	default:
		return "", "", err
	}
	info, err := os.Stat(target)
	if err != nil {
		return "", "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Sprintf("%s is not a regular file", target), nil
	}
	if owner := foreignOwner(info); owner != "" {
		return "", "it belongs to " + owner, nil
	}
	return target, "", nil
}

// writeRC replaces the contents of the rc file at path, or of the file a
// link at path points at, leaving the link as it is. An existing file is
// replaced through a temporary file next to it, so an interrupted write
// leaves the old version or the new one and never half of either, and the
// new file keeps the old one's mode and group.
func writeRC(path, content string) error {
	target, manual, err := rcTarget(path)
	if err != nil {
		return err
	}
	if manual != "" {
		return &manualEdit{path: path, reason: manual}
	}
	info, err := os.Stat(target)
	if errors.Is(err, os.ErrNotExist) {
		// A new file gets the mode the umask gives any file the user creates.
		// O_EXCL: a file that appeared since is not overwritten unseen.
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // G302: an rc file holds nothing secret
		if err != nil {
			return err
		}
		if _, err := f.WriteString(content); err != nil {
			_ = f.Close() // the write error is the one worth reporting
			return err
		}
		return f.Close()
	}
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".firmfact-*")
	if err != nil {
		return err
	}
	// Until the rename, a failure leaves the rc file as it was and takes the
	// half-written copy away.
	done := false
	defer func() {
		if !done {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := keepGroup(tmp, info); err != nil {
		return err
	}
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		return err
	}
	if _, err := tmp.WriteString(content); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return err
	}
	done = true
	return nil
}
