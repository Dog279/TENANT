package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"tenant/internal/agent"
	"tenant/internal/model"
	"tenant/internal/memory/working"
)

func newEchoAg(t *testing.T) *agent.Agent {
	t.Helper()
	r, err := buildRouter(&commonFlags{backend: "echo"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("buildRouter(echo): %v", err)
	}
	ag, err := agent.New(agent.Config{
		AgentID: "main", Router: r, Working: working.New(),
		Tools:      agent.NewStaticRegistry(),
		Dispatcher: agent.DispatcherFunc(func(context.Context, model.ToolCall) (string, bool, error) { return "", false, nil }),
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return ag
}

// TestUseModel_PinsModelVariant — the operator's exact ask: switch to
// a provider AND pin a specific model variant in one command. Verifies
// the model field on the provider is updated on disk.
func TestUseModel_PinsModelVariant(t *testing.T) {
	dir := t.TempDir()
	lc := &launchConfig{
		Provider: "echo",
		Providers: map[string]*providerConfig{
			"echo": {Kind: "echo"},
			"zai":  {Kind: "zai-coding", Endpoint: "https://api.z.ai/api/coding/paas/v4", Model: "glm-4.6", ToolFmt: "openai"},
		},
	}
	if err := lc.save(dir); err != nil {
		t.Fatal(err)
	}
	ag := newEchoAg(t)
	mc := &modelControl{cfgDir: dir, dataDir: filepath.Join(dir, "data"), agentID: "main", ag: ag,
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// Swap to zai + pin glm-5.1. Probe will degrade (no live server)
	// but the persist + override should land regardless.
	_, _, _ = mc.UseModel("zai", "glm-5.1")
	got, _ := loadLaunchConfig(dir)
	if got.Provider != "zai" {
		t.Errorf("primary not switched: %q", got.Provider)
	}
	if got.Providers["zai"].Model != "glm-5.1" {
		t.Errorf("model not pinned: got %q, want glm-5.1", got.Providers["zai"].Model)
	}
}

// TestUseModel_EmptyModelPreservesSaved — pinning is opt-in; empty
// model override MUST NOT clobber the operator's saved variant.
// Drift guard against an over-aggressive trim that would convert "" to
// something destructive.
func TestUseModel_EmptyModelPreservesSaved(t *testing.T) {
	dir := t.TempDir()
	lc := &launchConfig{
		Provider: "echo",
		Providers: map[string]*providerConfig{
			"echo": {Kind: "echo"},
			"zai":  {Kind: "zai-coding", Endpoint: "https://api.z.ai/api/coding/paas/v4", Model: "glm-5.1", ToolFmt: "openai"},
		},
	}
	if err := lc.save(dir); err != nil {
		t.Fatal(err)
	}
	ag := newEchoAg(t)
	mc := &modelControl{cfgDir: dir, dataDir: filepath.Join(dir, "data"), agentID: "main", ag: ag,
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	_, _, _ = mc.UseModel("zai", "")
	got, _ := loadLaunchConfig(dir)
	if got.Providers["zai"].Model != "glm-5.1" {
		t.Errorf("empty override should preserve glm-5.1; got %q", got.Providers["zai"].Model)
	}
}

// TestUseModel_WhitespaceOverrideAlsoPreserves — defensive trim. An
// operator typing "/model use zai " (trailing space) shouldn't blank
// out the model.
func TestUseModel_WhitespaceOverrideAlsoPreserves(t *testing.T) {
	dir := t.TempDir()
	lc := &launchConfig{
		Provider:  "echo",
		Providers: map[string]*providerConfig{"echo": {Kind: "echo"}, "zai": {Kind: "zai-coding", Model: "glm-5.1"}},
	}
	_ = lc.save(dir)
	ag := newEchoAg(t)
	mc := &modelControl{cfgDir: dir, dataDir: filepath.Join(dir, "data"), agentID: "main", ag: ag,
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_, _, _ = mc.UseModel("zai", "   ")
	got, _ := loadLaunchConfig(dir)
	if got.Providers["zai"].Model != "glm-5.1" {
		t.Errorf("whitespace override should preserve; got %q", got.Providers["zai"].Model)
	}
}

// TestListProviderModels_LiveFetch — the discovery path. Stubs an
// httptest server that returns an OpenAI-shaped /v1/models response;
// verifies modelControl.ListProviderModels parses + sorts the IDs.
func TestListProviderModels_LiveFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "glm-5.1"},
				{"id": "glm-4.6"},
				{"id": "glm-5"},
			},
		})
	}))
	defer srv.Close()

	dir := t.TempDir()
	lc := &launchConfig{
		Provider:  "echo",
		Providers: map[string]*providerConfig{"echo": {Kind: "echo"}, "zai": {Kind: "vllm", Endpoint: srv.URL}},
	}
	_ = lc.save(dir)
	ag := newEchoAg(t)
	mc := &modelControl{cfgDir: dir, dataDir: filepath.Join(dir, "data"), agentID: "main", ag: ag,
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	ids, err := mc.ListProviderModels("zai")
	if err != nil {
		t.Fatal(err)
	}
	// Alphabetical sort: glm-4.6 < glm-5 < glm-5.1
	want := []string{"glm-4.6", "glm-5", "glm-5.1"}
	if len(ids) != len(want) {
		t.Fatalf("got %d ids, want %d: %v", len(ids), len(want), ids)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("sort order wrong at %d: got %q, want %q (full: %v)", i, ids[i], want[i], ids)
		}
	}
}

