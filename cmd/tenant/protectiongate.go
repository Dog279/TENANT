package main

// protectiongate.go (TEN-535, section 8 of the process-protection plan,
// "When a protection cannot be applied"): a process protection the operating
// system could not apply is acknowledged by a person before Tenant runs the
// agent. docs/serve-runbook.md has the operator's view.
//
// `tenant tui` and `tenant serve` read the report before they load anything
// else (startGate). At a terminal they say which protection is missing and
// what that leaves open, and ask; anything but yes ends the start. With no
// terminal there is nobody to ask, so the start is refused, with the reason
// in Tenant's log and in the system's, unless the same control failing for
// the same reason in the same version was acknowledged before.
//
// `tenant service install | start | restart`, `tenant setup` when it
// installs the service, and `tenant update` (for the new binary's report)
// ask at the terminal they were typed in (acknowledgeProtection), so the
// question comes up while the operator is there. Every other command runs
// without asking. A build made with -tags debugattach reports no controls,
// so there is nothing to ask about.
//
// The acknowledgement is protection_acknowledged in settings.<agent>.json.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-isatty"

	"tenant/internal/logs"
	"tenant/internal/logs/catalog"
	"tenant/internal/logs/event"
	"tenant/internal/procprotect"
	"tenant/internal/version"
)

// ackConsole is where a missing protection is put to a person: what they
// type, where they read it, and whether anyone is there (both ends are a
// terminal). Tests swap it: a test has no terminal.
var ackConsole = func() (in *bufio.Reader, out io.Writer, interactive bool) {
	return bufio.NewReader(os.Stdin), os.Stderr, fileIsTerminal(os.Stdin) && fileIsTerminal(os.Stderr)
}

func fileIsTerminal(f *os.File) bool {
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}

// systemLog writes one line to the operating system's own log: the
// Application event log on Windows, syslog elsewhere, which is the journal
// under systemd (systemlog_<os>.go). It is the one thing Tenant writes there
// (docs/design/logging-v2.md keeps everything else in the log book): a
// Tenant that did not start cannot show its own log, and the operator of a
// service that is not running looks at the system's. Tests swap it.
var systemLog = writeSystemLog

// testBuildNote is what `tenant version` says about a build made with -tags
// procprotecttest (protecttest.go); "" in every other build.
var testBuildNote string

// afterProtect is given Protect's report as soon as it is made, in a build
// made with -tags procprotecttest (protecttest.go); nil in every other build.
var afterProtect func(procprotect.Report)

// sessionHelperEnv is added to the environment of the helper process serve
// starts on Windows (sessionend_windows.go), which otherwise gets nothing of
// serve's. Nil in every build but one made with -tags procprotecttest, where
// the helper has to be told what its serve was told: it is the same program,
// and applies the controls to itself.
var sessionHelperEnv []string

// protectionLinePrefix starts the line `tenant version --protection` adds:
// the process's procprotect.Report as JSON.
const protectionLinePrefix = "protection: "

// maxProtectionLine and maxReportedControls bound what is read from another
// binary's output. A system has three controls today.
const (
	maxProtectionLine   = 64 << 10
	maxReportedControls = 64
)

// reportedProtection reads the report out of what `<binary> version
// --protection` printed. A binary from before process protection ignores the
// flag and prints no such line: that is the zero report, with nothing to
// acknowledge, since such a binary applies nothing and asks nothing. A line
// that is there and cannot be read is an error: the caller does not install
// a binary whose report it could not read.
func reportedProtection(out []byte) (procprotect.Report, error) {
	for _, line := range strings.Split(string(out), "\n") {
		body, ok := strings.CutPrefix(strings.TrimSpace(line), protectionLinePrefix)
		if !ok {
			continue
		}
		var r procprotect.Report
		bad := func(why string) (procprotect.Report, error) {
			return procprotect.Report{}, errors.New("the new binary's process-protection report could not be read: " + why)
		}
		if len(body) > maxProtectionLine {
			return bad("it is too long")
		}
		dec := json.NewDecoder(strings.NewReader(body)) // a later version's new fields are ignored
		if err := dec.Decode(&r); err != nil {
			return bad(err.Error())
		}
		if dec.More() {
			return bad("something follows it on the line")
		}
		if len(r.Controls) > maxReportedControls {
			return bad("it lists too many controls")
		}
		for _, c := range r.Controls {
			if c.ID == "" {
				return bad("a control has no id")
			}
		}
		return r, nil
	}
	return procprotect.Report{}, nil
}

