package backup

import (
	"os"
	"path/filepath"
	"testing"
)

func mkdirWith(t *testing.T, dir, file, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readBody(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSwapDirsKeepsTheOriginalAside(t *testing.T) {
	root := t.TempDir()
	live, staged, aside := filepath.Join(root, "data"), filepath.Join(root, "stage"), filepath.Join(root, "data.pre-restore")
	mkdirWith(t, live, "f", "old")
	mkdirWith(t, staged, "f", "new")
	if err := swapDirs([]*dirSwap{{target: live, staged: staged, aside: aside}}); err != nil {
		t.Fatal(err)
	}
	if got := readBody(t, filepath.Join(live, "f")); got != "new" {
		t.Fatalf("live = %q, want new", got)
	}
	if got := readBody(t, filepath.Join(aside, "f")); got != "old" {
		t.Fatalf("aside = %q, want old", got)
	}
}

func TestSwapDirsIntoAMissingTarget(t *testing.T) {
	root := t.TempDir()
	live, staged := filepath.Join(root, "data"), filepath.Join(root, "stage")
	mkdirWith(t, staged, "f", "new")
	s := &dirSwap{target: live, staged: staged, aside: filepath.Join(root, "aside")}
	if err := swapDirs([]*dirSwap{s}); err != nil {
		t.Fatal(err)
	}
	if s.movedAside {
		t.Fatal("reported moving aside a directory that did not exist")
	}
	if got := readBody(t, filepath.Join(live, "f")); got != "new" {
		t.Fatalf("live = %q", got)
	}
}

func TestSwapDirsRollsBackEarlierSwaps(t *testing.T) {
	root := t.TempDir()
	cfg, cfgStage := filepath.Join(root, "cfg"), filepath.Join(root, "cfg.stage")
	data := filepath.Join(root, "data")
	mkdirWith(t, cfg, "f", "old-cfg")
	mkdirWith(t, cfgStage, "f", "new-cfg")
	mkdirWith(t, data, "f", "old-data")
	// The data swap fails: its staged copy does not exist.
	swaps := []*dirSwap{
		{target: cfg, staged: cfgStage, aside: filepath.Join(root, "cfg.aside")},
		{target: data, staged: filepath.Join(root, "missing"), aside: filepath.Join(root, "data.aside")},
	}
	if err := swapDirs(swaps); err == nil {
		t.Fatal("swap with a missing staged copy succeeded")
	}
	if got := readBody(t, filepath.Join(cfg, "f")); got != "old-cfg" {
		t.Fatalf("config not rolled back: %q", got)
	}
	if got := readBody(t, filepath.Join(data, "f")); got != "old-data" {
		t.Fatalf("data not put back: %q", got)
	}
	if got := readBody(t, filepath.Join(cfgStage, "f")); got != "new-cfg" {
		t.Fatalf("restored config copy lost on rollback: %q", got)
	}
}

func TestSwapDirsRemovesAnEmptyTargetInsteadOfKeepingIt(t *testing.T) {
	root := t.TempDir()
	live, staged := filepath.Join(root, "data"), filepath.Join(root, "stage")
	// Only the lock file, as a restore's own lock leaves in a fresh dir.
	mkdirWith(t, live, ".tenant.lock", "")
	mkdirWith(t, staged, "f", "new")
	s := &dirSwap{target: live, staged: staged, aside: filepath.Join(root, "aside")}
	if err := swapDirs([]*dirSwap{s}); err != nil {
		t.Fatal(err)
	}
	if s.movedAside {
		t.Fatal("an empty target was kept aside")
	}
	if _, err := os.Stat(filepath.Join(root, "aside")); !os.IsNotExist(err) {
		t.Fatal("aside dir created for an empty target")
	}
	if got := readBody(t, filepath.Join(live, "f")); got != "new" {
		t.Fatalf("live = %q", got)
	}
}
