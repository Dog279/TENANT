// Package wiki is Tenant's "Karpathy-style" knowledge connector.
//
// Design ethos (nanoGPT / micrograd / llm.c): single purpose, no
// framework magic, plain inspectable files, the simple correct thing
// over the clever thing — every line here is meant to be readable
// top-to-bottom.
//
//   - The .md/.txt files in the wiki dir are CANONICAL. We only read
//     them, never write — so this plugin has no dangerous modes and
//     therefore no safety gate (unlike web/sql). One capability,
//     done plainly.
//
//   - The index is a DERIVED, DISPOSABLE artifact: one plain JSON file
//     (`cat` it and read it). Delete it → next search rebuilds it,
//     nothing lost. No sqlite-vec, no FTS5 vtable, no ANN: just
//     []float32 and a cosine loop. Brute force is fine at personal
//     scale and it is the most transparent thing that works.
//
//   - The index records which embedder+dim built it. A different
//     embedder ⇒ full rebuild. (Directly applying the silent
//     dimension-mismatch bug `tenant doctor` was written to catch.)
//
//   - Incremental but minimal: per-file (mtime,size) fingerprint;
//     only changed/new files are re-embedded; removed files are
//     dropped. Engineered enough, not over-engineered.
package wiki

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"tenant/internal/memory/cosine"
	"tenant/internal/model"
)

// Chunk is one searchable unit: a slice of a note plus the heading
// path it lived under (so retrieval says WHICH note/section).
type Chunk struct {
	File    string    `json:"file"`    // path relative to the wiki root
	Heading string    `json:"heading"` // nearest markdown heading ("" if none)
	Ord     int       `json:"ord"`     // chunk index within the file
	Text    string    `json:"text"`
	Vec     []float32 `json:"vec"`
}

// fileFingerprint detects "did this file change since we indexed it".
type fileFingerprint struct {
	ModUnix int64 `json:"mtime"`
	Size    int64 `json:"size"`
}

// indexFormat is bumped whenever the embedded-text recipe or sidecar
// schema changes (link/tag enrichment changed vectors). A mismatch
// forces a clean rebuild — the same discard-stale discipline used for
// an embedder change. v3 added noteMeta.Virtual (auto-derived edges).
const indexFormat = 3

// indexFile is the entire on-disk index — one JSON file you can read.
type indexFile struct {
	Format   int                        `json:"format"`
	Embedder string                     `json:"embedder"` // id+dim; mismatch ⇒ full rebuild
	Files    map[string]fileFingerprint `json:"files"`
	Notes    map[string]noteMeta        `json:"notes"` // per-note graph data (links/tags/aliases)
	Chunks   []Chunk                    `json:"chunks"`
}

// Index is the in-memory knowledge base over a directory of notes.
type Index struct {
	root     string
	sidecar  string // where the JSON index lives (data dir, not the vault)
	embedder model.Embedder
	embedID  string // embedder fingerprint, e.g. "nomic-embed-text/768"

	idx indexFile

	// Derived graph (never persisted — rebuilt from idx.Notes so it
	// can't drift): resolved forward links + inverted backlinks.
	links     map[string][]string // note → resolved target notes
	backlinks map[string][]string // note → notes that link TO it

	// Virtual (semantic) graph — same shape, derived from idx.Notes[*].Virtual.
	// Kept SEPARATE from manual links so expansion can weight author intent
	// above statistical similarity, and so the manual-link maps + every existing
	// consumer (Links, dispatch, tests) stay untouched.
	vlinks     map[string][]string
	vbacklinks map[string][]string
}

// chunking parameters — kept obvious, not tunable-via-config (Karpathy:
// fewer knobs, readable defaults).
const (
	maxChunkRunes = 1200 // ~300 tokens
	overlapRunes  = 150
)

// New opens (or prepares) an index. embedID is a stable string like
// "<model>/<dim>" so a different embedder forces a clean rebuild.
func New(root, sidecar, embedID string, e model.Embedder) (*Index, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("wiki: resolve root: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("wiki: %s is not a directory", root)
	}
	ix := &Index{root: abs, sidecar: sidecar, embedder: e, embedID: embedID}
	ix.load() // best-effort; a bad/missing sidecar just means "rebuild"
	return ix, nil
}

