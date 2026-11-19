package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"

	"tenant/internal/config"
	"tenant/internal/procprotect"
	"tenant/internal/version"
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
	// Acknowledged are the operator's acknowledgements of the controls that
	// are off, for this version (protection_acknowledged in the settings
	// file). A control that is off and not here stops serve and the TUI
	// until someone says yes at a terminal (protectiongate.go).
	Acknowledged []config.ProtectionAck `json:"acknowledged,omitempty"`
	// Signature is this binary's code signature, on macOS: the
	// hardened-runtime flag is what makes the system refuse a debugger
	// outright, and the install script, `tenant service install` and `tenant
	// update` put it on the binary they install (codesign.go).
	Signature *codeSignature `json:"signature,omitempty"`
	// SourceBuild: not a release build, so nothing installed it and nothing
	// was expected to sign it.
	SourceBuild bool `json:"source_build,omitempty"`
}

// auditProcessProtection fills the section for this process, and for the
// acknowledgements in c's settings file.
func auditProcessProtection(c *commonFlags) processProtectionAudit {
	a := newProcessProtectionAudit(protection, runtime.GOOS, procprotect.System(),
		exposedSecretEnv(runtime.GOOS, c.lc, os.Getenv), os.Getenv("GOTRACEBACK"))
	if missing := a.missing(); len(missing) > 0 {
		stg, _ := loadSettings(c.cfgDir, c.agent) // unreadable: nothing counts as acknowledged
		for _, m := range missing {
			if ack, ok := stg.Acknowledged(m.ID, m.Reason, version.Version); ok {
				a.Acknowledged = append(a.Acknowledged, ack)
			}
		}
	}
	if signsBinaries {
		a.SourceBuild = devBuild()
		sig := codeSignature{Kind: "its signature could not be read: this binary's path is unknown"}
		if exe, err := os.Executable(); err == nil {
			if exe, err = resolveExe(exe); err == nil {
				sig = readSignature(context.Background(), exe)
			}
		}
		a.Signature = &sig
	}
	return a
}

// unhardened: an installed macOS binary without the hardened-runtime flag.
func (a processProtectionAudit) unhardened() bool {
	return a.Signature != nil && !a.Signature.Runtime && !a.SourceBuild
}

const signFix = "`tenant service install` signs this binary; the install script and `tenant update` sign what they install"

// acknowledgedAt is when the operator acknowledged c being off, "" if never.
func (a processProtectionAudit) acknowledgedAt(c procprotect.Control) string {
	for _, k := range a.Acknowledged {
		if k.Control == c.ID && k.Reason == c.Reason {
			return k.At.UTC().Format("2006-01-02 15:04 UTC")
		}
	}
	return ""
}

// ackNote says whether c being off was acknowledged, and what follows.
func (a processProtectionAudit) ackNote(c procprotect.Control) string {
	if at := a.acknowledgedAt(c); at != "" {
		return "Acknowledged " + at + ": Tenant starts, with a warning in its log"
	}
	return "Not acknowledged: `tenant tui` and `tenant serve` ask at a terminal, and a service does not start"
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
	return processProtectionCheck(auditProcessProtection(e.c), runtime.GOOS)
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
	unacknowledged := false
	for _, c := range a.Controls {
		switch {
		case !c.OK:
			r.Status = statusWarn
			lines = append(lines, "OFF: "+c.Name+": "+c.Reason+". That leaves this open: "+c.Leaves+". "+a.ackNote(c))
			if a.acknowledgedAt(c) == "" {
				unacknowledged = true
			}
		case c.Note != "":
			lines = append(lines, "on:  "+c.Name+" ("+c.Note+")")
		default:
			lines = append(lines, "on:  "+c.Name)
		}
	}
	if len(a.missing()) > 0 {
		fixes = append(fixes, "the reason above is the system's own; `tenant security audit` lists it as a finding")
	}
	if unacknowledged {
		fixes = append(fixes, "to run without it, `tenant service start` (or `tenant serve`) at a terminal asks, and records your yes")
	}
	switch s := a.Signature; {
	case s == nil:
	case s.Runtime:
		lines = append(lines, "on:  the hardened runtime on this binary, which makes macOS refuse a debugger outright ("+s.Kind+")")
	case a.SourceBuild:
		lines = append(lines, "a source build without the hardened runtime ("+s.Kind+"): attach denied, not verified. `tenant service install` signs it")
	default:
		r.Status = statusWarn
		lines = append(lines, "OFF: the hardened runtime on this binary ("+s.Kind+"): a debugger is kept out by PT_DENY_ATTACH alone (attach denied, not verified)")
		fixes = append(fixes, signFix)
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
		if !c.OK {
			fmt.Printf("      %s\n", a.ackNote(c))
		}
	}
	if s := a.Signature; s != nil {
		state := "on "
		if !s.Runtime {
			state = "OFF"
		}
		fmt.Printf("  %s %-24s the hardened runtime on this binary (%s)\n", state, "darwin.hardened-runtime", s.Kind)
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
			"process protection is off: "+c.Name+": "+c.Reason+". "+c.Leaves+". "+a.ackNote(c),
			"read the reason, which is the system's own; `tenant doctor` shows every control")
	}
	if a.unhardened() {
		add("process.darwin.hardened-runtime", "process_protection", auditWarning,
			"the installed binary does not carry the hardened-runtime flag ("+a.Signature.Kind+"), so macOS does not refuse a debugger outright: PT_DENY_ATTACH at start is what keeps one out (attach denied, not verified)",
			signFix)
	}
	if len(a.SecretEnv) > 0 {
		add("process.secret-env", "process_protection", auditWarning,
			"a secret is in an environment variable ("+strings.Join(a.SecretEnv, ", ")+"), which macOS shows to the user's other programs",
			"save the key with /configure <plugin> or the dashboard's keys page, and remove the variable")
	}
}
