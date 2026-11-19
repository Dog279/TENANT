package main

import (
	"context"
	"strings"
	"testing"

	"tenant/internal/procprotect"
)

func linuxControls(dumpableOK bool) []procprotect.Control {
	dump := procprotect.Control{ID: "linux.dumpable", Name: "memory reads refused (the dumpable flag is off)", OK: dumpableOK, Leaves: "another program can read Tenant's memory"}
	if !dumpableOK {
		dump.Reason = "operation not permitted"
	}
	return []procprotect.Control{dump, {ID: "linux.core-limit", Name: "no core file", OK: true, Leaves: "a core file"}}
}

const scopeLine = "kernel.yama.ptrace_scope=1 (the system's own setting, not what protects Tenant)"

// Doctor's check: each control as on or off, the flag first with the
// system's own setting after it, what stays readable, and the limits.
func TestProcessProtectionCheck(t *testing.T) {
	audit := func(r procprotect.Report, goos string, secretEnv []string, gotraceback string) processProtectionAudit {
		return newProcessProtectionAudit(r, goos, []string{scopeLine}, secretEnv, gotraceback)
	}
	ok := procprotect.Report{Controls: linuxControls(true)}

	r := processProtectionCheck(audit(ok, "linux", nil, ""), "linux")
	flag, scope := strings.Index(r.Detail, "on:  memory reads refused"), strings.Index(r.Detail, scopeLine)
	if r.Status != statusOK || flag < 0 || scope < flag || r.Fix != "" {
		t.Errorf("all on: %+v", r)
	}
	for _, want := range []string{"still readable: the command line", "This is one layer", "not the files on disk", "administrator (root)"} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("all on: detail lacks %q:\n%s", want, r.Detail)
		}
	}

	r = processProtectionCheck(audit(procprotect.Report{Controls: linuxControls(false)}, "linux", nil, ""), "linux")
	if r.Status != statusWarn || !strings.Contains(r.Detail, "OFF: memory reads refused (the dumpable flag is off): operation not permitted. That leaves this open: another program can read Tenant's memory") || r.Fix == "" {
		t.Errorf("a control off: %+v", r)
	}

	r = processProtectionCheck(audit(procprotect.Report{Disabled: true}, "linux", nil, ""), "linux")
	if r.Status != statusWarn || r.Detail != debugAttachNote || !strings.Contains(r.Fix, "debugattach") {
		t.Errorf("compiled out: %+v", r)
	}
	if r = processProtectionCheck(audit(procprotect.Report{}, "plan9", nil, ""), "plan9"); r.Status != statusSkip {
		t.Errorf("a system with no controls: %+v", r)
	}

	mac := procprotect.Report{Controls: []procprotect.Control{
		{ID: "darwin.deny-attach", Name: "debugger attach refused (PT_DENY_ATTACH)", OK: true, Note: "attach denied, not verified"},
		{ID: "darwin.core-limit", Name: "no core file (soft core limit 0)", OK: true}}}
	r = processProtectionCheck(audit(mac, "darwin", []string{"GITHUB_TOKEN"}, ""), "darwin")
	if r.Status != statusWarn || !strings.Contains(r.Detail, "(attach denied, not verified)") ||
		!strings.Contains(r.Detail, "a secret is in an environment variable (GITHUB_TOKEN)") || !strings.Contains(r.Fix, "/configure") {
		t.Errorf("macOS with a secret in the environment: %+v", r)
	}

	// GOTRACEBACK=crash: moot on Linux while the flag is off, said so; a
	// warning where the control that stops the dump is missing, and always on
	// Windows; wer means something on Windows only.
	r = processProtectionCheck(audit(ok, "linux", nil, "crash"), "linux")
	if r.Status != statusOK || !strings.Contains(r.Detail, "GOTRACEBACK=crash asks the Go runtime for a memory dump") || !strings.Contains(r.Detail, "is what stops the dump") {
		t.Errorf("crash on Linux: %+v", r)
	}
	r = processProtectionCheck(audit(procprotect.Report{Controls: linuxControls(false)}, "linux", nil, "crash"), "linux")
	if r.Status != statusWarn || !strings.Contains(r.Detail, "nothing in force here stops it") || !strings.Contains(r.Fix, "unset GOTRACEBACK") {
		t.Errorf("crash on Linux with the flag on: %+v", r)
	}
	win := procprotect.Report{Controls: []procprotect.Control{{ID: "windows.wer-exclusion", Name: "no crash dump by Windows Error Reporting", OK: true}}}
	for _, v := range []string{"crash", "wer"} {
		if r = processProtectionCheck(audit(win, "windows", nil, v), "windows"); r.Status != statusWarn || !strings.Contains(r.Detail, "GOTRACEBACK="+v) {
			t.Errorf("%s on Windows: %+v", v, r)
		}
	}
	if r = processProtectionCheck(audit(ok, "linux", nil, "wer"), "linux"); r.Status != statusOK || strings.Contains(r.Detail, "GOTRACEBACK") {
		t.Errorf("wer on Linux means nothing: %+v", r)
	}
	if r = processProtectionCheck(audit(ok, "linux", nil, "all"), "linux"); strings.Contains(r.Detail, "GOTRACEBACK") {
		t.Errorf("GOTRACEBACK=all asks for no dump: %+v", r)
	}
}

