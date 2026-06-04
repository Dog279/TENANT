package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestSafeDisplayEndpoint_RedactsNonURL — the load-bearing defense.
// Any string about to be shown to the operator that ISN'T an http(s)
// URL gets replaced with a redaction marker. This catches anything
// that slipped past AddModel's entry-point validation (bad migration,
// hand-edited config, pre-fix data).
func TestSafeDisplayEndpoint_RedactsNonURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"valid https", "https://api.z.ai/api/coding/paas/v4", "https://api.z.ai/api/coding/paas/v4"},
		{"valid http localhost", "http://localhost:11434", "http://localhost:11434"},
		{"empty", "", "(unset)"},
		{"whitespace", "   ", "(unset)"},
		{"api key shape", "3b8ff1a652474e9ca3c38c71582a47c6.4KhXDHVsyrhS6AwP", "[redacted — non-URL endpoint, see `tenant doctor`]"},
		{"sk-prefix", "sk-abc123def456", "[redacted — non-URL endpoint, see `tenant doctor`]"},
		{"bare hostname", "api.z.ai", "[redacted — non-URL endpoint, see `tenant doctor`]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := safeDisplayEndpoint(c.in)
			if got != c.want {
				t.Errorf("safeDisplayEndpoint(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestSafeDisplayEndpoint_NeverLeaksSecret — defense-in-depth drift
// guard. For any plausibly-secret input, the OUTPUT must not contain
// the input substring. Even partial leakage is unacceptable here —
// the goal is "nothing identifying the bad value reaches the screen."
func TestSafeDisplayEndpoint_NeverLeaksSecret(t *testing.T) {
	for _, secret := range []string{
		"sk-AbCdEf1234567890ZzYyXxWwVvUu",
		"3b8ff1a652474e9ca3c38c71582a47c6.4KhXDHVsyrhS6AwP",
		"ghp_VeryLongGitHubPersonalAccessTokenStyleString12345",
	} {
		got := safeDisplayEndpoint(secret)
		if strings.Contains(got, secret) {
			t.Errorf("safeDisplayEndpoint LEAKED %q in output %q", secret, got)
		}
		// Also: no substring of length ≥6 from the input may appear in
		// the output. Catches partial leaks too.
		for i := 0; i+6 <= len(secret); i++ {
			chunk := secret[i : i+6]
			if strings.Contains(got, chunk) {
				t.Errorf("safeDisplayEndpoint leaked 6-char substring %q from input; output: %q", chunk, got)
			}
		}
	}
}

// TestSanitizeEndpoints_BlankBadEndpoint — config-load guard. When
// loading a config whose providers contain a non-URL endpoint (the
// pre-fix bug shape), sanitizeEndpoints REPLACES the bad value
// before any code path can render it. Catalog default wins; else
// blank.
func TestSanitizeEndpoints_BlankBadEndpoint(t *testing.T) {
	lc := &launchConfig{
		Providers: map[string]*providerConfig{
			"good": {Kind: "vllm", Endpoint: "http://localhost:8000"},
			"leaked-key": {
				Kind:     "zai", // known kind → catalog default available
				Endpoint: "3b8ff1a652474e9ca3c38c71582a47c6.4KhXDHVsyrhS6AwP",
			},
			"orphaned": {
				Kind:     "totally-unknown",
				Endpoint: "sk-some-key",
			},
		},
	}
	lc.sanitizeEndpoints()
	if lc.Providers["good"].Endpoint != "http://localhost:8000" {
		t.Errorf("good endpoint mutated: %q", lc.Providers["good"].Endpoint)
	}
	leaked := lc.Providers["leaked-key"].Endpoint
	if leaked == "3b8ff1a652474e9ca3c38c71582a47c6.4KhXDHVsyrhS6AwP" {
		t.Error("leaked-key endpoint NOT sanitized — secret still in config")
	}
	if !strings.Contains(leaked, "api.z.ai") {
		t.Errorf("leaked-key endpoint should have been replaced with zai catalog default; got %q", leaked)
	}
	if lc.Providers["orphaned"].Endpoint != "" {
		t.Errorf("orphaned endpoint (unknown kind) should be blanked, not replaced; got %q", lc.Providers["orphaned"].Endpoint)
	}
}

// TestLoadLaunchConfig_SanitizesOnLoad — integration test. Write a
// config with a leaked-as-endpoint key to disk, load it, verify the
// bad value is gone in the loaded result. Operators who somehow
// inherited a bad config never see the secret.
func TestLoadLaunchConfig_SanitizesOnLoad(t *testing.T) {
	dir := t.TempDir()
	// Construct the broken config shape directly.
	bad := &launchConfig{
		Provider: "zai",
		Providers: map[string]*providerConfig{
			"zai": {
				Kind:     "vllm",
				Endpoint: "3b8ff1a652474e9ca3c38c71582a47c6.4KhXDHVsyrhS6AwP",
			},
		},
	}
	if err := bad.save(dir); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadLaunchConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := loaded.Providers["zai"].Endpoint
	if strings.Contains(got, "3b8ff1a652474e9ca3c38c71582a47c6") {
		t.Errorf("loadLaunchConfig should sanitize bad endpoint on load; got %q", got)
	}
}

// TestModelInfos_UsesSafeEndpoint — what the TUI eventually displays.
// modelInfos feeds the ModelList that renderModelList iterates over.
// The endpoint field of the returned info MUST be safe-rendered.
func TestModelInfos_UsesSafeEndpoint(t *testing.T) {
	lc := &launchConfig{
		Provider: "leaked",
		Providers: map[string]*providerConfig{
			"leaked": {Kind: "vllm", Endpoint: "sk-veryverysecretAPIkey1234567890"},
			"good":   {Kind: "vllm", Endpoint: "http://localhost:8000"},
		},
	}
	infos := modelInfos(lc)
	for _, mi := range infos {
		if strings.Contains(mi.Endpoint, "sk-veryverysecret") {
			t.Errorf("modelInfos leaked secret in endpoint for %q: %q", mi.Name, mi.Endpoint)
		}
		if mi.Name == "good" && mi.Endpoint != "http://localhost:8000" {
			t.Errorf("modelInfos mangled valid endpoint: %q", mi.Endpoint)
		}
	}
}

// --- End-to-end: real config flow doesn't leak the secret anywhere ---

// TestEndToEnd_NoSecretLeaksThroughDisplay — given a config with a
// leaked-key endpoint, walk EVERY operator-facing render path
// (loadLaunchConfig → ModelControl.ModelList → renderable Info) and
// assert the secret never appears in any output string.
func TestEndToEnd_NoSecretLeaksThroughDisplay(t *testing.T) {
	dir := t.TempDir()
	secret := "sk-EndToEndLeakCanaryToken_DoNotEcho_123456"
	bad := &launchConfig{
		Provider:  "zai",
		Providers: map[string]*providerConfig{"zai": {Kind: "vllm", Endpoint: secret}},
	}
	_ = bad.save(dir)
	loaded, err := loadLaunchConfig(filepath.Clean(dir))
	if err != nil {
		t.Fatal(err)
	}
	infos := modelInfos(loaded)
	for _, mi := range infos {
		if strings.Contains(mi.Endpoint, secret) {
			t.Errorf("END-TO-END LEAK: secret survived loadLaunchConfig + modelInfos in field Endpoint=%q", mi.Endpoint)
		}
		if strings.Contains(mi.Endpoint, secret[:10]) {
			t.Errorf("END-TO-END PARTIAL LEAK: 10-char prefix of secret reached display in %q", mi.Endpoint)
		}
	}
}
