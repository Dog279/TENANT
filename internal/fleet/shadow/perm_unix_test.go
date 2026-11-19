//go:build !windows

package shadow

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Everything either side keeps is the owner's alone, whatever the umask.
func TestFilesAreOwnerOnly(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)

	f := newFleet(t, quick)
	f.note(hostA, "web1 runs nginx")
	c := f.mint(hostA)
	_, blob, err := f.Newest(hostA)
	must(t, err)
	if _, err := f.Adopt(t.Context(), hostA, c, blob, nil); err != nil { // sets a snapshot aside
		t.Fatal(err)
	}
	must(t, f.Store.Close())
	child, err := OpenChildStore(filepath.Join(t.TempDir(), "child", "memory"))
	must(t, err)
	must(t, child.Put(c, blob))

	seen := 0
	for _, root := range []string{f.dir, child.dir, filepath.Dir(f.keyPath)} {
		must(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			seen++
			want := fs.FileMode(0o600)
			if info.IsDir() {
				want = 0o700
			}
			if got := info.Mode().Perm(); got != want {
				t.Errorf("%s has mode %04o, want %04o", path, got, want)
			}
			return nil
		}))
	}
	if seen < 9 {
		t.Fatalf("only %d files and directories were looked at", seen)
	}
}
