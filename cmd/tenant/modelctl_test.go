package main

import (
	"context"
	"flag"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/agent"
	"tenant/internal/memory/working"
	"tenant/internal/model"
)

// The plan-loop ceiling defaults to 16, is overridable via config, and an
// explicit flag wins over config.
func TestPlanLoopCeiling_Configurable(t *testing.T) {
	dir := t.TempDir()
	build := func(args []string) model.Profile {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		c := bindCommon(fs)
		if err := fs.Parse(append([]string{"--config", dir, "--data", filepath.Join(dir, "data")}, args...)); err != nil {
			t.Fatal(err)
		}
		if err := c.resolve(); err != nil {
			t.Fatal(err)
		}
		r, err := buildRouter(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatalf("buildRouter: %v", err)
		}
		p, err := r.ForRole(model.RolePlanner)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	// Config with a vLLM provider + explicit model (no autodetect/network).
	cfg := &launchConfig{
		Provider:  "v",
		Providers: map[string]*providerConfig{"v": {Kind: "vllm", Endpoint: "http://127.0.0.1:1", Model: "m", ToolFmt: "qwen"}},
		Embed:     &providerConfig{Kind: "ollama", Endpoint: "http://127.0.0.1:1", Model: "e"},
	}
	if err := cfg.save(dir); err != nil {
		t.Fatal(err)
	}

	// Default → 16.
	if p := build(nil); p.PlanLoopCeiling != defaultPlanCeiling {
		t.Errorf("default ceiling = %d, want %d", p.PlanLoopCeiling, defaultPlanCeiling)
	}
	// Config override → 40.
	cfg.PlanLoopCeiling = 40
	if err := cfg.save(dir); err != nil {
		t.Fatal(err)
	}
	if p := build(nil); p.PlanLoopCeiling != 40 {
		t.Errorf("config ceiling = %d, want 40", p.PlanLoopCeiling)
	}
	// Explicit flag wins over config.
	if p := build([]string{"--plan-loop-ceiling", "25"}); p.PlanLoopCeiling != 25 {
		t.Errorf("flag ceiling = %d, want 25 (flag overrides config)", p.PlanLoopCeiling)
	}
}

func newEchoAgent(t *testing.T, log *slog.Logger) *agent.Agent {
	t.Helper()
	r, err := buildRouter(&commonFlags{backend: "echo"}, log)
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

// modelControl lists backends, hot-swaps the primary into the live agent, and
// adds a new backend — all persisted to config.json.
func TestModelControl_ListUseAdd(t *testing.T) {
	dir := t.TempDir()
	lc := &launchConfig{
		Provider: "a",
		Providers: map[string]*providerConfig{
			"a": {Kind: "echo"},
			"b": {Kind: "echo"},
		},
	}
	if err := lc.save(dir); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ag := newEchoAgent(t, log)
	r0 := ag.Router()
	mc := &modelControl{cfgDir: dir, dataDir: filepath.Join(dir, "data"), agentID: "main", ag: ag, log: log}

	// List: two backends, "a" active.
	list := mc.ModelList()
	if len(list) != 2 {
		t.Fatalf("ModelList = %d, want 2", len(list))
	}
	active := ""
	for _, mi := range list {
		if mi.Active {
			active = mi.Name
		}
	}
	if active != "a" {
		t.Fatalf("active = %q, want a", active)
	}

	// Use "b": persists primary + swaps the live router.
	if _, _, err := mc.UseModel("b", ""); err != nil {
		t.Fatalf("UseModel(b): %v", err)
	}
	got, _ := loadLaunchConfig(dir)
	if got.Provider != "b" {
		t.Fatalf("primary not persisted: %q", got.Provider)
	}
	// In-place swap: SAME router pointer (so sub-agents + jobs follow), mutated.
	if ag.Router() != r0 {
		t.Fatal("router pointer should be stable (mutated in place), not replaced")
	}
	if _, _, err := ag.Router().LLMForRole(context.Background(), model.RolePlanner); err != nil {
		t.Fatalf("router should still resolve a planner after swap: %v", err)
	}

	// Unknown name errors.
	if _, _, err := mc.UseModel("ghost", ""); err == nil {
		t.Fatal("UseModel(ghost) should error")
	}

	// Add a new vLLM backend → persisted, switchable.
	if _, err := mc.AddModel("dgx", "http://localhost:8000/", "qwen"); err != nil {
		t.Fatalf("AddModel: %v", err)
	}
	got, _ = loadLaunchConfig(dir)
	p := got.Providers["dgx"]
	if p == nil || p.Kind != "vllm" || p.ToolFmt != "qwen" || p.Endpoint != "http://localhost:8000" {
		t.Fatalf("added backend wrong: %+v", p)
	}

	// Remove a non-active backend (and its stored credential).
	creds, _ := loadCredentials(dir)
	creds.set("dgx", "sk-x")
	_ = creds.save(dir)
	if _, err := mc.RemoveModel("dgx"); err != nil {
		t.Fatalf("RemoveModel(dgx): %v", err)
	}
	got, _ = loadLaunchConfig(dir)
	if got.Providers["dgx"] != nil {
		t.Fatal("dgx not removed")
	}
	creds, _ = loadCredentials(dir)
	if _, ok := creds.Secrets["dgx"]; ok {
		t.Fatal("dgx credential not cleaned up")
	}

	// Removing the active model is refused.
	if _, err := mc.RemoveModel("b"); err == nil { // "b" is active from the UseModel above
		t.Fatal("removing the active model should be refused")
	}
}

// AddCloudModel registers a keyed cloud provider (zai/openai/grok/anthropic)
// using catalog defaults + stores the key in credentials.json (0600). This
// is the TUI-facing "set me up without dropping to the CLI wizard" path.
func TestModelControl_AddCloudModel(t *testing.T) {
	dir := t.TempDir()
	lc := &launchConfig{Provider: "a", Providers: map[string]*providerConfig{"a": {Kind: "echo"}}}
	if err := lc.save(dir); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ag := newEchoAgent(t, log)
	mc := &modelControl{cfgDir: dir, dataDir: filepath.Join(dir, "data"), agentID: "main", ag: ag, log: log}

	// Z.ai: catalog has endpoint + model + tool format, expects an API key.
	status, err := mc.AddCloudModel("zai", "sk-zai-test-key")
	if err != nil {
		t.Fatalf("AddCloudModel(zai): %v", err)
	}
	if !strings.Contains(status, "Z.ai") {
		t.Errorf("status missing provider label: %q", status)
	}
	if !strings.Contains(status, "/model use zai") {
		t.Errorf("status missing switch hint: %q", status)
	}

	got, _ := loadLaunchConfig(dir)
	p := got.Providers["zai"]
	if p == nil {
		t.Fatal("zai provider not registered")
	}
	if p.Kind != "zai" {
		t.Errorf("Kind = %q, want zai", p.Kind)
	}
	// As of 2026-05-26 the `zai` catalog default is the coding-plan
	// endpoint (operators on the coding plan are the common case).
	// Metered API access lives under the explicit `zai-metered` kind.
	if p.Endpoint != "https://api.z.ai/api/coding/paas/v4" {
		t.Errorf("Endpoint not pulled from catalog (expected coding-plan default): %q", p.Endpoint)
	}
	if p.Model != "glm-4.6" {
		t.Errorf("Model not pulled from catalog: %q", p.Model)
	}
	if p.ToolFmt != "openai" {
		t.Errorf("ToolFmt not pulled from catalog: %q", p.ToolFmt)
	}
	if p.Auth.Mode != "apikey" || !p.Auth.Stored {
		t.Errorf("Auth wrong: %+v", p.Auth)
	}
	// Key stored in credentials.json, NOT in providers (separation of concerns).
	creds, _ := loadCredentials(dir)
	if got := creds.get("zai"); got != "sk-zai-test-key" {
		t.Errorf("key not stored: %q", got)
	}

	// Embed provider auto-populated so a later /model use zai builds a full router.
	if got.Embed == nil || got.Embed.Kind != "ollama" {
		t.Errorf("embed provider not auto-populated: %+v", got.Embed)
	}

	// Re-running with a new key for the same provider OVERWRITES the secret
	// but preserves any custom model override.
	got.Providers["zai"].Model = "glm-4.5-air"
	_ = got.save(dir)
	if _, err := mc.AddCloudModel("zai", "sk-zai-newkey"); err != nil {
		t.Fatalf("AddCloudModel re-run: %v", err)
	}
	got2, _ := loadLaunchConfig(dir)
	if got2.Providers["zai"].Model != "glm-4.5-air" {
		t.Errorf("custom Model override clobbered on re-add: %q", got2.Providers["zai"].Model)
	}
	creds2, _ := loadCredentials(dir)
	if creds2.get("zai") != "sk-zai-newkey" {
		t.Errorf("key not updated on re-add: %q", creds2.get("zai"))
	}
}

// AddCloudModel rejects bad inputs with useful errors so the TUI can surface
// them to the user instead of silently no-op'ing.
func TestModelControl_AddCloudModel_Rejects(t *testing.T) {
	dir := t.TempDir()
	(&launchConfig{}).save(dir)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ag := newEchoAgent(t, log)
	mc := &modelControl{cfgDir: dir, dataDir: filepath.Join(dir, "data"), agentID: "main", ag: ag, log: log}

	cases := []struct {
		name, kind, key, wantSubstr string
	}{
		{"unknown kind", "made-up", "sk-x", "unknown provider kind"},
		{"empty key", "zai", "  ", "API key is required"},
		{"vllm not keyed", "vllm", "sk-x", "does not use an API key"},
		{"echo not keyed", "echo", "sk-x", "does not use an API key"},
	}
	for _, c := range cases {
		_, err := mc.AddCloudModel(c.kind, c.key)
		if err == nil {
			t.Errorf("[%s] AddCloudModel(%q, %q) should error", c.name, c.kind, c.key)
			continue
		}
		if !strings.Contains(err.Error(), c.wantSubstr) {
			t.Errorf("[%s] error %q should contain %q", c.name, err.Error(), c.wantSubstr)
		}
	}
}

// All four wired keyed providers (zai, openai, grok, anthropic) must have a
// non-empty DefaultEndpoint in the catalog — otherwise AddCloudModel can't
// pre-populate them. Guards against catalog drift.
// TestProviderKinds_ZaiCodingPlanEndpointShape — drift guard for the
// load-bearing distinction discovered 2026-05-26: the metered Z.ai
// route `/api/paas/v4` and the coding-plan route `/api/coding/paas/v4`
// are SEPARATE billing surfaces. An operator on the coding plan
// pointing at the metered route gets HTTP 429 "Insufficient balance"
// (which now maps to ErrInsufficientBalance — see vllm/classify_test.go).
// If a refactor accidentally collapses the kinds back to one URL,
// this test breaks BEFORE the user hits the billing wall again.
func TestProviderKinds_ZaiCodingPlanEndpointShape(t *testing.T) {
	cases := []struct {
		id          string
		mustContain string
		mustNotHave string
	}{
		// As of 2026-05-26, kind=zai DEFAULTS to the coding plan (most
		// common operator case). The metered route lives behind the
		// explicit `zai-metered` kind.
		{"zai", "/api/coding/paas/v4", ""},
		{"zai-coding", "/api/coding/paas/v4", ""},
		{"zai-coding-cn", "open.bigmodel.cn/api/coding/paas/v4", ""},
		// Metered: must NOT contain /coding/ — that distinction is the
		// whole point of the separate kind.
		{"zai-metered", "/api/paas/v4", "/coding/"},
	}
	for _, c := range cases {
		pk, ok := providerKinds[c.id]
		if !ok {
			t.Errorf("%s missing from providerKinds", c.id)
			continue
		}
		if !strings.Contains(pk.DefaultEndpoint, c.mustContain) {
			t.Errorf("%s endpoint %q must contain %q (route is the only thing distinguishing metered vs coding-plan)",
				c.id, pk.DefaultEndpoint, c.mustContain)
		}
		if c.mustNotHave != "" && strings.Contains(pk.DefaultEndpoint, c.mustNotHave) {
			t.Errorf("%s endpoint must NOT contain %q (got %q)", c.id, c.mustNotHave, pk.DefaultEndpoint)
		}
	}
	// providerOrder must include all variants so the setup wizard surfaces them.
	for _, id := range []string{"zai", "zai-coding", "zai-coding-cn", "zai-metered"} {
		found := false
		for _, o := range providerOrder {
			if o == id {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s missing from providerOrder — won't appear in the setup wizard menu", id)
		}
	}
}

func TestProviderKinds_KeyedHaveDefaults(t *testing.T) {
	for _, id := range []string{"zai", "zai-coding", "zai-coding-cn", "openai", "grok", "anthropic"} {
		pk, ok := providerKinds[id]
		if !ok {
			t.Errorf("%s missing from providerKinds", id)
			continue
		}
		if !pk.NeedsKey {
			t.Errorf("%s should have NeedsKey=true", id)
		}
		if !pk.Wired {
			t.Errorf("%s should have Wired=true (catalog entry exists, backend must too)", id)
		}
		if pk.DefaultEndpoint == "" {
			t.Errorf("%s missing DefaultEndpoint — /model add-cloud can't pre-populate", id)
		}
		if pk.DefaultModel == "" {
			t.Errorf("%s missing DefaultModel — /model add-cloud has nothing to call", id)
		}
		if pk.KeyEnv == "" {
			t.Errorf("%s missing KeyEnv — operators rely on env-var refs", id)
		}
	}
}
