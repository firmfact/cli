//go:build !windows

package claim

import (
	"fmt"
	"os"
	"os/user"
	"slices"
	"strconv"
	"syscall"
)

// currentIDs returns the user's uid and every group they are in. It is a
// variable so tests can play another user.
var currentIDs = func() (uid int, gids []int) {
	gids, _ = os.Getgroups()
	return os.Getuid(), append(gids, os.Getgid())
}

// foreignOwner describes who else owns the file, or returns "" when it is
// the user's own. Replacing a file by renaming a new one over it makes the
// user its owner and can change its group, so a file owned by root, by
// another user, or by a group the user is not in has to be edited by hand.
func foreignOwner(info os.FileInfo) string {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	uid, gids := currentIDs()
	if int(st.Uid) != uid {
		id := strconv.FormatUint(uint64(st.Uid), 10)
		if u, err := user.LookupId(id); err == nil {
			return u.Username
		}
		return "user " + id
	}
	if !slices.Contains(gids, int(st.Gid)) {
		id := strconv.FormatUint(uint64(st.Gid), 10)
		if g, err := user.LookupGroupId(id); err == nil {
			return fmt.Sprintf("the group %s, which you are not in", g.Name)
		}
		return fmt.Sprintf("group %s, which you are not in", id)
	}
	return ""
}

// keepGroup gives the replacement file the group of the file it replaces,
// which a new file in the same directory does not always get by itself.
func keepGroup(f *os.File, old os.FileInfo) error {
	st, ok := old.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if cur, ok := info.Sys().(*syscall.Stat_t); ok && cur.Gid == st.Gid {
		return nil
	}
	return f.Chown(-1, int(st.Gid))
}
