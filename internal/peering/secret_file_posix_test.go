//go:build !windows

package peering

import (
	"os"
	"testing"
)

func assertSecretFileSecurity(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("%s perms = %o, want 600", path, perm)
	}
}