// protectionRefused is a start, an install or an update that did not go
// ahead: a protection is missing and nobody acknowledged it.
type protectionRefused struct {
	missing  []procprotect.Control
	version  string
	declined bool // a person was asked, and did not say yes
}

func (e *protectionRefused) Error() string {
	if e.declined {
		return "the missing process protection was not acknowledged, so nothing was started or changed"
	}
	return fmt.Sprintf("process protection is incomplete, and nobody has acknowledged that for version %s: %s. "+
		"Run `tenant service start` (or `tenant serve`) at a terminal to acknowledge it; `tenant doctor` shows every control",
		e.version, describeControls(e.missing))
}

// describeControls is each control with the system's reason and what its
// absence leaves open, on one line.
func describeControls(cs []procprotect.Control) string {
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = fmt.Sprintf("%s (%s) could not be applied: %s. That leaves this open: %s", c.Name, c.ID, c.Reason, c.Leaves)
	}
	return strings.Join(parts, " | ")
}

// pendingProtection returns the controls of r that are not in force and that
// nobody has acknowledged for version ver. With every control in force the
// settings file is not read.
func pendingProtection(cfgDir, agentID string, r procprotect.Report, ver string) ([]procprotect.Control, error) {
	missing := r.Missing()
	if r.Disabled || len(missing) == 0 {
		return nil, nil
	}
	stg, err := loadSettings(cfgDir, agentID)
	if err != nil {
		// Nothing in an unreadable file counts as acknowledged, and a yes is
		// not written over it.
		return nil, fmt.Errorf("a process protection is missing (%s), and %s, which records whether that was acknowledged, cannot be read: %w",
			describeControls(missing), settingsPath(cfgDir, agentID), err)
	}
	var need []procprotect.Control
	for _, c := range missing {
		if _, ok := stg.Acknowledged(c.ID, c.Reason, ver); !ok {
			need = append(need, c)
		}
	}
	return need, nil
}

// askProtection says what is missing and what that leaves open, and asks.
// Only y or yes, with its line end, is a yes: a read error and the end of
// the input are not.
func askProtection(need []procprotect.Control, ver string, in *bufio.Reader, out io.Writer) bool {
	fmt.Fprintf(out, "\nProcess protection is incomplete. On this system Tenant %s could not apply:\n\n", ver)
	for _, c := range need {
		fmt.Fprintf(out, "  %s (%s)\n    why:   %s\n    open:  %s\n\n", c.Name, c.ID, c.Reason, c.Leaves)
	}
	fmt.Fprint(out, "Continue without it? [y/N] ")
	line, err := in.ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return err == nil && (a == "y" || a == "yes")
}

// recordProtection writes the acknowledgement of need for ver into the
// settings file as it is now, not as it was before the question.
func recordProtection(cfgDir, agentID string, need []procprotect.Control, ver string, out io.Writer) error {
	path := settingsPath(cfgDir, agentID)
	stg, err := loadSettings(cfgDir, agentID)
	if err == nil {
		now := time.Now()
		for _, c := range need {
			stg.Acknowledge(c.ID, c.Reason, ver, now)
		}
		err = stg.Save(cfgDir, agentID)
	}
	if err != nil {
		return fmt.Errorf("the acknowledgement could not be recorded in %s, so Tenant would ask again and a service would not start: %w", path, err)
	}
	fmt.Fprintf(out, "Acknowledged, and recorded in %s. Tenant %s starts with a warning in its log until this is fixed.\n", path, ver)
	return nil
}

// acknowledgeProtection is the check for a command typed by the operator
// that starts, or is about to install, the binary that reported r as version
// ver. Nothing missing, or all of it acknowledged: nil. Otherwise it asks at
// the console and records a yes; a no, or nobody to ask, is a
// *protectionRefused.
func acknowledgeProtection(cfgDir, agentID string, r procprotect.Report, ver string, in *bufio.Reader, out io.Writer, interactive bool) error {
	need, err := pendingProtection(cfgDir, agentID, r, ver)
	if err != nil || len(need) == 0 {
		return err
	}
	if !interactive {
		return &protectionRefused{missing: need, version: ver}
	}
	if !askProtection(need, ver, in, out) {
		return &protectionRefused{missing: need, version: ver, declined: true}
	}
	return recordProtection(cfgDir, agentID, need, ver, out)
}

