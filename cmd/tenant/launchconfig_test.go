package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/model"
)

func TestResolve_RefusesUnsafeRouteInsteadOfIgnoringIt(t *testing.T) {
	tests := []struct {
		name  string
		route *routeConfig
	}{
		{
			name: "active mode",
			route: &routeConfig{Enabled: true, Mode: "active",
				Provider: &routeProviderConfig{Endpoint: "http://localhost:11434"}},
		},
		{
			name: "cloud endpoint",
			route: &routeConfig{Enabled: true, Mode: "shadow",
				Provider: &routeProviderConfig{Endpoint: "https://router.example.com"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			raw, err := json.Marshal(&launchConfig{Route: tt.route})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(launchConfigPath(dir), raw, 0o644); err != nil {
				t.Fatal(err)
			}
			fs := flag.NewFlagSet("route-reject", flag.ContinueOnError)
			c := bindCommon(fs)
			if err := fs.Parse([]string{"--config", dir, "--data", filepath.Join(dir, "data")}); err != nil {
				t.Fatal(err)
			}
			err = c.resolve()
			if err == nil || !strings.HasPrefix(err.Error(), "route:") {
				t.Fatalf("resolve error = %v, want clear route rejection", err)
			}
		})
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
	if err := cfg.Save(dir); err != nil {
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
	if err := cfg.Save(dir); err != nil {
		t.Fatal(err)
	}
	creds, _ := loadCredentials(dir)
	creds.Set(skillSecretID("x", "bearer"), "tok-123")
	if err := creds.Save(dir); err != nil {
		t.Fatal(err)
	}
	cfg.Skills["x"] = &skillConfig{Enabled: true}
	_ = cfg.Save(dir)

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
