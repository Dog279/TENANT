package userprofile

import (
	"os"
	"path/filepath"
	"testing"
)

// Save replaces the profile in one step. It used to remove the old file
// first on Windows and write through a fixed "<name>.tmp", so a crash
// between the two left no profile at all, and nothing was flushed before the
// rename (the power-cut finding of 2026-11-17, fsperm.Replace).
func TestSaveReplacesAnExistingProfile(t *testing.T) {
	dir := t.TempDir()
	for _, body := range []string{"## Identity\n- first", "## Identity\n- second"} {
		p := &Profile{AgentID: "main", Markdown: body}
		if err := p.Save(dir); err != nil {
			t.Fatalf("Save: %v", err)
		}
		b, err := os.ReadFile(Path(dir, "main"))
		if err != nil {
			t.Fatal(err)
		}
		if want := body + "\n"; string(b) != want {
			t.Errorf("the profile on disk is %q, want %q", b, want)
		}
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(m) != 0 {
		t.Errorf("temporary files left behind: %v", m)
	}
}
