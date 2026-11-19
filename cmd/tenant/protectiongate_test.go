package main

import (
	"bufio"
	"bytes"
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
	"testing"
	"testing/iotest"

	"time"

	"tenant/internal/config"
	"tenant/internal/logs/event/eventtest"
	"tenant/internal/procprotect"
	"tenant/internal/version"
)

// The acknowledgement flow (TEN-535, section 8 of the plan). A test has no
// terminal, and Windows has no pseudo-terminal to give it, so these tests
// inject the two things a terminal is to the gate: whether a person is there
// (the interactive flag, or ackConsole) and what they type (the reader).
// protectiongate_process_test.go runs the same cases as real processes.

// offReport is a report with control id off for reason, and one control on.
func offReport(id, reason string) procprotect.Report {
	return procprotect.Report{Controls: []procprotect.Control{
		{ID: id, Name: "the " + id + " control", Reason: reason, Leaves: "what " + id + " leaves open"},
		{ID: "test.on", Name: "a control that is on", OK: true, Leaves: "nothing"},
	}}
}

func savedAcks(t *testing.T, cfg string) []config.ProtectionAck {
	t.Helper()
	stg, err := loadSettings(cfg, "main")
	if err != nil {
		t.Fatal(err)
	}
	return stg.ProtectionAcknowledged
}

func gate(cfg string, r procprotect.Report, ver, typed string, interactive bool) (string, error) {
	var out bytes.Buffer
	err := acknowledgeProtection(cfg, "main", r, ver, bufio.NewReader(strings.NewReader(typed)), &out, interactive)
	return out.String(), err
}

func refusal(err error) (r *protectionRefused) {
	errors.As(err, &r)
	return r
}

