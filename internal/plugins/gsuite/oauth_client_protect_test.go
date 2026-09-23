package gsuite

import (
	"os"
	"path/filepath"
	"testing"

	"tenant/internal/secrets"
)

// TEN-337: the runtime OAuth client file is read through the secrets package,
// which tightens a copy an older build left loose.
func TestLoadEmbeddedOAuthTightensALooseClientFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	path := EmbeddedOAuthPath(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(validOAuthClientJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if report := secrets.Probe(path); report.Protected {
		t.Fatalf("fixture is already protected, so the test proves nothing: %+v", report)
	}
	b, err := LoadEmbeddedOAuth(dir)
	if err != nil || string(b) != validOAuthClientJSON {
		t.Fatalf("LoadEmbeddedOAuth = %q, %v", b, err)
	}
	if report := secrets.Probe(path); !report.Protected {
		t.Fatalf("LoadEmbeddedOAuth left a loose client file unprotected: %+v", report)
	}
}
