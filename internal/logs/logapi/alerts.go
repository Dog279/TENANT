package logapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"tenant/internal/logs/catalog"
)

// The alert rules (TEN-399) live in config.json's logs.alerts, which the
// composition root holds, so these routes go through AlertConfig. Each
// change files a Security record (LogReconfigured, log alerts); the save of
// config.json files its own (ConfigChanged, 4501).
//
//	GET    /api/logbook/alerts          the rules: the built-in ones, then the operator's
//	POST   /api/logbook/alerts          add a rule, or replace the entry of its name
//	PATCH  /api/logbook/alerts/{name}   {"disabled": true|false}
//	DELETE /api/logbook/alerts/{name}   remove the entry: a built-in rule is back to its defaults

// AlertConfig manages the alert rules. A nil func is not available here.
// Put and Disable fail with ErrBadAlertRule for a rule that doesn't
// compile, and Disable and Remove with ErrNoAlertRule for a name there is
// no rule (or entry) of.
type AlertConfig struct {
	List    func(ctx context.Context) ([]AlertRule, error)
	Put     func(ctx context.Context, r AlertRule) error
	Disable func(ctx context.Context, name string, disabled bool) error
	Remove  func(ctx context.Context, name string) error
}

var (
	ErrBadAlertRule = errors.New("alert rule not valid")
	ErrNoAlertRule  = errors.New("no such alert rule")
)

// AlertRule is a rule in logs.alerts' shape. Listed, it is the rule in
// force (a built-in one with its entry laid over it), whether it is built
// in, and why it can't run, if so; posted, it is the entry.
type AlertRule struct {
	Name      string         `json:"name"`
	Filter    string         `json:"filter,omitempty"`
	Threshold AlertThreshold `json:"threshold,omitzero"`
	Action    AlertAction    `json:"action,omitzero"`
	Throttle  string         `json:"throttle,omitempty"`
	Disabled  bool           `json:"disabled,omitempty"`
	Builtin   bool           `json:"builtin,omitempty"`
	Error     string         `json:"error,omitempty"`
}

// AlertThreshold fires a rule once Count events match within Window.
type AlertThreshold struct {
	Count  int    `json:"count,omitempty"`
	Window string `json:"window,omitempty"`
}

// AlertAction is dashboard, discord, imessage or cron (Target: the prompt).
type AlertAction struct {
	Kind   string `json:"kind,omitempty"`
	Target string `json:"target,omitempty"`
}

// AlertRules is every route's answer: the rules as they are now.
type AlertRules struct {
	Rules []AlertRule `json:"rules"`
}

func (a *API) putAlertRule(w http.ResponseWriter, r *http.Request) {
	var rule AlertRule
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rule); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	rule.Builtin, rule.Error = false, ""
	a.changeAlerts(w, r, a.Alerts.Put != nil, func(ctx context.Context) (string, error) {
		return fmt.Sprintf("alert rule %s saved: filter %q, action %s", rule.Name, rule.Filter, rule.Action.Kind), a.Alerts.Put(ctx, rule)
	})
}

func (a *API) disableAlertRule(w http.ResponseWriter, r *http.Request) {
	var c struct {
		Disabled *bool `json:"disabled"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil || c.Disabled == nil {
		writeError(w, http.StatusBadRequest, `body: {"disabled": true} or {"disabled": false}`)
		return
	}
	name, state := r.PathValue("name"), "enabled"
	if *c.Disabled {
		state = "disabled"
	}
	a.changeAlerts(w, r, a.Alerts.Disable != nil, func(ctx context.Context) (string, error) {
		return "alert rule " + name + " " + state, a.Alerts.Disable(ctx, name, *c.Disabled)
	})
}

func (a *API) removeAlertRule(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	a.changeAlerts(w, r, a.Alerts.Remove != nil, func(ctx context.Context) (string, error) {
		return "alert rule " + name + " removed", a.Alerts.Remove(ctx, name)
	})
}

// changeAlerts makes a change, files its Security record and answers with
// the rules.
func (a *API) changeAlerts(w http.ResponseWriter, r *http.Request, available bool, change func(context.Context) (string, error)) {
	if !available {
		writeError(w, http.StatusServiceUnavailable, "the alert rules can't be changed here")
		return
	}
	h, ok := a.hub(w)
	if !ok {
		return
	}
	details, err := change(r.Context())
	switch {
	case errors.Is(err, ErrBadAlertRule):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, ErrNoAlertRule):
		writeError(w, http.StatusNotFound, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.Record(r.Context(), h, catalog.LogReconfigured, "alerts", details)
	a.answerAlerts(w, r)
}

func (a *API) answerAlerts(w http.ResponseWriter, r *http.Request) {
	if a.Alerts.List == nil {
		writeError(w, http.StatusServiceUnavailable, "the alert rules can't be read here")
		return
	}
	rules, err := a.Alerts.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, AlertRules{Rules: append([]AlertRule{}, rules...)})
}
