package logapi

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"tenant/internal/logs/event"
)

// fakeAlerts is an AlertConfig over a list, as the composition root's is
// over config.json.
func fakeAlerts(rules *[]AlertRule) AlertConfig {
	find := func(name string) int {
		return slices.IndexFunc(*rules, func(r AlertRule) bool { return r.Name == name })
	}
	return AlertConfig{
		List: func(context.Context) ([]AlertRule, error) { return *rules, nil },
		Put: func(_ context.Context, r AlertRule) error {
			if r.Filter == "" {
				return fmt.Errorf("%w: filter: empty", ErrBadAlertRule)
			}
			if i := find(r.Name); i >= 0 {
				(*rules)[i] = r
			} else {
				*rules = append(*rules, r)
			}
			return nil
		},
		Disable: func(_ context.Context, name string, off bool) error {
			i := find(name)
			if i < 0 {
				return ErrNoAlertRule
			}
			(*rules)[i].Disabled = off
			return nil
		},
		Remove: func(_ context.Context, name string) error {
			i := find(name)
			if i < 0 {
				return ErrNoAlertRule
			}
			*rules = slices.Delete(*rules, i, i+1)
			return nil
		},
	}
}

// The alert rules are listed, added, disabled and removed through the API;
// a bad rule is a 400 and an unknown one a 404; each change is a Security
// record.
func TestAlertRules(t *testing.T) {
	b := newBook(t)
	if rec := b.do(t, "GET", "/api/logbook/alerts", "", ""); rec.Code != 503 {
		t.Fatalf("without the hooks: %d", rec.Code)
	}
	rules := []AlertRule{{Name: "critical", Filter: "level:critical", Action: AlertAction{Kind: "dashboard"}, Builtin: true}}
	b.api.Alerts = fakeAlerts(&rules)

	got := decode[AlertRules](t, b.do(t, "GET", "/api/logbook/alerts", "", ""), 200)
	if len(got.Rules) != 1 || !got.Rules[0].Builtin {
		t.Fatalf("list = %+v", got)
	}
	got = decode[AlertRules](t, b.do(t, "POST", "/api/logbook/alerts", "application/json",
		`{"name":"cron-errors","filter":"log:cron level>=error","threshold":{"count":3,"window":"1h"},"action":{"kind":"discord"}}`), 200)
	if len(got.Rules) != 2 || got.Rules[1].Threshold.Count != 3 || got.Rules[1].Action.Kind != "discord" {
		t.Fatalf("after POST = %+v", got)
	}
	got = decode[AlertRules](t, b.do(t, "PATCH", "/api/logbook/alerts/critical", "application/json", `{"disabled":true}`), 200)
	if !got.Rules[0].Disabled {
		t.Fatalf("after PATCH = %+v", got)
	}
	got = decode[AlertRules](t, b.do(t, "DELETE", "/api/logbook/alerts/cron-errors", "", ""), 200)
	if len(got.Rules) != 1 {
		t.Fatalf("after DELETE = %+v", got)
	}
	for _, c := range []struct {
		method, path, body string
		code               int
	}{
		{"POST", "/api/logbook/alerts", `{"name":"x"}`, 400},
		{"POST", "/api/logbook/alerts", `{"name":"x","filter":"a","nope":1}`, 400},
		{"PATCH", "/api/logbook/alerts/critical", `{}`, 400},
		{"PATCH", "/api/logbook/alerts/nosuch", `{"disabled":false}`, 404},
		{"DELETE", "/api/logbook/alerts/nosuch", ``, 404},
	} {
		if rec := b.do(t, c.method, c.path, "application/json", c.body); rec.Code != c.code {
			t.Errorf("%s %s %s = %d, want %d: %s", c.method, c.path, c.body, rec.Code, c.code, rec.Body.String())
		}
	}
	b.waitFor(t, "security", func(evs []event.Event) bool {
		var msgs []string
		for _, e := range evs {
			if e.ID == 4902 {
				msgs = append(msgs, e.Msg)
			}
		}
		return slices.Equal(msgs, []string{
			`Log alerts reconfigured: alert rule cron-errors saved: filter "log:cron level>=error", action discord`,
			"Log alerts reconfigured: alert rule critical disabled",
			"Log alerts reconfigured: alert rule cron-errors removed",
		}) && evs[len(evs)-1].Surface == "dashboard" // an attribute since TEN-397, not data
	})
}
