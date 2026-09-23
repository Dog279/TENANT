// Package fsperm owns Tenant's local private-data permission policy.
package fsperm

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	// PrivateDirMode permits access only to the owning user.
	PrivateDirMode fs.FileMode = 0o700
	// PrivateFileMode permits reads and writes only by the owning user.
	PrivateFileMode fs.FileMode = 0o600
)

// Issue describes a path whose permissions exceed Tenant's policy.
type Issue struct {
	Path string
	Mode fs.FileMode
	Want fs.FileMode
}

// Audit reports an overly broad regular file or directory. Missing paths,
// symlinks, and special files are ignored.
func Audit(path string) (*Issue, error) {
	if isVolumeRoot(path) {
		return nil, fmt.Errorf("fsperm: refusing to audit filesystem root %s", path)
	}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
		return nil, nil
	}
	want := PrivateFileMode
	if info.IsDir() {
		want = PrivateDirMode
	}
	if info.Mode().Perm()&^want == 0 {
		return nil, nil
	}
	return &Issue{Path: path, Mode: info.Mode().Perm(), Want: want}, nil
}

// Repair tightens one regular file or directory that exceeds the private
// policy. It never follows a symlink: the path is opened without following
// one, and the object is checked and tightened through that descriptor, so a
// symlink swapped in after an audit is skipped instead of redirecting the
// chmod to its target (TEN-333).
func Repair(path string) (bool, error) {
	if isVolumeRoot(path) {
		return false, fmt.Errorf("fsperm: refusing to repair filesystem root %s", path)
	}
	return repairPath(path)
}

// MkdirAll creates a private directory tree and tightens the final directory
// when it already exists. Existing parent directories are left unchanged.
func MkdirAll(path string) error {
	if err := os.MkdirAll(path, PrivateDirMode); err != nil {
		return err
	}
	if isVolumeRoot(path) {
		return nil
	}
	return restrict(path, PrivateDirMode)
}

// WriteFile writes private data and tightens an existing destination as well.
func WriteFile(path string, data []byte) error {
	f, err := OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// OpenFile opens a private file and tightens it when it already exists.
func OpenFile(path string, flag int) (*os.File, error) {
	f, err := os.OpenFile(path, flag, PrivateFileMode)
	if err != nil {
		return nil, err
	}
	if err := restrict(path, PrivateFileMode); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// RestrictFile removes permissions that exceed the private-file policy.
func RestrictFile(path string) error { return restrict(path, PrivateFileMode) }

// RestrictDir removes permissions that exceed the private-directory policy.
func RestrictDir(path string) error { return restrict(path, PrivateDirMode) }

// PrepareFile creates or tightens a private file before another library opens
// it. This is used for SQLite, whose driver otherwise chooses the create mode.
func PrepareFile(path string) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." && !isVolumeRoot(dir) {
		if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
			if err := MkdirAll(dir); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	f, err := OpenFile(path, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return err
	}
	return f.Close()
}

// AuditTree reports overly broad permissions below root. Symlinks are skipped
// so an audit of a Tenant root never follows a link into operator-owned data.
func AuditTree(root string) ([]Issue, error) {
	if isVolumeRoot(root) {
		return nil, fmt.Errorf("fsperm: refusing to audit filesystem root %s", root)
	}
	var issues []Issue
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		issue, err := Audit(path)
		if err != nil {
			return err
		}
		if issue != nil {
			issues = append(issues, *issue)
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return issues, err
}

func isVolumeRoot(path string) bool {
	clean := filepath.Clean(path)
	volume := filepath.VolumeName(clean)
	return clean == volume+string(filepath.Separator)
}

// RepairTree removes permissions that exceed the private policy below root,
// without granting any missing owner permission. It walks through directory
// descriptors, opening each entry relative to its parent without following
// symlinks, so no path is re-resolved between the check and the chmod and a
// symlink swapped in mid-walk is skipped (TEN-333).
func RepairTree(root string) (int, error) {
	if isVolumeRoot(root) {
		return 0, fmt.Errorf("fsperm: refusing to repair filesystem root %s", root)
	}
	return repairTree(root)
}
