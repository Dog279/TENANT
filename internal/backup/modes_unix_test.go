//go:build !windows

package backup

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// On POSIX the backup file and everything a restore writes are owner-only,
// whatever the umask or the source modes were.
func TestBackupAndRestoreAreOwnerOnly(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t, false)
	// Loose modes at the source must not leak into the backup or the restore.
	if err := os.Chmod(filepath.Join(tr.data, "research", "r1", "report.md"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(tr.data, "research"), 0o755); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(tr.root, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := Create(ctx, filepath.Join(outDir, "b.tar.gz.age"), tr.opts(Keys{Passphrase: "pw", scryptWorkFactor: 10}))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("backup file mode %o, want 600", info.Mode().Perm())
	}
	if d, _ := os.Stat(outDir); d.Mode().Perm() != 0o755 {
		t.Fatalf("the chosen output folder's mode changed to %o", d.Mode().Perm())
	}
	if _, err := Restore(ctx, res.Path, tr.restoreOpts(Keys{Passphrase: "pw"})); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{tr.cfg, tr.data} {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			want := os.FileMode(0o600)
			if d.IsDir() {
				want = 0o700
			}
			if d.Name() == ".tenant.lock" {
				return nil
			}
			if got := info.Mode().Perm(); got != want {
				t.Errorf("%s restored with mode %o, want %o", p, got, want)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
