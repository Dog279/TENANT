package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
)

var (
	hostProbe   = surfaceProbe{Surface: surfaceLocal, Backend: "host", Mode: "dry-run", Isolation: "none", Caps: &sbxCaps{Isolation: "none"}}
	dryRunProbe = surfaceProbe{Surface: surfaceLocal, Backend: "restricted", Mode: "dry-run", Isolation: "process", Caps: &sbxCaps{Isolation: "process"}}
)

// The audit's execution block is the live resolved profiles (TEN-345): the
// built-in backend per surface, with the reason when it fell back to the host,
// and host exec allow critical only where commands really run on the host.
func TestSecurityAuditExecutionFromLiveProfiles(t *testing.T) {
	resetExecProfiles(t)
	cfgDir, dataDir := t.TempDir(), t.TempDir()
	withBuiltinExec(t, "host", "bubblewrap (bwrap) not on PATH")
	report, err := runSecurityAuditJSON(t, "--config", cfgDir, "--data", dataDir, "--os", "--os-allow-exec")
	if err == nil || !hasSecurityFinding(report.Findings, "permissions.host-exec-allow") {
		t.Fatalf("host fallback + exec allow must be critical: err=%v findings=%+v", err, report.Findings)
	}
	if len(report.Execution.Surfaces) != len(execSurfaces) || report.Execution.Isolation != "none" ||
		report.Execution.Surfaces[0].Fallback != "bubblewrap (bwrap) not on PATH" {
		t.Fatalf("execution = %+v", report.Execution)
	}
	var host securityFinding
	for _, f := range report.Findings {
		if f.ID == "execution.host" {
			host = f
		}
	}
	if !strings.Contains(host.Summary, "bwrap") || !strings.Contains(host.Summary, "cron-shell") {
		t.Errorf("execution.host must name the surfaces and why: %+v", host)
	}

	// The operator names restricted for local: not host exec any more, whatever
	// this OS has (enforcing and missing, it runs nothing: a warning).
	stg := &settings{ExecProfiles: map[string]execProfileSetting{"local": {Backend: "restricted"}}}
	if err := stg.Save(cfgDir, "main"); err != nil {
		t.Fatal(err)
	}
	report, _ = runSecurityAuditJSON(t, "--config", cfgDir, "--data", dataDir, "--os", "--os-allow-exec")
	if hasSecurityFinding(report.Findings, "permissions.host-exec-allow") || report.Execution.Backend != "restricted" {
		t.Fatalf("a restricted local profile must not trip host-exec-allow: %+v / %+v", report.Execution, report.Findings)
	}
}

// `tenant permissions effective` reports the surface's resolved exec profile
// and its exceptions, not a hard-coded host.
func TestPermissionsEffectiveExecutionProfile(t *testing.T) {
	resetExecProfiles(t)
	t.Cleanup(func() { defaultExecProfiles.LoadExceptions(nil) })
	cfgDir, dataDir := t.TempDir(), t.TempDir()
	withBuiltinExec(t, "host", "reason")
	work := t.TempDir()
	stg := &settings{
		ExecProfiles:   map[string]execProfileSetting{"discord": {Backend: "host"}},
		ExecExceptions: map[string][]string{"discord": {work}},
	}
	if err := stg.Save(cfgDir, "main"); err != nil {
		t.Fatal(err)
	}
	report := runPermissionsEffectiveJSON(t, "--config", cfgDir, "--data", dataDir, "--surface", "discord")
	p := report.Execution.Profile
	if report.Execution.Backend != "host" || p.Surface != "discord" || p.Fallback != "" || !slices.Equal(p.Exceptions, []string{work}) {
		t.Fatalf("discord execution = %+v", report.Execution)
	}
	if local := runPermissionsEffectiveJSON(t, "--config", cfgDir, "--data", dataDir).Execution.Profile; local.Fallback != "reason" || local.Mode != "dry-run" {
		t.Fatalf("local execution = %+v", local)
	}
}

