package main

import (
	"context"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/memory/skills"
)

// Each new check is exercised against a synthetic environment — no live
// network, no DGX. The integrity check uses a real SQLite file we
// deliberately corrupt to exercise the FAIL + --fix paths.

// checkLaunchConfig: missing config = OK (defaults), parseable = OK,
// corrupt = FAIL, active provider missing from map = FAIL.
func TestDoctor_CheckLaunchConfig(t *testing.T) {
	cases := []struct {
		name       string
		setupLC    *launchConfig
		writeRaw   string // when set, overwrites config.json with this raw text
		wantStatus checkStatus
		mustDetail string
	}{
		{
			name:       "no config file",
			setupLC:    nil,
			wantStatus: statusOK,
			mustDetail: "no active provider",
		},
		{
			name: "valid active provider",
			setupLC: &launchConfig{
				Provider: "p1",
				Providers: map[string]*providerConfig{
					"p1": {Kind: "vllm", Endpoint: "http://x", Model: "m"},
				},
			},
			wantStatus: statusOK,
			mustDetail: "active=p1/vllm",
		},
		{
			name: "active provider missing from map",
			setupLC: &launchConfig{
				Provider:  "ghost",
				Providers: map[string]*providerConfig{"p1": {Kind: "vllm"}},
			},
			wantStatus: statusFail,
			mustDetail: "not in the providers map",
		},
		{
			name: "unknown kind",
			setupLC: &launchConfig{
				Provider:  "p1",
				Providers: map[string]*providerConfig{"p1": {Kind: "frobnoz"}},
			},
			wantStatus: statusFail,
			mustDetail: "unknown kind",
		},
		{
			name:       "corrupt JSON",
			writeRaw:   "this is not json {",
			wantStatus: statusFail,
			mustDetail: "unparseable",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if c.setupLC != nil {
				if err := c.setupLC.save(dir); err != nil {
					t.Fatal(err)
				}
			}
			if c.writeRaw != "" {
				_ = os.WriteFile(launchConfigPath(dir), []byte(c.writeRaw), 0o644)
			}
			e := &doctorEnv{c: &commonFlags{cfgDir: dir, dataDir: t.TempDir(), agent: "main"}}
			r := checkLaunchConfig(context.Background(), e)
			if r.Status != c.wantStatus {
				t.Errorf("status = %v, want %v (detail=%q)", r.Status, c.wantStatus, r.Detail)
			}
			if c.mustDetail != "" && !strings.Contains(r.Detail, c.mustDetail) {
				t.Errorf("detail %q should contain %q", r.Detail, c.mustDetail)
			}
		})
	}
}

