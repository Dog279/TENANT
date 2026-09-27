package main

// logs_alerts.go manages the alert rules (TEN-399) in config.json's
// logs.alerts, for `tenant logs alerts` and the dashboard: list them, add or
// replace one, disable or enable one, remove one. The evaluator reads the
// file at every run, so a change applies without a restart, and the save is
// audited like every config change.
//
// ponytail: these are the functions; the `tenant logs alerts` subcommand and
// the dashboard's /api/logbook/alerts routes wire them once TEN-377's
// command registry and TEN-378's API are merged.

import (
	"fmt"
	"slices"

	"tenant/internal/config"
	"tenant/internal/logs/alerts"
)

// alertRuleView is one rule as it runs: a built-in one with its logs.alerts
// entry laid over it, or the operator's own, and why it can't run, if so.
type alertRuleView struct {
	config.AlertRule
	Builtin bool   `json:"builtin,omitempty"`
	Error   string `json:"error,omitempty"`
}

// alertRules lists the rules, the built-in ones first.
func alertRules(cfgDir string) ([]alertRuleView, error) {
	lc, err := loadLaunchConfig(cfgDir)
	if err != nil {
		return nil, err
	}
	specs := alertSpecs(lc.Logs.Alerts)
	_, bad := alerts.Rules(specs)
	var out []alertRuleView
	for _, s := range alerts.Merge(specs) {
		v := alertRuleView{AlertRule: config.AlertRule{Name: s.Name, Filter: s.Filter,
			Threshold: config.AlertThreshold{Count: s.Count, Window: s.Window},
			Action:    config.AlertAction{Kind: s.Action, Target: s.Target}, Throttle: s.Throttle, Disabled: s.Disabled},
			Builtin: alerts.IsBuiltin(s.Name)}
		if v.Action.Kind == "" {
			v.Action.Kind = alerts.Dashboard
		}
		if err := bad[s.Name]; err != nil {
			v.Error = err.Error()
		}
		out = append(out, v)
	}
	return out, nil
}

// putAlertRule adds r to logs.alerts, or replaces the entry of its name,
// once the rules compile with it. An entry for a built-in rule overrides
// only the fields it sets.
func putAlertRule(cfgDir string, mem *launchConfig, r config.AlertRule) error {
	return editAlertRules(cfgDir, mem, r.Name, func(rules []config.AlertRule) ([]config.AlertRule, error) {
		i := slices.IndexFunc(rules, func(x config.AlertRule) bool { return x.Name == r.Name })
		if i < 0 {
			return append(rules, r), nil
		}
		rules[i] = r
		return rules, nil
	})
}

// setAlertRuleDisabled turns a rule off or back on. A built-in rule without
// an entry gets one that says only that.
func setAlertRuleDisabled(cfgDir string, mem *launchConfig, name string, disabled bool) error {
	return editAlertRules(cfgDir, mem, name, func(rules []config.AlertRule) ([]config.AlertRule, error) {
		i := slices.IndexFunc(rules, func(x config.AlertRule) bool { return x.Name == name })
		switch {
		case i >= 0:
			rules[i].Disabled = disabled
		case alerts.IsBuiltin(name):
			rules = append(rules, config.AlertRule{Name: name, Disabled: disabled})
		default:
			return nil, fmt.Errorf("no alert rule %q", name)
		}
		return rules, nil
	})
}

// removeAlertRule deletes the logs.alerts entry of name: the operator's
// rule is gone, and a built-in one is back to its defaults.
func removeAlertRule(cfgDir string, mem *launchConfig, name string) error {
	return editAlertRules(cfgDir, mem, name, func(rules []config.AlertRule) ([]config.AlertRule, error) {
		i := slices.IndexFunc(rules, func(x config.AlertRule) bool { return x.Name == name })
		if i < 0 {
			return nil, fmt.Errorf("no logs.alerts entry %q", name)
		}
		return slices.Delete(rules, i, i+1), nil
	})
}

// editAlertRules applies edit to logs.alerts as config.json holds it now,
// checks that rule name still compiles, and saves.
func editAlertRules(cfgDir string, mem *launchConfig, name string, edit func([]config.AlertRule) ([]config.AlertRule, error)) error {
	lc, err := loadLaunchConfig(cfgDir)
	if err != nil {
		return err
	}
	next, err := edit(slices.Clone(lc.Logs.Alerts))
	if err != nil {
		return err
	}
	if _, bad := alerts.Rules(alertSpecs(next)); bad[name] != nil {
		return fmt.Errorf("alert rule %s: %w", name, bad[name])
	}
	return persistLaunchConfig(cfgDir, mem, func(lc *launchConfig) { lc.Logs.Alerts = slices.Clone(next) })
}
