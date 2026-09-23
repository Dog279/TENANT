package x

import (
	"os"
	"path/filepath"
	"testing"

	"tenant/internal/secrets"
)

// TEN-337: the X OAuth cache goes through the secrets package, so it is
// owner-only on POSIX and has a current-user-only DACL on Windows
// (secrets.Probe checks the platform's mechanism), and reading it tightens a
// cache an older build left loose.

func TestTokenStoreSaveIsProtected(t *testing.T) {
	path := TokenPath(t.TempDir())
	st := &tokenStore{ClientID: "cid", Refresh: "refresh-token"}
	if err := st.save(path); err != nil {
		t.Fatal(err)
	}
	if report := secrets.Probe(path); !report.Protected {
		t.Fatalf("saved X cache is not protected: %+v", report)
	}
}

func TestLoadStoreTightensALooseCache(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	path := TokenPath(dir)
	writeLooseFile(t, path, `{"client_id":"cid","refresh_token":"r"}`)
	if report := secrets.Probe(path); report.Protected {
		t.Fatalf("fixture is already protected, so the test proves nothing: %+v", report)
	}
	st, err := loadStore(path)
	if err != nil || st.Refresh != "r" {
		t.Fatalf("loadStore = %+v, %v", st, err)
	}
	if report := secrets.Probe(path); !report.Protected {
		t.Fatalf("loadStore left a loose X cache unprotected: %+v", report)
	}
}

// writeLooseFile creates path the way an older build left it: a 0755 parent
// and a 0644 file on POSIX, the inherited DACL on Windows.
func writeLooseFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
}
