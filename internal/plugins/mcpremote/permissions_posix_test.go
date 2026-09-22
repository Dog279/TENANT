//go:build !windows

package mcpremote

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTokenCachePOSIXModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	path := tokenCachePath(dir, "https://example.test/mcp")
	if err := saveTokenCache(path, &tokenCache{ServerURL: "https://example.test/mcp"}); err != nil {
		t.Fatal(err)
	}
	assertPOSIXMode(t, path, 0o600)
	assertPOSIXMode(t, dir, 0o700)
}

func assertPOSIXMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %04o, want %04o", path, got, want)
	}
}
