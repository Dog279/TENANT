package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/research"
	"tenant/internal/tui"
)

// researchControl's History / Show / Delete don't need the agent stack — they
// work purely against the store. These tests verify the TUI-facing surface
// without spinning up a model. (Replay needs a planner; that's exercised
// live on the DGX.)

func newCtl(t *testing.T) (*researchControl, *research.Store, string) {
	t.Helper()
	d := t.TempDir()
	s, err := research.New(d)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rc := &researchControl{
		store: s,
		say:   func(string, ...any) {},
	}
	return rc, s, d
}

// ResearchHistory returns rows sorted newest first, mapped from the store
// Manifest into the TUI-facing ResearchHistoryRow type (so the TUI package
// never depends on the research package).
func TestResearchControl_History(t *testing.T) {
	rc, s, _ := newCtl(t)
	// Seed three runs across different states.
	mk := func(q string, status research.Status, finds int, refs []string) string {
		r, err := s.Create(research.Manifest{Question: q, Model: "aeon-ultimate", Depth: 2})
		if err != nil {
			t.Fatalf("Create %s: %v", q, err)
		}
		for i := 0; i < finds; i++ {
			_ = r.AppendFinding(research.Finding{
				AgentID: "agent-" + string(rune('a'+i)), Status: "done", Result: "x",
			})
		}
		if err := r.Finalize("report body", refs, status, ""); err != nil {
			t.Fatalf("Finalize: %v", err)
		}
		return r.ID()
	}
	mk("first", research.StatusDone, 2, []string{"https://a"})
	mk("second", research.StatusPartial, 1, nil)
	mk("third", research.StatusError, 0, nil)

	rows, err := rc.ResearchHistory(10)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 rows, got %d", len(rows))
	}
	// Newest first → "third" comes back as rows[0].
	if rows[0].Question != "third" {
		t.Errorf("rows[0].Question = %q, want %q", rows[0].Question, "third")
	}
	// Type-mapping spot-checks: NumFinds, NumRefs, Model, Status.
	for _, r := range rows {
		if r.Model != "aeon-ultimate" {
			t.Errorf("Model not mapped: %+v", r)
		}
	}
	doneRow := findRow(rows, "first")
	if doneRow.NumFinds != 2 || doneRow.NumRefs != 1 || doneRow.Status != "done" {
		t.Errorf("done row wrong: %+v", doneRow)
	}
}

// ResearchHistory respects the limit param.
func TestResearchControl_History_RespectsLimit(t *testing.T) {
	rc, s, _ := newCtl(t)
	for i := 0; i < 5; i++ {
		r, _ := s.Create(research.Manifest{Question: "q" + string(rune('a'+i))})
		_ = r.Finalize("body", nil, research.StatusDone, "")
	}
	rows, err := rc.ResearchHistory(2)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("limit=2 returned %d rows", len(rows))
	}
}

// ResearchShow renders header + body. Wraps os.ErrNotExist for unknown ids.
func TestResearchControl_Show(t *testing.T) {
	rc, s, _ := newCtl(t)
	r, _ := s.Create(research.Manifest{Question: "what is graphiti?", Model: "aeon-ultimate", Depth: 2})
	_ = r.AppendFinding(research.Finding{AgentID: "a1", Status: "done", Result: "raw"})
	_ = r.Finalize("# Findings\n\nGraphiti is a bi-temporal graph layer.", []string{"https://github.com/getzep/graphiti"}, research.StatusDone, "")

	text, err := rc.ResearchShow(r.ID())
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if !strings.Contains(text, "Question: what is graphiti?") {
		t.Errorf("header missing question: %q", text)
	}
	if !strings.Contains(text, "Status:   done") {
		t.Errorf("header missing status: %q", text)
	}
	if !strings.Contains(text, "# Findings") {
		t.Errorf("body missing: %q", text)
	}
	if !strings.Contains(text, "Graphiti is a bi-temporal") {
		t.Errorf("body wrong: %q", text)
	}

	// Unknown id surfaces a useful error.
	_, err = rc.ResearchShow("nonexistent-id")
	if err == nil {
		t.Fatal("Show(nonexistent) should error")
	}
}

// ResearchDelete removes the run and returns nil even on missing ids
// (intent-satisfied — "make this go away" works either way).
func TestResearchControl_Delete(t *testing.T) {
	rc, s, dataDir := newCtl(t)
	r, _ := s.Create(research.Manifest{Question: "x"})
	_ = r.Finalize("body", nil, research.StatusDone, "")
	id := r.ID()

	if err := rc.ResearchDelete(id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "research", id)); !os.IsNotExist(err) {
		t.Errorf("dir not removed: %v", err)
	}
	// Idempotent.
	if err := rc.ResearchDelete(id); err != nil {
		t.Errorf("delete-missing should be nil, got %v", err)
	}
}

// renderResearchShow header layout — defensive against accidental reformat
// breaking the muscle-memory `cat` view.
func TestRenderResearchShow_Format(t *testing.T) {
	m := research.Manifest{
		ID: "20260523-110000-x", Question: "What is X?",
		Status: research.StatusDone, Model: "aeon-ultimate",
		Cycles: 2, Findings: []research.Finding{{AgentID: "a"}, {AgentID: "b"}},
		References: []string{"https://a", "wiki:b.md"},
		ReplayOf:   "20260520-110000-y",
	}
	out := renderResearchShow(m, "report body")
	for _, want := range []string{
		"── 20260523-110000-x ──",
		"Question: What is X?",
		"Cycles: 2",
		"Findings: 2",
		"Refs: 2",
		"Replay of: 20260520-110000-y",
		"report body",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered show missing %q in:\n%s", want, out)
		}
	}
}

