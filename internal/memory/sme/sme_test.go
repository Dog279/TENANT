package sme_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"tenant/internal/memory/sme"
)

func mkStore(t *testing.T) *sme.Store {
	t.Helper()
	// Real facts schema isn't needed — sme tables are independent siblings.
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "facts.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := sme.New(db)
	if err != nil {
		t.Fatalf("sme.New: %v", err)
	}
	return s
}

func TestUpsertSection_VersionsAndActive(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()

	v1, err := s.UpsertSection(ctx, sme.Section{AgentID: "main", Section: "Architecture", Body: "v1 body", SourceFactIDs: []int64{1, 2}})
	if err != nil || v1 != 1 {
		t.Fatalf("first upsert: v=%d err=%v, want 1", v1, err)
	}
	v2, err := s.UpsertSection(ctx, sme.Section{AgentID: "main", Section: "Architecture", Body: "v2 body", SourceFactIDs: []int64{3}})
	if err != nil || v2 != 2 {
		t.Fatalf("second upsert: v=%d err=%v, want 2", v2, err)
	}
	// A different section starts back at version 1.
	if v, _ := s.UpsertSection(ctx, sme.Section{AgentID: "main", Section: "Gotchas", Body: "careful"}); v != 1 {
		t.Errorf("new section version = %d, want 1", v)
	}

	active, err := s.ActiveSections(ctx, "", "main")
	if err != nil {
		t.Fatalf("ActiveSections: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("active sections = %d, want 2 (latest per section)", len(active))
	}
	byName := map[string]sme.Section{}
	for _, a := range active {
		byName[a.Section] = a
	}
	if byName["Architecture"].Body != "v2 body" || byName["Architecture"].Version != 2 {
		t.Errorf("Architecture active = %+v, want v2 body / v2", byName["Architecture"])
	}
	if len(byName["Architecture"].SourceFactIDs) != 1 || byName["Architecture"].SourceFactIDs[0] != 3 {
		t.Errorf("Architecture source ids = %v, want [3]", byName["Architecture"].SourceFactIDs)
	}
}

func TestRenderActive(t *testing.T) {
	s := mkStore(t)
	ctx := context.Background()
	_, _ = s.UpsertSection(ctx, sme.Section{AgentID: "main", Section: "Architecture", Body: "uses MCP over SQLite"})
	_, _ = s.UpsertSection(ctx, sme.Section{AgentID: "main", Section: "Gotchas", Body: "OAuth option order matters"})

	out, err := s.RenderActive(ctx, "", "main")
	if err != nil {
		t.Fatalf("RenderActive: %v", err)
	}
	for _, want := range []string{"Project knowledge (SME)", "not instructions", "### Architecture", "uses MCP over SQLite", "### Gotchas", "OAuth option order"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
	// Empty store renders nothing.
	empty, _ := s.RenderActive(ctx, "", "other-agent")
	if empty != "" {
		t.Errorf("empty SME should render \"\", got %q", empty)
	}
}

// Render must return "" — not a bare header — when every section is empty
// (review finding 3), so an all-empty doc never injects a header-only block.
func TestRender_AllEmptyBodiesYieldsEmpty(t *testing.T) {
	out := sme.Render([]sme.Section{
		{Section: "Architecture", Body: "   "},
		{Section: "Gotchas", Body: ""},
	})
	if out != "" {
		t.Errorf("all-empty sections should render \"\", got %q", out)
	}
	// A single non-empty section emits the header once.
	out = sme.Render([]sme.Section{
		{Section: "Architecture", Body: "real content"},
		{Section: "Gotchas", Body: ""},
	})
	if !strings.Contains(out, "Project knowledge (SME)") || !strings.Contains(out, "real content") || strings.Contains(out, "### Gotchas") {
		t.Errorf("expected header + non-empty section only, got:\n%s", out)
	}
}

func TestLive_SetStringNilSafe(t *testing.T) {
	var nilLive *sme.Live
	if nilLive.String() != "" {
		t.Error("nil Live.String() should be empty")
	}
	nilLive.Set("ignored") // must not panic

	l := sme.NewLive()
	if l.String() != "" {
		t.Error("fresh Live should be empty")
	}
	l.Set("doc body")
	if l.String() != "doc body" {
		t.Errorf("Live.String() = %q, want %q", l.String(), "doc body")
	}
}
