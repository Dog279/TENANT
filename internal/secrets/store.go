// Package secrets owns persistence and protection for Tenant's at-rest
// credentials, OAuth caches, peering tokens, and private keys.
package secrets

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := protectPath(tmp, false); err != nil {
		return fmt.Errorf("secrets: protect temporary file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
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