// startGate is the check for serve and the TUI (role), run once the dirs are
// known and before either loads its configuration, a secret or a store.
// visible is false when nobody can see this process's console (serve
// --hidden, which the Windows logon task passes).
func startGate(ctx context.Context, c *commonFlags, role string, visible bool) error {
	need, err := pendingProtection(c.cfgDir, c.agent, protection, version.Version)
	if err == nil && len(need) == 0 {
		return nil
	}
	in, out, interactive := ackConsole()
	interactive = interactive && visible
	switch {
	case err != nil:
	case !interactive:
		err = &protectionRefused{missing: need, version: version.Version}
	case !askProtection(need, version.Version, in, out):
		err = &protectionRefused{missing: need, version: version.Version, declined: true}
	default:
		// A yes: the start goes on, so the log book may open. The one-shot
		// commands' book files the settings change in the Security log.
		release := borrowCommandLog(c)
		err = recordProtection(c.cfgDir, c.agent, need, version.Version, out)
		release()
	}
	if err != nil && !interactive {
		reportNotStarted(ctx, c, role, err)
	}
	return err
}

// reportNotStarted puts the reason serve or the TUI did not start where the
// operator of a service looks: Tenant's log book and the system's log.
// Nothing else is loaded for it: the log folder's place is the one key read
// from config.json.
func reportNotStarted(ctx context.Context, c *commonFlags, role string, why error) {
	if err := toLogbook(ctx, c, sysNotStartedUnprotected.Build(ctx, "role", role, "version", version.Version, "why", why)); err != nil {
		slog.Warn("the reason Tenant did not start could not be written to its log", "err", err)
	}
	if err := systemLog(fmt.Sprintf("Tenant %s (%s) did not start: %v", version.Version, role, why)); err != nil {
		slog.Warn("the reason Tenant did not start could not be written to the system's log", "err", err)
	}
}

// toLogbook writes events into Tenant's log book, logbook.db, which `tenant
// logs` and the dashboard read, through a hub of its own that is closed
// again. Nothing else is loaded for it: the log folder's place is the one
// key read from config.json.
func toLogbook(ctx context.Context, c *commonFlags, evs ...*event.Event) error {
	root := filepath.Join(c.dataDir, "logs")
	var cfg struct {
		Logs struct {
			Dir string `json:"dir"`
		} `json:"logs"`
	}
	if b, err := os.ReadFile(launchConfigPath(c.cfgDir)); err == nil && json.Unmarshal(b, &cfg) == nil && cfg.Logs.Dir != "" {
		root = expandPath(cfg.Logs.Dir)
	}
	if c.logDir != "" {
		root = expandPath(c.logDir)
	}
	hub, err := logs.Open(root, logs.Options{})
	if err != nil {
		return err
	}
	// If logbook.db does not open, the day file under <root>/system still
	// gets the line.
	_, _ = hub.OpenStore(nil)
	for _, e := range evs {
		hub.Emit(ctx, e)
	}
	return hub.Close()
}

// borrowCommandLog opens the one-shot commands' log book for serve or the
// TUI, which has not opened its own yet, and returns what closes it again
// and leaves logging as it was.
func borrowCommandLog(c *commonFlags) (release func()) {
	if commandLog != nil {
		return func() {}
	}
	prev := slog.Default()
	openOneShotLog(c)
	return func() {
		closeCommandLog()
		commandLog = nil
		event.Install(nil)
		logs.SetFallback(nil)
		slog.SetDefault(prev)
	}
}

var sysNotStartedUnprotected = catalog.System.Event(3030, event.Error, "Lifecycle", "{role} did not start: {why}", "role", "why", "version").
	Keywords("lifecycle").
	Doc("Tenant refused to start. The operating system could not apply one of its process protections, there was no terminal to ask at, and nobody had acknowledged that control failing for that reason in this version. why names the control, the system's reason and what its absence leaves open. The same line is in the system's own log: the Application event log on Windows, syslog or the journal elsewhere.",
		"A service or a scheduled start met a control the system refuses, on a system where that was never acknowledged, or after an update: a new version asks again.",
		"Run tenant service start (or tenant serve) at a terminal, read what is missing and answer the question. tenant doctor shows every control and why it is off.")

