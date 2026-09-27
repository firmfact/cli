//go:build windows

package config

import "os"

// Windows keeps a user's profile private through its access control lists,
// which Unix permission bits say nothing about, so these checks are left to
// them.

func checkPrivateDir(string) error { return nil }

func openPrivate(path string) (*os.File, error) { return os.Open(path) }

// syncDir has nothing to do: Windows cannot open a directory to flush it,
// and NTFS journals the rename itself.
func syncDir(string) {}