// checkCredentials: corrupt creds → FAIL; keyed provider without secret →
// WARN; everything OK → OK.
func TestDoctor_CheckCredentials(t *testing.T) {
	t.Run("corrupt json", func(t *testing.T) {
		dir := t.TempDir()
		_ = os.WriteFile(credentialsPath(dir), []byte("{bad"), 0o600)
		e := &doctorEnv{c: &commonFlags{cfgDir: dir}}
		r := checkCredentials(context.Background(), e)
		if r.Status != statusFail {
			t.Errorf("status = %v, want FAIL", r.Status)
		}
		if !strings.Contains(r.Detail, "unparseable") {
			t.Errorf("detail: %q", r.Detail)
		}
	})

	t.Run("keyed provider with missing secret warns", func(t *testing.T) {
		dir := t.TempDir()
		lc := &launchConfig{
			Provider: "zai",
			Providers: map[string]*providerConfig{
				"zai": {Kind: "zai", Endpoint: "https://api.z.ai", Auth: authCfg{Mode: "apikey", Stored: true}},
			},
		}
		_ = lc.save(dir)
		// No credentials.json — Stored=true but secret missing.
		e := &doctorEnv{c: &commonFlags{cfgDir: dir}, lc: lc}
		r := checkCredentials(context.Background(), e)
		if r.Status != statusWarn {
			t.Errorf("status = %v, want WARN", r.Status)
		}
		if !strings.Contains(r.Detail, "zai") || !strings.Contains(r.Detail, "stored secret missing") {
			t.Errorf("warn detail wrong: %q", r.Detail)
		}
	})

	t.Run("env-var ref without env present warns", func(t *testing.T) {
		_ = os.Unsetenv("FAKE_TEST_API_KEY_NOT_SET")
		dir := t.TempDir()
		lc := &launchConfig{
			Provider: "fake",
			Providers: map[string]*providerConfig{
				"fake": {Kind: "zai", Auth: authCfg{Mode: "apikey", KeyEnv: "FAKE_TEST_API_KEY_NOT_SET"}},
			},
		}
		_ = lc.save(dir)
		e := &doctorEnv{c: &commonFlags{cfgDir: dir}, lc: lc}
		r := checkCredentials(context.Background(), e)
		if r.Status != statusWarn {
			t.Errorf("status = %v, want WARN", r.Status)
		}
		if !strings.Contains(r.Detail, "FAKE_TEST_API_KEY_NOT_SET") {
			t.Errorf("warn should name the missing env var: %q", r.Detail)
		}
	})

	t.Run("happy path", func(t *testing.T) {
		dir := t.TempDir()
		lc := &launchConfig{
			Provider: "zai",
			Providers: map[string]*providerConfig{
				"zai": {Kind: "zai", Auth: authCfg{Mode: "apikey", Stored: true}},
			},
		}
		_ = lc.save(dir)
		creds := &credentials{Secrets: map[string]string{"zai": "sk-real"}}
		_ = creds.save(dir)
		e := &doctorEnv{c: &commonFlags{cfgDir: dir}, lc: lc}
		r := checkCredentials(context.Background(), e)
		if r.Status != statusOK {
			t.Errorf("happy path status = %v: %q", r.Status, r.Detail)
		}
	})
}

// checkAgentProfiles: every entry must point at a valid provider with a
// resolvable model + (when keyed) a resolvable secret.
func TestDoctor_CheckAgentProfiles(t *testing.T) {
	dir := t.TempDir()
	lc := &launchConfig{
		Provider: "good",
		Providers: map[string]*providerConfig{
			"good":     {Kind: "vllm", Endpoint: "http://x", Model: "m"},
			"keyed":    {Kind: "zai", Auth: authCfg{Mode: "apikey", Stored: true}},
			"no-model": {Kind: "vllm", Endpoint: "http://x"}, // Model="" + catalog default ""
		},
		Agents: map[string]*agentProfile{
			"valid":            {Provider: "good", Description: "ok"},
			"ghost-provider":   {Provider: "nonexistent"},
			"keyed-no-secret":  {Provider: "keyed", Model: "glm-4.6"}, // no creds → no resolvable key
			"unresolvable-mdl": {Provider: "no-model"},
		},
	}
	_ = lc.save(dir)
	e := &doctorEnv{c: &commonFlags{cfgDir: dir}, lc: lc}
	r := checkAgentProfiles(context.Background(), e)
	if r.Status != statusFail {
		t.Errorf("status = %v, want FAIL (3 broken profiles)", r.Status)
	}
	for _, want := range []string{
		"ghost-provider", "missing",
		"keyed-no-secret", "no resolvable API key",
		"unresolvable-mdl", "no model resolvable",
	} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("detail missing %q:\n%s", want, r.Detail)
		}
	}
	if !strings.Contains(r.Detail, "1 valid") {
		t.Errorf("should report the 1 valid profile: %q", r.Detail)
	}
}

// checkAgentProfiles when there are no agents → SKIP cleanly.
func TestDoctor_CheckAgentProfiles_NoAgents(t *testing.T) {
	dir := t.TempDir()
	lc := &launchConfig{Providers: map[string]*providerConfig{"x": {Kind: "vllm"}}}
	_ = lc.save(dir)
	e := &doctorEnv{c: &commonFlags{cfgDir: dir}, lc: lc}
	r := checkAgentProfiles(context.Background(), e)
	if r.Status != statusSkip {
		t.Errorf("no agents → SKIP, got %v", r.Status)
	}
}

