//go:build !windows

package main

// child_unix.go (TEN-531): what `tenant child install --system` needs from
// Linux and macOS: who is root, which unit files start the child, and the
// two checks that make "runs as root" safe to offer.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
)

// childElevated reports whether this command runs as root. Tests swap it.
var childElevated = func() bool { return os.Geteuid() == 0 }

// childUnitFiles are the unit files that start the fleet child in dir, as
// one account's and as the machine's. A child's file tools never write them.
func childUnitFiles(dir string) []string {
	var out []string
	for _, system := range []bool{false, true} {
		if s, err := childSpecFor(runtime.GOOS, "", dir, system); err == nil {
			out = append(out, s.unitPath())
		}
	}
	return out
}

// sealSystemChild checks that the files of a --system child are its
// account's alone: every one of them owned by the account installing them,
// which is root, and closed to group and world. They are written that way
// (internal/secrets); this is the look before the service first starts.
func sealSystemChild(paths ...string) error {
	me := uint32(os.Geteuid())
	for _, root := range paths {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Uid != me {
				return fmt.Errorf("%s belongs to another account (uid %d)", p, st.Uid)
			}
			if info.Mode().Perm()&0o077 != 0 {
				return fmt.Errorf("%s is open to other accounts (mode %04o)", p, info.Mode().Perm())
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// childExeSafe refuses a binary that an account other than root could
// replace: the service would run whatever was put there, as root. The file
// and every directory above it must be root's and closed to writing by
// group and world.
func childExeSafe(exe string) error {
	for p := exe; ; p = filepath.Dir(p) {
		info, err := os.Stat(p)
		if err != nil {
			return err
		}
		st, _ := info.Sys().(*syscall.Stat_t)
		// A sticky directory (/tmp) lets nobody replace another's file, but
		// nothing that runs as root belongs under one.
		if st == nil || st.Uid != 0 || info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("%s can be changed by an account other than root, and the child runs this binary as root at every boot; "+
				"install it where only root can write (/usr/local/bin) and run it from there", p)
		}
		if p == filepath.Dir(p) {
			return nil
		}
	}
}
