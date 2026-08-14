//go:build !windows

package peering

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPeeringSecretPOSIXModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	store, err := LoadStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(&Peer{Name: "peer", Token: "secret"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrMintCert(dir); err != nil {
		t.Fatal(err)
	}
	assertPOSIXMode(t, dir, 0o700)
	for _, name := range []string{"peers.json", peerCertFile, peerKeyFile} {
		assertPOSIXMode(t, filepath.Join(dir, name), 0o600)
	}
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
