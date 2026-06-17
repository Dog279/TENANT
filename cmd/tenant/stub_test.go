package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"tenant/internal/model"
)

// With no plugin flags, every plugin should still appear in /tools as a
// disabled stub; enabling one and calling it returns a "needs setup"
// status rather than failing to load (the API-runs-even-unauthenticated
// behavior).
func TestBuildToolMux_StubsUnconfiguredPlugins(t *testing.T) {
	c := &commonFlags{agent: "main", dataDir: t.TempDir()}
	mux, _, cleanup, err := buildToolMux(context.Background(), c, nil, &pluginFlags{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("buildToolMux: %v", err)
	}
	defer cleanup()

	seen := map[string]bool{}
	for _, ti := range mux.ToolList() {
		seen[ti.Plugin] = true
		// memory_search is enabled by default as of TEN-249 (active recall of the
		// agent's own long-term memory + the federated path always exists) — it is
		// NOT an unconfigured stub, so it's exempt from the disabled-by-default rule.
		if ti.Name == "memory_search" {
			if !ti.Enabled {
				t.Errorf("memory_search should be enabled by default (TEN-249)")
			}
			continue
		}
		if ti.Enabled {
			t.Errorf("unconfigured tool %s should start disabled", ti.Name)
		}
	}
	// NOTE: "web" is intentionally absent from the shared mux — it's
	// registered per-agent via TeamRuntime.addWebTool so each agent
	// gets its own Chrome session. (Previously web appeared as a
	// duplicate stub here AND as a live entry in the per-agent mux,
	// which broke /enable persistence and showed dupes in /tools.)
	for _, p := range []string{"os", "sql", "wiki", "gsuite", "x", "imessage"} {
		if !seen[p] {
			t.Errorf("plugin %q not visible in /tools", p)
		}
	}
	if seen["web"] {
		t.Error("web should NOT appear in shared mux — it's per-agent now")
	}

	// Enable a stub, then call it → needs-setup status (not a crash).
	mux.SetEnabled("sql", true)
	out, isErr, _ := mux.Dispatch(context.Background(),
		model.ToolCall{Name: "sql_query", Arguments: []byte(`{"sql":"SELECT 1"}`)})
	if !isErr || !strings.Contains(out, "not configured") {
		t.Fatalf("stub should return a needs-setup status, got: isErr=%v %q", isErr, out)
	}
}
