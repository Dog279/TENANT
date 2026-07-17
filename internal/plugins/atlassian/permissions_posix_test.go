//go:build !windows

package atlassian

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ctreminiom/go-atlassian/v2/service/common"
)

func TestFileTokenStorePOSIXModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	path := filepath.Join(dir, "atlassian-token.json")
	store := fileTokenStore{path: path}
	if err := store.SetToken(context.Background(), &common.OAuth2Token{AccessToken: "secret"}); err != nil {
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
