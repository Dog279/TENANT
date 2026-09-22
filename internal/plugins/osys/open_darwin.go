//go:build darwin

package osys

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// openFile opens path refusing a symbolic link in ANY component
// (O_NOFOLLOW_ANY, macOS 11 and later). The file tools open the path
// Roots.Resolve checked, which has no symlinks left in it, so a link that
// appears on it afterwards makes the open fail instead of redirecting it.
func openFile(path string, flag int, perm os.FileMode) (*os.File, error) {
	f, err := os.OpenFile(path, flag|unix.O_NOFOLLOW_ANY, perm)
	if errors.Is(err, syscall.EINVAL) {
		// A macOS without O_NOFOLLOW_ANY: at least refuse a link as the last
		// component. The identity checks still apply.
		return os.OpenFile(path, flag|syscall.O_NOFOLLOW, perm)
	}
	return f, linkAppeared(path, err)
}
