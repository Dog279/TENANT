package backup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"tenant/internal/fsperm"
)

// extract writes every entry of r under its root's staging directory, in
// private directories and owner-only files. Each file is created exclusively,
// so nothing already in a staging directory is overwritten; staging
// directories should start empty. The stream's own checks (size, checksum,
// no extra or missing entries) run as it goes, so on any error the staging
// directories hold a partial copy and must be discarded.
func extract(r *Reader, roots map[string]string) error {
	return r.Each(func(e Entry, content io.Reader) error {
		root, rel, _ := strings.Cut(e.Path, "/")
		base, ok := roots[root]
		if !ok || base == "" {
			return fmt.Errorf("backup: no restore target for the %s root", root)
		}
		dst := filepath.Join(base, filepath.FromSlash(rel))
		// validEntryPath already rules out traversal; this is the belt to
		// that brace, checked on the joined OS path.
		if relOut, err := filepath.Rel(base, dst); err != nil || !filepath.IsLocal(relOut) {
			return fmt.Errorf("backup: %s resolves outside %s", e.Path, base)
		}
		if err := fsperm.MkdirAll(filepath.Dir(dst)); err != nil {
			return fmt.Errorf("backup: create %s: %w", filepath.Dir(dst), err)
		}
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fsperm.PrivateFileMode)
		if err != nil {
			return fmt.Errorf("backup: restore %s: %w", e.Path, err)
		}
		if _, err := io.Copy(f, content); err != nil {
			_ = f.Close()
			return fmt.Errorf("backup: restore %s: %w", e.Path, err)
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return fmt.Errorf("backup: restore %s: %w", e.Path, err)
		}
		return f.Close()
	})
}

// errNotEmpty is returned when a staging directory already has content.
var errNotEmpty = errors.New("backup: staging directory is not empty")

// emptyDir creates dir privately, or accepts it when it exists and is empty.
func emptyDir(dir string) error {
	if err := fsperm.MkdirAll(dir); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("%w: %s", errNotEmpty, dir)
	}
	return nil
}
