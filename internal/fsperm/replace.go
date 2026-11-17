package fsperm

import (
	"os"
	"path/filepath"
	"runtime"
)

// Replace puts data at path as a private file without ever leaving a torn or
// empty one there: the bytes go to a temporary file in the same directory,
// are flushed to the disk, and only then is the file renamed over path.
//
// The flush is the point. A rename is recorded by the file system at once;
// the bytes of a file that was just written sit in the write cache for some
// seconds. Without the flush a power cut in that window leaves the new name
// on a file whose contents never reached the disk: on Windows Server 2025
// (2026-11-17) config.json came back as a run of zero bytes and Tenant would
// not start. A flush that fails leaves the old file in place.
//
// The temporary file has a unique name, so two writers of one path never
// share it; the rename is atomic, so a reader sees the whole old file or the
// whole new one.
func Replace(path string, data []byte) error {
	return replace(path, data, PrivateFileMode, true)
}

// ReplaceWithMode is Replace for a file that is not private: it gets mode
// (as far as this OS has modes) and is not tightened.
func ReplaceWithMode(path string, data []byte, mode os.FileMode) error {
	return replace(path, data, mode, false)
}

func replace(path string, data []byte, mode os.FileMode, private bool) error {
	dir, base := filepath.Dir(path), filepath.Base(path)
	f, err := os.CreateTemp(dir, base+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// Best-effort cleanup if we bail before the rename.
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := Flush(f); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if private {
		if err := RestrictFile(tmp); err != nil {
			return err
		}
	} else if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	FlushDir(dir)
	return nil
}

// Flush forces a file's contents to the disk: the step a writer that streams
// into a temporary file takes before it closes and renames it. Tests swap
// syncFile to see when it runs and to make it fail.
func Flush(f *os.File) error { return syncFile(f) }

var syncFile = func(f *os.File) error { return f.Sync() }

// FlushDir flushes a directory, which is what makes a rename in it survive a
// power cut on Linux and macOS. Best effort: the file is whole either way,
// and Windows, whose file system journals the rename itself, does not allow
// a directory to be flushed.
func FlushDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}
