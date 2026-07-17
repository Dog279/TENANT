package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"tenant/internal/model"
	"tenant/internal/plugins/wiki"
)

type fakeWikiEmbedder struct{}

func (fakeWikiEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0, 0, 0}
	}
	return out, nil
}

func newTestWiki(t *testing.T) (*wiki.Index, string) {
	t.Helper()
	root := t.TempDir()
	side := filepath.Join(t.TempDir(), "idx.json")
	ix, err := wiki.New(root, side, "fake/4", fakeWikiEmbedder{})
	if err != nil {
		t.Fatalf("wiki.New: %v", err)
	}
	return ix, root
}

func saveCall(t *testing.T, d *wikiSaveDispatcher, file, content string, overwrite bool) (string, bool, error) {
	t.Helper()
	args, _ := json.Marshal(map[string]any{"file": file, "content": content, "overwrite": overwrite})
	return d.Dispatch(context.Background(), model.ToolCall{Name: "wiki_save", Arguments: args})
}

func TestWikiSave_WriteReindexAndGuards(t *testing.T) {
	ix, root := newTestWiki(t)
	var confirmed []string
	confirm := func(_ context.Context, action, detail string) bool {
		confirmed = append(confirmed, action+":"+detail)
		return true
	}
	d := newWikiSaveDispatcher(root, ix, confirm)

	// 1. Save → written, gated, reindexed, searchable.
	out, isErr, err := saveCall(t, d, "milk.md", "# Milk\nnutrient notes", false)
	if err != nil || isErr {
		t.Fatalf("save failed: %q isErr=%v err=%v", out, isErr, err)
	}
	if b, e := os.ReadFile(filepath.Join(root, "milk.md")); e != nil || !strings.Contains(string(b), "nutrient notes") {
		t.Errorf("note not written: %v", e)
	}
	indexed := false
	for _, f := range ix.List() {
		if f == "milk.md" {
			indexed = true
		}
	}
	if !indexed {
		t.Error("saved note should be reindexed (in List)")
	}
	if len(confirmed) != 1 {
		t.Errorf("save must be gated through confirm exactly once, got %d", len(confirmed))
	}

	// 2. Non-clobbering by default.
	out, isErr, _ = saveCall(t, d, "milk.md", "new body", false)
	if !isErr || !strings.Contains(out, "already exists") {
		t.Errorf("existing note should be refused without overwrite: %q", out)
	}

	// 3. overwrite=true replaces.
	if _, isErr, _ := saveCall(t, d, "milk.md", "replaced body", true); isErr {
		t.Error("overwrite=true should succeed")
	}
	if b, _ := os.ReadFile(filepath.Join(root, "milk.md")); !strings.Contains(string(b), "replaced body") {
		t.Error("overwrite did not replace content")
	}

	// 4. Missing extension defaults to .md.
	if _, isErr, _ := saveCall(t, d, "soy", "soy notes", false); isErr {
		t.Error("save 'soy' should succeed")
	}
	if _, e := os.Stat(filepath.Join(root, "soy.md")); e != nil {
		t.Error("missing extension should default to soy.md")
	}

	// 5. Traversal refused AND nothing written outside the root.
	out, isErr, _ = saveCall(t, d, "../escape.md", "x", false)
	if !isErr || !strings.Contains(out, "outside the wiki") {
		t.Errorf("traversal must be refused: %q", out)
	}
	if _, e := os.Stat(filepath.Join(filepath.Dir(root), "escape.md")); e == nil {
		t.Error("traversal WROTE outside the wiki root")
	}

	// 6. Empty content refused.
	if out, isErr, _ := saveCall(t, d, "empty.md", "   ", false); !isErr || !strings.Contains(out, "content is required") {
		t.Errorf("empty content should be refused: %q", out)
	}
}