func TestAcknowledgeProtection(t *testing.T) {
	off := offReport("test.a", "operation not permitted")
	const prompt = "Continue without it? [y/N] "

	// Anything but yes is a no: nothing goes ahead, nothing is recorded.
	cfg := t.TempDir()
	for _, typed := range []string{"n\n", "\n", "", "y", "yes", "no\n", "maybe\n", "true\n", "1\n", "y y\n", " \n"} {
		out, err := gate(cfg, off, "1.0.0", typed, true)
		if r := refusal(err); r == nil || !r.declined || !strings.HasSuffix(out, prompt) {
			t.Errorf("typed %q: err = %v, out ends %q; want a declined refusal after the question", typed, err, out)
		}
	}
	var out bytes.Buffer
	err := acknowledgeProtection(cfg, "main", off, "1.0.0", bufio.NewReader(iotest.ErrReader(errors.New("the terminal went away"))), &out, true)
	if r := refusal(err); r == nil || !r.declined {
		t.Errorf("a read error: err = %v, want a declined refusal", err)
	}
	if _, err := os.Stat(settingsPath(cfg, "main")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a no wrote the settings file: %v", err)
	}

	// Nobody to ask: refused without a word on the console, and the error is
	// the whole reason.
	said, err := gate(cfg, off, "1.0.0", "y\n", false)
	r := refusal(err)
	if r == nil || r.declined || said != "" {
		t.Fatalf("no terminal: err = %v, said %q; want a refusal that asked nobody", err, said)
	}
	for _, want := range []string{"the test.a control", "test.a", "operation not permitted", "what test.a leaves open", "version 1.0.0", "at a terminal"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal lacks %q: %v", want, err)
		}
	}
	if len(savedAcks(t, cfg)) != 0 {
		t.Error("a refusal recorded an acknowledgement")
	}

	// Yes: what is missing and what that leaves open, the question, and the record.
	before := time.Now().Add(-time.Second)
	said, err = gate(cfg, off, "1.0.0", "y\n", true)
	if err != nil {
		t.Fatalf("yes: %v", err)
	}
	for _, want := range []string{"Tenant 1.0.0 could not apply", "the test.a control (test.a)", "why:   operation not permitted", "open:  what test.a leaves open", prompt, "recorded in " + settingsPath(cfg, "main")} {
		if !strings.Contains(said, want) {
			t.Errorf("the console lacks %q:\n%s", want, said)
		}
	}
	if strings.Contains(said, "a control that is on") {
		t.Errorf("the console lists a control that is in force:\n%s", said)
	}
	acks := savedAcks(t, cfg)
	if len(acks) != 1 || acks[0].Control != "test.a" || acks[0].Reason != "operation not permitted" || acks[0].Version != "1.0.0" || acks[0].At.Before(before) {
		t.Fatalf("recorded %+v", acks)
	}
	for _, typed := range []string{"Y\n", "yes\n", " YES \r\n"} {
		dir := t.TempDir()
		if _, err := gate(dir, off, "1.0.0", typed, true); err != nil || len(savedAcks(t, dir)) != 1 {
			t.Errorf("typed %q: %v, recorded %+v", typed, err, savedAcks(t, dir))
		}
	}

	// The same control, reason and version: remembered, with or without a terminal.
	for _, interactive := range []bool{false, true} {
		if said, err := gate(cfg, off, "1.0.0", "", interactive); err != nil || said != "" {
			t.Errorf("acknowledged, interactive=%v: err = %v, said %q; want to go ahead unasked", interactive, err, said)
		}
	}

	// A different control, a different reason, a new version: each asks again.
	for name, tc := range map[string]struct {
		r   procprotect.Report
		ver string
	}{
		"another reason":  {offReport("test.a", "permission denied"), "1.0.0"},
		"another control": {offReport("test.b", "operation not permitted"), "1.0.0"},
		"a new version":   {off, "1.0.1"},
	} {
		if _, err := gate(cfg, tc.r, tc.ver, "", false); refusal(err) == nil {
			t.Errorf("%s with no terminal: err = %v, want a refusal", name, err)
		}
		if said, err := gate(cfg, tc.r, tc.ver, "n\n", true); refusal(err) == nil || !strings.Contains(said, prompt) {
			t.Errorf("%s at a terminal: err = %v, want the question again:\n%s", name, err, said)
		}
	}
	if got := savedAcks(t, cfg); len(got) != 1 {
		t.Errorf("the refusals changed the record: %+v", got)
	}
	if _, err := gate(cfg, off, "1.0.1", "y\n", true); err != nil {
		t.Fatal(err)
	}
	if got := savedAcks(t, cfg); len(got) != 2 || got[0].Version != "1.0.0" || got[1].Version != "1.0.1" {
		t.Errorf("after a yes for the new version: %+v; want the old one kept, for a rollback", got)
	}

	// Two controls off, one acknowledged: only the other is asked about.
	two := offReport("test.a", "operation not permitted")
	two.Controls = append(two.Controls, procprotect.Control{ID: "test.c", Name: "the test.c control", Reason: "no such device", Leaves: "what test.c leaves open"})
	said, err = gate(cfg, two, "1.0.0", "y\n", true)
	if err != nil || !strings.Contains(said, "the test.c control") || strings.Contains(said, "the test.a control") {
		t.Errorf("one of two acknowledged: err = %v:\n%s", err, said)
	}
}