// load reads the sidecar if present + embedder-compatible.
func (ix *Index) load() {
	b, err := os.ReadFile(ix.sidecar)
	if err != nil {
		return
	}
	var f indexFile
	if json.Unmarshal(b, &f) != nil {
		return
	}
	if f.Embedder != ix.embedID || f.Format != indexFormat {
		// Different embedder/dim OR an older index schema → stored
		// vectors are unusable. Drop it; next search rebuilds.
		return
	}
	if f.Files == nil {
		f.Files = map[string]fileFingerprint{}
	}
	if f.Notes == nil {
		f.Notes = map[string]noteMeta{}
	}
	ix.idx = f
	ix.rebuildGraph()
}

// rebuildGraph derives resolved forward links + backlinks from the
// raw targets in idx.Notes. Cheap (O(total links)); called after any
// load/Reindex so the graph always matches the note set on disk.
func (ix *Index) rebuildGraph() {
	r := newResolver(ix.idx.Notes)
	ix.links = map[string][]string{}
	ix.backlinks = map[string][]string{}
	ix.vlinks = map[string][]string{}
	ix.vbacklinks = map[string][]string{}
	for rel, m := range ix.idx.Notes {
		seen := map[string]bool{}
		for _, t := range m.Targets {
			if t.Target == "" {
				continue // same-note heading link — no edge
			}
			tgt, ok := r.resolve(t.Target)
			if !ok || tgt == rel || seen[tgt] {
				continue
			}
			seen[tgt] = true
			ix.links[rel] = append(ix.links[rel], tgt)
			ix.backlinks[tgt] = append(ix.backlinks[tgt], rel)
		}
		// Virtual (semantic) edges: targets are already resolved note paths.
		// Skip any that vanished or that duplicate a manual forward link —
		// manual is the stronger signal, no weaker virtual twin.
		for _, vl := range m.Virtual {
			tgt := vl.Target
			if tgt == "" || tgt == rel || seen[tgt] {
				continue
			}
			if _, ok := ix.idx.Notes[tgt]; !ok {
				continue
			}
			ix.vlinks[rel] = append(ix.vlinks[rel], tgt)
			ix.vbacklinks[tgt] = append(ix.vbacklinks[tgt], rel)
		}
	}
	for k := range ix.links {
		sort.Strings(ix.links[k])
	}
	for k := range ix.backlinks {
		sort.Strings(ix.backlinks[k])
	}
	for k := range ix.vlinks {
		sort.Strings(ix.vlinks[k])
	}
	for k := range ix.vbacklinks {
		sort.Strings(ix.vbacklinks[k])
	}
}

// virtual-link knobs — readable defaults (Karpathy: no config until real data
// demands it). Tuned conservative: catch genuine relationships, miss the noise.
const (
	virtualThreshold  = 0.72 // min best-chunk cosine for an auto edge; below this is noise
	virtualMaxPerNote = 8    // cap auto edges per note so a dense hub can't flood expansion
)

// computeVirtualLinks derives note-level semantic edges from chunk cosine
// similarity and stores them in idx.Notes[*].Virtual. Runs at the end of a
// Reindex on the vectors already in hand — NO embedding calls. O(C²) on
// pre-computed vectors (sub-second at personal scale). Deterministic ordering
// so the sidecar doesn't churn between identical runs. This is what gives graph
// expansion something to traverse on notes an author never [[linked]].
func (ix *Index) computeVirtualLinks() {
	// Chunk indices grouped by note, notes in stable order.
	byNote := map[string][]int{}
	var notes []string
	for i := range ix.idx.Chunks {
		f := ix.idx.Chunks[i].File
		if _, ok := byNote[f]; !ok {
			notes = append(notes, f)
		}
		byNote[f] = append(byNote[f], i)
	}
	sort.Strings(notes)

	type cand struct {
		target string
		score  float64
	}
	acc := map[string][]cand{}
	for a := 0; a < len(notes); a++ {
		for b := a + 1; b < len(notes); b++ {
			na, nb := notes[a], notes[b]
			best := 0.0
			for _, i := range byNote[na] {
				for _, j := range byNote[nb] {
					if s := cosine.Similarity(ix.idx.Chunks[i].Vec, ix.idx.Chunks[j].Vec); s > best {
						best = s
					}
				}
			}
			if best >= virtualThreshold {
				acc[na] = append(acc[na], cand{nb, best})
				acc[nb] = append(acc[nb], cand{na, best})
			}
		}
	}

	// Persist onto every note: highest score first, capped, deterministic.
	// noteMeta is a VALUE in the map, so reassign rather than mutate in place.
	for rel, m := range ix.idx.Notes {
		cs := acc[rel]
		sort.Slice(cs, func(i, j int) bool {
			if cs[i].score != cs[j].score {
				return cs[i].score > cs[j].score
			}
			return cs[i].target < cs[j].target
		})
		if len(cs) > virtualMaxPerNote {
			cs = cs[:virtualMaxPerNote]
		}
		var vls []VirtualLink
		for _, c := range cs {
			vls = append(vls, VirtualLink{Target: c.target, Score: c.score})
		}
		m.Virtual = vls // nil clears stale edges (e.g. a neighbour was deleted)
		ix.idx.Notes[rel] = m
	}
}

