package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tenant/internal/memory/episodic"
	"tenant/internal/route"
)

func TestExportRouteDatasetJoinsAndExcludes(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Now().UTC()

	mkDecision := func(id, surface string, tier route.Tier, reason string) route.Decision {
		return route.Decision{
			ID: id, Timestamp: now, AgentID: "main", Status: route.StatusOK,
			Domain: "software", Complexity: 4, Code: true, Label: "big",
			Justification: "j", SelectedTier: tier, PolicyReason: reason,
			Mode: route.ModeShadow, Surface: surface, PromptHash: route.HashPrompt(id),
		}
	}
	decisions := []route.Decision{
		mkDecision("d-small-ok", "tui", route.TierSmall, string(route.ReasonMinComplexity)),
		mkDecision("d-relay", "relay", route.TierSmall, string(route.ReasonMinComplexity)),
		mkDecision("d-unjoined", "tui", route.TierSmall, string(route.ReasonMinComplexity)),
		mkDecision("d-gold", "tui", route.TierBig, string(route.ReasonOperatorBig)),
	}
	var lines strings.Builder
	for _, d := range decisions {
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		lines.Write(raw)
		lines.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dataDir, route.DecisionLogName), []byte(lines.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := episodic.Open(filepath.Join(dataDir, "episodes.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	insert := func(decisionID, prompt, outcome string) {
		t.Helper()
		_, err := store.Insert(ctx, &episodic.Episode{
			AgentID: "main", Prompt: prompt, Response: "r", Outcome: outcome,
			Timestamp: now, EmbedderID: "test", Embedding: []float32{0.1},
			Tags: []string{route.TagDecisionPrefix + decisionID},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	insert("d-small-ok", "refactor the auth handler", "success")
	insert("d-relay", "private relay message", "success")
	insert("d-gold", "hard task", "success")
	store.Close()

	out := filepath.Join(dataDir, "out.jsonl")
	summary, err := exportRouteDataset(ctx, dataDir, exportOptions{Out: out})
	if err != nil {
		t.Fatalf("exportRouteDataset: %v", err)
	}
	if !strings.Contains(summary, "2 row(s) (1 gold)") {
		t.Fatalf("summary = %q, want 2 rows / 1 gold", summary)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	if strings.Contains(content, "private relay message") {
		t.Fatal("relay content leaked into dataset without --include-relay")
	}
	if !strings.Contains(content, "refactor the auth handler") || !strings.Contains(content, "hard task") {
		t.Fatalf("expected joined rows missing:\n%s", content)
	}

	// Opt-in includes the relay row.
	summary, err = exportRouteDataset(ctx, dataDir, exportOptions{Out: out, IncludeRelay: true})
	if err != nil {
		t.Fatalf("exportRouteDataset(include-relay): %v", err)
	}
	if !strings.Contains(summary, "3 row(s)") {
		t.Fatalf("summary = %q, want 3 rows with relay opt-in", summary)
	}
}
