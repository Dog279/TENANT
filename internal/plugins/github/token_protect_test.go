package github

import (
	"os"
	"path/filepath"
	"testing"

	"tenant/internal/secrets"
)

// TEN-337: the GitHub PAT cache goes through the secrets package. A plain
// os.WriteFile(0600) left an existing loose cache loose and gave Windows no
// DACL at all.

func TestSaveTokenIsProtected(t *testing.T) {
	dir := t.TempDir()
	if err := SaveToken(dir, "ghp_protected"); err != nil {
		t.Fatal(err)
	}
	if report := secrets.Probe(TokenPath(dir)); !report.Protected {
		t.Fatalf("saved GitHub cache is not protected: %+v", report)
	}
}

func TestLoadTokenTightensALooseCache(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	path := TokenPath(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"token":"ghp_loose"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if report := secrets.Probe(path); report.Protected {
		t.Fatalf("fixture is already protected, so the test proves nothing: %+v", report)
	}
	token, err := LoadToken(dir)
	if err != nil || token != "ghp_loose" {
		t.Fatalf("LoadToken = %q, %v", token, err)
	}
	if report := secrets.Probe(path); !report.Protected {
		t.Fatalf("LoadToken left a loose cache unprotected: %+v", report)
	}
}