func (ix *Index) save() error {
	if err := os.MkdirAll(filepath.Dir(ix.sidecar), 0o755); err != nil {
		return err
	}
	ix.idx.Embedder = ix.embedID
	ix.idx.Format = indexFormat
	b, err := json.MarshalIndent(ix.idx, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(ix.sidecar, b, 0o644)
}

// Reindex brings the index in sync with the files on disk. Only
// changed/new files are re-embedded; deleted files are dropped.
// Returns (filesIndexed, chunksEmbedded).
func (ix *Index) Reindex(ctx context.Context) (int, int, error) {
	if ix.idx.Files == nil {
		ix.idx.Files = map[string]fileFingerprint{}
	}
	if ix.idx.Notes == nil {
		ix.idx.Notes = map[string]noteMeta{}
	}
	onDisk := map[string]fileFingerprint{}
	var changed []string

	// Resolve the root once so symlink containment below compares like-for-like
	// even when the root path itself goes through a symlink.
	rootEval, evErr := filepath.EvalSymlinks(ix.root)
	if evErr != nil {
		rootEval = ix.root
	}

	err := filepath.WalkDir(ix.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip hidden dirs (.git, .obsidian) — not knowledge.
			if d.Name() != "." && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !isNote(d.Name()) {
			return nil
		}
		// A symlinked note that resolves OUTSIDE the wiki root is NOT indexed, so
		// its target can never leak via peer_wiki_search snippets (or any read).
		// In-root symlinks stay indexed — local symlinked-notes workflows are
		// unaffected. (TEN-243 hardening: stop egress at the indexer, the root
		// cause; ReadSharedNote's containment is then defense-in-depth.)
		if d.Type()&fs.ModeSymlink != 0 {
			real, rerr := filepath.EvalSymlinks(path)
			if rerr != nil {
				return nil // unresolvable symlink → skip
			}
			if r, rerr := filepath.Rel(rootEval, real); rerr != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
				return nil // escapes the wiki root → don't index its target
			}
		}
		rel, _ := filepath.Rel(ix.root, path)
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		fp := fileFingerprint{ModUnix: info.ModTime().Unix(), Size: info.Size()}
		onDisk[rel] = fp
		if old, ok := ix.idx.Files[rel]; !ok || old != fp {
			changed = append(changed, rel)
		}
		return nil
	})
	if err != nil {
		return 0, 0, fmt.Errorf("wiki: walk: %w", err)
	}

	// Drop chunks for files that vanished or changed (changed ones get
	// re-added below).
	keep := ix.idx.Chunks[:0]
	dropped := map[string]bool{}
	for _, c := range ix.idx.Chunks {
		_, stillOnDisk := onDisk[c.File]
		isChanged := contains(changed, c.File)
		if stillOnDisk && !isChanged {
			keep = append(keep, c)
		} else {
			dropped[c.File] = true
		}
	}
	ix.idx.Chunks = keep

	// Re-chunk + embed changed/new files.
	embedded := 0
	for _, rel := range changed {
		raw, err := os.ReadFile(filepath.Join(ix.root, filepath.FromSlash(rel)))
		if err != nil {
			continue // file raced away; skip
		}
		meta, body := parseNote(string(raw))
		ix.idx.Notes[rel] = meta
		chunks := chunkNote(rel, body) // body = frontmatter stripped
		if len(chunks) == 0 {
			ix.idx.Files[rel] = onDisk[rel]
			continue
		}
		// Enrich the embedded text with what this note links to + its
		// tags: the vector then reflects the note's CONNECTIONS, not
		// just its prose — the semantic half of link-aware RAG.
		enrich := linkTagEnrich(meta)
		texts := make([]string, len(chunks))
		for i, c := range chunks {
			texts[i] = embedText(c) + enrich
		}
		vecs, err := ix.embedder.Embed(ctx, texts)
		if err != nil {
			return len(onDisk), embedded, fmt.Errorf("wiki: embed %s: %w", rel, err)
		}
		if len(vecs) != len(chunks) {
			return len(onDisk), embedded, fmt.Errorf("wiki: embedder returned %d vecs for %d chunks", len(vecs), len(chunks))
		}
		for i := range chunks {
			chunks[i].Vec = vecs[i]
		}
		ix.idx.Chunks = append(ix.idx.Chunks, chunks...)
		ix.idx.Files[rel] = onDisk[rel]
		embedded += len(chunks)
	}
	// Forget files no longer on disk.
	for rel := range ix.idx.Files {
		if _, ok := onDisk[rel]; !ok {
			delete(ix.idx.Files, rel)
			delete(ix.idx.Notes, rel)
		}
	}
	ix.computeVirtualLinks() // derive semantic edges from the fresh vectors…
	ix.rebuildGraph()        // …then fold them + manual links into the graph
	if err := ix.save(); err != nil {
		return len(onDisk), embedded, fmt.Errorf("wiki: save index: %w", err)
	}
	return len(onDisk), embedded, nil
}

