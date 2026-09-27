package main

// logs_alerts.go manages the alert rules (TEN-399) in config.json's
// logs.alerts: `tenant logs alerts` and the dashboard, both through the log
// book API's /api/logbook/alerts (logapi.AlertConfig, built here). The
// evaluator reads the file at every run, so a change applies without a
// restart; the save is audited like every config change (4501), and the API
// files the change itself (4902, log alerts).

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"tenant/internal/config"
	"tenant/internal/logs/alerts"
	"tenant/internal/logs/logapi"
)

// alertRulesAPI is the API's hooks for the rules in c's config.json.
func alertRulesAPI(c *commonFlags) logapi.AlertConfig {
	return logapi.AlertConfig{
		List: func(context.Context) ([]logapi.AlertRule, error) { return alertRules(c.cfgDir) },
		Put: func(_ context.Context, r logapi.AlertRule) error {
			return putAlertRule(c.cfgDir, c.lc, config.AlertRule{Name: r.Name, Filter: r.Filter, Threshold: config.AlertThreshold(r.Threshold),
				Action: config.AlertAction(r.Action), Throttle: r.Throttle, Disabled: r.Disabled})
		},
		Disable: func(_ context.Context, name string, off bool) error {
			return setAlertRuleDisabled(c.cfgDir, c.lc, name, off)
		},
		Remove: func(_ context.Context, name string) error { return removeAlertRule(c.cfgDir, c.lc, name) },
	}
}

// alertRules lists the rules as they run, the built-in ones first: a
// built-in rule with its logs.alerts entry laid over it, and why a rule
// can't run, if so.
func alertRules(cfgDir string) ([]logapi.AlertRule, error) {
	lc, err := loadLaunchConfig(cfgDir)
	if err != nil {
		return nil, err
	}
	specs := alertSpecs(lc.Logs.Alerts)
	_, bad := alerts.Rules(specs)
	var out []logapi.AlertRule
	for _, s := range alerts.Merge(specs) {
		v := logapi.AlertRule{Name: s.Name, Filter: s.Filter, Threshold: logapi.AlertThreshold{Count: s.Count, Window: s.Window},
			Action: logapi.AlertAction{Kind: s.Action, Target: s.Target}, Throttle: s.Throttle, Disabled: s.Disabled,
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
			return nil, fmt.Errorf("%w %q", logapi.ErrNoAlertRule, name)
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
			return nil, fmt.Errorf("%w entry %q in logs.alerts", logapi.ErrNoAlertRule, name)
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
		return fmt.Errorf("%w: %s: %v", logapi.ErrBadAlertRule, name, bad[name])
	}
	return persistLaunchConfig(cfgDir, mem, func(lc *launchConfig) { lc.Logs.Alerts = slices.Clone(next) })
}

// logsAlerts is `tenant logs alerts [list|add|enable|disable|remove]`, pos
// being what follows alerts.
func logsAlerts(ctx context.Context, lb *logbook, out logsOut, o logsOpts, pos []string) error {
	verb := "list"
	if len(pos) > 0 {
		verb, pos = pos[0], pos[1:]
	}
	var name string
	if verb != "list" {
		if len(pos) == 0 {
			return fmt.Errorf("logs alerts %s: name the rule", verb)
		}
		name = pos[0]
	}
	path := "/api/logbook/alerts/" + url.PathEscape(name)
	var rs logapi.AlertRules
	var err error
	switch verb {
	case "list":
		err = lb.do(ctx, http.MethodGet, "/api/logbook/alerts", nil, &rs)
	case "add":
		r := logapi.AlertRule{Name: name, Filter: strings.Join(pos[1:], " "), Threshold: logapi.AlertThreshold{Count: o.count, Window: o.window},
			Action: logapi.AlertAction{Kind: o.action, Target: o.target}, Throttle: o.throttle}
		err = lb.do(ctx, http.MethodPost, "/api/logbook/alerts", r, &rs)
	case "enable", "disable":
		err = lb.do(ctx, http.MethodPatch, path, map[string]bool{"disabled": verb == "disable"}, &rs)
	case "remove":
		err = lb.do(ctx, http.MethodDelete, path, nil, &rs)
	default:
		return fmt.Errorf("logs alerts: no %q; list, add, enable, disable or remove", verb)
	}
	if err != nil {
		return err
	}
	if verb != "list" {
		i := slices.IndexFunc(rs.Rules, func(r logapi.AlertRule) bool { return r.Name == name })
		if i < 0 { // removed
			if out.format == "text" {
				_, err = fmt.Fprintf(out.w, "%s: removed\n", name)
			}
			return err
		}
		rs.Rules = rs.Rules[i : i+1]
	}
	return out.alerts(rs.Rules)
}

func (o logsOut) alerts(rs []logapi.AlertRule) error {
	switch o.format {
	case "json":
		return o.json(logapi.AlertRules{Rules: rs})
	case "csv":
		rows := [][]string{{"name", "on", "builtin", "filter", "count", "window", "action", "target", "throttle", "error"}}
		for _, r := range rs {
			rows = append(rows, []string{r.Name, strconv.FormatBool(!r.Disabled), strconv.FormatBool(r.Builtin), r.Filter,
				strconv.Itoa(r.Threshold.Count), r.Threshold.Window, r.Action.Kind, r.Action.Target, r.Throttle, r.Error})
		}
		return o.csv(rows)
	}
	rows := [][]string{{"NAME", "ON", "FILTER", "THRESHOLD", "ACTION", "THROTTLE"}}
	for _, r := range rs {
		on, threshold, throttle, action := "yes", "-", r.Throttle, r.Action.Kind
		if r.Disabled {
			on = "no"
		}
		if r.Error != "" {
			on = "error: " + r.Error
		}
		if r.Builtin {
			on += " (built in)"
		}
		if r.Threshold.Count > 1 {
			threshold = fmt.Sprintf("%d in %s", r.Threshold.Count, r.Threshold.Window)
		}
		if throttle == "" {
			throttle = strings.TrimSuffix(alerts.DefaultThrottle.String(), "0s")
		}
		if r.Action.Target != "" {
			action += ": " + strconv.Quote(r.Action.Target)
		}
		rows = append(rows, []string{r.Name, on, r.Filter, threshold, action, throttle})
	}
	return o.table(rows)
}
