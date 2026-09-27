//go:build !windows

package httpx

import (
	"errors"
	"syscall"
)

// refused reports whether err is a connection the host turned away.
func refused(err error) bool { return errors.Is(err, syscall.ECONNREFUSED) }

// reset reports whether err is a connection the other end reset.
func reset(err error) bool { return errors.Is(err, syscall.ECONNRESET) }
