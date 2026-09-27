// Package secrets owns persistence and protection for Tenant's at-rest
// credentials, OAuth caches, peering tokens, and private keys.
package secrets

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
)

const (
	fileMode = 0o600
	dirMode  = 0o700
)

// Protection describes the effective on-disk protection for a secret file.
type Protection struct {
	Path      string
	Exists    bool
	Protected bool
	Mechanism string
	Detail    string
}

// ReadFile hardens an existing secret file and its parent directory before
// reading it. Missing files retain os.IsNotExist semantics.
func ReadFile(path string) ([]byte, error) {
	if path == "" {
		return nil, &os.PathError{Op: "read", Path: path, Err: os.ErrInvalid}
	}
	if _, err := os.Lstat(path); err != nil {
		return nil, err
	}
	if err := Harden(path); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// Peek reads a secret file as it is, for read-only audits such as plain
// `tenant doctor` (TEN-333). Like ReadFile it refuses a symlink or a
// non-regular file, but it never changes the file's or directory's protection.
func Peek(path string) ([]byte, error) {
	if path == "" {
		return nil, &os.PathError{Op: "read", Path: path, Err: os.ErrInvalid}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("secrets: refusing symlink %s", path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("secrets: %s is not a regular file", path)
	}
	return os.ReadFile(path)
}

// onWrite is the hook OnWrite registers.
var onWrite atomic.Pointer[func(path string)]

// OnWrite registers fn to run after each secret file this process writes or
// creates here, so a copy of the values kept elsewhere (the log scrubber's)
// is refreshed the moment a credential or OAuth token changes. It runs on the
// writer's goroutine; a later call replaces it, and nil removes it.
func OnWrite(fn func(path string)) {
	if fn == nil {
		onWrite.Store(nil)
		return
	}
	onWrite.Store(&fn)
}

func wrote(path string) {
	if fn := onWrite.Load(); fn != nil {
		(*fn)(path)
	}
}

// WriteFile atomically replaces path and verifies owner-only protection on the
// file and its direct parent directory before returning.
func WriteFile(path string, data []byte) error {
	if path == "" {
		return &os.PathError{Op: "write", Path: path, Err: os.ErrInvalid}
	}
	dir, base := filepath.Dir(path), filepath.Base(path)
	if err := ensurePrivateDir(dir); err != nil {
		return fmt.Errorf("secrets: protect parent %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, base+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := writeSyncClose(f, data); err != nil {
		return err
	}
	if err := protectPath(tmp, false); err != nil {
		return fmt.Errorf("secrets: protect temporary file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	defer wrote(path)
	if err := protectPath(path, false); err != nil {
		return fmt.Errorf("secrets: protect %s: %w", path, err)
	}
	report := Probe(path)
	if !report.Protected {
		if report.Detail == "" {
			report.Detail = "effective protection could not be verified"
		}
		return fmt.Errorf("secrets: %s: %s", path, report.Detail)
	}
	return nil
}

// linkFile publishes a finished temporary file under its final name. Tests
// replace it to exercise the no-hard-link fallback.
var linkFile = os.Link

// CreateExclusive writes data to path only when path does not exist yet, with
// the owner-only protection WriteFile verifies. It returns created=false, and
// leaves the existing file untouched, when another writer got there first, so
// racing processes converge on the first value instead of the last. The data
// is written and protected under a temporary name, then published with a hard
// link, which fails when path exists; a reader never sees a partial file. On a
// filesystem without hard links it claims path with O_EXCL and then writes.
func CreateExclusive(path string, data []byte) (created bool, err error) {
	if path == "" {
		return false, &os.PathError{Op: "create", Path: path, Err: os.ErrInvalid}
	}
	dir, base := filepath.Dir(path), filepath.Base(path)
	if err := ensurePrivateDir(dir); err != nil {
		return false, fmt.Errorf("secrets: protect parent %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, base+".*.tmp")
	if err != nil {
		return false, err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := writeSyncClose(f, data); err != nil {
		return false, err
	}
	if err := protectPath(tmp, false); err != nil {
		return false, fmt.Errorf("secrets: protect temporary file: %w", err)
	}
	if err := linkFile(tmp, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		if created, err := createExclusiveInPlace(path, data); !created || err != nil {
			return created, err
		}
	}
	defer wrote(path)
	report := Probe(path)
	if !report.Protected {
		if report.Detail == "" {
			report.Detail = "effective protection could not be verified"
		}
		return true, fmt.Errorf("secrets: %s: %s", path, report.Detail)
	}
	return true, nil
}

// createExclusiveInPlace is CreateExclusive's fallback for filesystems without
// hard links. The name is claimed empty and protected before data is written,
// so a concurrent reader can briefly see an empty file.
func createExclusiveInPlace(path string, data []byte) (bool, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, err
	}
	if err := protectPath(path, false); err != nil {
		_ = f.Close()
		return true, fmt.Errorf("secrets: protect %s: %w", path, err)
	}
	return true, writeSyncClose(f, data)
}

func writeSyncClose(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Harden applies and verifies owner-only protection to an existing secret file
// and its direct parent directory.
func Harden(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("secrets: refusing symlink %s", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("secrets: %s is not a regular file", path)
	}
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	if err := protectPath(path, false); err != nil {
		return err
	}
	report := Probe(path)
	if !report.Protected {
		if report.Detail == "" {
			report.Detail = "effective protection could not be verified"
		}
		return errors.New(report.Detail)
	}
	return nil
}

// ProtectFile applies and verifies owner-only protection on one existing
// regular file and leaves its parent directory alone. It is for private files
// Tenant writes outside its own directories, such as a backup archive in a
// folder the operator chose (TEN-305), where re-protecting the parent would
// change access to everything else in it.
func ProtectFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("secrets: refusing symlink %s", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("secrets: %s is not a regular file", path)
	}
	if err := protectPath(path, false); err != nil {
		return err
	}
	ok, _, detail, err := probePath(path, false)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("secrets: %s: %s", path, detail)
	}
	return nil
}

// Probe reports the effective protection of path without changing it. A
// missing file is not an error and is reported with Exists=false.
func Probe(path string) Protection {
	report := Protection{Path: path}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		report.Detail = "not present"
		return report
	}
	if err != nil {
		report.Detail = err.Error()
		return report
	}
	report.Exists = true
	if info.Mode()&os.ModeSymlink != 0 {
		report.Detail = "secret path is a symlink"
		return report
	}
	if !info.Mode().IsRegular() {
		report.Detail = "secret path is not a regular file"
		return report
	}
	fileOK, mechanism, fileDetail, err := probePath(path, false)
	report.Mechanism = mechanism
	if err != nil {
		report.Detail = err.Error()
		return report
	}
	dir := filepath.Dir(path)
	dirOK, dirMechanism, dirDetail, err := probePath(dir, true)
	if report.Mechanism == "" {
		report.Mechanism = dirMechanism
	}
	if err != nil {
		report.Detail = err.Error()
		return report
	}
	report.Protected = fileOK && dirOK
	if !fileOK {
		report.Detail = fileDetail
	} else if !dirOK {
		report.Detail = dirDetail
	} else {
		report.Detail = fileDetail + "; " + dirDetail
	}
	return report
}

func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, dirMode); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("secrets: parent %s is not a real directory", path)
	}
	// Skip re-protecting a directory that already probes private: on Windows,
	// setting the DACL re-propagates it to every child, and the data dir can
	// hold thousands of archive files (TEN-337).
	if ok, _, _, err := probePath(path, true); err == nil && ok {
		return nil
	}
	if err := protectPath(path, true); err != nil {
		return err
	}
	ok, _, detail, err := probePath(path, true)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New(detail)
	}
	return nil
}
