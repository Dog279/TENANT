package main

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

func TestFormatBrowserSandboxAudit(t *testing.T) {
	tests := []struct {
		name       string
		configured bool
		detected   bool
		want       []string
	}{
		{name: "secure default", want: []string{"ENABLED", "secure default"}},
		{name: "active opt-in", configured: true, detected: true, want: []string{"DISABLED", "WARNING", "test container"}},
		{name: "rejected opt-in", configured: true, want: []string{"ENFORCED", "launch will be refused"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatBrowserSandboxAudit(tt.configured, "test container", tt.detected)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("audit output %q does not contain %q", got, want)
				}
			}
		})
	}
}

func TestSecurityAuditReadsBrowserSandboxConfig(t *testing.T) {
	dir := t.TempDir()
	if err := (&launchConfig{}).save(dir); err != nil {
		t.Fatal(err)
	}
	out, err := captureSecurityOutput(t, func() error {
		return cmdSecurity(context.Background(), []string{"audit", "--config", dir})
	})
	if err != nil {
		t.Fatalf("security audit: %v", err)
	}
	if !strings.Contains(out, "Browser process sandbox: ENABLED") || !strings.Contains(out, "--no-sandbox is excluded") {
		t.Fatalf("unexpected security audit output: %q", out)
	}
}

func captureSecurityOutput(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	callErr := fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b, readErr := io.ReadAll(r)
	if closeErr := r.Close(); readErr == nil {
		readErr = closeErr
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(b), callErr
}
