package main

// wikisave.go (TEN-243 ingest): the GATED LOCAL-WRITE counterpart to the
// read-only wiki plugin. Peers share their wiki transiently (peer_wiki_read is a
// read-on-demand fetch — nothing is copied locally); this tool is the "fully
// copy only when DIRECTED to ingest and save" step. The user says "save that to
// my wiki" → the agent calls wiki_save with the note it fetched → it's persisted
// as a first-class local note (searchable offline, no longer needs the peer).
//
// Kept at the HOST layer so the wiki package stays read-only (its design ethos).
// Confined to the wiki root (no traversal / no symlink escape), gated through the
// approval broker (so it writes only on explicit approval), and non-clobbering
// unless overwrite=true.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"tenant/internal/model"
	"tenant/internal/plugins/wiki"
)

type wikiSaveDispatcher struct {
	root    string // absolute wiki dir
	ix      *wiki.Index
	confirm func(context.Context, string, string) bool
}

func newWikiSaveDispatcher(wikiDir string, ix *wiki.Index, confirm func(context.Context, string, string) bool) *wikiSaveDispatcher {
	abs, err := filepath.Abs(wikiDir)
	if err != nil {
		abs = wikiDir
	}
	return &wikiSaveDispatcher{root: abs, ix: ix, confirm: confirm}
}

func (d *wikiSaveDispatcher) Tools() []model.ToolSpec {
	return []model.ToolSpec{{
		Name:        "wiki_save",
		Description: "Save/ingest a note into your LOCAL wiki: writes <file> with <content> and reindexes so it's searchable offline. Use to PERMANENTLY keep a peer's note you fetched (peers are otherwise read-on-demand), or to save research. Non-clobbering unless overwrite=true. Gated — writes only when you approve.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"file":{"type":"string","description":"note path relative to the wiki root, e.g. milk.md"},"content":{"type":"string","description":"full markdown body to write"},"overwrite":{"type":"boolean","description":"replace an existing note (default false)"}},"required":["file","content"]}`),
		Gated:       true,
	}}
}

func (d *wikiSaveDispatcher) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	if call.Name != "wiki_save" {
		return "unknown tool: " + call.Name, true, nil
	}
	var a struct {
		File      string `json:"file"`
		Content   string `json:"content"`
		Overwrite bool   `json:"overwrite"`
	}
	if err := json.Unmarshal(call.Arguments, &a); err != nil {
		return "invalid arguments: " + err.Error(), true, nil
	}
	rel := strings.TrimSpace(a.File)
	if rel == "" {
		return "file is required", true, nil
	}
	if strings.TrimSpace(a.Content) == "" {
		return "content is required", true, nil
	}
	// Default to a markdown note so it indexes.
	if !isNoteExt(rel) {
		rel += ".md"
	}
	full, err := d.safePath(rel)
	if err != nil {
		return err.Error(), true, nil
	}

	// Lstat (does NOT follow symlinks): if the final component is itself a
	// symlink, REFUSE — writing through it would escape the wiki root no matter
	// where it points (os.Stat would follow it and a dangling link even reads as
	// "doesn't exist", skipping the overwrite gate). This mirrors the read side's
	// final-target EvalSymlinks containment. (TEN-244 review: HIGH egress fix.)
	exists := false
	if fi, lerr := os.Lstat(full); lerr == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Sprintf("note %q is a symlink — refusing to write through it", rel), true, nil
		}
		exists = true
	}
	if exists && !a.Overwrite {
		return fmt.Sprintf("note %q already exists — pass overwrite=true to replace it", rel), true, nil
	}

	// Gated: persist ONLY on explicit approval (the "only when directed" rule).
	// No broker (headless) ⇒ trust the operator who launched it, like other
	// host tools.
	if d.confirm != nil {
		detail := "write " + rel + " to your local wiki"
		if exists {
			detail = "OVERWRITE existing note " + rel + " in your local wiki"
		}
		if !d.confirm(ctx, "wiki_save", detail) {
			return "wiki_save denied — nothing written", true, nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "save failed: could not create the target directory", true, nil
	}
	// Symlink defense: after MkdirAll the parent dir is real; re-verify the
	// RESOLVED parent is still inside the resolved root, so a pre-existing
	// symlinked subdir can't redirect the write outside the wiki.
	if err := d.parentContained(full); err != nil {
		return err.Error(), true, nil
	}
	if err := os.WriteFile(full, []byte(a.Content), 0o644); err != nil {
		return "save failed: could not write the note", true, nil
	}

	verb := "saved"
	if exists {
		verb = "overwrote"
	}
	if d.ix != nil {
		if files, _, rerr := d.ix.Reindex(ctx); rerr == nil {
			return fmt.Sprintf("✓ %s %s (%d notes indexed) — now searchable locally", verb, rel, files), false, nil
		}
	}
	return fmt.Sprintf("✓ %s %s (reindex pending)", verb, rel), false, nil
}

// safePath confines rel to the wiki root with the same lexical guard as the
// read path (Clean + ".."/IsAbs + Rel double-check). No absolute paths leak.
func (d *wikiSaveDispatcher) safePath(rel string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return "", fmt.Errorf("wiki_save: %q is outside the wiki", rel)
	}
	full := filepath.Join(d.root, clean)
	if r, rerr := filepath.Rel(d.root, full); rerr != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("wiki_save: %q escapes the wiki root", rel)
	}
	return full, nil
}

// parentContained ensures the resolved parent dir is genuinely inside the
// resolved root (catches a pre-existing in-root symlinked dir pointing out).
func (d *wikiSaveDispatcher) parentContained(full string) error {
	rootEval, err := filepath.EvalSymlinks(d.root)
	if err != nil {
		rootEval = d.root
	}
	parentEval, err := filepath.EvalSymlinks(filepath.Dir(full))
	if err != nil {
		return fmt.Errorf("wiki_save: target directory unresolved")
	}
	if r, rerr := filepath.Rel(rootEval, parentEval); rerr != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return fmt.Errorf("wiki_save: target escapes the wiki root")
	}
	return nil
}

func isNoteExt(name string) bool {
	n := strings.ToLower(name)
	return strings.HasSuffix(n, ".md") || strings.HasSuffix(n, ".markdown") || strings.HasSuffix(n, ".txt")
}