var sysStartedUnprotected = catalog.System.Event(2030, event.Warning, "Lifecycle", "Running without a process protection ({command}): {name} ({control}) could not be applied: {reason}", "control", "name", "reason", "leaves", "acknowledged", "version", "command").
	Keywords("lifecycle").
	Doc("A Tenant command started although the operating system could not apply one of its process protections. It is written once at every such start, by every command that opens Tenant's data (serve and the TUI, which first ask at a terminal and go ahead only on an acknowledgement, and every other command, which is not asked: chat, os, the plugin commands, eval and the rest); help, version, completion and a plain doctor or security audit open nothing and write nothing. command is the command; leaves says what the missing control leaves open; acknowledged is when the operator said yes for this control, reason and version, empty when nobody did.",
		"The system refuses the control for the reason given. For serve and the TUI a person answered yes to \"Continue without it?\"; another command runs without asking.",
		"None if that is accepted. Removing the cause named in reason restores the protection at the next start; tenant doctor shows every control.")

// warnUnprotected files, at every start of serve or the TUI that goes ahead
// without a protection, which one, and when that was acknowledged: an event
// in the system log, and a line in the status feed (serve's stdout, which
// the supervisor keeps; the TUI's feed pane).
func warnUnprotected(ctx context.Context, c *commonFlags, feed chan<- string) {
	missing := protection.Missing()
	if len(missing) == 0 {
		return
	}
	stg, _ := loadSettings(c.cfgDir, c.agent) // never nil; the gate has read it once already
	for _, m := range missing {
		ack, _ := stg.Acknowledged(m.ID, m.Reason, version.Version)
		sysStartedUnprotected.Log(ctx, "control", m.ID, "name", m.Name, "reason", m.Reason, "leaves", m.Leaves,
			"acknowledged", ack.At.Format(time.RFC3339), "version", version.Version, "command", runningCommand)
		select {
		case feed <- fmt.Sprintf("process protection: running without %s (%s): %s. Acknowledged %s; `tenant doctor` says what that leaves open",
			m.Name, m.ID, m.Reason, ack.At.Format("2006-01-02 15:04 UTC")):
		default: // a full feed costs the line, never the start
		}
	}
}

// runningCommand is the command this process runs ("chat", "serve"): main
// sets it, and the service sets "serve".
var runningCommand string

var unprotectedLogged sync.Once

// stderrAtTerminal: a person can read what is written to stderr. Tests swap
// it (protecttest.go).
var stderrAtTerminal = func() bool { return fileIsTerminal(os.Stderr) }

// logUnprotected files every control that is not in force, once per process,
// for a command other than serve and the TUI (which do it at their start,
// warnUnprotected): the same event, sysStartedUnprotected, in the log book,
// and a short line on stderr when that is a terminal. These commands are not
// asked (decision A40-bis): the record is what the operator finds afterwards.
//
// It runs where every command that opens Tenant's data passes, once the
// directories are known (commonFlags.resolveDirs), so a command added later
// is covered without knowing of it. A command that opens no data (help,
// version, completion, doctor and security audit without --fix) writes
// nothing; with every control in force nothing is written either.
func logUnprotected(ctx context.Context, c *commonFlags) {
	missing := protection.Missing()
	if len(missing) == 0 {
		return
	}
	unprotectedLogged.Do(func() {
		stg, _ := loadSettings(c.cfgDir, c.agent) // unreadable: nothing counts as acknowledged
		atTerminal := stderrAtTerminal()
		var evs []*event.Event
		for _, m := range missing {
			at := ""
			if ack, ok := stg.Acknowledged(m.ID, m.Reason, version.Version); ok {
				at = ack.At.Format(time.RFC3339)
			}
			evs = append(evs, sysStartedUnprotected.Build(ctx, "control", m.ID, "name", m.Name, "reason", m.Reason, "leaves", m.Leaves,
				"acknowledged", at, "version", version.Version, "command", runningCommand))
			if atTerminal {
				fmt.Fprintf(os.Stderr, "process protection: running without %s: %s. `tenant doctor` says what that leaves open\n", m.ID, m.Reason)
			}
		}
		if err := toLogbook(ctx, c, evs...); err != nil {
			slog.Warn("a missing process protection could not be written to the log book", "err", err)
		}
	})
}
