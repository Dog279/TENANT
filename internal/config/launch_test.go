package config

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"tenant/internal/route"
)

func TestLaunchConfig_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	neverCloud := false
	stickySession := false
	relayNeverCloud := true

	// Missing file → empty config, no error.
	lc, err := Load(dir)
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if lc.Active() != nil {
		t.Fatalf("missing file should yield empty config")
	}

	// Save then reload → provider/embed/gateway survive.
	want := &Launch{
		Provider:         "vllm",
		BrowserNoSandbox: true,
		Providers: map[string]*Provider{
			"vllm": {Kind: "vllm", Endpoint: "http://h:8000", Model: "gemma-4-26b", ToolFmt: "gemma"},
		},
		Embed: &Provider{Kind: "ollama", Endpoint: "http://localhost:11434", Model: "nomic-embed-text", EmbedDim: 768},
		Route: &Route{
			Enabled: true, Mode: "active", TimeoutMS: 300,
			Provider:   &RouteProvider{Endpoint: "http://127.0.0.1:11434", Model: "supra-router:test"},
			NeverCloud: &neverCloud, MinComplexityForBig: 5, StickySession: &stickySession,
			BigRole: "vllm", ForceProfile: "vllm-planner",
			Agents: map[string]RouteAgent{
				"tenant-discord": {Mode: "shadow", NeverCloud: &relayNeverCloud},
			},
		},
		Gateway:       Gateway{Mode: "sse", SSEAddr: "127.0.0.1:8765"},
		TurnScheduler: TurnScheduler{QueueDepth: 24, SourceQuotas: map[string]int{"dashboard": 12, "cron": 3}},
	}
	if err := want.Save(dir); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := got.Active()
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
	if !got.BrowserNoSandbox {
		t.Fatal("browser_no_sandbox did not survive config round-trip")
	}
	if got.TurnScheduler.QueueDepth != 24 || got.TurnScheduler.SourceQuotas["dashboard"] != 12 || got.TurnScheduler.SourceQuotas["cron"] != 3 {
		t.Fatalf("turn scheduler round-trip wrong: %+v", got.TurnScheduler)
	}
	if got.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("schema version not stamped: %d", got.SchemaVersion)
	}

	// Corrupt file → error.
	if err := os.WriteFile(Path(dir), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("corrupt config should error")
	}
}

// v1 flat config migrates forward into the v2 providers model.
func TestLaunchConfig_MigrateV1(t *testing.T) {
	dir := t.TempDir()
	v1 := `{"backend":"vllm","vllm_endpoint":"http://h:8000","vllm_model":"m",
	        "vllm_tool_format":"gemma","embed_endpoint":"http://localhost:11434",
	        "embed_model":"nomic-embed-text","embed_dim":768,"sse_addr":"1.2.3.4:9"}`
	if err := os.WriteFile(Path(dir), []byte(v1), 0o644); err != nil {
		t.Fatal(err)
	}
	lc, err := Load(dir)
	if err != nil {
		t.Fatalf("load v1: %v", err)
	}
	p := lc.Active()
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
	if err := lc.Save(dir); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(Path(dir))
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	for _, legacy := range []string{"backend", "vllm_endpoint", "sse_addr"} {
		if _, ok := m[legacy]; ok {
			t.Fatalf("legacy field %q should not be re-serialized", legacy)
		}
	}
}