func TestEvaluateSecurityAudit(t *testing.T) {
	tests := []struct {
		name          string
		report        securityAuditReport
		wantCritical  int
		wantWarnings  int
		wantFindingID string
	}{
		{
			name: "loopback listener and disabled risky integrations",
			report: securityAuditReport{
				Listeners: []listenerAudit{{Name: "dashboard", Enabled: true, Address: "127.0.0.1:8770", Loopback: true}},
				Browser:   browserAudit{Enabled: false, NoSandbox: true},
				Web:       webAudit{Enabled: false},
				Execution: executionAudit{Isolation: "none"},
			},
			wantCritical: 0,
		},
		{
			name: "non-loopback listener needs tls and auth",
			report: securityAuditReport{
				Listeners: []listenerAudit{{Name: "dashboard", Enabled: true, Address: "0.0.0.0:8770", TLS: true, Auth: false}},
			},
			wantCritical:  1,
			wantFindingID: "listener.dashboard",
		},
		{
			name: "non-loopback listener safe with tls and auth",
			report: securityAuditReport{
				Listeners: []listenerAudit{{Name: "dashboard", Enabled: true, Address: "192.168.1.5:8770", TLS: true, Auth: true}},
			},
			wantCritical: 0,
		},
		{
			// Merged reality: browser_no_sandbox opt-in is the one critical here;
			// the remote reader is an opt-in warning; egress is guarded (TEN-307)
			// so there is no unrestricted-egress finding.
			name: "web exposes sandbox reader and egress state",
			report: securityAuditReport{
				Browser: browserAudit{Enabled: true, NoSandbox: true},
				Web:     webAudit{Enabled: true, RemoteReaderEnabled: true},
			},
			wantCritical:  1,
			wantWarnings:  1,
			wantFindingID: "web.remote-reader",
		},
		{
			name: "remote mcp write bypass is critical",
			report: securityAuditReport{
				RemoteMCP: remoteMCPAudit{Servers: []string{"https://mcp.example"}, TrustAnnotations: true, AllowWrite: true},
			},
			wantCritical:  1,
			wantWarnings:  1,
			wantFindingID: "mcp.write-bypass",
		},
		{
			name: "host execution is review only",
			report: securityAuditReport{
				Execution: executionAudit{Enabled: true, Isolation: "none", Surfaces: []surfaceProbe{hostProbe}},
			},
			wantCritical:  0,
			wantWarnings:  1,
			wantFindingID: "execution.host",
		},
		{
			name: "unprompted destructive and host exec are critical",
			report: securityAuditReport{
				Permissions: effectivePermissions{Categories: map[string]effectiveCategory{
					catDestructive: {Mode: "allow"},
					catExec:        {Mode: "allow"},
				}},
				Execution: executionAudit{Enabled: true, Isolation: "none", Surfaces: []surfaceProbe{hostProbe}},
			},
			wantCritical:  2,
			wantWarnings:  1,
			wantFindingID: "permissions.destructive-allow",
		},
		{
			// TEN-345: exec allowed on the restricted sandbox, even in dry run,
			// is not host exec; the dry run is a warning of its own.
			name: "exec allow on a restricted profile is not host exec",
			report: securityAuditReport{
				Permissions: effectivePermissions{Categories: map[string]effectiveCategory{catExec: {Mode: "allow"}}},
				Execution:   executionAudit{Enabled: true, Isolation: "process", Surfaces: []surfaceProbe{dryRunProbe}},
			},
			wantCritical:  0,
			wantWarnings:  1,
			wantFindingID: "execution.dry-run",
		},
		{
			name: "enforcing restricted raises nothing",
			report: securityAuditReport{
				Permissions: effectivePermissions{Categories: map[string]effectiveCategory{catExec: {Mode: "allow"}}},
				Execution: executionAudit{Enabled: true, Isolation: "process", Surfaces: []surfaceProbe{
					{Surface: surfaceLocal, Backend: "restricted", Mode: "enforce", Isolation: "process", Caps: &sbxCaps{Isolation: "process"}}}},
			},
			wantCritical: 0,
		},
		{
			name: "an enforcing profile whose backend is missing runs nothing",
			report: securityAuditReport{
				Execution: executionAudit{Enabled: true, Isolation: "unavailable", Surfaces: []surfaceProbe{
					{Surface: surfaceCronShell, Backend: "restricted", Mode: "enforce", Error: "no bwrap"}}},
			},
			wantWarnings:  1,
			wantFindingID: "execution.unavailable",
		},
		{
			name: "unprompted sandbox escalation is critical",
			report: securityAuditReport{
				Permissions: effectivePermissions{Categories: map[string]effectiveCategory{
					catSandbox: {Mode: "allow"},
				}},
			},
			wantCritical:  1,
			wantFindingID: "permissions.sandbox-allow",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evaluateSecurityAudit(&tt.report)
			if tt.report.Critical != tt.wantCritical || tt.report.Warnings != tt.wantWarnings {
				t.Fatalf("counts = critical %d warning %d, want %d/%d; findings=%+v",
					tt.report.Critical, tt.report.Warnings, tt.wantCritical, tt.wantWarnings, tt.report.Findings)
			}
			if tt.report.Safe != (tt.wantCritical == 0) {
				t.Errorf("safe = %t, want %t", tt.report.Safe, tt.wantCritical == 0)
			}
			if gotFailure := securityAuditError(tt.report) != nil; gotFailure != (tt.wantCritical > 0) {
				t.Errorf("exit failure = %t, want %t", gotFailure, tt.wantCritical > 0)
			}
			if tt.wantFindingID != "" && !hasSecurityFinding(tt.report.Findings, tt.wantFindingID) {
				t.Errorf("missing finding %q in %+v", tt.wantFindingID, tt.report.Findings)
			}
		})
	}
}

