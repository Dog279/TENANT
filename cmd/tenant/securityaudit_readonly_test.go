//go:build !windows

package main

import (
	"context"
	"testing"
)

// TEN-338: `tenant security audit` and `tenant permissions effective` are
// reports. Reading the config hardened credentials.json before the audit's own
// file checks ran, so the audit could not see the loose file it existed to
// report. Both now resolve read-only, as plain `tenant doctor` does (TEN-333).
func TestSecurityAuditLeavesModesAlone(t *testing.T) {
	cfg, data := t.TempDir(), t.TempDir()
	seedLooseTenantTree(t, cfg, data)
	before := modeSnapshot(t, cfg, data)

	report, _ := runSecurityAuditJSON(t, "--config", cfg, "--data", data)
	if _, err := captureTenantStdout(t, func() error {
		return cmdPermissions(context.Background(), []string{"effective", "--config", cfg, "--data", data})
	}); err != nil {
		t.Fatal(err)
	}

	after := modeSnapshot(t, cfg, data)
	for path, mode := range before {
		got, ok := after[path]
		if !ok {
			t.Errorf("the audit removed %s", path)
			continue
		}
		if got != mode {
			t.Errorf("the audit changed %s from %04o to %04o", path, mode, got)
		}
	}
	if !hasSecurityFinding(report.Findings, "file.credentials.json") {
		t.Errorf("the loose credentials.json should be a finding: %+v", report.Findings)
	}
}
