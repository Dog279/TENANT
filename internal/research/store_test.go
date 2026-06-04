package research

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// New creates the research dir under the given data dir and returns a usable
// store. Missing data dir → created. Empty data dir → error (defensive — a
// store rooted at "" would scatter files into the cwd).
func TestNew_CreatesDir(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Fatal("empty data dir should error")
	}
	d := t.TempDir()
	s, err := New(d)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	want := filepath.Join(d, "research")
	if s.Dir() != want {
		t.Errorf("Dir() = %q, want %q", s.Dir(), want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("research dir not created: %v", err)
	}
}

// Create writes a manifest immediately so a crash mid-run still leaves an
// entry visible in List() — the operator can see "this run started and never
// finished" instead of total silence.
func TestCreate_PersistsManifestImmediately(t *testing.T) {
	s := mustStore(t)
	r, err := s.Create(Manifest{Question: "what is graphiti?"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if r.ID() == "" {
		t.Fatal("id not assigned")
	}
	mfPath := filepath.Join(s.Dir(), r.ID(), "run.json")
	b, err := os.ReadFile(mfPath)
	if err != nil {
		t.Fatalf("manifest not written: %v", err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("manifest invalid JSON: %v", err)
	}
	if m.Status != StatusRunning {
		t.Errorf("initial status = %q, want %q", m.Status, StatusRunning)
	}
	if m.Started.IsZero() {
		t.Error("Started not set")
	}
}

// IDs are time-prefixed + slug — sortable AND readable. Two creates within
// the same second must NOT collide (the dedup suffix kicks in).
func TestCreate_IDSlugAndCollision(t *testing.T) {
	s := mustStore(t)
	r1, err := s.Create(Manifest{Question: "Compare X vs Y!"})
	if err != nil {
		t.Fatalf("Create #1: %v", err)
	}
	r2, err := s.Create(Manifest{Question: "Compare X vs Y!"})
	if err != nil {
		t.Fatalf("Create #2: %v", err)
	}
	if r1.ID() == r2.ID() {
		t.Fatalf("collision: both runs got id %q", r1.ID())
	}
	for _, id := range []string{r1.ID(), r2.ID()} {
		if !strings.Contains(id, "compare-x-vs-y") {
			t.Errorf("id %q missing slug", id)
		}
		// YYYYMMDD-HHMMSS prefix → at least 15 chars before the slug.
		if len(id) < 15 {
			t.Errorf("id %q missing time prefix", id)
		}
	}
}

// AppendFinding writes the body to findings/<agent>.md (so a future viewer
// can grep without parsing JSON) and adds a manifest entry.
func TestAppendFinding_WritesBodyAndManifest(t *testing.T) {
	r := newRun(t, "nvidia")
	err := r.AppendFinding(Finding{
		AgentID: "main-researcher-1",
		Role:    "researcher",
		Status:  "done",
		Result:  "NVDA closed at $219.51 [1].\n## Sources\n[1] https://example.com",
		Started: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("AppendFinding: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(r.Dir(), "findings", "main-researcher-1.md"))
	if err != nil {
		t.Fatalf("finding body missing: %v", err)
	}
	if !strings.Contains(string(body), "NVDA closed") {
		t.Errorf("body wrong: %q", body)
	}
	if got := r.Manifest().Findings; len(got) != 1 || got[0].AgentID != "main-researcher-1" {
		t.Errorf("manifest findings wrong: %+v", got)
	}
}

// Re-appending the same agent id replaces (not duplicates) — defensive
// against a future re-spawn pattern.
func TestAppendFinding_ReplacesByAgentID(t *testing.T) {
	r := newRun(t, "x")
	for _, body := range []string{"first", "second", "third"} {
		if err := r.AppendFinding(Finding{AgentID: "a1", Status: "done", Result: body}); err != nil {
			t.Fatalf("Append %s: %v", body, err)
		}
	}
	if got := r.Manifest().Findings; len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got))
	}
	body, _ := os.ReadFile(filepath.Join(r.Dir(), "findings", "a1.md"))
	if string(body) != "third" {
		t.Errorf("body not replaced: %q", body)
	}
}

// AppendEvent appends one JSON-encoded line per call. Pre-populated timestamp
// is preserved; missing one is filled in. Test calls Finalize at the end so
// Windows can clean the TempDir — the events handle is held open until then,
// matching how real runs always reach Finalize.
func TestAppendEvent_JSONL(t *testing.T) {
	r := newRun(t, "x")
	if err := r.AppendEvent(Event{Kind: "tool_call", Tool: "web_search", Args: `{"q":"x"}`}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if err := r.AppendEvent(Event{Kind: "tool_result", Tool: "web_search", Result: "ok"}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(r.Dir(), "events.jsonl"))
	if err != nil {
		t.Fatalf("events file missing: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 events, got %d: %q", len(lines), b)
	}
	for i, line := range lines {
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("event %d not valid JSON: %v", i, err)
		}
		if e.Timestamp.IsZero() {
			t.Errorf("event %d timestamp not auto-set", i)
		}
	}
	// Close handles so TempDir cleanup can unlink on Windows.
	_ = r.Finalize("", nil, StatusDone, "")
}

// Finalize seals the run: writes report.md, sets terminal status, computes a
// preview, closes the events handle.
func TestFinalize_SealsRun(t *testing.T) {
	r := newRun(t, "x")
	_ = r.AppendEvent(Event{Kind: "tool_call", Tool: "x"})
	report := "# Title\n\nReal report body that's long enough to be useful but reasonable to preview."
	refs := []string{"https://a", "wiki:b.md"}
	if err := r.Finalize(report, refs, StatusDone, ""); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(r.Dir(), "report.md"))
	if err != nil {
		t.Fatalf("report.md missing: %v", err)
	}
	if string(body) != report {
		t.Errorf("report body wrong")
	}
	m := r.Manifest()
	if m.Status != StatusDone {
		t.Errorf("status = %q, want %q", m.Status, StatusDone)
	}
	if m.Finished.IsZero() {
		t.Error("Finished not set")
	}
	if len(m.References) != 2 {
		t.Errorf("references not persisted: %+v", m.References)
	}
	if !strings.Contains(m.ReportPreview, "Title") {
		t.Errorf("preview wrong: %q", m.ReportPreview)
	}
}

// Finalize is idempotent — a second call is a no-op (covers the truncated-but-
// completed agent re-finalize race in the wave-timeout salvage path).
func TestFinalize_Idempotent(t *testing.T) {
	r := newRun(t, "x")
	if err := r.Finalize("first", nil, StatusDone, ""); err != nil {
		t.Fatalf("first Finalize: %v", err)
	}
	if err := r.Finalize("second", nil, StatusError, "boom"); err != nil {
		t.Fatalf("second Finalize: %v", err)
	}
	body, _ := os.ReadFile(filepath.Join(r.Dir(), "report.md"))
	if string(body) != "first" {
		t.Errorf("second Finalize clobbered report: %q", body)
	}
	if r.Manifest().Status != StatusDone {
		t.Error("second Finalize changed status — should be no-op")
	}
}

// Post-finalize Append calls are silently dropped (not errors — late-arriving
// events from a cancelled sub-agent must never panic the orchestrator).
func TestAppendAfterFinalize_Silent(t *testing.T) {
	r := newRun(t, "x")
	_ = r.Finalize("done", nil, StatusDone, "")
	if err := r.AppendFinding(Finding{AgentID: "late", Result: "nope"}); err != nil {
		t.Errorf("AppendFinding after Finalize errored: %v", err)
	}
	if err := r.AppendEvent(Event{Kind: "late"}); err != nil {
		t.Errorf("AppendEvent after Finalize errored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.Dir(), "findings", "late.md")); err == nil {
		t.Error("late finding leaked to disk")
	}
}

// List returns runs most-recent-first and respects the limit.
func TestList_OrderAndLimit(t *testing.T) {
	s := mustStore(t)
	for i := 0; i < 4; i++ {
		r, err := s.Create(Manifest{Question: "q" + string(rune('a'+i))})
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		_ = r.Finalize("body", nil, StatusDone, "")
		// Sleep a tick so timestamps differ — otherwise the secondary id
		// sort kicks in but the test is clearer with distinct stamps.
		time.Sleep(2 * time.Millisecond)
	}
	got, err := s.List(0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("want 4 runs, got %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Started.Before(got[i].Started) {
			t.Errorf("list not sorted desc: %v before %v", got[i-1].Started, got[i].Started)
		}
	}
	// Limit applies.
	got2, _ := s.List(2)
	if len(got2) != 2 {
		t.Errorf("limit=2 returned %d", len(got2))
	}
	if got2[0].ID != got[0].ID {
		t.Errorf("limited list lost newest: %q vs %q", got2[0].ID, got[0].ID)
	}
}

// List tolerates a corrupt run.json — skip the bad entry, return the rest.
// Same principle as the episodic-hydrate fix: one bad row must not block the
// whole list.
func TestList_TolerantToCorruptManifest(t *testing.T) {
	s := mustStore(t)
	r, _ := s.Create(Manifest{Question: "good"})
	_ = r.Finalize("body", nil, StatusDone, "")
	// Poison: create a bad run.
	badDir := filepath.Join(s.Dir(), "20260523-999999-bad")
	_ = os.MkdirAll(badDir, 0o755)
	_ = os.WriteFile(filepath.Join(badDir, "run.json"), []byte("this is not json"), 0o644)
	got, err := s.List(0)
	if err != nil {
		t.Fatalf("List should tolerate corrupt: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("want 1 (good) run, got %d", len(got))
	}
	if got[0].ID != r.ID() {
		t.Errorf("wrong run returned: %q", got[0].ID)
	}
}

// Get returns manifest + the report body, errors on missing.
func TestGet_RoundTrip(t *testing.T) {
	s := mustStore(t)
	r, _ := s.Create(Manifest{Question: "x"})
	_ = r.AppendFinding(Finding{AgentID: "a", Status: "done", Result: "raw"})
	_ = r.Finalize("# Final\nreport", []string{"https://x"}, StatusDone, "")

	m, body, err := s.Get(r.ID())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if m.Question != "x" || m.Status != StatusDone {
		t.Errorf("manifest wrong: %+v", m)
	}
	if !strings.Contains(body, "# Final") {
		t.Errorf("body wrong: %q", body)
	}
	if _, _, err := s.Get("nonexistent"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing run should be os.ErrNotExist, got: %v", err)
	}
}

// GetFinding reads the raw subagent body — useful for /research show <id> drill-in.
func TestGetFinding(t *testing.T) {
	s := mustStore(t)
	r, _ := s.Create(Manifest{Question: "x"})
	_ = r.AppendFinding(Finding{AgentID: "a1", Status: "done", Result: "raw a1"})
	body, err := s.GetFinding(r.ID(), "a1")
	if err != nil {
		t.Fatalf("GetFinding: %v", err)
	}
	if body != "raw a1" {
		t.Errorf("body wrong: %q", body)
	}
	if _, err := s.GetFinding(r.ID(), "missing"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing finding should be os.ErrNotExist: %v", err)
	}
}

// Delete purges the directory. Deleting a missing id is fine (intent-satisfied).
func TestDelete(t *testing.T) {
	s := mustStore(t)
	r, _ := s.Create(Manifest{Question: "x"})
	if err := s.Delete(r.ID()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(r.Dir()); !os.IsNotExist(err) {
		t.Errorf("dir not removed: %v", err)
	}
	// Idempotent.
	if err := s.Delete(r.ID()); err != nil {
		t.Errorf("delete-missing should not error: %v", err)
	}
}

// Delete rejects path-traversal ids — defensive even though our own ids are
// clean, an operator might paste a hand-typed id with "..".
func TestDelete_RejectsUnsafeID(t *testing.T) {
	s := mustStore(t)
	bad := []string{"..", "../etc", "a/b", "a\\b", `a"b`, "."}
	for _, id := range bad {
		if err := s.Delete(id); err == nil {
			t.Errorf("Delete(%q) should reject, accepted", id)
		}
	}
}

// Concurrent AppendFinding from many goroutines must not interleave / corrupt
// the manifest. (Sub-agents fan out + finalize concurrently in real runs.)
func TestAppendFinding_Concurrent(t *testing.T) {
	r := newRun(t, "concurrent")
	const n = 25
	var wg sync.WaitGroup
	wg.Add(n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			err := r.AppendFinding(Finding{
				AgentID: "agent-" + string(rune('a'+i%26)) + string(rune('0'+i)),
				Status:  "done",
				Result:  "body " + string(rune('0'+i%10)),
			})
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent AppendFinding: %v", err)
	}
	m := r.Manifest()
	if len(m.Findings) != n {
		t.Errorf("want %d findings, got %d", n, len(m.Findings))
	}
	// Re-read manifest from disk — must round-trip cleanly (no torn write).
	b, err := os.ReadFile(filepath.Join(r.Dir(), "run.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var on Manifest
	if err := json.Unmarshal(b, &on); err != nil {
		t.Fatalf("manifest torn under concurrent writes: %v", err)
	}
	if len(on.Findings) != n {
		t.Errorf("on-disk findings = %d, want %d", len(on.Findings), n)
	}
}

// SetCycles updates the cycles-completed counter (called between reflection
// cycles).
func TestSetCycles(t *testing.T) {
	r := newRun(t, "x")
	r.SetCycles(2)
	if r.Manifest().Cycles != 2 {
		t.Errorf("Cycles = %d, want 2", r.Manifest().Cycles)
	}
	_ = r.Finalize("body", nil, StatusDone, "")
	r.SetCycles(5) // post-finalize: no-op
	if r.Manifest().Cycles != 2 {
		t.Errorf("Cycles changed after Finalize: %d", r.Manifest().Cycles)
	}
}

// safeFilename strips path separators / odd characters from a subagent id
// before using it as a filename. Tenant's ids are clean, but defensive.
func TestSafeFilename(t *testing.T) {
	cases := []struct{ in, want string }{
		{"main-researcher-1", "main-researcher-1"},
		{"agent/with/slashes", "agent_with_slashes"},
		{`a:b*c?"d`, "a_b_c__d"},
		{"", "agent"},
	}
	for _, c := range cases {
		if got := safeFilename(c.in); got != c.want {
			t.Errorf("safeFilename(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// safeID is the path-traversal guard for incoming ids.
func TestSafeID(t *testing.T) {
	good := []string{"20260523-110000-x", "abc", "a-b_c"}
	bad := []string{"", ".", "..", "a/b", "a\\b", `a"b`, "a..b", "a:b"}
	for _, id := range good {
		if !safeID(id) {
			t.Errorf("safeID(%q) rejected a good id", id)
		}
	}
	for _, id := range bad {
		if safeID(id) {
			t.Errorf("safeID(%q) accepted a bad id", id)
		}
	}
}

// --- helpers ---

func mustStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func newRun(t *testing.T, q string) *Run {
	t.Helper()
	s := mustStore(t)
	r, err := s.Create(Manifest{Question: q})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Windows file-lock guard: ensure handles released before TempDir cleanup
	// runs (which happens AFTER subtests, in any order). Close is idempotent
	// with Finalize, so tests that finalize explicitly aren't disturbed.
	t.Cleanup(r.Close)
	return r
}
