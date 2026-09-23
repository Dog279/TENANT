//go:build unix

package fsperm

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// openNoFollow opens an object for fstat/fchmod without following a symlink
// and without blocking on a FIFO swapped in for a file.
const openNoFollow = unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC

// beforeRepairOpen runs just before RepairTree opens a directory entry. Tests
// use it to swap the entry for a symlink inside the race window.
var beforeRepairOpen func(dir, name string)

func repairPath(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return false, nil // symlinks and special files are never repaired
	}
	fd, err := unix.Open(path, openNoFollow, 0)
	if skippedOpen(err) {
		return false, nil
	}
	if err != nil {
		return false, &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer unix.Close(fd)
	_, changed, err := tightenFD(fd, path)
	return changed, err
}

func repairTree(root string) (int, error) {
	fd, err := unix.Open(root, openNoFollow, 0)
	if skippedOpen(err) {
		return 0, nil
	}
	if err != nil {
		return 0, &os.PathError{Op: "open", Path: root, Err: err}
	}
	return repairFD(fd, root)
}

// repairFD tightens the object behind fd and, for a directory, everything
// below it, opening each entry relative to its parent's descriptor. It owns
// fd and closes it.
func repairFD(fd int, path string) (int, error) {
	defer unix.Close(fd)
	isDir, changed, err := tightenFD(fd, path)
	if err != nil {
		return 0, err
	}
	fixed := 0
	if changed {
		fixed++
	}
	if !isDir {
		return fixed, nil
	}
	names, err := dirNames(fd, path)
	if err != nil {
		return fixed, err
	}
	for _, name := range names {
		child := filepath.Join(path, name)
		// Skip symlinks and special files without opening them; the O_NOFOLLOW
		// open below still refuses one swapped in after this check.
		var st unix.Stat_t
		if err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if skippedOpen(err) {
				continue
			}
			return fixed, &os.PathError{Op: "fstatat", Path: child, Err: err}
		}
		if kind := uint32(st.Mode) & unix.S_IFMT; kind != unix.S_IFDIR && kind != unix.S_IFREG {
			continue
		}
		if beforeRepairOpen != nil {
			beforeRepairOpen(path, name)
		}
		cfd, err := unix.Openat(fd, name, openNoFollow, 0)
		if skippedOpen(err) {
			continue
		}
		if err != nil {
			return fixed, &os.PathError{Op: "openat", Path: child, Err: err}
		}
		n, err := repairFD(cfd, child)
		fixed += n
		if err != nil {
			return fixed, err
		}
	}
	return fixed, nil
}

// tightenFD removes permission bits beyond the private policy from the object
// behind fd, choosing the policy from what fstat says the object is, so a
// directory swapped in for a file still gets the directory policy. Anything
// but a regular file or directory is left alone.
func tightenFD(fd int, path string) (isDir, changed bool, err error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return false, false, &os.PathError{Op: "fstat", Path: path, Err: err}
	}
	mode := uint32(st.Mode)
	var maximum uint32
	switch mode & unix.S_IFMT {
	case unix.S_IFDIR:
		isDir, maximum = true, uint32(PrivateDirMode)
	case unix.S_IFREG:
		maximum = uint32(PrivateFileMode)
	default:
		return false, false, nil
	}
	perm := mode & 0o777
	if perm&^maximum == 0 {
		return isDir, false, nil
	}
	if err := unix.Fchmod(fd, perm&maximum); err != nil {
		return isDir, false, &os.PathError{Op: "fchmod", Path: path, Err: err}
	}
	return isDir, true, nil
}

// dirNames lists the directory behind fd through a duplicate descriptor, so
// fd itself stays usable for openat.
func dirNames(fd int, path string) ([]string, error) {
	dup, err := unix.Dup(fd)
	if err != nil {
		return nil, &os.PathError{Op: "dup", Path: path, Err: err}
	}
	dir := os.NewFile(uintptr(dup), path)
	defer dir.Close()
	return dir.Readdirnames(-1)
}

// skippedOpen reports open errors that mean there is nothing to repair: the
// entry is a symlink (O_NOFOLLOW refuses it with ELOOP, or EMLINK on the
// BSDs), vanished, or is a socket.
func skippedOpen(err error) bool {
	return errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EMLINK) ||
		errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENXIO)
}
