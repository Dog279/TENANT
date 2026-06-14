package main

import (
	"log/slog"
	"testing"

	"tenant/internal/model"
)

// TEN-195: the improve.profile resolver decides whether the self-improvement
// PROPOSER/reflection jobs run on a pinned reasoning model or fall back to the
// main router. Empty/unknown/no-provider ⇒ main (fail-soft); a buildable
// profile ⇒ a distinct pinned router + its model id for provenance.

func newTestMainRouter(t *testing.T) *model.Router {
	t.Helper()
	return model.NewRouter(model.NewEmptyRegistry(), slog.Default())
}

func TestImproveProposerRouter_EmptyProfile_UsesMain(t *testing.T) {
	main := newTestMainRouter(t)
	got, modelID := improveProposerRouter("", main, map[string]*agentProfile{}, &launchConfig{}, t.TempDir(), model.Profile{}, slog.Default())
	if got != main {
		t.Fatalf("empty profile should return the main router")
	}
	if modelID != "" {
		t.Fatalf("empty profile modelID = %q, want empty", modelID)
	}
}

func TestImproveProposerRouter_UnknownProfile_FallsBackToMain(t *testing.T) {
	main := newTestMainRouter(t)
	agents := map[string]*agentProfile{"someone": {Provider: "local"}}
	got, modelID := improveProposerRouter("nope", main, agents, &launchConfig{}, t.TempDir(), model.Profile{}, slog.Default())
	if got != main {
		t.Fatalf("unknown profile should fall back to the main router")
	}
	if modelID != "" {
		t.Fatalf("unknown profile modelID = %q, want empty", modelID)
	}
}

func TestImproveProposerRouter_NoProvider_UsesMain(t *testing.T) {
	// A profile whose value is a soul (no pinned provider) is NOT a model swap.
	main := newTestMainRouter(t)
	agents := map[string]*agentProfile{"persona": {Provider: ""}}
	got, _ := improveProposerRouter("persona", main, agents, &launchConfig{}, t.TempDir(), model.Profile{}, slog.Default())
	if got != main {
		t.Fatalf("no-provider profile should return the main router")
	}
}

func TestImproveProposerRouter_NilConfig_FallsBackToMain(t *testing.T) {
	main := newTestMainRouter(t)
	got, _ := improveProposerRouter("improver", main, nil, nil, t.TempDir(), model.Profile{}, slog.Default())
	if got != main {
		t.Fatalf("nil launch config should fall back to the main router")
	}
}

func TestImproveProposerRouter_ValidProfile_ReturnsPinned(t *testing.T) {
	main := newTestMainRouter(t)
	// A local vLLM provider needs no API key, so the router builds offline.
	lc := &launchConfig{
		Providers: map[string]*providerConfig{
			"local": {Kind: "vllm", Endpoint: "http://127.0.0.1:9999", Model: "improver-1"},
		},
		Agents: map[string]*agentProfile{
			"improver": {Provider: "local", Model: "improver-1"},
		},
	}
	got, modelID := improveProposerRouter("improver", main, effectiveAgents(lc), lc, t.TempDir(), model.Profile{}, slog.Default())
	if got == main {
		t.Fatalf("a buildable profile should return a DISTINCT pinned router, got the main router")
	}
	if got == nil {
		t.Fatalf("pinned router is nil")
	}
	if modelID != "improver-1" {
		t.Fatalf("provenance modelID = %q, want improver-1", modelID)
	}
}

func TestImproveProposerRouter_CaseInsensitiveProfileName(t *testing.T) {
	main := newTestMainRouter(t)
	lc := &launchConfig{
		Providers: map[string]*providerConfig{
			"local": {Kind: "vllm", Endpoint: "http://127.0.0.1:9999", Model: "improver-1"},
		},
		Agents: map[string]*agentProfile{
			"Improver": {Provider: "local", Model: "improver-1"},
		},
	}
	// Request lower-case "improver" against the "Improver" entry.
	got, _ := improveProposerRouter("improver", main, effectiveAgents(lc), lc, t.TempDir(), model.Profile{}, slog.Default())
	if got == main || got == nil {
		t.Fatalf("case-insensitive lookup should resolve to the pinned router")
	}
}
