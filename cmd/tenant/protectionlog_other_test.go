//go:build !windows

package main

import (
	"os"
	"testing"
)

// denyWrites makes dir unwritable to this user, as another program of the
// same user can. Root writes regardless, so the test cannot run as root.
func denyWrites(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root writes into a folder whatever its mode")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}