func TestResolveRouteConfig_DefaultsAndEmbedInheritance(t *testing.T) {
	lc := &Launch{
		Embed: &Provider{
			Kind:     "ollama",
			Endpoint: "http://embed-box.local:11434",
			Model:    "nomic-embed-text",
		},
		Route: &Route{Enabled: true},
	}
	got, err := ResolveRoute(lc)
	if err != nil {
		t.Fatalf("resolve route: %v", err)
	}
	if !got.Enabled || got.Mode != DefaultRouteMode {
		t.Errorf("enabled/mode = %v/%q, want true/%q", got.Enabled, got.Mode, DefaultRouteMode)
	}
	if got.Endpoint != lc.Embed.Endpoint {
		t.Errorf("endpoint = %q, want inherited %q", got.Endpoint, lc.Embed.Endpoint)
	}
	if got.Model != DefaultRouteModel {
		t.Errorf("model = %q, want classifier default %q (never the embedding model)", got.Model, DefaultRouteModel)
	}
	if got.TimeoutMS != DefaultRouteTimeoutMS {
		t.Errorf("timeout_ms = %d, want %d", got.TimeoutMS, DefaultRouteTimeoutMS)
	}
	if !got.NeverCloud {
		t.Error("never_cloud = false, want deny-by-default true")
	}
	if got.MinComplexityForBig != DefaultRouteMinComplexityForBig {
		t.Errorf("min_complexity_for_big = %d, want %d", got.MinComplexityForBig, DefaultRouteMinComplexityForBig)
	}
	if !got.StickySession {
		t.Error("sticky_session = false, want default true")
	}
}

