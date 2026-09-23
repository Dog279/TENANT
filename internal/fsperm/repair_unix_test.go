//go:build unix

package fsperm

import (
	"os"
	"path/filepath"
	"testing"
)

// TEN-333 swap race: an attacker who can write a legacy group/world-writable
// directory replaces an audited entry with a symlink before the repair
// reaches it. The repair must skip the symlink, never chmod its target.

func TestRepairTreeSkipsEntriesSwappedForSymlinksMidWalk(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	mustMkdir(t, filepath.Join(root, "sub"), 0o777)
	mustChmod(t, root, 0o777)
	mustWrite(t, filepath.Join(root, "report.md"), 0o666)
	mustWrite(t, filepath.Join(root, "sub", "notes.md"), 0o666)

	outside := t.TempDir()
	targetFile := filepath.Join(outside, "victim.txt")
	mustWrite(t, targetFile, 0o666)
	targetDir := filepath.Join(outside, "victim-dir")
	mustMkdir(t, targetDir, 0o777)

	swaps := map[string]string{"report.md": targetFile, "sub": targetDir}
	beforeRepairOpen = func(dir, name string) {
		target, ok := swaps[name]
		if !ok || dir != root {
			return
		}
		entry := filepath.Join(dir, name)
		if err := os.RemoveAll(entry); err != nil {
			t.Errorf("swap %s: %v", entry, err)
		}
		if err := os.Symlink(target, entry); err != nil {
			t.Errorf("swap %s: %v", entry, err)
		}
	}
	t.Cleanup(func() { beforeRepairOpen = nil })

	fixed, err := RepairTree(root)
	// The symlink targets are the property under test, so check them first.
	assertMode(t, targetFile, 0o666)
	assertMode(t, targetDir, 0o777)
	if err != nil {
		t.Fatal(err)
	}
	if fixed != 1 {
		t.Errorf("fixed = %d, want 1 (only the root; both entries were swapped away)", fixed)
	}
	assertAtMost(t, root, PrivateDirMode)
}

func TestRepairSkipsPathSwappedForSymlinkAfterAudit(t *testing.T) {
	dir := t.TempDir()
	mustChmod(t, dir, 0o777)
	path := filepath.Join(dir, "episodes.db")
	mustWrite(t, path, 0o666)
	victim := filepath.Join(t.TempDir(), "victim-dir")
	mustMkdir(t, victim, 0o755)

	issue, err := Audit(path)
	if err != nil || issue == nil {
		t.Fatalf("Audit = %+v, %v; want an issue for the loose file", issue, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, path); err != nil {
		t.Fatal(err)
	}

	changed, err := Repair(path)
	if err != nil || changed {
		t.Fatalf("Repair = %v, %v; want the swapped-in symlink skipped", changed, err)
	}
	// The old path-based repair would have applied the file policy to the
	// directory, leaving it 0600 and unusable.
	assertMode(t, victim, 0o755)
}

func TestRepairTreeAppliesThePolicyOfWhatItOpened(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	mustMkdir(t, filepath.Join(root, "nested"), 0o775)
	mustChmod(t, root, 0o775)
	mustWrite(t, filepath.Join(root, "nested", "log.jsonl"), 0o664)

	fixed, err := RepairTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if fixed != 3 {
		t.Errorf("fixed = %d, want 3", fixed)
	}
	// Owner bits survive: directories keep search permission, files are not
	// made executable.
	assertMode(t, root, 0o700)
	assertMode(t, filepath.Join(root, "nested"), 0o700)
	assertMode(t, filepath.Join(root, "nested", "log.jsonl"), 0o600)
}

func mustMkdir(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	mustChmod(t, path, mode)
}

func mustWrite(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustChmod(t, path, mode)
}

func mustChmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %04o, want %04o", path, got, want)
	}
}
