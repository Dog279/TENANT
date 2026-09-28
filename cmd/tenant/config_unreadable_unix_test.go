//go:build !windows

package main

import (
	"os"
	"testing"
)

// makeUnreadable takes every permission off path.
func makeUnreadable(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
}