func TestResolveRouteConfig_PolicyOverrides(t *testing.T) {
	neverCloud := false
	stickySession := false
	relayNeverCloud := false
	lc := &Launch{
		Providers: map[string]*Provider{"big": {Kind: "vllm"}},
		Route: &Route{
			Mode: "active", BigRole: " big ", ForceProfile: " forced/planner ",
			NeverCloud: &neverCloud, MinComplexityForBig: 2, StickySession: &stickySession,
			Agents: map[string]RouteAgent{
				"tenant-discord": {Mode: " shadow ", NeverCloud: &relayNeverCloud},
			},
		},
	}

	got, err := ResolveRoute(lc)
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
		lc      *Launch
		wantErr bool
	}{
		{
			name: "active mode requires big role",
			lc: &Launch{Route: &Route{
				Enabled: true, Mode: "active",
				Provider: &RouteProvider{Endpoint: "http://localhost:11434"},
			}},
			wantErr: true,
		},
		{
			name: "active mode accepted with wired big provider",
			lc: &Launch{
				Providers: map[string]*Provider{"big": {Kind: "vllm"}},
				Route: &Route{
					Enabled: true, Mode: "active", BigRole: "big",
					Provider: &RouteProvider{Endpoint: "http://localhost:11434"},
				},
			},
		},
		{
			name: "per-agent active mode requires big role",
			lc: &Launch{Route: &Route{
				Agents: map[string]RouteAgent{"main": {Mode: "active"}},
			}},
			wantErr: true,
		},
		{
			name: "relay active override accepted with wired big provider",
			lc: &Launch{
				Providers: map[string]*Provider{"big": {Kind: "ollama"}},
				Route: &Route{
					BigRole: "big",
					Agents:  map[string]RouteAgent{"tenant-discord": {Mode: "active"}},
				},
			},
		},
		{
			name: "everywhere mode accepted with wired big provider",
			lc: &Launch{
				Providers: map[string]*Provider{"big": {Kind: "ollama"}},
				Route: &Route{Enabled: true, Mode: route.ModeEverywhere, BigRole: "big",
					Provider: &RouteProvider{Endpoint: "http://localhost:11434"}},
			},
		},
		{
			name:    "assist requires enabled shadow mode",
			lc:      &Launch{Route: &Route{Assist: true}},
			wantErr: true,
		},
		{
			name: "assist rejects active global mode",
			lc: &Launch{Providers: map[string]*Provider{"big": {Kind: "ollama"}}, Route: &Route{
				Enabled: true, Mode: route.ModeActive, Assist: true, BigRole: "big",
				Provider: &RouteProvider{Endpoint: "http://localhost:11434"},
			}},
			wantErr: true,
		},
		{
			name: "assist rejects active agent override",
			lc: &Launch{Providers: map[string]*Provider{"big": {Kind: "ollama"}}, Route: &Route{
				Enabled: true, Mode: route.ModeShadow, Assist: true, BigRole: "big",
				Provider: &RouteProvider{Endpoint: "http://localhost:11434"},
				Agents:   map[string]RouteAgent{"main": {Mode: route.ModeActive}},
			}},
			wantErr: true,
		},
		{
			name: "interactive agent active mode accepted with wired big provider",
			lc: &Launch{
				Providers: map[string]*Provider{"big": {Kind: "ollama"}},
				Route: &Route{
					BigRole: "big",
					Agents:  map[string]RouteAgent{"main": {Mode: "active"}},
				},
			},
		},
		{
			name:    "unknown mode rejected",
			lc:      &Launch{Route: &Route{Mode: "observe"}},
			wantErr: true,
		},
		{
			name: "unknown per-agent mode rejected",
			lc: &Launch{Route: &Route{
				Agents: map[string]RouteAgent{"tenant-discord": {Mode: "observe"}},
			}},
			wantErr: true,
		},
		{
			name:    "minimum complexity above range rejected",
			lc:      &Launch{Route: &Route{MinComplexityForBig: 6}},
			wantErr: true,
		},
		{
			name:    "negative minimum complexity rejected",
			lc:      &Launch{Route: &Route{MinComplexityForBig: -1}},
			wantErr: true,
		},
		{
			name: "unknown big provider rejected",
			lc: &Launch{
				Providers: map[string]*Provider{"small": {Kind: "vllm"}},
				Route:     &Route{BigRole: "missing"},
			},
			wantErr: true,
		},
		{
			name: "unwired big provider kind rejected",
			lc: &Launch{
				Providers: map[string]*Provider{"big": {Kind: "not-wired"}},
				Route:     &Route{BigRole: "big"},
			},
			wantErr: true,
		},
		{
			name:    "negative timeout rejected",
			lc:      &Launch{Route: &Route{TimeoutMS: -1}},
			wantErr: true,
		},
		{
			name: "explicit cloud endpoint rejected",
			lc: &Launch{Route: &Route{
				Enabled:  true,
				Provider: &RouteProvider{Endpoint: "https://api.openai.com", Model: "classifier"},
			}},
			wantErr: true,
		},
		{
			name: "inherited cloud provider rejected",
			lc: &Launch{
				Embed: &Provider{Kind: "openai", Endpoint: "https://api.openai.com"},
				Route: &Route{Enabled: true},
			},
			wantErr: true,
		},
		{
			name: "explicit LAN endpoint without local kind rejected",
			lc: &Launch{Route: &Route{
				Enabled:  true,
				Provider: &RouteProvider{Endpoint: "http://192.168.1.20:11434"},
			}},
			wantErr: true,
		},
		{
			name: "localhost accepted",
			lc: &Launch{Route: &Route{
				Enabled:  true,
				Provider: &RouteProvider{Endpoint: "http://localhost:11434"},
			}},
		},
		{
			name: "IPv4 loopback accepted",
			lc: &Launch{Route: &Route{
				Enabled:  true,
				Provider: &RouteProvider{Endpoint: "http://127.0.0.2:11434"},
			}},
		},
		{
			name: "IPv6 loopback accepted",
			lc: &Launch{Route: &Route{
				Enabled:  true,
				Provider: &RouteProvider{Endpoint: "http://[::1]:11434"},
			}},
		},
		{
			name: "inherited local provider kind accepted",
			lc: &Launch{
				Embed: &Provider{Kind: "ollama", Endpoint: "http://ollama.lan:11434"},
				Route: &Route{Enabled: true},
			},
		},
		{
			name: "disabled remote endpoint is inert",
			lc: &Launch{Route: &Route{
				Provider: &RouteProvider{Endpoint: "https://example.com"},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ResolveRoute(tt.lc)
			if tt.wantErr && err == nil {
				t.Fatal("resolveRouteConfig() error = nil, want rejection")
			}
			if tt.wantErr {
				var configErr *RouteError
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
	_, err := ResolveRoute(&Launch{Route: &Route{Mode: "active"}})
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
		route *Route
	}{
		{
			name: "active",
			route: &Route{
				Enabled: true, Mode: "active",
				Provider: &RouteProvider{Endpoint: "http://localhost:11434"},
			},
		},
		{
			name: "cloud",
			route: &Route{
				Enabled: true, Mode: "shadow",
				Provider: &RouteProvider{Endpoint: "https://api.openai.com"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			raw, err := json.Marshal(&Launch{Route: tt.route})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(Path(dir), raw, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(dir); err == nil {
				t.Fatal("loadLaunchConfig() error = nil, want route config rejection")
			}
		})
	}
}

// TEN-516: the pre-rename spellings migrate on load. judge_loop becomes
// judge (kind clef, max_rounds → rounds); improve.judge* become
// improve.quality_model*; when a file carries both, the new key wins; a save
// writes only the new keys.
func TestLaunchConfig_MigrateJudgeAndQualityModel(t *testing.T) {
	dir := t.TempDir()
	old := `{"judge_loop":{"enabled":true,"endpoint":"http://h:30000","model":"clef-flash","max_rounds":2},
	         "improve":{"judge":"claude-x","judge_kind":"anthropic","judge_endpoint":"https://a","judge_key_env":"K"}}`
	if err := os.WriteFile(Path(dir), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	lc, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if j := lc.Judge; j == nil || !j.Enabled || j.Kind != "clef" || j.Endpoint != "http://h:30000" || j.Model != "clef-flash" || j.Rounds != 2 || j.RoundsOrDefault() != 2 {
		t.Fatalf("judge_loop not migrated: %+v", lc.Judge)
	}
	if lc.LegacyJudgeLoop != nil {
		t.Fatal("legacy judge_loop still set after load")
	}
	ic := lc.Improve
	if ic.QualityModel != "claude-x" || ic.QualityModelKind != "anthropic" || ic.QualityModelEndpoint != "https://a" || ic.QualityModelKeyEnv != "K" || ic.LegacyJudge != "" || ic.LegacyJudgeKind != "" {
		t.Fatalf("improve.judge* not migrated: %+v", ic)
	}
	if err := lc.Save(dir); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(Path(dir))
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if _, ok := m["judge_loop"]; ok {
		t.Fatal("judge_loop re-serialized")
	}
	if _, ok := m["judge"]; !ok {
		t.Fatal("judge missing after save")
	}
	imp, _ := m["improve"].(map[string]any)
	for _, legacy := range []string{"judge", "judge_kind", "judge_endpoint", "judge_key_env"} {
		if _, ok := imp[legacy]; ok {
			t.Fatalf("improve.%s re-serialized", legacy)
		}
	}
	if imp["quality_model"] != "claude-x" {
		t.Fatalf("quality_model missing after save: %v", imp)
	}

	// Both spellings present: the new one wins.
	both := `{"judge_loop":{"enabled":true,"endpoint":"http://old:1"},"judge":{"enabled":false,"kind":"clef","endpoint":"http://new:2"},
	          "improve":{"judge":"old-model","quality_model":"new-model"}}`
	if err := os.WriteFile(Path(dir), []byte(both), 0o644); err != nil {
		t.Fatal(err)
	}
	lc, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if lc.Judge.Enabled || lc.Judge.Endpoint != "http://new:2" || lc.LegacyJudgeLoop != nil {
		t.Fatalf("new judge key should win: %+v", lc.Judge)
	}
	if lc.Improve.QualityModel != "new-model" || lc.Improve.LegacyJudge != "" {
		t.Fatalf("new quality_model key should win: %+v", lc.Improve)
	}
	// Rounds is bounded however the file spells it.
	if (&Judge{Rounds: 500}).RoundsOrDefault() != JudgeMaxRounds || (&Judge{}).RoundsOrDefault() != JudgeDefaultRounds || (*Judge)(nil).RoundsOrDefault() != JudgeDefaultRounds {
		t.Fatal("RoundsOrDefault bounds")
	}
}
