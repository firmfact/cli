//go:build !windows

package ui

import "os"

// Terminals elsewhere understand escape sequences without being asked.

func escapesWork(*os.File) bool { return true }

func restoreConsoles() {}
