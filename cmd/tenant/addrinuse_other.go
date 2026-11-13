//go:build !windows

package main

import (
	"errors"
	"syscall"
)

// addrInUse reports a bind refused because the address is taken.
func addrInUse(err error) bool { return errors.Is(err, syscall.EADDRINUSE) }
