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

func rulesBoolPtr(v bool) *bool { return &v }

func TestResolveRouteConfigRules(t *testing.T) {
	lc := &launchConfig{Route: &routeConfig{
		Enabled: true, Mode: route.ModeShadow,
		Rules: []routeRuleConfig{
			{When: routeRuleWhen{Code: rulesBoolPtr(true), ComplexityGTE: 3}, Role: "coder"},
			{When: routeRuleWhen{Domain: "legal"}, Role: "planner-big"},
		},
	}}
	got, err := resolveRouteConfig(lc)
	if err != nil {
		t.Fatalf("resolveRouteConfig: %v", err)
	}
	if len(got.Rules) != 2 {
		t.Fatalf("rules = %d, want 2", len(got.Rules))
	}
	if got.Rules[0].Role != "coder" || got.Rules[0].When.ComplexityGTE != 3 {
		t.Fatalf("rule 0 mangled: %+v", got.Rules[0])
	}
	if got.LogRetentionDays != route.DefaultLogRetentionDays {
		t.Fatalf("retention default = %d, want %d", got.LogRetentionDays, route.DefaultLogRetentionDays)
	}
}

func TestResolveRouteConfigRuleValueValidation(t *testing.T) {
	for name, rules := range map[string][]routeRuleConfig{
		"missing role":    {{When: routeRuleWhen{Code: rulesBoolPtr(true)}}},
		"empty predicate": {{When: routeRuleWhen{}, Role: "coder"}},
		"bad complexity":  {{When: routeRuleWhen{ComplexityGTE: 9}, Role: "coder"}},
	} {
		t.Run(name, func(t *testing.T) {
			lc := &launchConfig{Route: &routeConfig{Enabled: true, Mode: route.ModeShadow, Rules: rules}}
			if _, err := resolveRouteConfig(lc); err == nil {
				t.Fatal("resolveRouteConfig accepted an invalid rule")
			}
		})
	}
}

// TestRoutePredicateVocabularyIsFrozen is the 4-field anti-scope-creep
// commitment: any unknown predicate key is a decode error, never a no-op.
func TestRoutePredicateVocabularyIsFrozen(t *testing.T) {
	blob := `{"route":{"enabled":true,"mode":"shadow","rules":[{"when":{"code":true,"language":"go"},"role":"coder"}]}}`
	var lc launchConfig
	err := json.Unmarshal([]byte(blob), &lc)
	if err == nil {
		t.Fatal("unknown predicate key decoded without error")
	}
	if !strings.Contains(err.Error(), "domain, complexity_gte, math, code") {
		t.Fatalf("rejection does not name the frozen vocabulary: %v", err)
	}
	// The four allowed keys still decode.
	ok := `{"route":{"enabled":true,"mode":"shadow","rules":[{"when":{"domain":"software","complexity_gte":2,"math":false,"code":true},"role":"coder"}]}}`
	if err := json.Unmarshal([]byte(ok), &lc); err != nil {
		t.Fatalf("full frozen vocabulary rejected: %v", err)
	}
}

func TestResolveRouteConfigRetention(t *testing.T) {
	for name, tt := range map[string]struct {
		configured int
		want       int
	}{
		"default 90":       {0, route.DefaultLogRetentionDays},
		"explicit":         {30, 30},
		"explicit disable": {-1, -1},
	} {
		t.Run(name, func(t *testing.T) {
			lc := &launchConfig{Route: &routeConfig{Enabled: true, Mode: route.ModeShadow, LogRetentionDays: tt.configured}}
			got, err := resolveRouteConfig(lc)
			if err != nil {
				t.Fatalf("resolveRouteConfig: %v", err)
			}
			if got.LogRetentionDays != tt.want {
				t.Fatalf("retention = %d, want %d", got.LogRetentionDays, tt.want)
			}
		})
	}
}

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
