//go:build linux

package osys

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// openFile opens path refusing a symbolic link in ANY component (openat2 with
// RESOLVE_NO_SYMLINKS, Linux 5.6 and later). The file tools open the path
// Roots.Resolve checked, which has no symlinks left in it, so a link that
// appears on it afterwards makes the open fail instead of redirecting it.
func openFile(path string, flag int, perm os.FileMode) (*os.File, error) {
	how := unix.OpenHow{Flags: uint64(flag | unix.O_CLOEXEC), Resolve: unix.RESOLVE_NO_SYMLINKS}
	if flag&os.O_CREATE != 0 {
		how.Mode = uint64(perm.Perm()) // openat2 rejects a mode without O_CREAT
	}
	fd, err := unix.Openat2(unix.AT_FDCWD, path, &how)
	if err == nil {
		return os.NewFile(uintptr(fd), path), nil
	}
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EPERM) {
		// No openat2 (an older kernel, or a seccomp filter that blocks it): at
		// least refuse a link as the last component. The identity checks still
		// apply.
		return os.OpenFile(path, flag|syscall.O_NOFOLLOW, perm)
	}
	return nil, linkAppeared(path, &os.PathError{Op: "open", Path: path, Err: err})
}
