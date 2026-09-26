package backup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"tenant/internal/datalock"
)

// dirSwap puts a staged directory in place of a live one and keeps the live
// one beside it.
type dirSwap struct {
	target string // the live directory (may not exist yet)
	staged string // the restored copy, fully written and verified
	aside  string // where the live directory is kept
	// set as the swap runs, for rollback and reporting
	movedAside   bool
	removedEmpty bool // the target was an empty directory and was removed
	movedIn      bool
}

// swapDirs performs every swap in order. If one fails, the swaps already done
// are undone in reverse, so the live directories end up as they started. The
// error says where anything is if undoing also failed.
func swapDirs(swaps []*dirSwap) error {
	for i, s := range swaps {
		if err := s.do(); err != nil {
			for j := i - 1; j >= 0; j-- {
				if uerr := swaps[j].undo(); uerr != nil {
					err = errors.Join(err, uerr)
				}
			}
			return err
		}
	}
	return nil
}

func (s *dirSwap) do() error {
	if _, err := os.Lstat(s.target); err == nil {
		// An empty directory (a fresh install, or one a command created on
		// its way to the restore, holding at most the lock file) is not
		// worth keeping aside.
		if removeIfEmpty(s.target) {
			s.removedEmpty = true
		} else {
			if err := os.Rename(s.target, s.aside); err != nil {
				return fmt.Errorf("backup: move %s aside: %w", s.target, err)
			}
			s.movedAside = true
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("backup: %w", err)
	}
	if err := os.Rename(s.staged, s.target); err != nil {
		err = fmt.Errorf("backup: move the restored copy into %s: %w", s.target, err)
		if uerr := s.putBack(); uerr != nil {
			return fmt.Errorf("%w; %v", err, uerr)
		}
		return err
	}
	s.movedIn = true
	return nil
}

// putBack returns the original target to its place.
func (s *dirSwap) putBack() error {
	if s.movedAside {
		if err := os.Rename(s.aside, s.target); err != nil {
			return fmt.Errorf("putting the original back failed (%v): it is at %s", err, s.aside)
		}
		s.movedAside = false
	}
	if s.removedEmpty {
		if err := os.Mkdir(s.target, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("recreating the empty %s failed: %v", s.target, err)
		}
		s.removedEmpty = false
	}
	return nil
}

func (s *dirSwap) undo() error {
	if s.movedIn {
		if err := os.Rename(s.target, s.staged); err != nil {
			return fmt.Errorf("backup: undo %s: %w (the restored copy is still in place; the original is at %s)", s.target, err, s.aside)
		}
		s.movedIn = false
	}
	if err := s.putBack(); err != nil {
		return fmt.Errorf("backup: undo %s: %v", s.target, err)
	}
	return nil
}

// removeIfEmpty removes dir when it holds nothing but, at most, the data-dir
// lock file, and reports whether it did.
func removeIfEmpty(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) > 1 {
		return false
	}
	if len(entries) == 1 {
		if entries[0].Name() != datalock.FileName || !entries[0].Type().IsRegular() {
			return false
		}
		if err := os.Remove(filepath.Join(dir, datalock.FileName)); err != nil {
			return false
		}
	}
	return os.Remove(dir) == nil
}
