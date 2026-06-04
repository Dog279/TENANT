package wiki_test

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tenant/internal/model"
	"tenant/internal/plugins/wiki"
)

// fakeEmbedder: deterministic feature-hash so "shared words → closer
// vectors". No network — same fakes-for-logic discipline used
// everywhere; real Nomic is exercised by `tenant wiki` live.
type fakeEmbedder struct {
	calls int
	dim   int
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls++
	if f.dim == 0 {
		// 256, not 64: at 64 dims FNV collisions dominate ~7-token
		// notes and an unrelated note can out-cosine the right one —
		// the fake would violate its own "shared words → closer
		// vectors" contract. 256 makes it a faithful proxy (real
		// Nomic is 768). Verified empirically before picking this.
		f.dim = 256
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, f.dim)
		for _, w := range strings.Fields(strings.ToLower(t)) {
			h := fnv.New32a()
			_, _ = h.Write([]byte(w))
			v[int(h.Sum32())%f.dim] += 1
		}
		out[i] = v
	}
	return out, nil
}

func writeNote(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkIndex(t *testing.T) (*wiki.Index, string, *fakeEmbedder) {
	t.Helper()
	root := t.TempDir()
	side := filepath.Join(t.TempDir(), "idx.json")
	fe := &fakeEmbedder{}
	ix, err := wiki.New(root, side, "fake/256", fe)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ix, root, fe
}

func TestNew_RejectsNonDir(t *testing.T) {
	if _, err := wiki.New(filepath.Join(t.TempDir(), "nope"), "s", "e", &fakeEmbedder{}); err == nil {
		t.Fatal("non-dir root must error")
	}
}

func TestReindex_AndSearch(t *testing.T) {
	ix, root, _ := mkIndex(t)
	writeNote(t, root, "go.md", "# Concurrency\nGoroutines are cheap green threads.\n\n# Errors\nGo uses explicit error returns.")
	writeNote(t, root, "notes/python.md", "Python uses the GIL for threading.")
	writeNote(t, root, ".obsidian/config", "should be ignored")
	writeNote(t, root, "readme.txt", "plain text note about widgets")

	n, ch, err := ix.Reindex(context.Background())
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if n != 3 { // go.md, notes/python.md, readme.txt — .obsidian skipped
		t.Fatalf("indexed %d files, want 3", n)
	}
	if ch == 0 {
		t.Fatal("no chunks embedded")
	}

	hits, err := ix.Search(context.Background(), "tell me about goroutines and concurrency", 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	top := hits[0]
	if top.File != "go.md" || !strings.Contains(strings.ToLower(top.Snippet), "goroutine") {
		t.Fatalf("top hit wrong: %+v", top)
	}
	if top.Heading != "Concurrency" {
		t.Errorf("heading not carried: %q", top.Heading)
	}
}

func TestReindex_Incremental(t *testing.T) {
	ix, root, fe := mkIndex(t)
	writeNote(t, root, "a.md", "alpha content")
	writeNote(t, root, "b.md", "beta content")
	if _, _, err := ix.Reindex(context.Background()); err != nil {
		t.Fatal(err)
	}
	callsAfterFirst := fe.calls

	// No changes → reindex embeds nothing new.
	if _, ch, err := ix.Reindex(context.Background()); err != nil || ch != 0 {
		t.Fatalf("clean reindex should embed 0, got ch=%d err=%v", ch, err)
	}
	if fe.calls != callsAfterFirst {
		t.Errorf("clean reindex called embedder again (%d→%d)", callsAfterFirst, fe.calls)
	}

	// Modify one file (bump mtime + content) → only that one re-embeds.
	time.Sleep(1100 * time.Millisecond) // mtime has 1s resolution
	writeNote(t, root, "a.md", "alpha content CHANGED with new words")
	files, ch, err := ix.Reindex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if files != 2 || ch == 0 {
		t.Fatalf("expected 2 files, >0 chunks re-embedded; got files=%d ch=%d", files, ch)
	}

	// Delete b.md → it drops out of List + search.
	if err := os.Remove(filepath.Join(root, "b.md")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ix.Reindex(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, f := range ix.List() {
		if f == "b.md" {
			t.Fatal("deleted file still indexed")
		}
	}
}

func TestEmbedderMismatch_ForcesRebuild(t *testing.T) {
	root := t.TempDir()
	side := filepath.Join(t.TempDir(), "idx.json")
	writeNote(t, root, "n.md", "some knowledge")

	ix1, _ := wiki.New(root, side, "embedderA/64", &fakeEmbedder{})
	if _, _, err := ix1.Reindex(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Reopen with a DIFFERENT embedder id → stored vecs unusable →
	// load() must discard, Search rebuilds from scratch.
	feB := &fakeEmbedder{}
	ix2, err := wiki.New(root, side, "embedderB/128", feB)
	if err != nil {
		t.Fatal(err)
	}
	if len(ix2.List()) != 0 {
		t.Fatalf("stale (wrong-embedder) index not discarded: %v", ix2.List())
	}
	if _, err := ix2.Search(context.Background(), "knowledge", 3); err != nil {
		t.Fatal(err)
	}
	if feB.calls == 0 {
		t.Fatal("expected rebuild with the new embedder")
	}
}

func TestSidecarIsPlainReadableJSON(t *testing.T) {
	// The whole point of the Karpathy design: you can `cat` the index.
	root := t.TempDir()
	side := filepath.Join(t.TempDir(), "idx.json")
	ix, err := wiki.New(root, side, "fake/256", &fakeEmbedder{})
	if err != nil {
		t.Fatal(err)
	}
	writeNote(t, root, "k.md", "# Topic\nreadable knowledge here")
	if _, _, err := ix.Reindex(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(side)
	if err != nil {
		t.Fatalf("sidecar unreadable: %v", err)
	}
	var f map[string]any
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("sidecar is not plain JSON: %v", err)
	}
	if _, ok := f["chunks"]; !ok {
		t.Error("sidecar missing chunks (not the documented plain format)")
	}
	if _, ok := f["embedder"]; !ok {
		t.Error("sidecar missing embedder fingerprint")
	}
}

// --- dispatcher / safety ---

func call(name string, args map[string]any) model.ToolCall {
	b, _ := json.Marshal(args)
	if len(args) == 0 {
		b = []byte(`{}`)
	}
	return model.ToolCall{Name: name, Arguments: b}
}

func TestDispatch_SearchReadListReindex(t *testing.T) {
	ix, root, _ := mkIndex(t)
	writeNote(t, root, "doc.md", "# Setup\nRun `go build ./...` to compile the project.")
	d := wiki.NewDispatcher(ix)

	out, isErr, _ := d.Dispatch(context.Background(), call("wiki_reindex", nil))
	if isErr || !strings.Contains(out, "indexed 1 file") {
		t.Fatalf("reindex: isErr=%v %q", isErr, out)
	}
	out, isErr, _ = d.Dispatch(context.Background(), call("wiki_list", nil))
	if isErr || !strings.Contains(out, "doc.md") {
		t.Fatalf("list: %q", out)
	}
	out, isErr, _ = d.Dispatch(context.Background(), call("wiki_search", map[string]any{"query": "how do I compile"}))
	if isErr || !strings.Contains(out, "doc.md") {
		t.Fatalf("search: %q", out)
	}
	out, isErr, _ = d.Dispatch(context.Background(), call("wiki_read", map[string]any{"file": "doc.md"}))
	if isErr || !strings.Contains(out, "go build ./...") {
		t.Fatalf("read: %q", out)
	}
}

func TestDispatch_ReadRejectsPathTraversal(t *testing.T) {
	ix, root, _ := mkIndex(t)
	writeNote(t, root, "ok.md", "fine")
	_, _, _ = ix.Reindex(context.Background())
	d := wiki.NewDispatcher(ix)
	for _, evil := range []string{"../../../etc/passwd", "/etc/passwd", "..\\..\\secret", "../" + filepath.Base(root)} {
		out, isErr, _ := d.Dispatch(context.Background(), call("wiki_read", map[string]any{"file": evil}))
		if !isErr {
			t.Errorf("path traversal %q must be refused, got: %q", evil, out)
		}
	}
}

func TestDispatch_BadArgs(t *testing.T) {
	d := wiki.NewDispatcher(func() *wiki.Index { ix, _, _ := mkIndex(t); return ix }())
	out, isErr, _ := d.Dispatch(context.Background(), model.ToolCall{Name: "wiki_search", Arguments: json.RawMessage(`{bad`)})
	if !isErr || !strings.Contains(out, "invalid arguments") {
		t.Fatalf("got isErr=%v %q", isErr, out)
	}
	out, isErr, _ = d.Dispatch(context.Background(), call("wiki_search", map[string]any{"query": "  "}))
	if !isErr || !strings.Contains(out, "query is required") {
		t.Fatalf("empty query must error: %q", out)
	}
}

func TestTools(t *testing.T) {
	ix, _, _ := mkIndex(t)
	names := map[string]bool{}
	for _, sp := range wiki.NewDispatcher(ix).Tools() {
		names[sp.Name] = true
		if !json.Valid(sp.Parameters) {
			t.Errorf("%s invalid params", sp.Name)
		}
	}
	for _, w := range []string{"wiki_search", "wiki_read", "wiki_links", "wiki_list", "wiki_reindex"} {
		if !names[w] {
			t.Errorf("missing tool %s", w)
		}
	}
}

// Graph expansion: a note that links to another should drag that
// neighbour into the results even when the neighbour's own text barely
// matches the query — with Via set so the provenance is visible.
func TestSearch_GraphExpansionPullsLinkedNote(t *testing.T) {
	ix, root, _ := mkIndex(t)
	// "alpha" matches the query strongly and links to [[Beta]].
	writeNote(t, root, "alpha.md", "# Alpha\nquantum entanglement teleportation qubits.\nSee also [[Beta]].")
	// "beta" shares NO words with the query — only the link connects it.
	writeNote(t, root, "beta.md", "# Beta\nrutabaga marmalade accordion.")
	if _, _, err := ix.Reindex(context.Background()); err != nil {
		t.Fatal(err)
	}
	hits, err := ix.Search(context.Background(), "quantum entanglement teleportation qubits", 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].File != "alpha.md" {
		t.Fatalf("alpha.md should be the top direct hit, got %+v", hits)
	}
	var beta *wiki.Hit
	for i := range hits {
		if hits[i].File == "beta.md" {
			beta = &hits[i]
		}
	}
	if beta == nil {
		t.Fatalf("beta.md (linked-only, no lexical overlap) was not pulled in: %+v", hits)
	}
	if beta.Via != "alpha.md" {
		t.Errorf("beta.md should carry Via=alpha.md (graph provenance), got %q", beta.Via)
	}
}

// wiki_links exposes the Obsidian graph (forward + back + tags) and
// resolves aliases; unknown notes are refused.
func TestDispatch_WikiLinks(t *testing.T) {
	ix, root, _ := mkIndex(t)
	writeNote(t, root, "hub.md", "# Hub\n#core #area/x\nlinks [[Spoke]] and [[By Its Alias]].")
	writeNote(t, root, "spoke.md", "plain spoke note")
	writeNote(t, root, "aliased.md", "---\naliases: [By Its Alias]\n---\nthe aliased note")
	d := wiki.NewDispatcher(ix)
	if _, _, err := ix.Reindex(context.Background()); err != nil {
		t.Fatal(err)
	}

	out, isErr, _ := d.Dispatch(context.Background(), call("wiki_links", map[string]any{"file": "hub.md"}))
	if isErr {
		t.Fatalf("wiki_links errored: %q", out)
	}
	for _, want := range []string{"spoke.md", "aliased.md", "#core", "area/x"} {
		if !strings.Contains(out, want) {
			t.Errorf("wiki_links missing %q in:\n%s", want, out)
		}
	}
	// Backlink direction: spoke is linked FROM hub.
	out, _, _ = d.Dispatch(context.Background(), call("wiki_links", map[string]any{"file": "spoke.md"}))
	if !strings.Contains(out, "linked from: hub.md") {
		t.Errorf("backlink not reported: %q", out)
	}
	// Unknown note refused.
	out, isErr, _ = d.Dispatch(context.Background(), call("wiki_links", map[string]any{"file": "nope.md"}))
	if !isErr {
		t.Errorf("unknown note must be refused, got: %q", out)
	}
}

// An older on-disk index format must be discarded (vectors built with
// a different embed-text recipe are unusable) — same discipline as the
// embedder-mismatch rebuild.
func TestIndexFormatBump_ForcesRebuild(t *testing.T) {
	root := t.TempDir()
	side := filepath.Join(t.TempDir(), "idx.json")
	writeNote(t, root, "n.md", "some knowledge")
	ix1, _ := wiki.New(root, side, "fake/256", &fakeEmbedder{})
	if _, _, err := ix1.Reindex(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Downgrade the persisted format on disk.
	b, _ := os.ReadFile(side)
	var f map[string]any
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	f["format"] = 1
	nb, _ := json.Marshal(f)
	if err := os.WriteFile(side, nb, 0o644); err != nil {
		t.Fatal(err)
	}
	feB := &fakeEmbedder{}
	ix2, err := wiki.New(root, side, "fake/256", feB)
	if err != nil {
		t.Fatal(err)
	}
	if len(ix2.List()) != 0 {
		t.Fatalf("stale-format index not discarded: %v", ix2.List())
	}
	if _, err := ix2.Search(context.Background(), "knowledge", 3); err != nil {
		t.Fatal(err)
	}
	if feB.calls == 0 {
		t.Fatal("expected a rebuild with the new index format")
	}
}
