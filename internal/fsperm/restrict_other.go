//go:build !windows

package fsperm

import "os"

// Supported reports whether POSIX owner/group/world modes are enforceable.
func Supported() bool { return true }

func passOn(string) error { return nil }

func restrict(path string, maximum os.FileMode) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	mode := info.Mode().Perm()
	return os.Chmod(path, mode&maximum)
}