func TestWikiSave_RefusesFinalComponentSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require admin on Windows")
	}
	ix, root := newTestWiki(t)
	d := newWikiSaveDispatcher(root, ix, nil)

	// A note path INSIDE the root that is itself a symlink pointing OUTSIDE —
	// writing through it must be refused (target may not even exist yet).
	outside := t.TempDir()
	target := filepath.Join(outside, "pwned.md")
	if err := os.Symlink(target, filepath.Join(root, "evil.md")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	out, isErr, _ := saveCall(t, d, "evil.md", "owned", true) // even with overwrite=true
	if !isErr || !strings.Contains(out, "symlink") {
		t.Errorf("writing through a symlink must be refused: %q", out)
	}
	if _, e := os.Stat(target); e == nil {
		t.Error("write escaped the wiki root through a final-component symlink")
	}
}

// mkJunction creates a Windows directory junction (mklink /J needs no admin
// rights, unlike symlinks). Skips the test if junction creation is unavailable.
func mkJunction(t *testing.T, link, target string) {
	t.Helper()
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("cannot create junction: %v (%s)", err, strings.TrimSpace(string(out)))
	}
}

// Regression: a wiki ROOT reached through a directory junction (sandboxed
// %TMP%, MSIX overlay, mapped workspace) must still accept saves. The old
// EvalSymlinks-based containment check errored out for any path traversing a
// junction, so every save failed as "target directory unresolved". Links
// ABOVE the root are the operator's chosen location, not an escape.
func TestWikiSave_RootBehindJunctionStillWrites(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("directory junctions are Windows-specific")
	}
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(real, "wiki"), 0o755); err != nil {
		t.Fatal(err)
	}
	mkJunction(t, filepath.Join(base, "j"), real)
	root := filepath.Join(base, "j", "wiki") // traverses the junction

	d := newWikiSaveDispatcher(root, nil, nil)
	out, isErr, err := saveCall(t, d, "sub/note.md", "body", false)
	if err != nil || isErr {
		t.Fatalf("save under a junction-reached root must succeed: %q isErr=%v err=%v", out, isErr, err)
	}
	if _, e := os.Stat(filepath.Join(real, "wiki", "sub", "note.md")); e != nil {
		t.Errorf("note not written through the junction-reached root: %v", e)
	}
}

// A junction INSIDE the root pointing outside is the Windows analogue of the
// POSIX symlinked-subdir escape — it must be refused and nothing written.
func TestWikiSave_RefusesJunctionSubdirEscape(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("directory junctions are Windows-specific")
	}
	ix, root := newTestWiki(t)
	outside := t.TempDir()
	mkJunction(t, filepath.Join(root, "evil"), outside)

	d := newWikiSaveDispatcher(root, ix, nil)
	out, isErr, _ := saveCall(t, d, "evil/pwn.md", "owned", true)
	if !isErr || !strings.Contains(out, "wiki root") {
		t.Errorf("write through an in-root junction must be refused: %q", out)
	}
	if _, e := os.Stat(filepath.Join(outside, "pwn.md")); e == nil {
		t.Error("write escaped the wiki root through an in-root junction")
	}
}

func TestWikiSave_DenialBlocksWrite(t *testing.T) {
	ix, root := newTestWiki(t)
	d := newWikiSaveDispatcher(root, ix, func(context.Context, string, string) bool { return false }) // deny

	out, isErr, _ := saveCall(t, d, "secret.md", "x", false)
	if !isErr || !strings.Contains(out, "denied") {
		t.Errorf("a denied save should report denial: %q", out)
	}
	if _, e := os.Stat(filepath.Join(root, "secret.md")); e == nil {
		t.Error("a denied save MUST NOT write the file")
	}
}

func TestWikiSave_HeadlessNoBrokerStillWrites(t *testing.T) {
	ix, root := newTestWiki(t)
	d := newWikiSaveDispatcher(root, ix, nil) // headless: no broker

	if _, isErr, err := saveCall(t, d, "note.md", "body", false); err != nil || isErr {
		t.Fatalf("headless save should proceed (operator-launched): isErr=%v err=%v", isErr, err)
	}
	if _, e := os.Stat(filepath.Join(root, "note.md")); e != nil {
		t.Error("headless save should write the note")
	}
}