// With nothing missing, or a debugattach build, the settings file is not
// even read. With something missing it has to be readable: a file that is
// not is never taken for an acknowledgement, and never written over.
func TestAcknowledgeProtection_SettingsFile(t *testing.T) {
	cfg := t.TempDir()
	const corrupt = `{"permissions": {"exec": "ask"`
	path := settingsPath(cfg, "main")
	if err := os.WriteFile(path, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}
	disabled := offReport("test.a", "x")
	disabled.Disabled = true
	for name, r := range map[string]procprotect.Report{
		"every control on":    {Controls: []procprotect.Control{{ID: "test.on", OK: true}}},
		"no controls":         {},
		"a debugattach build": {Disabled: true},
		"a disabled report that lists a control as off": disabled,
	} {
		if said, err := gate(cfg, r, "1.0.0", "", false); err != nil || said != "" {
			t.Errorf("%s: err = %v, said %q; want no question", name, err, said)
		}
	}
	for _, interactive := range []bool{false, true} {
		said, err := gate(cfg, offReport("test.a", "x"), "1.0.0", "y\n", interactive)
		if err == nil || refusal(err) != nil || !strings.Contains(err.Error(), "cannot be read") || !strings.Contains(err.Error(), path) || said != "" {
			t.Errorf("an unreadable settings file, interactive=%v: err = %v, said %q", interactive, err, said)
		}
	}
	if b, _ := os.ReadFile(path); string(b) != corrupt {
		t.Errorf("the unreadable file was written over: %s", b)
	}

	// The file turns unreadable while the question is open: the yes is not
	// recorded, and that is an error, not a start.
	cfg = t.TempDir()
	breaks := readerFunc(func(p []byte) (int, error) {
		if err := os.WriteFile(settingsPath(cfg, "main"), []byte(corrupt), 0o600); err != nil {
			t.Error(err)
		}
		return copy(p, "y\n"), nil
	})
	var out bytes.Buffer
	err := acknowledgeProtection(cfg, "main", offReport("test.a", "x"), "1.0.0", bufio.NewReader(breaks), &out, true)
	if err == nil || refusal(err) != nil || !strings.Contains(err.Error(), "could not be recorded") {
		t.Errorf("a yes that cannot be recorded: %v", err)
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// gateFixture is serve or the TUI about to start with test.a off: its dirs,
// a console of typed, and what reached the system's log.
type gateFixture struct {
	c         *commonFlags
	said      *bytes.Buffer
	systemLog *[]string
}

func newGateFixture(t *testing.T, r procprotect.Report, typed string, interactive bool) gateFixture {
	t.Helper()
	root := t.TempDir()
	f := gateFixture{c: &commonFlags{agent: "main", cfgDir: filepath.Join(root, "cfg"), dataDir: filepath.Join(root, "data")},
		said: &bytes.Buffer{}, systemLog: new([]string)}
	for _, d := range []string{f.c.cfgDir, f.c.dataDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	prevP, prevC, prevL := protection, ackConsole, systemLog
	t.Cleanup(func() { protection, ackConsole, systemLog = prevP, prevC, prevL })
	protection = r
	ackConsole = func() (*bufio.Reader, io.Writer, bool) {
		return bufio.NewReader(strings.NewReader(typed)), f.said, interactive
	}
	systemLog = func(msg string) error { *f.systemLog = append(*f.systemLog, msg); return nil }
	return f
}

// logBookText is every text line under a log book root.
func logBookText(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".log") {
			line, _ := os.ReadFile(path)
			b.Write(line)
		}
		return nil
	})
	return b.String()
}