// linkTagEnrich is the connection-aware suffix appended to each
// chunk's embedded text. Empty when a note has no links/tags so plain
// notes embed exactly as before (keeps incremental + tests stable).
func linkTagEnrich(m noteMeta) string {
	var parts []string
	if len(m.Targets) > 0 {
		var ds []string
		for _, t := range m.Targets {
			if d := strings.TrimSpace(t.display()); d != "" {
				ds = append(ds, d)
			}
		}
		if ds = uniqueStable(ds); len(ds) > 0 {
			parts = append(parts, "links: "+strings.Join(ds, ", "))
		}
	}
	if len(m.Tags) > 0 {
		parts = append(parts, "tags: "+strings.Join(m.Tags, ", "))
	}
	if len(parts) == 0 {
		return ""
	}
	return "\n" + strings.Join(parts, "\n")
}

// graph-expansion knobs — readable defaults, not config (Karpathy).
const (
	anchorK            = 3    // max top hits whose neighbours we pull in
	anchorFloorFrac    = 0.5  // an anchor must score ≥ this × the top score
	graphWeight        = 0.4  // manual-link neighbour bonus = graphWeight × anchor score
	virtualGraphWeight = 0.25 // auto-derived (virtual) edges: weaker than an author's [[link]]
)

// Hit is one search result. Via is set when the note was pulled in by
// the link graph rather than matching directly (provenance the agent
// and user can see). Tags carries the note's #tags/frontmatter tags.
type Hit struct {
	File    string
	Heading string
	Snippet string
	Score   float64
	Via     string
	Tags    []string
}