func TestConfiguredListeners(t *testing.T) {
	lc := &launchConfig{
		Dashboard: dashboardConfig{Addr: "0.0.0.0:8770", TLSCert: "cert.pem", TLSKey: "key.pem", Auth: "token"},
		Gateway:   gatewayConfig{Mode: "sse", SSEAddr: "127.0.0.1:8765"},
		Peer:      peerConfig{Listen: "192.168.1.2:9100"},
	}
	got := configuredListeners(lc)
	if len(got) != 3 {
		t.Fatalf("listeners = %d, want 3: %+v", len(got), got)
	}
	if got[0].Name != "dashboard" || got[0].Loopback || !got[0].TLS || !got[0].Auth {
		t.Errorf("dashboard listener = %+v", got[0])
	}
	if got[1].Name != "mcp-sse" || !got[1].Loopback || got[1].TLS || got[1].Auth {
		t.Errorf("mcp listener = %+v", got[1])
	}
	if got[2].Name != "peer" || got[2].Loopback || !got[2].TLS || !got[2].Auth {
		t.Errorf("peer listener = %+v", got[2])
	}
}

func TestSecurityAuditJSON(t *testing.T) {
	cfgDir, dataDir := t.TempDir(), t.TempDir()
	out, err := captureTenantStdout(t, func() error {
		return cmdSecurity(context.Background(), []string{"audit", "--config", cfgDir, "--data", dataDir, "--json"})
	})
	if err != nil {
		t.Fatalf("security audit: %v\n%s", err, out)
	}
	var report securityAuditReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode audit JSON: %v\n%s", err, out)
	}
	if len(report.Listeners) == 0 || report.Permissions.Agent != "main" || report.Execution.Backend == "" {
		t.Fatalf("audit report missing required sections: %+v", report)
	}
}

func TestSecurityAuditCriticalExit(t *testing.T) {
	// Post-audit-merge the default web config is SAFE (sandbox on, reader off,
	// egress guarded), so a critical finding now requires an explicitly unsafe
	// setting. browser_no_sandbox=true plus the opt-in reader gives one critical
	// (browser.no-sandbox) and one warning (web.remote-reader).
	t.Setenv("TENANT_WEB_READER", "true")
	cfgDir, dataDir := t.TempDir(), t.TempDir()
	lc := &launchConfig{
		BrowserNoSandbox: true,
		Skills:           map[string]*skillConfig{"web": {Enabled: true, Settings: map[string]string{"jina_reader": "on"}}},
	}
	if err := lc.Save(cfgDir); err != nil {
		t.Fatal(err)
	}
	out, err := captureTenantStdout(t, func() error {
		return cmdSecurity(context.Background(), []string{
			"audit", "--config", cfgDir, "--data", dataDir, "--web", "--json",
		})
	})
	if err == nil {
		t.Fatalf("web audit should fail on critical unsafe state\n%s", out)
	}
	var report securityAuditReport
	if decodeErr := json.Unmarshal([]byte(out), &report); decodeErr != nil {
		t.Fatalf("decode audit JSON: %v\n%s", decodeErr, out)
	}
	if report.Critical < 1 || !hasSecurityFinding(report.Findings, "browser.no-sandbox") ||
		!hasSecurityFinding(report.Findings, "web.remote-reader") {
		t.Fatalf("critical web findings = %+v", report.Findings)
	}
}

func hasSecurityFinding(findings []securityFinding, id string) bool {
	for _, finding := range findings {
		if finding.ID == id {
			return true
		}
	}
	return false
}

// captureTenantStdout runs fn with stdout redirected to a pipe. The pipe is
// drained while fn writes: output larger than the pipe buffer (a few KiB on
// Windows, which the multi-surface audit report exceeds) would otherwise block
// fn forever.
func captureTenantStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	type readResult struct {
		b   []byte
		err error
	}
	read := make(chan readResult, 1)
	go func() {
		b, err := io.ReadAll(r)
		read <- readResult{b, err}
	}()
	old := os.Stdout
	os.Stdout = w
	err = fn()
	os.Stdout = old
	_ = w.Close()
	got := <-read
	_ = r.Close()
	if got.err != nil {
		t.Fatal(got.err)
	}
	return strings.TrimSpace(string(got.b)), err
}
