//go:build !windows

package secrets

import (
	"fmt"
	"os"
)

func protectPath(path string, directory bool) error {
	mode := os.FileMode(fileMode)
	if directory {
		mode = dirMode
	}
	return os.Chmod(path, mode)
}

func probePath(path string, directory bool) (bool, string, string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, "POSIX mode", "", err
	}
	want := os.FileMode(fileMode)
	kind := "file"
	if directory {
		want = dirMode
		kind = "parent directory"
	}
	got := info.Mode().Perm()
	detail := fmt.Sprintf("%s mode %04o", kind, got)
	if got != want {
		detail = fmt.Sprintf("%s mode %04o, want %04o", kind, got, want)
		return false, "POSIX owner-only mode", detail, nil
	}
	return true, "POSIX owner-only mode", detail, nil
}