// Search embeds the query and ranks chunks by cosine, with a tiny
// lexical boost (substring of the longest query word). Auto-reindexes
// if the index looks empty. Brute force — readable, fine at personal
// scale; swap for ANN only if a real corpus ever demands it.
func (ix *Index) Search(ctx context.Context, query string, k int) ([]Hit, error) {
	if k <= 0 {
		k = 6
	}
	if len(ix.idx.Chunks) == 0 {
		if _, _, err := ix.Reindex(ctx); err != nil {
			return nil, err
		}
	}
	if len(ix.idx.Chunks) == 0 {
		return nil, nil
	}
	qv, err := ix.embedder.Embed(ctx, []string{"search query: " + query})
	if err != nil {
		return nil, fmt.Errorf("wiki: embed query: %w", err)
	}
	q := qv[0]
	longest := longestWord(query)

	type scored struct {
		c     *Chunk
		score float64
		via   string
	}
	all := make([]scored, 0, len(ix.idx.Chunks))
	for i := range ix.idx.Chunks {
		c := &ix.idx.Chunks[i]
		s := cosine.Similarity(q, c.Vec)
		// Heading is the strongest single relevance signal a note
		// carries — scan it too, not just the body.
		if longest != "" && strings.Contains(strings.ToLower(c.Heading+" "+c.Text), longest) {
			s += 0.05 // small lexical nudge; cosine stays primary
		}
		all = append(all, scored{c: c, score: s})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })

	// --- 1-hop graph expansion ---
	// The top hits are "anchors". Any note 1 link away (forward OR
	// back) from an anchor gets a decayed bonus, so a strongly-linked
	// note rides along even if its own cosine was mediocre — this is
	// the link half of link-aware RAG. Boost only, never penalize;
	// cosine stays primary (graphWeight < 1).
	anchorScore := map[string]float64{} // anchor note → its best score
	if len(all) > 0 && all[0].score > 0 {
		floor := anchorFloorFrac * all[0].score
		for _, s := range all {
			if s.score < floor || len(anchorScore) >= anchorK {
				break // only genuinely-relevant top hits anchor expansion
			}
			if _, ok := anchorScore[s.c.File]; !ok {
				anchorScore[s.c.File] = s.score
			}
		}
	}
	bonus := map[string]float64{} // neighbour note → best bonus
	bonusVia := map[string]string{}
	for anchor, as := range anchorScore {
		for _, nb := range neighborsOf(ix.links[anchor], ix.backlinks[anchor]) {
			if _, isAnchor := anchorScore[nb]; isAnchor {
				continue // don't reframe a direct hit as "via"
			}
			if b := graphWeight * as; b > bonus[nb] {
				bonus[nb] = b
				bonusVia[nb] = anchor
			}
		}
	}
	// Virtual (semantic) neighbours ride along too, at a lower weight than an
	// author's explicit link. max-bonus dedup means a note that is BOTH a manual
	// and a virtual neighbour keeps the stronger manual bonus.
	for anchor, as := range anchorScore {
		for _, nb := range neighborsOf(ix.vlinks[anchor], ix.vbacklinks[anchor]) {
			if _, isAnchor := anchorScore[nb]; isAnchor {
				continue
			}
			if b := virtualGraphWeight * as; b > bonus[nb] {
				bonus[nb] = b
				bonusVia[nb] = anchor
			}
		}
	}
	if len(bonus) > 0 {
		for i := range all {
			if b, ok := bonus[all[i].c.File]; ok {
				all[i].score += b
				all[i].via = bonusVia[all[i].c.File]
			}
		}
		sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })
	}

	if len(all) > k {
		all = all[:k]
	}
	out := make([]Hit, 0, len(all))
	for _, s := range all {
		out = append(out, Hit{
			File: s.c.File, Heading: s.c.Heading,
			Snippet: snippet(s.c.Text, 280), Score: s.score,
			Via: s.via, Tags: ix.idx.Notes[s.c.File].Tags,
		})
	}
	return out, nil
}

func neighborsOf(fwd, back []string) []string {
	return uniqueStable(append(append([]string{}, fwd...), back...))
}

// Links returns a note's resolved forward links, backlinks and tags —
// the data behind the wiki_links tool. rel must be an indexed note.
func (ix *Index) Links(rel string) (forward, back, tags []string, raw []linkTarget, err error) {
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	m, ok := ix.idx.Notes[rel]
	if !ok {
		return nil, nil, nil, nil, fmt.Errorf("wiki: %q is not an indexed note (use wiki_list)", rel)
	}
	return ix.links[rel], ix.backlinks[rel], m.Tags, m.Targets, nil
}

// VirtualLinks returns a note's auto-derived semantic edges (sidecar-only,
// never written to the markdown), highest score first. Empty for an unindexed
// note. Powers wiki_suggest_links + the virtual section of wiki_links.
func (ix *Index) VirtualLinks(rel string) []VirtualLink {
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	return ix.idx.Notes[rel].Virtual
}

// ReadFile returns a note's full contents. The path is model-supplied,
// so it is constrained to the wiki root — no ../../etc/passwd.
func (ix *Index) ReadFile(rel string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return "", fmt.Errorf("wiki: %q is outside the wiki", rel)
	}
	full := filepath.Join(ix.root, clean)
	// Defense in depth: ensure the resolved path is genuinely inside root.
	if r, err := filepath.Rel(ix.root, full); err != nil || strings.HasPrefix(r, "..") {
		return "", fmt.Errorf("wiki: %q escapes the wiki root", rel)
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return "", fmt.Errorf("wiki: read %s: %w", rel, err)
	}
	return string(b), nil
}

