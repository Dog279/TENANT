package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"tenant/internal/logs/catalog"
	"tenant/internal/logs/event"
)

// A serve or TUI run files its start, its start-up as an activity, and how it
// stopped (TEN-383).
func TestARunFilesItsStartAndStop(t *testing.T) {
	c := captureChannelEvents(t)
	ctx := context.Background()

	r := beginRun(ctx, "serve")
	r.ready()
	var none error
	r.failed(&none) // deferred: a ready run's return files nothing
	r.stopped(ctx, nil)
	if e := c.matching(catalog.Started, ""); len(e) != 1 || e[0].Msg != "Tenant "+eventField(e[0], "version").Text()+" started as serve" {
		t.Errorf("Started = %v", e)
	}
	if len(c.matching(sysStartup, event.OpStart)) != 1 || len(c.matching(sysStartup, event.OpStop)) != 1 {
		t.Fatalf("start-up activity: %d starts, %d stops", len(c.matching(sysStartup, event.OpStart)), len(c.matching(sysStartup, event.OpStop)))
	}
	if eventField(c.matching(sysStartup, event.OpStop)[0], "duration").Value == nil {
		t.Error("the start-up's stop has no duration")
	}
	if e := c.matching(catalog.StoppedOK, ""); len(e) != 1 || e[0].Log != "system" || eventField(e[0], "uptime").Value == nil {
		t.Errorf("StoppedOK = %v", e)
	}

	// A start-up that returns an error fails its activity; a panic (no
	// error) is left to the boot record.
	boom := errors.New("no model")
	r = beginRun(ctx, "tui")
	r.failed(&none)
	if n := len(c.matching(sysStartup, event.OpFail)); n != 0 {
		t.Fatalf("a panic filed %d failures", n)
	}
	r.failed(&boom)
	if e := c.matching(sysStartup, event.OpFail); len(e) != 1 || eventField(e[0], "error").Text() != "no model" || e[0].Level != event.Error {
		t.Errorf("start-up failure = %v", e)
	}

	// A ready run that stops with an error says so instead of stopping cleanly.
	r = beginRun(ctx, "tui")
	r.ready()
	r.stopped(ctx, errors.New("no terminal"))
	if e := c.matching(sysRunFailed, ""); len(e) != 1 || e[0].Msg != "tui stopped with an error: no terminal" {
		t.Errorf("RunFailed = %v", e)
	}
	if n := len(c.matching(catalog.StoppedOK, "")); n != 1 {
		t.Errorf("%d clean stops, want only the first run's", n)
	}
}

// The settings file's reloads, its load failures and failed saves are
// configuration events in the system log.
func TestSettingsChangesAreFiled(t *testing.T) {
	c := captureChannelEvents(t)
	cfg := t.TempDir()
	rules := newToolRules()
	var mu sync.Mutex
	stg := &settings{Tools: map[string]bool{}}

	if err := os.WriteFile(filepath.Join(cfg, "settings.main.json"), []byte(`{"tool_rules":{"delete_*":"deny"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	applySettingsFile(cfg, "main", nil, rules, &mu, stg)
	if e := c.matching(sysSettingsApplied, ""); len(e) != 1 || !strings.HasSuffix(e[0].Msg, "settings.main.json was edited; applied 1 tool rule(s)") {
		t.Errorf("SettingsApplied = %v", e)
	}
	_ = os.WriteFile(filepath.Join(cfg, "settings.main.json"), []byte("{not json"), 0o644)
	applySettingsFile(cfg, "main", nil, rules, &mu, stg)
	if e := c.matching(sysConfigNotLoaded, ""); len(e) != 1 || !strings.Contains(e[0].Msg, "did not load, so the live settings stay as they were") {
		t.Errorf("ConfigNotLoaded = %v", e)
	}

	saveSettingsOrTell(stg, notADir(t), "main", func(string) {})
	if e := c.matching(sysNotSaved, ""); len(e) != 1 || !strings.HasPrefix(e[0].Msg, "settings.main.json was not saved: ") || e[0].Level != event.Warning {
		t.Errorf("NotSaved = %v", e)
	}
}

// Publishing the dashboard to the tailnet is an activity the CLI runs under;
// a failure fails it.
func TestTailscaleServeIsAnActivity(t *testing.T) {
	c := captureChannelEvents(t)
	f := &fakeTSCLI{resp: map[string]string{"status --json": tsRunningJSON}}
	m := newTSMgr(f, "tailscale")
	var acts []string
	m.run = func(ctx context.Context, bin string, args ...string) (string, error) {
		acts = append(acts, event.ActivityFromContext(ctx))
		return f.run(ctx, bin, args...)
	}
	if _, err := m.Serve(); err != nil {
		t.Fatal(err)
	}
	start := c.matching(sysTailnetServe, event.OpStart)
	if len(start) != 1 || len(c.matching(sysTailnetServe, event.OpStop)) != 1 {
		t.Fatalf("serve activity: %v", c.events)
	}
	for _, a := range acts {
		if a != start[0].Corr.ActivityID {
			t.Errorf("a CLI run under activity %q, want the serve's %q", a, start[0].Corr.ActivityID)
		}
	}

	if _, err := newTSMgr(&fakeTSCLI{}, "").Serve(); err == nil {
		t.Fatal("serve without the CLI must fail")
	}
	if e := c.matching(sysTailnetServe, event.OpFail); len(e) != 1 || !strings.Contains(eventField(e[0], "error").Text(), "not found") {
		t.Errorf("serve failure = %v", e)
	}
}

// An agent profile's model that its endpoint serves in another case, or not
// at all, is a configuration warning.
func TestProfileModelMismatchesAreFiled(t *testing.T) {
	c := captureChannelEvents(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"Big-Model"}]}`))
	}))
	defer srv.Close()
	reconcileServedModel(srv.URL, "", "big-model", "Programmer")
	reconcileServedModel(srv.URL, "", "gone", "Programmer")
	if e := c.matching(sysProfileModelCased, ""); len(e) != 1 || e[0].Msg != "Agent profile Programmer: model big-model is served as Big-Model; using that" {
		t.Errorf("ProfileModelCased = %v", e)
	}
	if e := c.matching(sysProfileModelMissing, ""); len(e) != 1 || eventField(e[0], "served").Text() != "Big-Model" {
		t.Errorf("ProfileModelMissing = %v", e)
	}
}