func TestStartGate(t *testing.T) {
	off := offReport("test.a", "operation not permitted")
	ctx := context.Background()

	// No terminal, nothing recorded: refused, with the reason in Tenant's
	// log and the system's, and nothing else read or written.
	f := newGateFixture(t, off, "y\n", false)
	if err := os.WriteFile(launchConfigPath(f.c.cfgDir), []byte(`{not json`), 0o600); err != nil { // would fail the start, were it loaded
		t.Fatal(err)
	}
	err := startGate(ctx, f.c, "serve", true)
	if r := refusal(err); r == nil || r.declined {
		t.Fatalf("no terminal: %v", err)
	}
	if f.said.Len() != 0 {
		t.Errorf("asked nobody, yet wrote to the console: %s", f.said)
	}
	want := []string{"did not start", "serve", "the test.a control", "operation not permitted", "what test.a leaves open", version.Version}
	if len(*f.systemLog) != 1 {
		t.Fatalf("the system's log got %d lines: %q", len(*f.systemLog), *f.systemLog)
	}
	book := logBookText(t, filepath.Join(f.c.dataDir, "logs"))
	for _, w := range want {
		if !strings.Contains((*f.systemLog)[0], w) {
			t.Errorf("the system's log lacks %q: %s", w, (*f.systemLog)[0])
		}
		if !strings.Contains(book, w) {
			t.Errorf("Tenant's log lacks %q:\n%s", w, book)
		}
	}
	if _, err := os.Stat(settingsPath(f.c.cfgDir, "main")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refusal wrote the settings file: %v", err)
	}
	if _, err := os.Stat(credentialsPath(f.c.cfgDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refusal touched credentials.json: %v", err)
	}

	// The log book is where config.json's logs.dir or --log-dir puts it.
	f = newGateFixture(t, off, "", false)
	moved := filepath.Join(f.c.dataDir, "elsewhere")
	cfgJSON, _ := json.Marshal(map[string]any{"logs": map[string]string{"dir": moved}})
	if err := os.WriteFile(launchConfigPath(f.c.cfgDir), cfgJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := startGate(ctx, f.c, "serve", true); refusal(err) == nil || !strings.Contains(logBookText(t, moved), "did not start") {
		t.Errorf("logs.dir: err = %v, log book at %s:\n%s", err, moved, logBookText(t, moved))
	}
	f.c.logDir = filepath.Join(f.c.dataDir, "flagged")
	if err := startGate(ctx, f.c, "serve", true); refusal(err) == nil || !strings.Contains(logBookText(t, f.c.logDir), "did not start") {
		t.Errorf("--log-dir: err = %v", err)
	}

	// A console nobody can see (serve --hidden) is no terminal.
	f = newGateFixture(t, off, "y\n", true)
	if err := startGate(ctx, f.c, "serve", false); refusal(err) == nil || refusal(err).declined || f.said.Len() != 0 || len(*f.systemLog) != 1 {
		t.Errorf("hidden: err = %v, said %q, system log %q", err, f.said, *f.systemLog)
	}

	// A system log that cannot be written does not turn the refusal into a start.
	f = newGateFixture(t, off, "", false)
	systemLog = func(string) error { return errors.New("no syslog here") }
	if err := startGate(ctx, f.c, "serve", true); refusal(err) == nil {
		t.Errorf("the system's log failed: err = %v, want the refusal still", err)
	}

	// At a terminal, no: non-zero, nothing recorded, and nothing in the logs
	// (the person is looking at it).
	f = newGateFixture(t, off, "n\n", true)
	if err := startGate(ctx, f.c, "tui", true); refusal(err) == nil || !refusal(err).declined {
		t.Fatalf("no at the terminal: %v", err)
	}
	if len(*f.systemLog) != 0 || len(savedAcks(t, f.c.cfgDir)) != 0 || !strings.Contains(f.said.String(), "Continue without it? [y/N]") {
		t.Errorf("no at the terminal: system log %q, recorded %+v, said:\n%s", *f.systemLog, savedAcks(t, f.c.cfgDir), f.said)
	}

	// At a terminal, yes: goes on, recorded, and the settings change is in
	// the Security log (logbook.db), through a log book that is closed again.
	f = newGateFixture(t, off, "y\n", true)
	prevDefault := slog.Default()
	if err := startGate(ctx, f.c, "serve", true); err != nil {
		t.Fatalf("yes at the terminal: %v", err)
	}
	if got := savedAcks(t, f.c.cfgDir); len(got) != 1 || got[0].Control != "test.a" || got[0].Version != version.Version {
		t.Errorf("recorded %+v", got)
	}
	if _, err := os.Stat(filepath.Join(f.c.dataDir, "logs", "logbook.db")); err != nil {
		t.Errorf("the Security log has no record of the change: %v", err)
	}
	if commandLog != nil || slog.Default() != prevDefault {
		t.Error("the borrowed log book was left open, or logging was left pointing at it")
	}

	// Recorded: the next start goes ahead unasked, with no terminal, and
	// files the warning; it does so at every start.
	typed := f.said.Len()
	ackConsole = func() (*bufio.Reader, io.Writer, bool) { return bufio.NewReader(strings.NewReader("")), f.said, false }
	rec := eventtest.Capture(t)
	feed := make(chan string, 4)
	for range 2 {
		if err := startGate(ctx, f.c, "serve", true); err != nil {
			t.Fatalf("acknowledged, no terminal: %v", err)
		}
		warnUnprotected(ctx, f.c, feed)
	}
	if line := <-feed; len(feed) != 1 || !strings.Contains(line, "running without the test.a control (test.a): operation not permitted. Acknowledged 20") {
		t.Errorf("the status feed: %q, and %d more", line, len(feed))
	}
	if f.said.Len() != typed || len(*f.systemLog) != 0 {
		t.Errorf("an acknowledged start asked again or was refused: %s %q", f.said, *f.systemLog)
	}
	evs := rec.Events()
	if len(evs) != 2 || evs[0].ID != 2030 || !strings.Contains(evs[0].Msg, "the test.a control (test.a) could not be applied: operation not permitted") {
		t.Fatalf("the warning at each start: %+v", evs)
	}
	acked := savedAcks(t, f.c.cfgDir)[0].At.Format(time.RFC3339)
	fields := map[string]string{}
	for _, d := range evs[0].Data {
		fields[d.Key] = fmt.Sprint(d.Value)
	}
	if fields["acknowledged"] != acked || fields["leaves"] != "what test.a leaves open" || fields["version"] != version.Version {
		t.Errorf("the warning's fields: %v; want acknowledged=%s", fields, acked)
	}

	// Every control on: no question, no warning, and the settings are not read.
	f = newGateFixture(t, procprotect.Report{Controls: []procprotect.Control{{ID: "test.on", OK: true}}}, "", false)
	if err := os.WriteFile(settingsPath(f.c.cfgDir, "main"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec = eventtest.Capture(t)
	if err := startGate(ctx, f.c, "serve", true); err != nil {
		t.Errorf("every control on: %v", err)
	}
	warnUnprotected(ctx, f.c, nil)
	if len(rec.Events()) != 0 {
		t.Errorf("every control on, yet a warning: %+v", rec.Events())
	}
}

// What the new binary printed for `version --protection`, as `tenant update`
// reads it.
func TestReportedProtection(t *testing.T) {
	off := offReport("linux.dumpable", "operation not permitted")
	line, _ := json.Marshal(off)
	r, err := reportedProtection([]byte("tenant 9.9.9 (commit=x)\n" + protectionLinePrefix + string(line) + "\r\n"))
	if err != nil || len(r.Missing()) != 1 || r.Missing()[0].ID != "linux.dumpable" || r.Missing()[0].Reason != "operation not permitted" {
		t.Errorf("a report: %+v, %v", r, err)
	}
	// A binary from before process protection prints only its version.
	if r, err := reportedProtection([]byte("tenant 1.0.0 (commit=x)\n")); err != nil || r.Disabled || len(r.Controls) != 0 {
		t.Errorf("no report line: %+v, %v", r, err)
	}
	if r, err := reportedProtection([]byte(protectionLinePrefix + `{"disabled":true,"controls":null}`)); err != nil || !r.Disabled {
		t.Errorf("a debugattach build: %+v, %v", r, err)
	}
	// A later version's new fields are not this version's business.
	if r, err := reportedProtection([]byte(protectionLinePrefix + `{"controls":[{"id":"x","ok":false,"reason":"r","since":"2.0"}],"schema":2}`)); err != nil || len(r.Missing()) != 1 {
		t.Errorf("unknown fields: %+v, %v", r, err)
	}
	many := procprotect.Report{}
	for range maxReportedControls + 1 {
		many.Controls = append(many.Controls, procprotect.Control{ID: "x", OK: true})
	}
	tooMany, _ := json.Marshal(many)
	for name, out := range map[string]string{
		"not JSON":             protectionLinePrefix + "off",
		"cut short":            protectionLinePrefix + `{"controls":[{"id":"x"`,
		"two values":           protectionLinePrefix + `{} {}`,
		"a control with no id": protectionLinePrefix + `{"controls":[{"ok":false,"reason":"r"}]}`,
		"the wrong shape":      protectionLinePrefix + `{"controls":"all"}`,
		"too many controls":    protectionLinePrefix + string(tooMany),
		"too long":             protectionLinePrefix + `{"controls":[{"id":"x","reason":"` + strings.Repeat("a", maxProtectionLine) + `"}]}`,
	} {
		if r, err := reportedProtection([]byte(out)); err == nil || !strings.Contains(err.Error(), "could not be read") || len(r.Controls) != 0 {
			t.Errorf("%s: %+v, %v; want an error and no report", name, r, err)
		}
	}
}

// FuzzReportedProtection: whatever a binary prints, reading its report never
// panics, and a report that is accepted is within the bounds and names every
// control.
func FuzzReportedProtection(f *testing.F) {
	ok, _ := json.Marshal(offReport("linux.dumpable", "operation not permitted"))
	for _, seed := range []string{
		"tenant 9.9.9\n" + protectionLinePrefix + string(ok) + "\n",
		"tenant 1.0.0\n",
		protectionLinePrefix + `{"disabled":true,"controls":null}`,
		protectionLinePrefix + `{"controls":[{"id":"","ok":false}]}`,
		protectionLinePrefix + `{"controls":[{"id":"x"`,
		protectionLinePrefix + `{} {}`,
		protectionLinePrefix + protectionLinePrefix + "{}",
		protectionLinePrefix + `{"controls":[{"id":"a\u0000b","reason":"\ud800"}]}`,
		protectionLinePrefix + "[" + strings.Repeat("[", 4096),
		"\x00\xff" + protectionLinePrefix + "null",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, out []byte) {
		r, err := reportedProtection(out)
		if err != nil {
			if r.Disabled || len(r.Controls) != 0 {
				t.Fatalf("an error with a report: %+v", r)
			}
			return
		}
		if len(r.Controls) > maxReportedControls {
			t.Fatalf("%d controls accepted", len(r.Controls))
		}
		for _, c := range r.Controls {
			if c.ID == "" {
				t.Fatalf("a control with no id accepted: %+v", r)
			}
		}
		if (len(r.Controls) > 0 || r.Disabled) && !bytes.Contains(out, []byte(protectionLinePrefix)) {
			t.Fatalf("a report out of output with no report line: %q", out)
		}
	})
}

// `tenant version --protection` prints what reportedProtection reads back.
func TestVersionPrintsItsProtectionReport(t *testing.T) {
	prev := protection
	t.Cleanup(func() { protection = prev })
	protection = offReport("linux.dumpable", "operation not permitted")
	out, _ := captureTenantStdout(t, func() error { return cmdVersion(context.Background(), []string{"--protection"}) })
	r, err := reportedProtection([]byte(out))
	if err != nil || len(r.Controls) != 2 || len(r.Missing()) != 1 || r.Missing()[0] != protection.Missing()[0] {
		t.Errorf("read back %+v, %v from:\n%s", r, err, out)
	}
	if out, _ := captureTenantStdout(t, func() error { return cmdVersion(context.Background(), nil) }); strings.Contains(out, protectionLinePrefix) || strings.Contains(out, "TEST BUILD") {
		t.Errorf("plain `tenant version`:\n%s", out)
	}
}

// install, start and restart ask before they touch the service manager;
// --print, stop, status and uninstall do not ask.
func TestServiceVerbsAskFirst(t *testing.T) {
	starts := func(calls [][]string) bool {
		for _, c := range calls {
			for _, a := range c[1:] {
				switch a {
				case "/Run", "/Create", "start", "restart", "enable", "bootstrap", "kickstart", "create":
					return true
				}
			}
		}
		return false
	}
	home := t.TempDir()
	for _, k := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME"} {
		t.Setenv(k, home) // a unit path under here, never the developer's own
	}
	// No --system service (Windows) and nothing loaded (macOS), so neither
	// waits for a stop that the recorder never performs.
	noService := func(argv []string) (string, error) {
		if argv[0] == "sc" || argv[0] == "launchctl" && argv[1] == "print" {
			return "", errors.New("exit status 1")
		}
		return "", nil
	}
	for _, verb := range []string{"install", "start", "restart"} {
		f := newGateFixture(t, offReport("test.a", "operation not permitted"), "", false)
		rec := &cmdRecorder{reply: noService}
		rec.install(t)
		args := []string{verb, "--config", f.c.cfgDir, "--data", f.c.dataDir, "--now"}
		err := cmdService(context.Background(), args)
		if r := refusal(err); r == nil || r.declined || starts(rec.calls) {
			t.Errorf("%s with no terminal: err = %v, commands %v; want a refusal before the service manager is asked for anything", verb, err, rec.calls)
		}

		f = newGateFixture(t, offReport("test.a", "operation not permitted"), "n\n", true)
		rec.calls = nil
		args[2], args[4] = f.c.cfgDir, f.c.dataDir
		if err := cmdService(context.Background(), args); refusal(err) == nil || !refusal(err).declined || starts(rec.calls) || !strings.Contains(f.said.String(), "Continue without it?") {
			t.Errorf("%s, no at the terminal: err = %v, commands %v", verb, err, rec.calls)
		}

		f = newGateFixture(t, offReport("test.a", "operation not permitted"), "y\n", true)
		rec.calls = nil
		args[2], args[4] = f.c.cfgDir, f.c.dataDir
		_ = cmdService(context.Background(), args) // what the manager's commands return is not this test's
		if got := savedAcks(t, f.c.cfgDir); len(got) != 1 || got[0].Version != version.Version || !starts(rec.calls) {
			t.Errorf("%s, yes at the terminal: recorded %+v, commands %v", verb, got, rec.calls)
		}
	}
	// (status does not ask either; it is left out here because it asks the hub
	// at the dashboard's address for its version.)
	for _, args := range [][]string{{"install", "--print"}, {"stop"}, {"uninstall"}} {
		f := newGateFixture(t, offReport("test.a", "operation not permitted"), "", false)
		rec := &cmdRecorder{reply: noService}
		rec.install(t)

		_, err := captureTenantStdout(t, func() error {
			return cmdService(context.Background(), append(args, "--config", f.c.cfgDir, "--data", f.c.dataDir))
		})
		if refusal(err) != nil || f.said.Len() != 0 {
			t.Errorf("service %v asked about process protection: %v %s", args, err, f.said)
		}
	}
}

// The settings watcher keeps the live settings tracking the file's
// acknowledgements, so a save by the running hub does not drop one that
// `tenant service restart` or `tenant update` recorded from another process.
func TestSettingsWatcherKeepsAcknowledgements(t *testing.T) {
	cfg := t.TempDir()
	live, err := loadSettings(cfg, "main") // the hub's, loaded at its start
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gate(cfg, offReport("test.a", "operation not permitted"), "1.0.1", "y\n", true); err != nil { // another process
		t.Fatal(err)
	}
	var mu sync.Mutex
	_ = applySettingsFile(cfg, "main", nil, nil, &mu, live)
	live.Permissions = map[string]string{"exec": "ask"}
	if err := live.Save(cfg, "main"); err != nil {
		t.Fatal(err)
	}
	if got := savedAcks(t, cfg); len(got) != 1 || got[0].Version != "1.0.1" {
		t.Errorf("after the hub's own save: %+v; want the acknowledgement kept", got)
	}
}

// Doctor and the security audit show what is missing and when that was
// acknowledged; an acknowledgement of another reason or another version is
// not one of this start's.
func TestDoctorAndAuditShowTheAcknowledgement(t *testing.T) {
	f := newGateFixture(t, offReport("test.a", "operation not permitted"), "", false)
	findings := func(a processProtectionAudit) string {
		report := securityAuditReport{ProcessProtection: a}
		evaluateSecurityAudit(&report)
		var b strings.Builder
		for _, fd := range report.Findings {
			b.WriteString(fd.ID + ": " + fd.Summary + "\n")
		}
		return b.String()
	}
	section := func(a processProtectionAudit) string {
		out, _ := captureTenantStdout(t, func() error { printProcessProtection(a); return nil })
		return out
	}

	stg, _ := loadSettings(f.c.cfgDir, "main")
	stg.Acknowledge("test.a", "another reason", version.Version, time.Now())
	stg.Acknowledge("test.a", "operation not permitted", "0.0.0-other", time.Now())
	if err := stg.Save(f.c.cfgDir, "main"); err != nil {
		t.Fatal(err)
	}
	a := auditProcessProtection(f.c)
	r := checkProcessProtection(context.Background(), &doctorEnv{c: f.c})
	const not = "Not acknowledged: `tenant tui` and `tenant serve` ask at a terminal, and a service does not start"
	if len(a.Acknowledged) != 0 || r.Status != statusWarn || !strings.Contains(r.Detail, "what test.a leaves open. "+not) || !strings.Contains(r.Fix, "`tenant service start`") {
		t.Errorf("not acknowledged: %+v\n%+v", a.Acknowledged, r)
	}
	if got := findings(a); !strings.Contains(got, "process.test.a: ") || !strings.Contains(got, not) {
		t.Errorf("the audit's finding:\n%s", got)
	}
	if got := section(a); !strings.Contains(got, "OFF test.a") || !strings.Contains(got, not) {
		t.Errorf("the audit's section:\n%s", got)
	}

	at := time.Date(2026, 11, 18, 19, 2, 11, 0, time.UTC)
	stg.Acknowledge("test.a", "operation not permitted", version.Version, at)
	if err := stg.Save(f.c.cfgDir, "main"); err != nil {
		t.Fatal(err)
	}
	a = auditProcessProtection(f.c)
	r = checkProcessProtection(context.Background(), &doctorEnv{c: f.c})
	const when = "Acknowledged 2026-11-18 19:02 UTC: Tenant starts, with a warning in its log"
	if len(a.Acknowledged) != 1 || !a.Acknowledged[0].At.Equal(at) || r.Status != statusWarn || !strings.Contains(r.Detail, "what test.a leaves open. "+when) || strings.Contains(r.Fix, "`tenant service start`") {
		t.Errorf("acknowledged: %+v\n%+v", a.Acknowledged, r)
	}
	if got := findings(a); !strings.Contains(got, when) {
		t.Errorf("the audit's finding:\n%s", got)
	}
	if got := section(a); !strings.Contains(got, when) {
		t.Errorf("the audit's section:\n%s", got)
	}
	if b, _ := json.Marshal(a); !strings.Contains(string(b), `"acknowledged":[{"control":"test.a","reason":"operation not permitted"`) {
		t.Errorf("the audit's JSON: %s", b)
	}
}
