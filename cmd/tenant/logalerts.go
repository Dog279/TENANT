package main

// logalerts.go wires the log book's alert rules (internal/logs/alerts,
// TEN-399). The rules are the built-in ones laid under config.json's
// logs.alerts, read at every evaluation, so an edit applies without a
// restart. Actions go out through what already reaches the operator: the
// Discord relay's operator DM (witnessDM, set where the relay is built), an
// iMessage to imessage.operator, a one-off prompt through the cron
// scheduler, and an agent investigation (loginvestigate.go).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"tenant/internal/config"
	croneng "tenant/internal/cron"
	"tenant/internal/logs/alerts"
	"tenant/internal/logs/store"
	"tenant/internal/plugins/imessage"
)

// alertCron runs a one-off prompt through this process's cron scheduler;
// newCronManager sets it.
var alertCron atomic.Pointer[func(name, prompt string)]

// setAlertCron makes engine run alert prompts, under base.
func setAlertCron(base context.Context, engine *croneng.Engine) {
	fn := func(name, prompt string) {
		go engine.RunOnce(base, croneng.JobDef{Name: name, Prompt: prompt, Enabled: true}) // the safe runner: no exec
	}
	alertCron.Store(&fn)
}

// alertEvaluator is the housekeeping task that evaluates the alert rules
// over st; notify is the status feed's banner.
func alertEvaluator(cfgDir string, st *store.Store, notify func(string)) *alerts.Evaluator {
	return &alerts.Evaluator{
		Store:  st,
		Notify: notify,
		Specs: func() []alerts.Spec {
			lc, err := loadLaunchConfig(cfgDir)
			if err != nil || lc == nil {
				return nil // the built-in rules alone
			}
			return alertSpecs(lc.Logs.Alerts)
		},
		Act: func(ctx context.Context, a alerts.Alert) error {
			if a.Action == alerts.Investigate {
				return investigateAlert(ctx, cfgDir, st, a, notify) // TEN-400
			}
			return sendAlert(ctx, cfgDir, a)
		},
	}
}

// alertSpecs flattens logs.alerts into the evaluator's specs.
func alertSpecs(rules []config.AlertRule) []alerts.Spec {
	out := make([]alerts.Spec, len(rules))
	for i, r := range rules {
		out[i] = alerts.Spec{Name: r.Name, Filter: r.Filter, Count: r.Threshold.Count, Window: r.Threshold.Window,
			Action: r.Action.Kind, Target: r.Action.Target, Throttle: r.Throttle, Disabled: r.Disabled}
	}
	return out
}

// sendAlert carries out an alert's Discord, iMessage or cron action. Each
// gets only the alert (§11: no data fields; a cron prompt not even the
// event's message) and a link to the event on the dashboard.
func sendAlert(ctx context.Context, cfgDir string, a alerts.Alert) error {
	lc, _ := loadLaunchConfig(cfgDir)
	link := alerts.Link(dashboardBase(lc), a.Seq)
	switch a.Action {
	case alerts.Discord:
		dm := witnessDM.Load()
		if dm == nil {
			return errors.New("no Discord relay runs in this process")
		}
		return (*dm)(a.Text(link))
	case alerts.IMessage:
		var op string
		if lc != nil {
			op = strings.TrimSpace(lc.IMessage.Operator)
		}
		if op == "" {
			return errors.New("imessage.operator is not set")
		}
		nat, err := imessage.OpenNative(imessage.NativeConfig{})
		if err != nil {
			return err
		}
		defer nat.Close()
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		_, err = nat.NewChat(ctx, op, a.Text(link))
		return err
	case alerts.Cron:
		run := alertCron.Load()
		if run == nil {
			return errors.New("no cron scheduler runs in this process")
		}
		(*run)("alert "+a.Rule, a.Target+"\n\n"+a.Facts(link))
		return nil
	}
	return fmt.Errorf("unknown action %q", a.Action)
}

// dashboardBase is the dashboard's address: attachBaseURL's, over https when
// the dashboard serves TLS.
func dashboardBase(lc *launchConfig) string {
	base := attachBaseURL("", lc)
	if lc != nil && lc.Dashboard.TLSCert != "" {
		base = "https://" + strings.TrimPrefix(base, "http://")
	}
	return base
}