// TestListProviderModels_FallbackTo_NoV1 — Z.ai-shape: /v1/models 404,
// /models succeeds. Validates the fallback chain from fetchModels
// reaches the discovery path correctly.
func TestListProviderModels_FallbackTo_NoV1(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{"id": "glm-4.6"}, {"id": "glm-5.1"}},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	dir := t.TempDir()
	lc := &launchConfig{
		Provider:  "echo",
		Providers: map[string]*providerConfig{"echo": {Kind: "echo"}, "zai": {Kind: "vllm", Endpoint: srv.URL}},
	}
	_ = lc.save(dir)
	ag := newEchoAg(t)
	mc := &modelControl{cfgDir: dir, dataDir: filepath.Join(dir, "data"), agentID: "main", ag: ag,
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ids, err := mc.ListProviderModels("zai")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Errorf("want 2 ids from /models fallback path; got %d: %v", len(ids), ids)
	}
}

// TestListProviderModels_EmptyNameUsesActive — convenience: blank name
// queries the currently-active provider. Avoids operators having to
// re-type the name when they just want to list what's on the box they
// already pointed at.
func TestListProviderModels_EmptyNameUsesActive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "x"}}})
	}))
	defer srv.Close()
	dir := t.TempDir()
	lc := &launchConfig{
		Provider:  "active",
		Providers: map[string]*providerConfig{"active": {Kind: "vllm", Endpoint: srv.URL}},
	}
	_ = lc.save(dir)
	ag := newEchoAg(t)
	mc := &modelControl{cfgDir: dir, dataDir: filepath.Join(dir, "data"), agentID: "main", ag: ag,
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ids, err := mc.ListProviderModels("")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "x" {
		t.Errorf("expected single 'x' from active provider; got %v", ids)
	}
}

// TestListProviderModels_UnknownProvider — friendly error, not panic.
func TestListProviderModels_UnknownProvider(t *testing.T) {
	dir := t.TempDir()
	lc := &launchConfig{Provider: "echo", Providers: map[string]*providerConfig{"echo": {Kind: "echo"}}}
	_ = lc.save(dir)
	ag := newEchoAg(t)
	mc := &modelControl{cfgDir: dir, dataDir: filepath.Join(dir, "data"), agentID: "main", ag: ag,
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_, err := mc.ListProviderModels("ghost")
	if err == nil {
		t.Error("unknown provider should error, not panic")
	}
}
