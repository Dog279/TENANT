package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/memory/soul"
)

func TestMemControl_RecentShowsFeedbackMarkers(t *testing.T) {
	es, err := episodic.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ins := func(p string) int64 {
		id, err := es.Insert(ctx, &episodic.Episode{
			AgentID: "main", Prompt: p, Response: "r:" + p, EmbedderID: "x", Embedding: []float32{1, 0},
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	acked := ins("good turn")
	undone := ins("bad turn")
	ins("neutral turn")
	if err := es.SetUserFeedback(ctx, acked, episodic.FeedbackAck); err != nil {
		t.Fatal(err)
	}
	if err := es.SetUserFeedback(ctx, undone, episodic.FeedbackUndo); err != nil {
		t.Fatal(err)
	}

	out := memControl{episodic: es, agentID: "main"}.Recent(10)
	if !strings.Contains(out, "✓") || !strings.Contains(out, "✗") {
		t.Fatalf("expected ✓/✗ markers in output:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "ep:"+strconv.FormatInt(acked, 10)+" ") && !strings.Contains(line, "✓") {
			t.Errorf("acked episode line missing ✓: %q", line)
		}
		if strings.Contains(line, "ep:"+strconv.FormatInt(undone, 10)+" ") && !strings.Contains(line, "✗") {
			t.Errorf("undone episode line missing ✗: %q", line)
		}
	}
}

func TestMemControl_SoulImportFromMarkdown(t *testing.T) {
	dir := t.TempDir()
	mdPath := filepath.Join(dir, "persona.md")
	md := "# Persona\n\nBe terse and direct.\n\n- Always cite your sources\n"
	if err := os.WriteFile(mdPath, []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "cfg")
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	c := memControl{cfgDir: cfg, agentID: "main"}

	out := c.SoulImport(mdPath)
	if !strings.Contains(out, "imported") {
		t.Fatalf("import: %q", out)
	}
	sl, err := soul.Load(cfg, "main")
	if err != nil {
		t.Fatalf("load soul after import: %v", err)
	}
	joined := strings.Join(sl.Instructions.Items, " | ")
	if !strings.Contains(joined, "terse") || !strings.Contains(joined, "cite your sources") {
		t.Fatalf("imported instructions wrong: %v", sl.Instructions.Items)
	}
	// View renders it.
	if !strings.Contains(c.SoulView(), "terse") {
		t.Error("SoulView should show imported instructions")
	}
}

func TestMemControl_SoulImportFolderSkipsOversized(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "mem")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(src, "IDENTITY.md"), []byte("You are Tenant, a terse assistant."), 0o644)
	_ = os.WriteFile(filepath.Join(src, "SOUL.md"), []byte("Always be helpful.\n\nNever fabricate."), 0o644)
	// Oversized file must be skipped, not blow the soul budget.
	big := make([]byte, soulImportCap+1)
	for i := range big {
		big[i] = 'x'
	}
	_ = os.WriteFile(filepath.Join(src, "CLAUDE.md"), big, 0o644)

	cfg := filepath.Join(dir, "cfg")
	_ = os.MkdirAll(cfg, 0o755)
	c := memControl{cfgDir: cfg, agentID: "main"}
	out := c.SoulImport(src)
	if !strings.Contains(out, "IDENTITY.md") || !strings.Contains(out, "SOUL.md") {
		t.Fatalf("should import IDENTITY+SOUL: %q", out)
	}
	if !strings.Contains(out, "skipped") || !strings.Contains(out, "CLAUDE.md") {
		t.Fatalf("should skip oversized CLAUDE.md: %q", out)
	}
	sl, _ := soul.Load(cfg, "main")
	j := strings.Join(sl.Instructions.Items, " | ")
	if !strings.Contains(j, "terse") || !strings.Contains(j, "fabricate") || strings.Contains(j, "xxxx") {
		t.Fatalf("imported content wrong: %v", sl.Instructions.Items)
	}
}

// Import must update the SAME soul object the agent holds, so rules
// apply next turn without a restart.
func TestMemControl_SoulImportUpdatesLiveSoul(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "RULES.md")
	if err := os.WriteFile(f, []byte("Prefer minimal code comments."), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "cfg")
	_ = os.MkdirAll(cfg, 0o755)
	live := soul.NewLive(soul.NewDefault("main")) // stands in for the agent's live soul
	c := memControl{cfgDir: cfg, agentID: "main", soulLive: live}

	c.SoulImport(f)
	// The live soul the agent renders must reflect the import (a fresh
	// pointer swapped in, not a torn in-place mutation).
	if !strings.Contains(strings.Join(live.Load().Instructions.Items, " "), "minimal code comments") {
		t.Fatalf("live soul not updated (would need a restart): %v", live.Load().Instructions.Items)
	}
}

func TestMemControl_RulesViewReflectsImport(t *testing.T) {
	dir := t.TempDir()
	rules := filepath.Join(dir, "RULES.md")
	if err := os.WriteFile(rules, []byte("Prefer minimal code comments, explain why not what.\n\nBe direct."), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "cfg")
	_ = os.MkdirAll(cfg, 0o755)
	c := memControl{cfgDir: cfg, agentID: "main"}

	if v := c.RulesView(); !strings.Contains(v, "no operating rules") {
		t.Fatalf("empty rules view wrong: %q", v)
	}
	c.SoulImport(rules)
	v := c.RulesView()
	if !strings.Contains(v, "minimal code comments") {
		t.Fatalf("rules view should show imported rule: %q", v)
	}
}

func TestMemControl_Forget(t *testing.T) {
	ctx := context.Background()
	ss, err := semantic.Open(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	id, err := ss.Insert(ctx, &semantic.Fact{
		AgentID: "main", Visibility: semantic.VisibilityPrivate, Fact: "user likes Go",
		Confidence: 0.9, EmbedderID: "t/2", Embedding: []float32{1, 0},
	})
	if err != nil {
		t.Fatalf("insert fact: %v", err)
	}
	es, _ := episodic.Open(filepath.Join(t.TempDir(), "e.db"))
	defer es.Close()
	c := memControl{episodic: es, semantic: ss, agentID: "main"}

	if out := c.Forget("fact:" + strconv.FormatInt(id, 10)); !strings.Contains(out, "forgot fact") {
		t.Fatalf("forget: %q", out)
	}
	if n, _ := ss.Count(ctx, false, false); n != 0 {
		t.Fatalf("fact not tombstoned: count=%d", n)
	}
	if out := c.Forget("bogus"); !strings.Contains(out, "usage") {
		t.Errorf("bad target should show usage: %q", out)
	}
}
