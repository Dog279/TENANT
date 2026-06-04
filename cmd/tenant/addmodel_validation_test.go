package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/agent"
	"tenant/internal/memory/working"
	"tenant/internal/model"
)

func newEchoAgentForAdd(t *testing.T) *agent.Agent {
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

// TestAddModel_RejectsNonURLEndpoint — the load-bearing fix from the
// bug where a user typed `/model add zai <api-key>` and the API key
// got saved as the endpoint URL (then leaked through the model list).
// Endpoint MUST be http:// or https://; anything else gets rejected
// BEFORE touching disk.
func TestAddModel_RejectsNonURLEndpoint(t *testing.T) {
	dir := t.TempDir()
	mc := &modelControl{cfgDir: dir, dataDir: filepath.Join(dir, "data"), agentID: "main", ag: newEchoAgentForAdd(t),
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	cases := []struct {
		name, endpoint string
	}{
		{"api-key-shape", "3b8ff1a652474e9ca3c38c71582a47c6.4KhXDHVsyrhS6AwP"},
		{"sk-prefix", "sk-abcdef0123456789abcdef0123456789"},
		{"bare hostname (no scheme)", "api.z.ai"},
		{"colon-slash without scheme", "://api.z.ai/api/paas/v4"},
		{"ftp scheme", "ftp://files.example.com"},
		{"empty after trim", "   "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := mc.AddModel("custom", c.endpoint, "")
			if err == nil {
				t.Errorf("non-URL endpoint %q should be rejected", c.endpoint)
			}
		})
	}
}

// TestAddModel_HintsAddCloudForKnownProviderNames — the operator
// typed `/model add zai <key>` because it sounded right. The error
// message MUST point them at `/model add-cloud zai <key>` rather than
// just saying "bad URL." Names that match known kinds (zai, openai,
// grok, anthropic) get the smart hint.
func TestAddModel_HintsAddCloudForKnownProviderNames(t *testing.T) {
	dir := t.TempDir()
	mc := &modelControl{cfgDir: dir, dataDir: filepath.Join(dir, "data"), agentID: "main", ag: newEchoAgentForAdd(t),
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, name := range []string{"zai", "openai", "grok", "anthropic"} {
		t.Run(name, func(t *testing.T) {
			_, err := mc.AddModel(name, "3b8ff1a652474e9ca3c38c71582a47c6.4KhXDHVsyrhS6AwP", "")
			if err == nil {
				t.Fatalf("%s with token endpoint should be rejected", name)
			}
			if !strings.Contains(err.Error(), "/model add-cloud "+name) {
				t.Errorf("error should suggest /model add-cloud %s; got %q", name, err.Error())
			}
		})
	}
}

// TestAddModel_NeverLeaksFullSecretInError — defense in depth. Even
// if the operator's mistake echoes a real key into the error message,
// the error string MUST NOT contain the full token. We render the
// first 6 chars + an ellipsis + the length, never the whole thing.
func TestAddModel_NeverLeaksFullSecretInError(t *testing.T) {
	dir := t.TempDir()
	mc := &modelControl{cfgDir: dir, dataDir: filepath.Join(dir, "data"), agentID: "main", ag: newEchoAgentForAdd(t),
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	secret := "sk-veryverysecrettoken-AbCdEf123456-ShouldNotAppearVerbatimInLogs"
	_, err := mc.AddModel("custom", secret, "")
	if err == nil {
		t.Fatal("expected rejection")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("FULL secret leaked to error message — security regression: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "redacted") {
		t.Errorf("error should signal it redacted the input; got %q", err.Error())
	}
}

// TestAddModel_AcceptsValidHTTPURL — drift guard for the happy path.
// The validation must not over-restrict — http:// and https:// pass.
func TestAddModel_AcceptsValidHTTPURL(t *testing.T) {
	dir := t.TempDir()
	lc := &launchConfig{Provider: "echo", Providers: map[string]*providerConfig{"echo": {Kind: "echo"}}}
	_ = lc.save(dir)
	mc := &modelControl{cfgDir: dir, dataDir: filepath.Join(dir, "data"), agentID: "main", ag: newEchoAgentForAdd(t),
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, url := range []string{
		"http://localhost:8000",
		"https://api.openai.com",
		"http://localhost:11434",
		"HTTPS://api.z.ai/v1", // case-insensitive scheme check
	} {
		t.Run(url, func(t *testing.T) {
			_, err := mc.AddModel("test", url, "qwen")
			if err != nil {
				t.Errorf("valid URL %q rejected: %v", url, err)
			}
			// Cleanup so the next iteration doesn't see leftover state.
			got, _ := loadLaunchConfig(dir)
			delete(got.Providers, "test")
			_ = got.save(dir)
		})
	}
}

// --- looksLikeURL unit tests ---

func TestLooksLikeURL(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"https://api.openai.com", true},
		{"http://localhost:11434", true},
		{"HTTPS://api.z.ai/api/paas/v4", true},
		{"  http://x.com  ", true},
		{"api.openai.com", false},
		{"sk-token12345", false},
		{"://no-scheme", false},
		{"ftp://files.example.com", false},
		{"", false},
		{"file:///etc/passwd", false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := looksLikeURL(c.in); got != c.want {
				t.Errorf("looksLikeURL(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestClipSecret_DoesNotLeak(t *testing.T) {
	long := "sk-AbCdEf1234567890ZzYyXxWwVvUu"
	got := clipSecret(long)
	if strings.Contains(got, "AbCdEf1234567890") {
		t.Errorf("clipSecret leaked too much of the input: %q", got)
	}
	if !strings.Contains(got, "redacted") {
		t.Errorf("clipSecret should signal it redacted; got %q", got)
	}
	short := "abc"
	if clipSecret(short) != `"abc"` {
		t.Errorf("clipSecret should pass through short non-secret values: got %q", clipSecret(short))
	}
}

// --- Catalog drift guard: zai default = coding plan ---

// TestProviderKinds_ZaiDefaultIsCodingPlan — drift guard for the
// reshape that made `/model add-cloud zai` default to the coding-plan
// endpoint. If a future refactor changes this back to the metered URL,
// this test breaks BEFORE operators see "Insufficient balance" again.
func TestProviderKinds_ZaiDefaultIsCodingPlan(t *testing.T) {
	pk := providerKinds["zai"]
	if !strings.Contains(pk.DefaultEndpoint, "/api/coding/paas/v4") {
		t.Errorf("kind=zai default endpoint should be coding-plan (/api/coding/paas/v4); got %q", pk.DefaultEndpoint)
	}
	if !strings.Contains(pk.Label, "coding plan") {
		t.Errorf("kind=zai label should mention coding plan so the wizard surfaces it; got %q", pk.Label)
	}
}

// TestProviderKinds_ZaiMeteredExists — operators on per-token billing
// still need a path. Verify `zai-metered` is in the catalog with the
// old endpoint, registered in providerOrder so the wizard offers it.
func TestProviderKinds_ZaiMeteredExists(t *testing.T) {
	pk, ok := providerKinds["zai-metered"]
	if !ok {
		t.Fatal("zai-metered kind missing — operators on per-token billing have no path")
	}
	if !strings.Contains(pk.DefaultEndpoint, "/api/paas/v4") || strings.Contains(pk.DefaultEndpoint, "/coding/") {
		t.Errorf("zai-metered should point at the non-coding /api/paas/v4 endpoint; got %q", pk.DefaultEndpoint)
	}
	found := false
	for _, o := range providerOrder {
		if o == "zai-metered" {
			found = true
			break
		}
	}
	if !found {
		t.Error("zai-metered must be in providerOrder so the setup wizard offers it")
	}
}
