//go:build !darwin && !linux

package osys

import "os"

// openFile is a plain open here: Windows has no flag that refuses a symbolic
// link on the whole path. The identity checks (Roots.checkOpened) still apply,
// and on Windows the restricted exec backend does not confine the filesystem
// yet, so a process able to swap a symlink could reach the file directly
// anyway.
func openFile(path string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flag, perm)
}
