package main

import (
	"log/slog"
	"strings"
	"testing"
)

// A model-build failure must DEGRADE to echo (not abort): non-nil router,
// degraded gate set, an honest banner, and no error.
func TestBuildRouterResilient_DegradesOnModelFailure(t *testing.T) {
	c := &commonFlags{
		backend:      "vllm",
		vllmEndpoint: "http://127.0.0.1:9", // dead; with empty model => buildRouter errors deterministically
		vllmModel:    "",
		genKind:      "vllm",
	}
	r, ds, banner, err := buildRouterResilient(c, slog.Default())
	if err != nil {
		t.Fatalf("resilient build should not error on a model failure: %v", err)
	}
	if r == nil {
		t.Fatal("expected a non-nil echo fallback router")
	}
	if !ds.Degraded() {
		t.Fatal("expected the degraded gate to be set")
	}
	for _, want := range []string{"ECHO", "no tool execution"} {
		if !strings.Contains(banner, want) {
			t.Errorf("banner missing %q: %s", want, banner)
		}
	}
}

// A healthy build returns no degraded gate and no banner.
func TestBuildRouterResilient_HealthyEcho(t *testing.T) {
	c := &commonFlags{backend: "echo"}
	r, ds, banner, err := buildRouterResilient(c, slog.Default())
	if err != nil || r == nil {
		t.Fatalf("echo build = (%v, %v)", r, err)
	}
	if ds.Degraded() { // nil-safe: nil gate reports not-degraded
		t.Error("a healthy launch must not be degraded")
	}
	if banner != "" {
		t.Errorf("healthy launch should have no banner, got %q", banner)
	}
}

func TestClassifyDegrade(t *testing.T) {
	cases := map[string]degradeClass{
		"Anthropic provider needs an API key — run `tenant setup`": degradeCredential,
		"unknown backend \"frob\"":                                 degradeConfig,
		"Anthropic provider needs a model":                         degradeConfig,
		"could not determine vLLM model at X — is the server up?":  degradeReachability,
		"some unrecognized failure":                                degradeReachability,
	}
	for cause, want := range cases {
		if got := classifyDegrade(cause); got != want {
			t.Errorf("classifyDegrade(%q) = %d, want %d", cause, got, want)
		}
	}
}

// modelInfos must flag ONLY the active provider's row as degraded, and only
// when the shared gate is set — config still names the real provider.
func TestModelInfosDegradedMarker(t *testing.T) {
	lc := &launchConfig{
		Provider: "zai",
		Providers: map[string]*providerConfig{
			"zai":    {Kind: "vllm", Endpoint: "http://localhost:8000"},
			"openai": {Kind: "vllm", Endpoint: "https://api.openai.com"},
		},
	}
	// No gate → nothing degraded.
	for _, mi := range modelInfos(lc, nil) {
		if mi.Degraded {
			t.Errorf("nil gate must not mark %q degraded", mi.Name)
		}
	}
	// Gate set → only the active row.
	ds := &degradedState{}
	ds.on.Store(true)
	for _, mi := range modelInfos(lc, ds) {
		want := mi.Name == "zai"
		if mi.Degraded != want {
			t.Errorf("%q Degraded=%v, want %v", mi.Name, mi.Degraded, want)
		}
	}
}
