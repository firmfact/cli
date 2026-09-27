package httpx

import (
	"errors"

	"golang.org/x/sys/windows"
)

// refused reports whether err is a connection the host turned away. Winsock
// has its own error number for it, which syscall.ECONNREFUSED is not.
func refused(err error) bool { return errors.Is(err, windows.WSAECONNREFUSED) }

// reset reports whether err is a connection the other end reset, by
// Winsock's number for it as for refused.
func reset(err error) bool { return errors.Is(err, windows.WSAECONNRESET) }
