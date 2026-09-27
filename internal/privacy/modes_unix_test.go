//go:build !windows

package privacy

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// An export holds everything Tenant knows in plaintext, so on POSIX every
// file is 0600 and every directory 0700, and a rewritten archive file keeps
// owner-only access.
func TestExportAndRewritesAreOwnerOnly(t *testing.T) {
	fx := newFixture(t)
	out := filepath.Join(t.TempDir(), "export")
	if _, err := Export(context.Background(), fx.d, ExportOptions{Out: out}); err != nil {
		t.Fatal(err)
	}
	err := filepath.WalkDir(out, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		want := os.FileMode(0o600)
		if e.IsDir() {
			want = 0o700
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s has mode %o, want %o", p, got, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	forget(t, fx, Selector{Episodes: []int64{fx.ep["e2"]}})
	matches, _ := filepath.Glob(filepath.Join(fx.d.DataDir, "archive", "*", "sess_a.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("sess_a archive files: %v", matches)
	}
	info, err := os.Stat(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("rewritten archive file mode %o, want 600", info.Mode().Perm())
	}
}
