package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"tenant/internal/model"
)

func TestLaunchConfig_RoundTrip(t *testing.T) {
	dir := t.TempDir()

	// Missing file → empty config, no error.
	lc, err := loadLaunchConfig(dir)
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if lc.active() != nil {
		t.Fatalf("missing file should yield empty config")
	}

	// Save then reload → provider/embed/gateway survive.
	want := &launchConfig{
		Provider: "vllm",
		Providers: map[string]*providerConfig{
			"vllm": {Kind: "vllm", Endpoint: "http://h:8000", Model: "gemma-4-26b", ToolFmt: "gemma"},
		},
		Embed:   &providerConfig{Kind: "ollama", Endpoint: "http://localhost:11434", Model: "nomic-embed-text", EmbedDim: 768},
		Gateway: gatewayConfig{Mode: "sse", SSEAddr: "127.0.0.1:8765"},
	}
	if err := want.save(dir); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := loadLaunchConfig(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := got.active()
	if p == nil || p.Endpoint != "http://h:8000" || p.Model != "gemma-4-26b" {
		t.Fatalf("provider round-trip wrong: %+v", p)
	}
	if got.Embed == nil || got.Embed.Model != "nomic-embed-text" {
		t.Fatalf("embed round-trip wrong: %+v", got.Embed)
	}
	if got.Gateway.Mode != "sse" || got.Gateway.SSEAddr != "127.0.0.1:8765" {
		t.Fatalf("gateway round-trip wrong: %+v", got.Gateway)
	}
	if got.SchemaVersion != currentSchemaVersion {
		t.Fatalf("schema version not stamped: %d", got.SchemaVersion)
	}

	// Corrupt file → error.
	if err := os.WriteFile(launchConfigPath(dir), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadLaunchConfig(dir); err == nil {
		t.Fatal("corrupt config should error")
	}
}

// v1 flat config migrates forward into the v2 providers model.
func TestLaunchConfig_MigrateV1(t *testing.T) {
	dir := t.TempDir()
	v1 := `{"backend":"vllm","vllm_endpoint":"http://h:8000","vllm_model":"m",
	        "vllm_tool_format":"gemma","embed_endpoint":"http://localhost:11434",
	        "embed_model":"nomic-embed-text","embed_dim":768,"sse_addr":"1.2.3.4:9"}`
	if err := os.WriteFile(launchConfigPath(dir), []byte(v1), 0o644); err != nil {
		t.Fatal(err)
	}
	lc, err := loadLaunchConfig(dir)
	if err != nil {
		t.Fatalf("load v1: %v", err)
	}
	p := lc.active()
	if p == nil || p.Kind != "vllm" || p.Endpoint != "http://h:8000" || p.Model != "m" {
		t.Fatalf("v1 provider not migrated: %+v", p)
	}
	if lc.Embed == nil || lc.Embed.Endpoint != "http://localhost:11434" {
		t.Fatalf("v1 embed not migrated: %+v", lc.Embed)
	}
	if lc.Gateway.Mode != "sse" || lc.Gateway.SSEAddr != "1.2.3.4:9" {
		t.Fatalf("v1 gateway not migrated: %+v", lc.Gateway)
	}
	// Re-save must drop legacy fields.
	if err := lc.save(dir); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(launchConfigPath(dir))
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	for _, legacy := range []string{"backend", "vllm_endpoint", "sse_addr"} {
		if _, ok := m[legacy]; ok {
			t.Fatalf("legacy field %q should not be re-serialized", legacy)
		}
	}
}

func TestCredentials_RoundTripAndResolve(t *testing.T) {
	dir := t.TempDir()
	c, _ := loadCredentials(dir)
	c.set("openai", "sk-secret")
	if err := c.save(dir); err != nil {
		t.Fatalf("save: %v", err)
	}
	// 0600 perms on POSIX (skip the check on Windows where it's advisory).
	if fi, err := os.Stat(credentialsPath(dir)); err == nil && os.PathSeparator == '/' {
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("credentials perms = %o, want 600", fi.Mode().Perm())
		}
	}

	// Stored secret resolves.
	if got := resolveSecret(dir, "openai", authCfg{Stored: true}); got != "sk-secret" {
		t.Fatalf("stored secret = %q, want sk-secret", got)
	}
	// Env reference takes priority over stored.
	t.Setenv("MY_KEY", "env-secret")
	if got := resolveSecret(dir, "openai", authCfg{KeyEnv: "MY_KEY", Stored: true}); got != "env-secret" {
		t.Fatalf("env ref = %q, want env-secret", got)
	}
	// No auth → empty.
	if got := resolveSecret(dir, "openai", authCfg{}); got != "" {
		t.Fatalf("no-auth secret = %q, want empty", got)
	}
}

// resolve() picks the active provider and maps its kind → backend, with
// explicit flags overriding config.
func TestResolve_ProviderFromConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := &launchConfig{
		Provider: "openai",
		Providers: map[string]*providerConfig{
			"openai": {Kind: "openai", Endpoint: "https://api.openai.com", Model: "gpt-4o",
				ToolFmt: "openai", Auth: authCfg{KeyEnv: "TEST_OPENAI_KEY"}},
		},
		Embed: &providerConfig{Kind: "ollama", Endpoint: "http://localhost:11434", Model: "nomic-embed-text"},
	}
	if err := cfg.save(dir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_OPENAI_KEY", "sk-live")

	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	c := bindCommon(fs)
	if err := fs.Parse([]string{"--config", dir, "--data", filepath.Join(dir, "data")}); err != nil {
		t.Fatal(err)
	}
	if err := c.resolve(); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if c.backend != "vllm" { // openai routes through the OpenAI-compatible (vllm) factory
		t.Errorf("backend = %q, want vllm (openai is OpenAI-compatible)", c.backend)
	}
	if c.genKind != "openai" {
		t.Errorf("genKind = %q, want openai", c.genKind)
	}
	if c.vllmEndpoint != "https://api.openai.com" {
		t.Errorf("endpoint = %q", c.vllmEndpoint)
	}
	if c.vllmModel != "gpt-4o" {
		t.Errorf("model = %q, want gpt-4o (no autodetect for hosted)", c.vllmModel)
	}
	if c.genAPIKey != "sk-live" {
		t.Errorf("api key not resolved from env: %q", c.genAPIKey)
	}
}

// The anthropic provider builds a real router now (not the old "not wired"
// error). embedEndpoint is left empty so attachEmbedder uses the echo
// stand-in without a network probe.
func TestBuildRouter_Anthropic(t *testing.T) {
	withKey := &commonFlags{
		backend: "anthropic", genKind: "anthropic", genAPIKey: "sk-ant-x",
		vllmEndpoint: "https://api.anthropic.com", vllmModel: "claude-sonnet-4-20250514",
	}
	r, err := buildRouter(withKey, nil)
	if err != nil {
		t.Fatalf("buildRouter(anthropic): %v", err)
	}
	if _, _, err := r.LLMForRole(context.Background(), model.RolePlanner); err != nil {
		t.Fatalf("planner role should resolve to an anthropic LLM: %v", err)
	}

	noKey := &commonFlags{
		backend: "anthropic", genKind: "anthropic",
		vllmEndpoint: "https://api.anthropic.com", vllmModel: "claude-sonnet-4-20250514",
	}
	if _, err := buildRouter(noKey, nil); err == nil {
		t.Fatal("anthropic without an API key should error")
	}
}

// applyPluginConfig wires saved skills back into pluginFlags at launch.
func TestApplyPluginConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := &launchConfig{
		Provider:  "echo",
		Providers: map[string]*providerConfig{"echo": {Kind: "echo"}},
		Skills: map[string]*skillConfig{
			"wiki": {Enabled: true, Settings: map[string]string{"dir": "/notes"}},
			"os":   {Enabled: true},
			"sql":  {Enabled: false, Settings: map[string]string{"db": "/x.db"}},
		},
	}
	if err := cfg.save(dir); err != nil {
		t.Fatal(err)
	}
	creds, _ := loadCredentials(dir)
	creds.set(skillSecretID("x", "bearer"), "tok-123")
	if err := creds.save(dir); err != nil {
		t.Fatal(err)
	}
	cfg.Skills["x"] = &skillConfig{Enabled: true}
	_ = cfg.save(dir)

	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	c := bindCommon(fs)
	pf := bindPluginFlags(fs)
	if err := fs.Parse([]string{"--config", dir, "--data", filepath.Join(dir, "data")}); err != nil {
		t.Fatal(err)
	}
	if err := c.resolve(); err != nil {
		t.Fatal(err)
	}
	applyPluginConfig(c, pf)

	if pf.wikiDir != "/notes" {
		t.Errorf("wiki dir not applied: %q", pf.wikiDir)
	}
	if !pf.osEnable {
		t.Error("os not enabled")
	}
	if pf.sqlDB != "" {
		t.Errorf("disabled sql skill leaked: %q", pf.sqlDB)
	}
	if !pf.x || pf.xBearer != "tok-123" {
		t.Errorf("x bearer secret not applied: enabled=%v key=%q", pf.x, pf.xBearer)
	}
}