// ReadSharedNote reads a note for SHARING ACROSS A TRUST BOUNDARY (peer_wiki_read)
// and is deliberately stricter than ReadFile: an arbitrary remote peer supplies
// the path, so on top of the lexical traversal guard it (a) requires the path to
// be a KNOWN indexed note — a peer can't read non-note files (.env, secrets.txt)
// that merely sit in the wiki dir — and (b) resolves symlinks and re-checks
// containment, so an in-root symlink can't point OUT to /etc/... and leak its
// target. Errors are intentionally terse (no absolute paths) — the peer-facing
// handler maps every failure to one opaque message so a peer can't probe the
// serving filesystem. The local wiki_read keeps the lenient ReadFile.
func (ix *Index) ReadSharedNote(rel string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return "", fmt.Errorf("wiki: outside the wiki")
	}
	if _, known := ix.idx.Files[filepath.ToSlash(clean)]; !known {
		return "", fmt.Errorf("wiki: not an indexed note")
	}
	full := filepath.Join(ix.root, clean)
	// Symlink containment: the RESOLVED real path must stay under the resolved
	// root. EvalSymlinks follows every link component, so an in-root symlink
	// pointing outside is caught here (the lexical guard above can't see it).
	rootEval, err := filepath.EvalSymlinks(ix.root)
	if err != nil {
		return "", fmt.Errorf("wiki: root unresolved")
	}
	fullEval, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", fmt.Errorf("wiki: unreadable")
	}
	if r, rerr := filepath.Rel(rootEval, fullEval); rerr != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("wiki: escapes the wiki root")
	}
	b, err := os.ReadFile(fullEval)
	if err != nil {
		return "", fmt.Errorf("wiki: unreadable")
	}
	return string(b), nil
}

// List returns the indexed note paths (sorted).
func (ix *Index) List() []string {
	out := make([]string, 0, len(ix.idx.Files))
	for f := range ix.idx.Files {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// --- the simple, readable internals ---

func isNote(name string) bool {
	n := strings.ToLower(name)
	return strings.HasSuffix(n, ".md") || strings.HasSuffix(n, ".markdown") || strings.HasSuffix(n, ".txt")
}

// chunkNote splits a note by markdown headings, then windows long
// sections with overlap. Each chunk carries its heading so retrieval
// is contextual. Plain and obvious on purpose.
func chunkNote(file, content string) []Chunk {
	lines := strings.Split(content, "\n")
	var chunks []Chunk
	heading := ""
	var buf []string
	ord := 0

	flush := func() {
		text := strings.TrimSpace(strings.Join(buf, "\n"))
		buf = buf[:0]
		if text == "" {
			return
		}
		for _, w := range windowRunes(text, maxChunkRunes, overlapRunes) {
			chunks = append(chunks, Chunk{File: file, Heading: heading, Ord: ord, Text: w})
			ord++
		}
	}
	for _, ln := range lines {
		if h := mdHeading(ln); h != "" {
			flush()
			heading = h
			continue
		}
		buf = append(buf, ln)
	}
	flush()
	return chunks
}

func mdHeading(line string) string {
	t := strings.TrimSpace(line)
	if strings.HasPrefix(t, "#") {
		return strings.TrimSpace(strings.TrimLeft(t, "# "))
	}
	return ""
}

// windowRunes splits s into <=max-rune windows with `overlap` rune
// overlap so a fact spanning a boundary is still retrievable.
func windowRunes(s string, max, overlap int) []string {
	r := []rune(s)
	if len(r) <= max {
		return []string{s}
	}
	var out []string
	step := max - overlap
	for start := 0; start < len(r); start += step {
		end := start + max
		if end > len(r) {
			end = len(r)
		}
		out = append(out, string(r[start:end]))
		if end == len(r) {
			break
		}
	}
	return out
}

// embedText is what we actually embed: heading prefix improves recall
// ("Concurrency: goroutines are..." embeds closer to a query about
// concurrency than the bare paragraph would).
func embedText(c Chunk) string {
	if c.Heading != "" {
		// Newline (not "Heading: ") so heading words stay clean tokens
		// — "concurrency", not "concurrency:" glued to the colon.
		return "search document: " + c.Heading + "\n" + c.Text
	}
	return "search document: " + c.Text
}

func longestWord(q string) string {
	best := ""
	for _, w := range strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		if len(w) > len(best) {
			best = w
		}
	}
	if len(best) < 4 { // too short to be a useful lexical signal
		return ""
	}
	return best
}

func snippet(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
