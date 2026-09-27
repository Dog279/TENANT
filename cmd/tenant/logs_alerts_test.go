package main

import (
	"slices"
	"testing"

	"tenant/internal/config"
)

// Rules are managed in logs.alerts: a bad one is refused and nothing is
// saved; a built-in one is disabled by an entry and back to its defaults
// when the entry goes; and the evaluator reads each change at its next run.
func TestManageAlertRules(t *testing.T) {
	cfg := t.TempDir()
	mem := &launchConfig{}
	find := func(name string) alertRuleView {
		t.Helper()
		rules, err := alertRules(cfg)
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(rules, func(v alertRuleView) bool { return v.Name == name })
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