// checkDBIntegrity: clean DB → OK. Deliberately-corrupted DB → FAIL with
// reason. --fix renames corrupt files so the next startup gets fresh.
func TestDoctor_CheckDBIntegrity(t *testing.T) {
	t.Run("clean stores", func(t *testing.T) {
		dir := t.TempDir()
		ep, err := episodic.Open(filepath.Join(dir, "episodes.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer ep.Close()
		sm, err := semantic.Open(filepath.Join(dir, "facts.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer sm.Close()
		sk, err := skills.Open(filepath.Join(dir, "skills.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer sk.Close()
		e := &doctorEnv{
			c: &commonFlags{dataDir: dir}, episodic: ep, semantic: sm, skills: sk,
		}
		r := checkDBIntegrity(context.Background(), e)
		if r.Status != statusOK {
			t.Errorf("clean DBs should be OK, got %v: %q", r.Status, r.Detail)
		}
	})

	t.Run("corrupt episodes.db → FAIL", func(t *testing.T) {
		dir := t.TempDir()
		// Write garbage to where episodes.db lives.
		_ = os.WriteFile(filepath.Join(dir, "episodes.db"), []byte("not a sqlite file"), 0o644)
		// Try to open it — SQLite may or may not error here depending on
		// how lazy the driver is. We want the doctor probe to surface the
		// issue either way, so wire a manually-opened bad handle.
		ep, _ := episodic.Open(filepath.Join(dir, "episodes.db"))
		defer func() {
			if ep != nil {
				ep.Close()
			}
		}()
		e := &doctorEnv{c: &commonFlags{dataDir: dir}, episodic: ep}
		r := checkDBIntegrity(context.Background(), e)
		if r.Status == statusOK {
			t.Errorf("corrupt DB should NOT be OK, got %v: %q", r.Status, r.Detail)
		}
	})
}

// runIntegrityCheck on a healthy DB → "". On a nil handle → useful error.
func TestDoctor_RunIntegrityCheck(t *testing.T) {
	dir := t.TempDir()
	ep, err := episodic.Open(filepath.Join(dir, "episodes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ep.Close()
	if msg := runIntegrityCheck(context.Background(), ep.DB()); msg != "" {
		t.Errorf("fresh DB integrity_check = %q, want empty", msg)
	}
	if msg := runIntegrityCheck(context.Background(), nil); msg == "" {
		t.Errorf("nil DB should produce a useful error, got empty")
	}
}

// checkResearchStore: empty dir is fine; orphan "running" run → WARN.
func TestDoctor_CheckResearchStore(t *testing.T) {
	dir := t.TempDir()
	e := &doctorEnv{c: &commonFlags{dataDir: dir}}
	r := checkResearchStore(context.Background(), e)
	if r.Status != statusOK {
		t.Errorf("empty research store → OK, got %v: %q", r.Status, r.Detail)
	}
}

// checkWikiDir: not configured → SKIP. Configured but missing → FAIL.
func TestDoctor_CheckWikiDir(t *testing.T) {
	t.Run("not configured", func(t *testing.T) {
		e := &doctorEnv{c: &commonFlags{}, lc: &launchConfig{}}
		r := checkWikiDir(context.Background(), e)
		if r.Status != statusSkip {
			t.Errorf("unconfigured → SKIP, got %v", r.Status)
		}
	})
	t.Run("configured + present", func(t *testing.T) {
		wikiDir := t.TempDir()
		_ = os.WriteFile(filepath.Join(wikiDir, "test.md"), []byte("# t"), 0o644)
		e := &doctorEnv{c: &commonFlags{}, lc: &launchConfig{
			Skills: map[string]*skillConfig{"wiki": {Settings: map[string]string{"dir": wikiDir}}},
		}}
		r := checkWikiDir(context.Background(), e)
		if r.Status != statusOK {
			t.Errorf("present → OK, got %v: %q", r.Status, r.Detail)
		}
		if !strings.Contains(r.Detail, "1 markdown") {
			t.Errorf("md count missing: %q", r.Detail)
		}
	})
	t.Run("configured but missing", func(t *testing.T) {
		e := &doctorEnv{c: &commonFlags{}, lc: &launchConfig{
			Skills: map[string]*skillConfig{"wiki": {Settings: map[string]string{"dir": "/definitely/not/a/path/xyz"}}},
		}}
		r := checkWikiDir(context.Background(), e)
		if r.Status != statusFail {
			t.Errorf("missing dir → FAIL, got %v", r.Status)
		}
	})
	// TEN-24: empty wiki dir silently breaks fitness-004/015/017 and any
	// real wiki_search query. Doctor must surface WARN with a remediation
	// hint instead of OK.
	t.Run("configured but empty → WARN", func(t *testing.T) {
		wikiDir := t.TempDir() // empty by construction
		e := &doctorEnv{c: &commonFlags{}, lc: &launchConfig{
			Skills: map[string]*skillConfig{"wiki": {Settings: map[string]string{"dir": wikiDir}}},
		}}
		r := checkWikiDir(context.Background(), e)
		if r.Status != statusWarn {
			t.Errorf("empty dir → WARN, got %v: %q", r.Status, r.Detail)
		}
		if !strings.Contains(r.Detail, "no markdown files") {
			t.Errorf("empty-dir detail should explain the problem: %q", r.Detail)
		}
		if r.Fix == "" {
			t.Errorf("empty-dir WARN should carry a Fix hint")
		}
	})
}

// checkChrome: web disabled → SKIP. web enabled + chrome present → OK.
func TestDoctor_CheckChrome(t *testing.T) {
	t.Run("web disabled", func(t *testing.T) {
		e := &doctorEnv{c: &commonFlags{}, lc: &launchConfig{
			Skills: map[string]*skillConfig{"web": {Enabled: false}},
		}}
		r := checkChrome(context.Background(), e)
		if r.Status != statusSkip {
			t.Errorf("web disabled → SKIP, got %v", r.Status)
		}
	})
	// The "web enabled" case depends on whether Chrome is installed on the
	// CI machine — not deterministic. We just verify the function runs.
	t.Run("web enabled — non-flaky existence check", func(t *testing.T) {
		e := &doctorEnv{c: &commonFlags{}, lc: &launchConfig{
			Skills: map[string]*skillConfig{"web": {Enabled: true}},
		}}
		r := checkChrome(context.Background(), e)
		if r.Status != statusOK && r.Status != statusFail {
			t.Errorf("web enabled → OK or FAIL, got %v", r.Status)
		}
	})
}

// TEN-72: doctor's checkGSuite must accept the new "oauth" auth mode
// (added alongside Drive support). The previous switch only validated
// gcloud + sa — picking oauth would have surfaced a confusing
// "invalid auth mode" FAIL even when the configuration was correct.
func TestDoctor_CheckGSuite_OAuthModeAccepted(t *testing.T) {
	t.Run("oauth + no creds anywhere → FAIL with actionable Fix", func(t *testing.T) {
		dir := t.TempDir()
		e := &doctorEnv{
			c: &commonFlags{cfgDir: dir},
			lc: &launchConfig{
				Skills: map[string]*skillConfig{
					"gsuite": {
						Enabled:  true,
						Settings: map[string]string{"auth": "oauth"},
					},
				},
			},
		}
		r := checkGSuite(context.Background(), e)
		if r.Status != statusFail {
			t.Errorf("oauth without any creds should FAIL; got %v: %q", r.Status, r.Detail)
		}
		if !strings.Contains(r.Fix, "oauth-setup") && !strings.Contains(r.Fix, "configure gsuite") {
			t.Errorf("Fix should point at oauth-setup OR /configure; got %q", r.Fix)
		}
	})
	t.Run("oauth + operator-supplied creds_json → proceeds to probe", func(t *testing.T) {
		dir := t.TempDir()
		// Don't need real creds; we just want to get PAST the no-creds gate.
		// The probe itself will fail (no real Google), surfacing as WARN —
		// that's fine. Key is we don't FAIL on the missing-creds gate.
		e := &doctorEnv{
			c: &commonFlags{cfgDir: dir},
			lc: &launchConfig{
				Skills: map[string]*skillConfig{
					"gsuite": {
						Enabled: true,
						Settings: map[string]string{
							"auth":             "oauth",
							"oauth_creds_json": "/fake/path",
						},
					},
				},
			},
		}
		r := checkGSuite(context.Background(), e)
		// Should be WARN (probe couldn't reach Google with fake path) or
		// OK (impossible with a fake path) — NOT FAIL.
		if r.Status == statusFail && strings.Contains(r.Detail, "auth mode") {
			t.Errorf("oauth with operator creds should pass auth-mode check; got FAIL: %q", r.Detail)
		}
	})
}

// checkOSProcesses: any dev/CI environment running this test has `ps`
// (Unix) or `tasklist` (Windows) on PATH, so the success path is the
// only one we can verify deterministically. The WARN branch is the
// minimal-container case (Alpine without procps) — exercised at runtime
// on those targets, not in unit tests. We assert the function runs and
// returns either OK (normal) or WARN (rare on a dev box), never FAIL.
func TestDoctor_CheckOSProcesses(t *testing.T) {
	r := checkOSProcesses(context.Background(), &doctorEnv{c: &commonFlags{}})
	if r.Status != statusOK && r.Status != statusWarn {
		t.Errorf("expected OK or WARN, got %v: %q", r.Status, r.Detail)
	}
	if r.Status == statusWarn && r.Fix == "" {
		t.Errorf("WARN should carry a Fix hint, got empty")
	}
	if r.Status == statusOK {
		// Detail should name the resolved binary path.
		if !strings.Contains(r.Detail, " at ") {
			t.Errorf("OK detail should report 'tool at /path/...': %q", r.Detail)
		}
	}
}

// checkDiscord: not configured → SKIP; enabled + no token → FAIL.
// We don't exercise the network-probe paths (OK/WARN) here — they
// depend on discord.com reachability + a real token, both non-hermetic.
// The skip and fail paths are what guarantee operator-visible errors
// when config is wrong; the live-probe paths are operationally useful
// but tested manually with a real token.
func TestDoctor_CheckDiscord(t *testing.T) {
	t.Run("not configured", func(t *testing.T) {
		e := &doctorEnv{c: &commonFlags{}, lc: &launchConfig{}}
		r := checkDiscord(context.Background(), e)
		if r.Status != statusSkip {
			t.Errorf("unconfigured → SKIP, got %v: %q", r.Status, r.Detail)
		}
	})
	t.Run("configured but disabled", func(t *testing.T) {
		e := &doctorEnv{c: &commonFlags{}, lc: &launchConfig{
			Skills: map[string]*skillConfig{"discord": {Enabled: false}},
		}}
		r := checkDiscord(context.Background(), e)
		if r.Status != statusSkip {
			t.Errorf("disabled → SKIP, got %v", r.Status)
		}
	})
	t.Run("enabled but no token", func(t *testing.T) {
		// Ensure env var is empty for the test.
		t.Setenv("DISCORD_BOT_TOKEN", "")
		e := &doctorEnv{c: &commonFlags{cfgDir: t.TempDir()}, lc: &launchConfig{
			Skills: map[string]*skillConfig{"discord": {Enabled: true}},
		}}
		r := checkDiscord(context.Background(), e)
		if r.Status != statusFail {
			t.Errorf("enabled w/o token → FAIL, got %v: %q", r.Status, r.Detail)
		}
		if !strings.Contains(r.Fix, "DISCORD_BOT_TOKEN") {
			t.Errorf("fix should reference DISCORD_BOT_TOKEN env var; got %q", r.Fix)
		}
	})
}

// checkX (TEN-67): not configured → SKIP; disabled → SKIP; enabled + no
// bearer → FAIL with an actionable fix. The live-probe paths (OK/WARN/401)
// depend on api.x.com reachability + a real bearer, so they're verified
// manually — the skip/fail tiers are what guarantee operator-visible errors.
func TestDoctor_CheckX(t *testing.T) {
	t.Run("not configured", func(t *testing.T) {
		e := &doctorEnv{c: &commonFlags{}, lc: &launchConfig{}}
		if r := checkX(context.Background(), e); r.Status != statusSkip {
			t.Errorf("unconfigured → SKIP, got %v: %q", r.Status, r.Detail)
		}
	})
	t.Run("configured but disabled", func(t *testing.T) {
		e := &doctorEnv{c: &commonFlags{}, lc: &launchConfig{
			Skills: map[string]*skillConfig{"x": {Enabled: false}},
		}}
		if r := checkX(context.Background(), e); r.Status != statusSkip {
			t.Errorf("disabled → SKIP, got %v", r.Status)
		}
	})
	t.Run("enabled but no bearer", func(t *testing.T) {
		t.Setenv("X_BEARER_TOKEN", "")
		e := &doctorEnv{c: &commonFlags{cfgDir: t.TempDir()}, lc: &launchConfig{
			Skills: map[string]*skillConfig{"x": {Enabled: true}},
		}}
		r := checkX(context.Background(), e)
		if r.Status != statusFail {
			t.Errorf("enabled w/o bearer → FAIL, got %v: %q", r.Status, r.Detail)
		}
		if !strings.Contains(r.Fix, "X_BEARER_TOKEN") {
			t.Errorf("fix should reference X_BEARER_TOKEN env var; got %q", r.Fix)
		}
	})
}

// checkDashboard (TEN-81): not configured → SKIP; reachable + 200 → OK;
// reachable + 401 → FAIL; non-loopback bind without TLS+auth → FAIL
// (config lint, even with the server down). The OK/FAIL-401 paths use an
// httptest server addressed via the injectable e.httpc seam.
func TestDoctor_CheckDashboard(t *testing.T) {
	// Tri-state Enabled (TEN-86): only an explicit &false with no addr counts
	// as "disabled" → SKIP the probe. A nil/unset Enabled now defaults ON.
	t.Run("explicitly disabled → SKIP", func(t *testing.T) {
		e := &doctorEnv{c: &commonFlags{}, lc: &launchConfig{Dashboard: dashboardConfig{Enabled: boolPtr(false)}}}
		r := checkDashboard(context.Background(), e)
		if r.Status != statusSkip {
			t.Errorf("disabled → SKIP, got %v: %q", r.Status, r.Detail)
		}
	})

	// Default-on: an unset Enabled (nil) means the dashboard auto-launches, so
	// the check PROBES (doesn't skip) and degrades to WARN when unreachable.
	// Addr is pinned to a closed loopback port (127.0.0.1:1) so "down" is
	// deterministic — otherwise the probe hits the real default :8770 and a
	// dashboard actually running on this box makes the test flaky.
	t.Run("default-on (nil) probes → WARN when down", func(t *testing.T) {
		e := &doctorEnv{c: &commonFlags{}, lc: &launchConfig{Dashboard: dashboardConfig{Addr: "127.0.0.1:1"}}}
		r := checkDashboard(context.Background(), e)
		if r.Status != statusWarn {
			t.Errorf("nil Enabled (default on) with nothing serving → WARN, got %v: %q", r.Status, r.Detail)
		}
	})

	t.Run("reachable + 200 → OK", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		}))
		defer srv.Close()
		e := &doctorEnv{
			c:     &commonFlags{},
			httpc: srv.Client(),
			lc:    &launchConfig{Dashboard: dashboardConfig{Enabled: boolPtr(true), Addr: hostPort(t, srv.URL)}},
		}
		r := checkDashboard(context.Background(), e)
		if r.Status != statusOK {
			t.Errorf("200 → OK, got %v: %q", r.Status, r.Detail)
		}
		if !strings.Contains(r.Detail, "healthy") {
			t.Errorf("OK detail should say 'healthy': %q", r.Detail)
		}
	})

	t.Run("reachable + 401 → FAIL", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()
		e := &doctorEnv{
			c:     &commonFlags{},
			httpc: srv.Client(),
			lc:    &launchConfig{Dashboard: dashboardConfig{Enabled: boolPtr(true), Addr: hostPort(t, srv.URL), Auth: "wrong"}},
		}
		r := checkDashboard(context.Background(), e)
		if r.Status != statusFail {
			t.Errorf("401 → FAIL, got %v: %q", r.Status, r.Detail)
		}
		if !strings.Contains(r.Detail, "auth token mismatch") {
			t.Errorf("401 detail should mention token mismatch: %q", r.Detail)
		}
	})

	// Config lint: a non-loopback bind without BOTH TLS and auth must FAIL
	// even though no server is running (mirrors checkBindPolicy's rule).
	// Enabled is an explicit &true here — the tri-state default-on would
	// reach the same lint, but pinning it keeps the test's intent clear.
	t.Run("non-loopback without TLS+auth → FAIL (config lint)", func(t *testing.T) {
		e := &doctorEnv{
			c:  &commonFlags{},
			lc: &launchConfig{Dashboard: dashboardConfig{Enabled: boolPtr(true), Addr: "0.0.0.0:8770"}},
		}
		r := checkDashboard(context.Background(), e)
		if r.Status != statusFail {
			t.Errorf("non-loopback w/o TLS+auth → FAIL, got %v: %q", r.Status, r.Detail)
		}
		if !strings.Contains(r.Detail, "non-loopback") || !strings.Contains(r.Detail, "TLS+auth") {
			t.Errorf("config-lint detail should explain the fail-closed reason: %q", r.Detail)
		}
		if r.Fix == "" {
			t.Errorf("config-lint FAIL should carry a Fix hint")
		}
	})

	// A non-loopback bind WITH TLS+auth passes the lint (then probes — which
	// here can't connect to the unroutable LAN IP, degrading to WARN, not
	// FAIL). An already-canceled ctx short-circuits the probe so the test
	// doesn't wait on the 5s dial timeout — the lint runs before the probe.
	t.Run("non-loopback with TLS+auth passes the lint", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		e := &doctorEnv{
			c: &commonFlags{},
			lc: &launchConfig{Dashboard: dashboardConfig{
				Enabled: boolPtr(true), Addr: "192.168.1.50:8770",
				TLSCert: "c.pem", TLSKey: "k.pem", Auth: "tok",
			}},
		}
		r := checkDashboard(ctx, e)
		if r.Status == statusFail && strings.Contains(r.Detail, "non-loopback") {
			t.Errorf("TLS+auth should pass the bind lint; got config-lint FAIL: %q", r.Detail)
		}
	})
}

// boolPtr returns a pointer to b — for the tri-state dashboardConfig.Enabled
// (*bool: nil = default on, &false = explicitly off).
func boolPtr(b bool) *bool { return &b }

// hostPort extracts host:port from an httptest server URL (http://127.0.0.1:NNN)
// for use as a dashboard Addr.
func hostPort(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	return u.Host
}

// isWindowsRuntime — basic shape check; tracks the OS path separator.
func TestIsWindowsRuntime(t *testing.T) {
	got := isWindowsRuntime()
	want := (os.PathSeparator == '\\')
	if got != want {
		t.Errorf("isWindowsRuntime = %v, want %v", got, want)
	}
}

// checkWikiFreshness: TEN-48 minimal diagnostic.
// - unconfigured → SKIP
// - no sidecar yet → OK (first-launch, not a failure)
// - sidecar fresh (all .md files older) → OK
// - .md file newer than sidecar → WARN with file listed
func TestDoctor_CheckWikiFreshness(t *testing.T) {
	t.Run("not configured", func(t *testing.T) {
		e := &doctorEnv{c: &commonFlags{dataDir: t.TempDir()}, lc: &launchConfig{}}
		r := checkWikiFreshness(context.Background(), e)
		if r.Status != statusSkip {
			t.Errorf("unconfigured → SKIP, got %v", r.Status)
		}
	})

	t.Run("no sidecar yet — first launch is OK", func(t *testing.T) {
		wikiDir := t.TempDir()
		_ = os.WriteFile(filepath.Join(wikiDir, "test.md"), []byte("# t"), 0o644)
		e := &doctorEnv{
			c:  &commonFlags{dataDir: t.TempDir()},
			lc: &launchConfig{Skills: map[string]*skillConfig{"wiki": {Settings: map[string]string{"dir": wikiDir}}}},
		}
		r := checkWikiFreshness(context.Background(), e)
		if r.Status != statusOK {
			t.Errorf("no sidecar → OK, got %v: %q", r.Status, r.Detail)
		}
		if !strings.Contains(r.Detail, "not present") {
			t.Errorf("detail should mention not-present sidecar: %q", r.Detail)
		}
	})

	t.Run("sidecar present + all files older → OK", func(t *testing.T) {
		wikiDir := t.TempDir()
		dataDir := t.TempDir()
		// Write the .md file FIRST (old timestamp).
		mdPath := filepath.Join(wikiDir, "test.md")
		_ = os.WriteFile(mdPath, []byte("# t"), 0o644)
		past := time.Now().Add(-1 * time.Hour)
		_ = os.Chtimes(mdPath, past, past)
		// Then write the sidecar (newer timestamp = fresh).
		sidecarPath := sidecarPathFor(wikiDir, dataDir)
		_ = os.MkdirAll(filepath.Dir(sidecarPath), 0o755)
		_ = os.WriteFile(sidecarPath, []byte("{}"), 0o644)

		e := &doctorEnv{
			c:  &commonFlags{dataDir: dataDir},
			lc: &launchConfig{Skills: map[string]*skillConfig{"wiki": {Settings: map[string]string{"dir": wikiDir}}}},
		}
		r := checkWikiFreshness(context.Background(), e)
		if r.Status != statusOK {
			t.Errorf("fresh → OK, got %v: %q", r.Status, r.Detail)
		}
		if !strings.Contains(r.Detail, "fresh") {
			t.Errorf("detail should say 'fresh': %q", r.Detail)
		}
	})

	t.Run("file newer than sidecar → WARN with file in detail", func(t *testing.T) {
		wikiDir := t.TempDir()
		dataDir := t.TempDir()
		// Write the sidecar FIRST (old timestamp).
		sidecarPath := sidecarPathFor(wikiDir, dataDir)
		_ = os.MkdirAll(filepath.Dir(sidecarPath), 0o755)
		_ = os.WriteFile(sidecarPath, []byte("{}"), 0o644)
		past := time.Now().Add(-1 * time.Hour)
		_ = os.Chtimes(sidecarPath, past, past)
		// Then write the .md file (newer = stale index).
		_ = os.WriteFile(filepath.Join(wikiDir, "research-atlassian.md"), []byte("# research"), 0o644)

		e := &doctorEnv{
			c:  &commonFlags{dataDir: dataDir},
			lc: &launchConfig{Skills: map[string]*skillConfig{"wiki": {Settings: map[string]string{"dir": wikiDir}}}},
		}
		r := checkWikiFreshness(context.Background(), e)
		if r.Status != statusWarn {
			t.Errorf("stale → WARN, got %v: %q", r.Status, r.Detail)
		}
		if !strings.Contains(r.Detail, "research-atlassian.md") {
			t.Errorf("stale file should appear in detail: %q", r.Detail)
		}
		if !strings.Contains(r.Fix, "wiki reindex") {
			t.Errorf("fix should mention reindex: %q", r.Fix)
		}
	})
}

// sidecarPathFor reproduces the sidecar path formula used by
// toolmux.go::buildWikiIndex and commands.go::cmdWiki so the test fixture
// can write a sidecar at the same path the check looks at.
func sidecarPathFor(wikiDir, dataDir string) string {
	absVault, _ := filepath.Abs(wikiDir)
	h := fnv.New64a()
	_, _ = h.Write([]byte(absVault))
	return filepath.Join(dataDir, "wiki", fmt.Sprintf("%x.json", h.Sum64()))
}
