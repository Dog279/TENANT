package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

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
			name: "web exposes sandbox reader and egress state",
			report: securityAuditReport{
				Browser: browserAudit{Enabled: true, NoSandbox: true},
				Web:     webAudit{Enabled: true, RemoteReaderEnabled: true},
			},
			wantCritical:  2,
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
				Execution: executionAudit{Enabled: true, Isolation: "none"},
			},
			wantCritical: 0,
			wantWarnings: 1,
		},
		{
			name: "unprompted destructive and host exec are critical",
			report: securityAuditReport{
				Permissions: effectivePermissions{Categories: map[string]effectiveCategory{
					catDestructive: {Mode: "allow"},
					catExec:        {Mode: "allow"},
				}},
				Execution: executionAudit{Enabled: true, Isolation: "none"},
			},
			wantCritical:  2,
			wantWarnings:  1,
			wantFindingID: "permissions.destructive-allow",
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
	t.Setenv("TENANT_WEB_NO_READER", "0")
	cfgDir, dataDir := t.TempDir(), t.TempDir()
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
	if report.Critical != 2 || !hasSecurityFinding(report.Findings, "browser.no-sandbox") ||
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

func captureTenantStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	err = fn()
	_ = w.Close()
	os.Stdout = old
	b, readErr := io.ReadAll(r)
	_ = r.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	return strings.TrimSpace(string(b)), err
}
