//go:build windows

package claim

import "os"

// On Windows claim writes a shim and no rc files, and a file's owner is not
// in os.FileInfo; these only keep the package building there.

func foreignOwner(os.FileInfo) string { return "" }

func keepGroup(*os.File, os.FileInfo) error { return nil }
