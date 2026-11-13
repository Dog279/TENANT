//go:build windows

package main

import (
	"errors"

	"golang.org/x/sys/windows"
)

// addrInUse reports a bind refused because the address is taken. Winsock's
// code for it is not the syscall package's EADDRINUSE.
func addrInUse(err error) bool { return errors.Is(err, windows.WSAEADDRINUSE) }
