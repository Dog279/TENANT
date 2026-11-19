package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"

	"tenant/internal/procprotect"
)

// processprotection.go (TEN-535): what `tenant doctor` and `tenant security
// audit` say about process protection. Both report this process: the same
// binary applies the same controls, in the same way, when it runs as serve
// or the TUI.

// oneLayer is the limit every system shares, in the words the plan asks for.
const oneLayer = "This is one layer. It protects the running process, not the files on disk, which a program of the same user can still read; and it does not stop an administrator (root), or a program that starts Tenant itself."

// processProtectionAudit is the audit's process-protection section.
type processProtectionAudit struct {
	// CompiledOut: the build was made with -tags debugattach.
	CompiledOut bool                  `json:"compiled_out"`
	Controls    []procprotect.Control `json:"controls"`
	// System is what the operating system reports about itself.
	System []string `json:"system,omitempty"`
	// StillReadable is what the controls do not cover on this system.
	StillReadable string `json:"still_readable,omitempty"`
	// SecretEnv names the secret environment variables that are set, on a
	// system that shows them to the user's other programs (macOS).
	SecretEnv []string `json:"secret_env,omitempty"`
	// CrashDump is GOTRACEBACK when it asks the Go runtime for a memory dump
	// at a crash (crash, or wer on Windows), which Tenant cannot lower.
	CrashDump string `json:"crash_dump_requested,omitempty"`
}

// auditProcessProtection fills the section for this process.
func auditProcessProtection(lc *launchConfig) processProtectionAudit {
	return newProcessProtectionAudit(protection, runtime.GOOS, procprotect.System(),
		exposedSecretEnv(runtime.GOOS, lc, os.Getenv), os.Getenv("GOTRACEBACK"))
}

func newProcessProtectionAudit(r procprotect.Report, goos string, system, secretEnv []string, gotraceback string) processProtectionAudit {
	a := processProtectionAudit{CompiledOut: r.Disabled, Controls: r.Controls, System: system,
		StillReadable: procprotect.StillOpen(goos), SecretEnv: secretEnv}
	if gotraceback == "crash" || goos == "windows" && gotraceback == "wer" {
		a.CrashDump = gotraceback
	}
	return a
}

func (a processProtectionAudit) missing() []procprotect.Control {
	return procprotect.Report{Controls: a.Controls}.Missing()
}

// crashDumpNote says what stands between GOTRACEBACK=crash and a dump on
// goos, and whether it is in force.
func (a processProtectionAudit) crashDumpNote(goos string) (note string, covered bool) {
	guard := map[string]string{"linux": "linux.dumpable", "darwin": "darwin.core-limit", "windows": "windows.wer-exclusion"}[goos]
	for _, c := range a.Controls {
		if c.ID == guard && c.OK {
			return "GOTRACEBACK=" + a.CrashDump + " asks the Go runtime for a memory dump when Tenant crashes, and Tenant cannot lower it from inside; here " + c.Name + " is what stops the dump", true
		}
	}
	return "GOTRACEBACK=" + a.CrashDump + " asks the Go runtime for a memory dump when Tenant crashes, and nothing in force here stops it", false
}

// checkProcessProtection is doctor's "process protection": each control as
// on or off, what the system itself reports, what stays readable on this
// system, and the limits in plain words. It warns when a control is off, the
// build has protection compiled out, a secret sits in the environment on
// macOS, or a crash dump was asked for on Windows.
func checkProcessProtection(_ context.Context, e *doctorEnv) checkResult {
	return processProtectionCheck(auditProcessProtection(e.c.lc), runtime.GOOS)
}

func processProtectionCheck(a processProtectionAudit, goos string) checkResult {
	if a.CompiledOut {
		return checkResult{Status: statusWarn, Detail: debugAttachNote,
			Fix: "install a release, or build without -tags debugattach"}
	}
	if len(a.Controls) == 0 {
		return checkResult{Status: statusSkip, Detail: "this system has no process-protection controls"}
	}
	r := checkResult{Status: statusOK}
	var lines, fixes []string
	for _, c := range a.Controls {
		switch {
		case !c.OK:
			r.Status = statusWarn
			lines = append(lines, "OFF: "+c.Name+": "+c.Reason+". That leaves this open: "+c.Leaves)
		case c.Note != "":
			lines = append(lines, "on:  "+c.Name+" ("+c.Note+")")
		default:
			lines = append(lines, "on:  "+c.Name)
		}
	}
	if len(a.missing()) > 0 {
		fixes = append(fixes, "the reason above is the system's own; `tenant security audit` lists it as a finding")
	}
	lines = append(lines, a.System...)
	if a.StillReadable != "" {
		lines = append(lines, "still readable: "+a.StillReadable)
	}
	if len(a.SecretEnv) > 0 {
		r.Status = statusWarn
		lines = append(lines, "a secret is in an environment variable ("+strings.Join(a.SecretEnv, ", ")+"), which macOS shows to your other programs")
		fixes = append(fixes, "save the key with /configure <plugin> or the dashboard's keys page, and remove the variable")
	}
	if a.CrashDump != "" {
		note, covered := a.crashDumpNote(goos)
		lines = append(lines, note)
		if goos == "windows" || !covered {
			r.Status = statusWarn
			fixes = append(fixes, "unset GOTRACEBACK where Tenant is started")
		}
	}
	lines = append(lines, oneLayer)
	r.Detail = strings.Join(lines, "\n       ")
	r.Fix = strings.Join(fixes, "; ")
	return r
}

// printProcessProtection is the audit's section, a line per control.
func printProcessProtection(a processProtectionAudit) {
	fmt.Println("process protection (this process; serve applies the same at its start):")
	switch {
	case a.CompiledOut:
		fmt.Println("  compiled out (-tags debugattach)")
	case len(a.Controls) == 0:
		fmt.Println("  no controls on this system")
	}
	for _, c := range a.Controls {
		state := "on "
		if !c.OK {
			state = "OFF"
		}
		fmt.Printf("  %s %-24s %s\n", state, c.ID, c.Name)
	}
	for _, ln := range a.System {
		fmt.Println("  " + ln)
	}
}

// processProtectionFindings are the audit's findings: a control that is off,
// a build with protection compiled out, and a secret in the environment
// where the system shows it.
func processProtectionFindings(a processProtectionAudit, add func(id, area string, severity auditSeverity, summary, remedy string)) {
	if a.CompiledOut {
		add("process.debugattach", "process_protection", auditWarning, debugAttachNote,
			"install a release, or build without -tags debugattach")
	}
	for _, c := range a.missing() {
		add("process."+c.ID, "process_protection", auditWarning,
			"process protection is off: "+c.Name+": "+c.Reason+". "+c.Leaves,
			"read the reason, which is the system's own; `tenant doctor` shows every control")
	}
	if len(a.SecretEnv) > 0 {
		add("process.secret-env", "process_protection", auditWarning,
			"a secret is in an environment variable ("+strings.Join(a.SecretEnv, ", ")+"), which macOS shows to the user's other programs",
			"save the key with /configure <plugin> or the dashboard's keys page, and remove the variable")
	}
}