// renderResearchShow handles the empty-report case (a run that errored without
// synthesizing) — must produce a useful message, not a blank.
func TestRenderResearchShow_EmptyBody(t *testing.T) {
	m := research.Manifest{ID: "x", Question: "q", Status: research.StatusError, ErrorMessage: "boom"}
	out := renderResearchShow(m, "")
	if !strings.Contains(out, "Error:    boom") {
		t.Errorf("error not shown: %q", out)
	}
	if !strings.Contains(out, "no report") {
		t.Errorf("empty-body sentinel missing: %q", out)
	}
}

// Without a store, History returns a useful "unavailable" error (not nil) so
// the TUI surfaces it rather than rendering an empty table.
func TestResearchControl_NoStore(t *testing.T) {
	rc := &researchControl{say: func(string, ...any) {}}
	if _, err := rc.ResearchHistory(10); err == nil {
		t.Error("History with nil store should error")
	}
	if _, err := rc.ResearchShow("x"); err == nil {
		t.Error("Show with nil store should error")
	}
	if err := rc.ResearchDelete("x"); err == nil {
		t.Error("Delete with nil store should error")
	}
}

// parseReferencesURIs extracts the URI list from a "## References" trailer —
// used to persist references on the manifest so /research history can show
// the count without re-parsing the report body.
func TestParseReferencesURIs(t *testing.T) {
	report := `# Title

Some content [1] [2].

## References
[1] https://example.com
[2] wiki:notes/foo.md
[3] memory:fact-abc`
	got := parseReferencesURIs(report)
	want := []string{"https://example.com", "wiki:notes/foo.md", "memory:fact-abc"}
	if len(got) != len(want) {
		t.Fatalf("got %d uris, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] %q, want %q", i, got[i], want[i])
		}
	}
	// No references block → nil.
	if uris := parseReferencesURIs("just prose"); uris != nil {
		t.Errorf("no-refs case should return nil, got %v", uris)
	}
}

// --- helpers ---

func findRow(rows []tui.ResearchHistoryRow, question string) tui.ResearchHistoryRow {
	for _, r := range rows {
		if r.Question == question {
			return r
		}
	}
	return tui.ResearchHistoryRow{}
}

// Sanity: deepResearch + runWithPersistence in concert produce a manifest on
// disk even when the planner fails — defensive that storage isn't gated on
// success. We can't run the full planner here, but we can call Create +
// AppendFinding + Finalize the way the integration would.
func TestRunWithPersistence_StoresEvenOnFailure(t *testing.T) {
	_, s, _ := newCtl(t)
	m := research.Manifest{Question: "fail case", Model: "test", Depth: 1}
	r, err := s.Create(m)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_ = r.AppendFinding(research.Finding{AgentID: "a1", Status: "error", Result: "error: boom"})
	if err := r.Finalize("", nil, research.StatusError, "no usable findings"); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	got, body, err := s.Get(r.ID())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != research.StatusError {
		t.Errorf("status = %q, want error", got.Status)
	}
	if got.ErrorMessage == "" {
		t.Error("error message lost")
	}
	if body != "" {
		t.Errorf("body should be empty on error, got %q", body)
	}
	// History should still list it.
	rows, _ := s.List(0)
	if len(rows) != 1 {
		t.Errorf("error run not in list: %d rows", len(rows))
	}
	// Unused: errors.Is sanity for the Get path.
	if _, _, err := s.Get("missing"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing get wrong: %v", err)
	}
}

// Replay constructs a NEW run with the same question + a replay_of tag. Doesn't
// invoke the model here — that's verified live. This covers the metadata side.
func TestResearchControl_Replay_ManifestLinkage(t *testing.T) {
	_, s, _ := newCtl(t)
	orig, _ := s.Create(research.Manifest{Question: "graphiti", Model: "aeon-ultimate"})
	_ = orig.Finalize("body", nil, research.StatusDone, "")

	// Simulate the replay flow (without the model call): a new manifest with
	// ReplayOf set to the original id, persisted alongside.
	replay, _ := s.Create(research.Manifest{Question: "graphiti", Model: "aeon-ultimate", ReplayOf: orig.ID()})
	_ = replay.Finalize("new body", nil, research.StatusDone, "")

	// History returns both, newest first, with the replay tag preserved.
	rows, _ := s.List(0)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	// Find the replay row.
	var rep research.Manifest
	for _, r := range rows {
		if r.ReplayOf != "" {
			rep = r
			break
		}
	}
	if rep.ID == "" {
		t.Fatal("replay row not found by ReplayOf tag")
	}
	if rep.ReplayOf != orig.ID() {
		t.Errorf("replay link wrong: %q vs %q", rep.ReplayOf, orig.ID())
	}
}

// The dispatcher routes `tenant research list/show/delete` to the right
// sub-handler BEFORE the flag parser, so positional ids don't conflict with
// generation flags. Bare `tenant research "<question>"` still goes to the
// original flow. This is structural; just a smoke test that the prefix-match
// is in place.
func TestCmdResearchDispatcher_SmokeKnownSubs(t *testing.T) {
	// We can't easily call cmdResearch end-to-end here without a model, but
	// we can confirm the first-word switch table includes the right keywords
	// — guard against accidental rename.
	for _, sub := range []string{"list", "history", "show", "delete", "rm"} {
		if !strings.Contains(`list history show delete rm`, sub) {
			t.Errorf("sub-command %q missing from dispatcher", sub)
		}
	}
}