// The audit raises a finding when a control is off, when the build has
// protection compiled out, and when a secret is in the environment where the
// system shows it; and none otherwise.
func TestProcessProtectionFindings(t *testing.T) {
	find := func(a processProtectionAudit) map[string]securityFinding {
		report := securityAuditReport{ProcessProtection: a}
		evaluateSecurityAudit(&report)
		out := map[string]securityFinding{}
		for _, f := range report.Findings {
			if f.Area == "process_protection" {
				out[f.ID] = f
			}
		}
		if !report.Safe || report.Warnings != len(report.Findings) {
			t.Errorf("process-protection findings are warnings: %+v", report.Findings)
		}
		return out
	}
	if got := find(newProcessProtectionAudit(procprotect.Report{Controls: linuxControls(true)}, "linux", nil, nil, "")); len(got) != 0 {
		t.Errorf("all on: %v", got)
	}
	got := find(newProcessProtectionAudit(procprotect.Report{Controls: linuxControls(false)}, "linux", nil, nil, ""))
	if f, ok := got["process.linux.dumpable"]; !ok || len(got) != 1 || !strings.Contains(f.Summary, "operation not permitted") || !strings.Contains(f.Summary, "another program can read Tenant's memory") {
		t.Errorf("a control off: %v", got)
	}
	got = find(newProcessProtectionAudit(procprotect.Report{Disabled: true}, "linux", nil, nil, ""))
	if f, ok := got["process.debugattach"]; !ok || len(got) != 1 || f.Summary != debugAttachNote {
		t.Errorf("compiled out: %v", got)
	}
	got = find(newProcessProtectionAudit(procprotect.Report{}, "darwin", nil, []string{"GITHUB_TOKEN", "X_BEARER_TOKEN"}, ""))
	if f, ok := got["process.secret-env"]; !ok || len(got) != 1 || !strings.Contains(f.Summary, "GITHUB_TOKEN, X_BEARER_TOKEN") {
		t.Errorf("a secret in the environment: %v", got)
	}
}

// The check and the audit section read what main recorded for this process.
func TestProcessProtectionReportsThisProcess(t *testing.T) {
	prev := protection
	t.Cleanup(func() { protection = prev })
	protection = procprotect.Report{Controls: linuxControls(false)}
	r := checkProcessProtection(context.Background(), &doctorEnv{c: &commonFlags{}})
	if r.Status != statusWarn || !strings.Contains(r.Detail, "OFF: memory reads refused") {
		t.Errorf("doctor: %+v", r)
	}
	a := auditProcessProtection(&commonFlags{})
	if len(a.missing()) != 1 || a.missing()[0].ID != "linux.dumpable" {
		t.Errorf("audit: %+v", a)
	}
	printed := func(f func()) string {
		out, _ := captureTenantStdout(t, func() error { f(); return nil })
		return out
	}
	out := printed(func() { printProcessProtection(a) })
	if !strings.Contains(out, "OFF linux.dumpable") || !strings.Contains(out, "on  linux.core-limit") {
		t.Errorf("the audit's section:\n%s", out)
	}
	protection = procprotect.Report{Disabled: true}
	if out := printed(func() { _ = cmdVersion(context.Background(), nil) }); !strings.Contains(out, debugAttachNote) {
		t.Errorf("`tenant version` of a debugattach build:\n%s", out)
	}
	if out := printed(func() { printProcessProtection(auditProcessProtection(&commonFlags{})) }); !strings.Contains(out, "compiled out (-tags debugattach)") {
		t.Errorf("the audit's section of a debugattach build:\n%s", out)
	}
	protection = procprotect.Report{}
	if out := printed(func() { _ = cmdVersion(context.Background(), nil) }); strings.Contains(out, "debugattach") {
		t.Errorf("`tenant version` of an ordinary build:\n%s", out)
	}
	if out := printed(func() { printProcessProtection(auditProcessProtection(&commonFlags{})) }); !strings.Contains(out, "no controls on this system") {
		t.Errorf("the audit's section with no controls:\n%s", out)
	}
}
