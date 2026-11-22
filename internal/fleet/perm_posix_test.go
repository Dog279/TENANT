//go:build !windows

package fleet

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Every file the fleet writes that is worth stealing is readable by its
// owner alone: mode 0600 in a 0700 directory, whatever the umask, and still
// so after the file has been replaced by a later save.
func TestFleetFilesAreOwnerOnly(t *testing.T) {
	defer syscall.Umask(syscall.Umask(0)) // nothing may lean on a careful umask
	for _, path := range fleetFiles(t) {
		assertMode(t, path, 0o600)
		assertMode(t, filepath.Dir(path), 0o700)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s has mode %04o, want %04o", path, got, want)
	}
}
