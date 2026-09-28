package main

import (
	"context"
	"strings"
	"testing"

	"tenant/internal/config"
	"tenant/internal/logs/alerts"
	"tenant/internal/logs/event"
)

// The actions reach the operator's DM and the cron scheduler with the alert
// and a link to the event; a cron prompt never carries the log's text.
func TestSendAlert(t *testing.T) {
	cfg := t.TempDir()
	var dm, prompt string
	prevDM, prevCron := witnessDM.Load(), alertCron.Load()
	t.Cleanup(func() { witnessDM.Store(prevDM); alertCron.Store(prevCron) })
	send := func(s string) error { dm = s; return nil }
	witnessDM.Store(&send)
	run := func(_, p string) { prompt = p }
	alertCron.Store(&run)

	ctx := context.Background()
	a := alerts.Alert{Rule: "critical", Action: alerts.Discord, Seq: 42, Level: event.Critical, Log: "system", Provider: "system", ID: 4, Msg: "The previous run crashed"}
	link := "http://127.0.0.1:8770/logs/event/42"
	if err := sendAlert(ctx, cfg, a); err != nil || !strings.Contains(dm, "The previous run crashed") || !strings.HasSuffix(dm, link) {
		t.Fatalf("discord: %v\n%s", err, dm)
	}
	a.Action, a.Target = alerts.Cron, "Find out why."
	if err := sendAlert(ctx, cfg, a); err != nil || !strings.HasPrefix(prompt, "Find out why.\n\n") || !strings.HasSuffix(prompt, link) || strings.Contains(prompt, "crashed") {
		t.Fatalf("cron: %v\n%s", err, prompt)
	}
	a.Action = alerts.IMessage
	if err := sendAlert(ctx, cfg, a); err == nil || !strings.Contains(err.Error(), "imessage.operator") {
		t.Fatalf("imessage without an operator: %v", err)
	}

	got := alertSpecs([]config.AlertRule{{Name: "x", Filter: "log:cron", Threshold: config.AlertThreshold{Count: 3, Window: "5m"},
		Action: config.AlertAction{Kind: "cron", Target: "p"}, Throttle: "1h", Disabled: true}})
	if want := (alerts.Spec{Name: "x", Filter: "log:cron", Count: 3, Window: "5m", Action: "cron", Target: "p", Throttle: "1h", Disabled: true}); got[0] != want {
		t.Fatalf("alertSpecs = %+v", got[0])
	}
}
