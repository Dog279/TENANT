package main

import (
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"tenant/internal/config"
	"tenant/internal/logs"
	"tenant/internal/logs/logapi"
	"tenant/internal/logs/store"
)

// Rules are managed in logs.alerts: a bad one is refused and nothing is
// saved; a built-in one is disabled by an entry and back to its defaults
// when the entry goes; and the evaluator reads each change at its next run.
func TestManageAlertRules(t *testing.T) {
	cfg := t.TempDir()
	mem := &launchConfig{}
	find := func(name string) logapi.AlertRule {
		t.Helper()
		rules, err := alertRules(cfg)
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(rules, func(v logapi.AlertRule) bool { return v.Name == name })
		if i < 0 {
			t.Fatalf("no rule %s in %+v", name, rules)
		}
		return rules[i]
	}
	if v := find("critical"); !v.Builtin || v.Filter != "level:critical" || v.Action.Kind != "dashboard" || v.Disabled {
		t.Fatalf("critical = %+v", v)
	}

	if err := putAlertRule(cfg, mem, config.AlertRule{Name: "cron-errors", Filter: "log:cron level>>error"}); err == nil {
		t.Fatal("a rule whose filter doesn't parse was saved")
	}
	rule := config.AlertRule{Name: "cron-errors", Filter: "log:cron level>=error", Threshold: config.AlertThreshold{Count: 3, Window: "1h"},
		Action: config.AlertAction{Kind: "discord"}}
	if err := putAlertRule(cfg, mem, rule); err != nil {
		t.Fatal(err)
	}
	rule.Throttle = "2h"
	if err := putAlertRule(cfg, mem, rule); err != nil { // replaces it
		t.Fatal(err)
	}
	if v := find("cron-errors"); v.Builtin || v.Throttle != "2h" || v.Threshold.Count != 3 {
		t.Fatalf("cron-errors = %+v", v)
	}

	if err := setAlertRuleDisabled(cfg, mem, "critical", true); err != nil {
		t.Fatal(err)
	}
	if v := find("critical"); !v.Disabled || v.Filter != "level:critical" {
		t.Fatalf("critical after disable = %+v", v)
	}
	if err := setAlertRuleDisabled(cfg, mem, "nosuch", true); err == nil {
		t.Fatal("disabled a rule that doesn't exist")
	}
	specs := alertEvaluator(cfg, nil, nil).Specs() // what the next evaluation reads
	if len(specs) != 2 || specs[1].Name != "critical" || !specs[1].Disabled || len(mem.Logs.Alerts) != 2 {
		t.Fatalf("the evaluator reads %+v; memory holds %+v", specs, mem.Logs.Alerts)
	}

	if err := removeAlertRule(cfg, mem, "critical"); err != nil {
		t.Fatal(err)
	}
	if v := find("critical"); v.Disabled {
		t.Fatalf("critical after its entry went = %+v", v)
	}
	if err := removeAlertRule(cfg, mem, "critical"); err == nil {
		t.Fatal("removed an entry that isn't there")
	}
}

// tenant logs alerts, here and through a dashboard: add, list, disable a
// built-in rule, remove; a bad rule is refused.
func TestLogsAlerts(t *testing.T) {
	data, cfg, _ := logsBook(t)
	root := filepath.Join(data, "logs")
	h, err := logs.Open(root, logs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(root, "logbook.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h.AttachStore(st)
	defer h.Close()
	ts := httptest.NewServer((&logapi.API{Hub: func() *logs.Hub { return h }, Alerts: alertRulesAPI(&commonFlags{cfgDir: cfg})}).Handler())
	defer ts.Close()
	run := func(args ...string) (string, error) {
		t.Helper()
		return logsRun(t, append(args, "--data", data, "--config", cfg)...)
	}
	out, err := run("alerts", "add", "cron-errors", "log:cron", "level>=error", "--count", "3", "--window", "1h", "--action", "discord")
	if err != nil || !strings.Contains(out, "cron-errors") || !strings.Contains(out, "3 in 1h") || !strings.Contains(out, "log:cron level>=error") {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if _, err := run("alerts", "add", "bad", "level>>error"); err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Errorf("a bad rule: %v", err)
	}
	if out, err := run("alerts", "disable", "critical", "--url", ts.URL); err != nil || !strings.Contains(out, "no (built in)") {
		t.Fatalf("disable through the dashboard: %v\n%s", err, out)
	}
	local, err := run("alerts", "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	remote, err := run("alerts", "list", "--format", "json", "--url", ts.URL)
	if err != nil || local != remote || !strings.Contains(local, `"name": "cron-errors"`) {
		t.Fatalf("list here and remote differ (%v):\n%s\n%s", err, local, remote)
	}
	if out, err := run("alerts", "remove", "critical"); err != nil || !strings.Contains(out, "critical  yes (built in)") {
		t.Fatalf("remove a built-in's entry: %v\n%s", err, out)
	}
	if out, err := run("alerts", "remove", "cron-errors"); err != nil || out != "cron-errors: removed\n" {
		t.Fatalf("remove: %v\n%s", err, out)
	}
	if _, err := run("alerts", "enable", "nosuch"); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("an unknown rule: %v", err)
	}
}
