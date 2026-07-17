package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/model"
	"tenant/internal/route"
	"tenant/internal/secrets"
)

func TestLaunchConfig_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	neverCloud := false
	stickySession := false
	relayNeverCloud := true

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
		Embed: &providerConfig{Kind: "ollama", Endpoint: "http://localhost:11434", Model: "nomic-embed-text", EmbedDim: 768},
		Route: &routeConfig{
			Enabled: true, Mode: "active", TimeoutMS: 300,
			Provider:   &routeProviderConfig{Endpoint: "http://127.0.0.1:11434", Model: "supra-router:test"},
			NeverCloud: &neverCloud, MinComplexityForBig: 5, StickySession: &stickySession,
			BigRole: "vllm", ForceProfile: "vllm-planner",
			Agents: map[string]routeAgentConfig{
				"tenant-discord": {Mode: "shadow", NeverCloud: &relayNeverCloud},
			},
		},
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
	if got.Route == nil || !got.Route.Enabled || got.Route.Mode != "active" || got.Route.TimeoutMS != 300 ||
		got.Route.Provider == nil || got.Route.Provider.Endpoint != "http://127.0.0.1:11434" || got.Route.Provider.Model != "supra-router:test" {
		t.Fatalf("route round-trip wrong: %+v", got.Route)
	}
	if got.Route.NeverCloud == nil || *got.Route.NeverCloud || got.Route.MinComplexityForBig != 5 ||
		got.Route.StickySession == nil || *got.Route.StickySession || got.Route.BigRole != "vllm" ||
		got.Route.ForceProfile != "vllm-planner" {
		t.Fatalf("route policy round-trip wrong: %+v", got.Route)
	}
	relayRoute, ok := got.Route.Agents["tenant-discord"]
	if !ok || relayRoute.Mode != "shadow" || relayRoute.NeverCloud == nil || !*relayRoute.NeverCloud {
		t.Fatalf("route agent override round-trip wrong: %+v", got.Route.Agents)
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

func TestResolveRouteConfig_DefaultsAndEmbedInheritance(t *testing.T) {
	lc := &launchConfig{
		Embed: &providerConfig{
			Kind:     "ollama",
			Endpoint: "http://embed-box.local:11434",
			Model:    "nomic-embed-text",
		},
		Route: &routeConfig{Enabled: true},
	}
	got, err := resolveRouteConfig(lc)
	if err != nil {
		t.Fatalf("resolve route: %v", err)
	}
	if !got.Enabled || got.Mode != defaultRouteMode {
		t.Errorf("enabled/mode = %v/%q, want true/%q", got.Enabled, got.Mode, defaultRouteMode)
	}
	if got.Endpoint != lc.Embed.Endpoint {
		t.Errorf("endpoint = %q, want inherited %q", got.Endpoint, lc.Embed.Endpoint)
	}
	if got.Model != defaultRouteModel {
		t.Errorf("model = %q, want classifier default %q (never the embedding model)", got.Model, defaultRouteModel)
	}
	if got.TimeoutMS != defaultRouteTimeoutMS {
		t.Errorf("timeout_ms = %d, want %d", got.TimeoutMS, defaultRouteTimeoutMS)
	}
	if !got.NeverCloud {
		t.Error("never_cloud = false, want deny-by-default true")
	}
	if got.MinComplexityForBig != defaultRouteMinComplexityForBig {
		t.Errorf("min_complexity_for_big = %d, want %d", got.MinComplexityForBig, defaultRouteMinComplexityForBig)
	}
	if !got.StickySession {
		t.Error("sticky_session = false, want default true")
	}
}

func TestResolveRouteConfig_PolicyOverrides(t *testing.T) {
	neverCloud := false
	stickySession := false
	relayNeverCloud := false
	lc := &launchConfig{
		Providers: map[string]*providerConfig{"big": {Kind: "vllm"}},
		Route: &routeConfig{
			Mode: "active", BigRole: " big ", ForceProfile: " forced/planner ",
			NeverCloud: &neverCloud, MinComplexityForBig: 2, StickySession: &stickySession,
			Agents: map[string]routeAgentConfig{
				"tenant-discord": {Mode: " shadow ", NeverCloud: &relayNeverCloud},
			},
		},
	}

	got, err := resolveRouteConfig(lc)
	if err != nil {
		t.Fatalf("resolve route: %v", err)
	}
	if got.NeverCloud || got.StickySession || got.MinComplexityForBig != 2 {
		t.Fatalf("resolved policy overrides = never_cloud:%t sticky:%t complexity:%d",
			got.NeverCloud, got.StickySession, got.MinComplexityForBig)
	}
	if got.BigRole != "big" || got.ForceProfile != "forced/planner" {
		t.Fatalf("resolved bindings = big:%q force:%q", got.BigRole, got.ForceProfile)
	}
	relayRoute, ok := got.Agents["tenant-discord"]
	if !ok || relayRoute.Mode != "shadow" || relayRoute.NeverCloud == nil || *relayRoute.NeverCloud {
		t.Fatalf("resolved relay override = %+v", relayRoute)
	}
	// Resolution must not alias the persisted pointer: runtime consumers may
	// snapshot overrides while a control path reloads config concurrently.
	relayNeverCloud = true
	if *got.Agents["tenant-discord"].NeverCloud {
		t.Fatal("resolved per-agent never_cloud aliases persisted config")
	}
}

func TestResolveRouteConfig_Validation(t *testing.T) {
	tests := []struct {
		name    string
		lc      *launchConfig
		wantErr bool
	}{
		{
			name: "active mode requires big role",
			lc: &launchConfig{Route: &routeConfig{
				Enabled: true, Mode: "active",
				Provider: &routeProviderConfig{Endpoint: "http://localhost:11434"},
			}},
			wantErr: true,
		},
		{
			name: "active mode accepted with wired big provider",
			lc: &launchConfig{
				Providers: map[string]*providerConfig{"big": {Kind: "vllm"}},
				Route: &routeConfig{
					Enabled: true, Mode: "active", BigRole: "big",
					Provider: &routeProviderConfig{Endpoint: "http://localhost:11434"},
				},
			},
		},
		{
			name: "per-agent active mode requires big role",
			lc: &launchConfig{Route: &routeConfig{
				Agents: map[string]routeAgentConfig{"main": {Mode: "active"}},
			}},
			wantErr: true,
		},
		{
			name: "relay active override accepted with wired big provider",
			lc: &launchConfig{
				Providers: map[string]*providerConfig{"big": {Kind: "ollama"}},
				Route: &routeConfig{
					BigRole: "big",
					Agents:  map[string]routeAgentConfig{"tenant-discord": {Mode: "active"}},
				},
			},
		},
		{
			name: "everywhere mode accepted with wired big provider",
			lc: &launchConfig{
				Providers: map[string]*providerConfig{"big": {Kind: "ollama"}},
				Route: &routeConfig{Enabled: true, Mode: route.ModeEverywhere, BigRole: "big",
					Provider: &routeProviderConfig{Endpoint: "http://localhost:11434"}},
			},
		},
		{
			name:    "assist requires enabled shadow mode",
			lc:      &launchConfig{Route: &routeConfig{Assist: true}},
			wantErr: true,
		},
		{
			name: "assist rejects active global mode",
			lc: &launchConfig{Providers: map[string]*providerConfig{"big": {Kind: "ollama"}}, Route: &routeConfig{
				Enabled: true, Mode: route.ModeActive, Assist: true, BigRole: "big",
				Provider: &routeProviderConfig{Endpoint: "http://localhost:11434"},
			}},
			wantErr: true,
		},
		{
			name: "assist rejects active agent override",
			lc: &launchConfig{Providers: map[string]*providerConfig{"big": {Kind: "ollama"}}, Route: &routeConfig{
				Enabled: true, Mode: route.ModeShadow, Assist: true, BigRole: "big",
				Provider: &routeProviderConfig{Endpoint: "http://localhost:11434"},
				Agents:   map[string]routeAgentConfig{"main": {Mode: route.ModeActive}},
			}},
			wantErr: true,
		},
		{
			name: "interactive agent active mode accepted with wired big provider",
			lc: &launchConfig{
				Providers: map[string]*providerConfig{"big": {Kind: "ollama"}},
				Route: &routeConfig{
					BigRole: "big",
					Agents:  map[string]routeAgentConfig{"main": {Mode: "active"}},
				},
			},
		},
		{
			name:    "unknown mode rejected",
			lc:      &launchConfig{Route: &routeConfig{Mode: "observe"}},
			wantErr: true,
		},
		{
			name: "unknown per-agent mode rejected",
			lc: &launchConfig{Route: &routeConfig{
				Agents: map[string]routeAgentConfig{"tenant-discord": {Mode: "observe"}},
			}},
			wantErr: true,
		},
		{
			name:    "minimum complexity above range rejected",
			lc:      &launchConfig{Route: &routeConfig{MinComplexityForBig: 6}},
			wantErr: true,
		},
		{
			name:    "negative minimum complexity rejected",
			lc:      &launchConfig{Route: &routeConfig{MinComplexityForBig: -1}},
			wantErr: true,
		},
		{
			name: "unknown big provider rejected",
			lc: &launchConfig{
				Providers: map[string]*providerConfig{"small": {Kind: "vllm"}},
				Route:     &routeConfig{BigRole: "missing"},
			},
			wantErr: true,
		},
		{
			name: "unwired big provider kind rejected",
			lc: &launchConfig{
				Providers: map[string]*providerConfig{"big": {Kind: "not-wired"}},
				Route:     &routeConfig{BigRole: "big"},
			},
			wantErr: true,
		},
		{
			name:    "negative timeout rejected",
			lc:      &launchConfig{Route: &routeConfig{TimeoutMS: -1}},
			wantErr: true,
		},
		{
			name: "explicit cloud endpoint rejected",
			lc: &launchConfig{Route: &routeConfig{
				Enabled:  true,
				Provider: &routeProviderConfig{Endpoint: "https://api.openai.com", Model: "classifier"},
			}},
			wantErr: true,
		},
		{
			name: "inherited cloud provider rejected",
			lc: &launchConfig{
				Embed: &providerConfig{Kind: "openai", Endpoint: "https://api.openai.com"},
				Route: &routeConfig{Enabled: true},
			},
			wantErr: true,
		},
		{
			name: "explicit LAN endpoint without local kind rejected",
			lc: &launchConfig{Route: &routeConfig{
				Enabled:  true,
				Provider: &routeProviderConfig{Endpoint: "http://192.168.1.20:11434"},
			}},
			wantErr: true,
		},
		{
			name: "localhost accepted",
			lc: &launchConfig{Route: &routeConfig{
				Enabled:  true,
				Provider: &routeProviderConfig{Endpoint: "http://localhost:11434"},
			}},
		},
		{
			name: "IPv4 loopback accepted",
			lc: &launchConfig{Route: &routeConfig{
				Enabled:  true,
				Provider: &routeProviderConfig{Endpoint: "http://127.0.0.2:11434"},
			}},
		},
		{
			name: "IPv6 loopback accepted",
			lc: &launchConfig{Route: &routeConfig{
				Enabled:  true,
				Provider: &routeProviderConfig{Endpoint: "http://[::1]:11434"},
			}},
		},
		{
			name: "inherited local provider kind accepted",
			lc: &launchConfig{
				Embed: &providerConfig{Kind: "ollama", Endpoint: "http://ollama.lan:11434"},
				Route: &routeConfig{Enabled: true},
			},
		},
		{
			name: "disabled remote endpoint is inert",
			lc: &launchConfig{Route: &routeConfig{
				Provider: &routeProviderConfig{Endpoint: "https://example.com"},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolveRouteConfig(tt.lc)
			if tt.wantErr && err == nil {
				t.Fatal("resolveRouteConfig() error = nil, want rejection")
			}
			if tt.wantErr {
				var configErr *routeConfigError
				if !errors.As(err, &configErr) {
					t.Fatalf("resolveRouteConfig() error type = %T, want *routeConfigError", err)
				}
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("resolveRouteConfig() unexpected error: %v", err)
			}
		})
	}
}

func TestResolveRouteConfig_ActiveBindingErrorIsActionable(t *testing.T) {
	_, err := resolveRouteConfig(&launchConfig{Route: &routeConfig{Mode: "active"}})
	if err == nil {
		t.Fatal("resolveRouteConfig() error = nil, want missing big-role rejection")
	}
	for _, want := range []string{"route.big_role", "/model add-cloud"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing actionable hint %q", err, want)
		}
	}
}

func TestLoadLaunchConfig_RejectsUnsafeRoute(t *testing.T) {
	tests := []struct {
		name  string
		route *routeConfig
	}{
		{
			name: "active",
			route: &routeConfig{
				Enabled: true, Mode: "active",
				Provider: &routeProviderConfig{Endpoint: "http://localhost:11434"},
			},
		},
		{
			name: "cloud",
			route: &routeConfig{
				Enabled: true, Mode: "shadow",
				Provider: &routeProviderConfig{Endpoint: "https://api.openai.com"},
			},
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
			if _, err := loadLaunchConfig(dir); err == nil {
				t.Fatal("loadLaunchConfig() error = nil, want route config rejection")
			}
		})
	}
}

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

func TestCredentials_RoundTripAndResolve(t *testing.T) {
	dir := t.TempDir()
	c, _ := loadCredentials(dir)
	c.set("openai", "sk-secret")
	if err := c.save(dir); err != nil {
		t.Fatalf("save: %v", err)
	}
	if report := secrets.Probe(credentialsPath(dir)); !report.Protected {
		t.Fatalf("credentials are not protected: %+v", report)
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
	// Env var set but empty falls through to stored secret.
	t.Setenv("EMPTY_KEY", "")
	if got := resolveSecret(dir, "openai", authCfg{KeyEnv: "EMPTY_KEY", Stored: true}); got != "sk-secret" {
		t.Fatalf("empty env fallthrough = %q, want sk-secret", got)
	}
	// Env var set to nonexistent var falls through to stored secret.
	if got := resolveSecret(dir, "openai", authCfg{KeyEnv: "MISSING_KEY", Stored: true}); got != "sk-secret" {
		t.Fatalf("missing env fallthrough = %q, want sk-secret", got)
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
