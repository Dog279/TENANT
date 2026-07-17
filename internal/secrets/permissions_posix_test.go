//go:build !windows

package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPOSIXOwnerOnlyModes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "token")
	if err := WriteFile(path, []byte("secret")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode = %04o, want 0600", got)
	}
	info, err = os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("parent mode = %04o, want 0700", got)
	}
}
