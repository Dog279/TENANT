//go:build linux || darwin

package procprotect

import (
	"errors"

	"golang.org/x/sys/unix"
)

// noCoreFile sets the soft core-file limit to 0. The hard limit stays, so a
// command Tenant starts can raise its own again.
func noCoreFile() error {
	var lim unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_CORE, &lim); err != nil {
		return err
	}
	lim.Cur = 0
	if err := unix.Setrlimit(unix.RLIMIT_CORE, &lim); err != nil {
		return err
	}
	if err := unix.Getrlimit(unix.RLIMIT_CORE, &lim); err != nil || lim.Cur != 0 {
		return errors.Join(errors.New("the soft core limit did not stay 0"), err)
	}
	return nil
}
