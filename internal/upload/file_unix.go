//go:build unix

package upload

import "syscall"

// openFlags make opening a pipe (a FIFO put where a file was) return at
// once instead of waiting for a writer; Hash then refuses it, as it is not
// a regular file. They change nothing for a regular file.
const openFlags = syscall.O_NONBLOCK
