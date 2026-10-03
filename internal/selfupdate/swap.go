package selfupdate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ErrPrevInUse is Swap's refusal on Windows when the previous version is
// still running: Windows never deletes a mapped image, so tenant.prev.exe
// cannot make way until that process exits.
var ErrPrevInUse = errors.New("the previous version is still running and holds the .prev binary; restart Tenant, then retry")

// Test seams: TestSwapNoGap fails each to prove what every step leaves behind.
var (
	linkFn   = os.Link
	renameFn = os.Rename
)

// PrevPath is where Swap keeps the binary it replaced: exe + ".prev", with a
// Windows ".exe" kept last (tenant.prev.exe) so the name stays runnable.
func PrevPath(exe string) string { return sibling(exe, ".prev") }

// newPath is where Rollback parks the binary it demotes on Windows.
func newPath(exe string) string { return sibling(exe, ".new") }

func sibling(exe, tag string) string {
	if ext := filepath.Ext(exe); strings.EqualFold(ext, ".exe") {
		return strings.TrimSuffix(exe, ext) + tag + ext
	}
	return exe + tag
}

// Swap installs staged (a verified, smoke-tested binary on exe's filesystem)
// as exe, keeps the old binary at PrevPath(exe) for Rollback, and gives the
// new file the old one's mode.
//
// Unix: a hard link puts the old binary under both names, then one rename
// replaces the exe entry atomically, so there is no instant at which exe is
// missing, and a failure before the rename leaves exe untouched. A filesystem
// without hard links falls back to the two renames below.
//
// Windows: a running image may be renamed but not replaced or deleted, so the
// old binary is renamed aside and the new one renamed in. Between those two
// syscalls there is no tenant.exe; an OS stop landing there leaves the
// operator one line, `ren tenant.new.exe tenant.exe` (whatever the staged
// file is called). A failed second rename puts the old binary back itself.
func Swap(exe, staged string) error {
	info, err := os.Stat(exe)
	if err != nil {
		return err
	}
	if err := os.Chmod(staged, info.Mode().Perm()); err != nil {
		return err
	}
	prev := PrevPath(exe)
	if err := os.Remove(prev); err != nil && !errors.Is(err, fs.ErrNotExist) {
		if runtime.GOOS == "windows" {
			return fmt.Errorf("%w (%v)", ErrPrevInUse, err)
		}
		return err
	}
	if runtime.GOOS != "windows" && linkFn(exe, prev) == nil {
		if err := renameFn(staged, exe); err != nil {
			_ = os.Remove(prev) // exe was never touched; a spare link to it would only confuse a rollback
			return err
		}
		return nil
	}
	if err := os.Rename(exe, prev); err != nil {
		return err
	}
	if err := renameFn(staged, exe); err != nil {
		if back := os.Rename(prev, exe); back != nil {
			return fmt.Errorf("%w; and %s could not be put back (%v): rename it to %s by hand", err, prev, back, filepath.Base(exe))
		}
		return err
	}
	return nil
}

// Rollback puts PrevPath(exe) back as exe. On Unix one rename does it with no
// gap. On Windows the running new binary is first renamed to tenant.new.exe
// (it cannot be replaced while mapped), then the old one renamed in: the
// same two-syscall gap as Swap, with the same one-line fix.
func Rollback(exe string) error {
	prev := PrevPath(exe)
	if _, err := os.Stat(prev); err != nil {
		return fmt.Errorf("nothing to roll back to: %w", err)
	}
	if runtime.GOOS != "windows" {
		return os.Rename(prev, exe)
	}
	parked := newPath(exe)
	if err := os.Rename(exe, parked); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(prev, exe); err != nil {
		return fmt.Errorf("%w: %s is parked at %s; rename it back by hand", err, filepath.Base(exe), filepath.Base(parked))
	}
	_ = os.Remove(parked) // fails while the demoted binary still runs; then it is harmless until it exits
	return nil
}
