package pluginreg

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	"tenant/internal/logs/event"
)

// Events is a plugin's event catalog (TEN-393): its provider, plugin.<id>,
// writing the log of the same name, and the events every plugin has, with
// the same IDs in each. The host writes the lifecycle ones. A plugin that
// calls a web API sends its requests through Observe, which writes the API
// ones. A plugin declares its own events on Provider, from 1100, 2100 and
// 3100 up, so they never take a shared ID.
type Events struct {
	Provider *event.Provider

	Started, Enabled, Disabled, StartFailed *event.Def

	// The API events: nil for a plugin that calls no web API (NewEvents).
	RateLimited, AuthRejected, APIFailed *event.Def
}

// NewEvents declares plugin id's provider and its lifecycle events. title
// names the plugin in their messages. Like every provider, it is declared
// once, at package init.
func NewEvents(id, title string) *Events {
	p := event.NewProvider("plugin."+id, "plugin."+id)
	return &Events{
		Provider: p,
		Started: p.Event(1, event.Info, "Lifecycle", title+" started").
			Doc("The plugin was built and its tools can run.",
				"Tenant started with the plugin configured, or the operator enabled it for the first time since.",
				"None."),
		Enabled: p.Event(2, event.Info, "Lifecycle", "{target} enabled", "target", "tools").
			Doc("The operator turned the plugin, or one of its tools, on.",
				"An /enable in the terminal, or the Tools page of the dashboard.",
				"None."),
		Disabled: p.Event(3, event.Info, "Lifecycle", "{target} disabled", "target", "tools").
			Doc("The operator turned the plugin, or one of its tools, off. The agent can't call it until it is enabled again.",
				"A /disable in the terminal, or the Tools page of the dashboard.",
				"None."),
		StartFailed: p.Event(3001, event.Error, "Lifecycle", title+" could not start: {err}", "err").
			Doc("The plugin could not be built. At launch, Tenant stops with this error; on an /enable, the plugin stays off.",
				"Its credentials are missing, expired or revoked, its service or program can't be reached, or a setting is wrong.",
				"Read the error. Run `tenant doctor`, fix the setting or sign in again with /configure, then /enable the plugin."),
	}
}

// NewAPIEvents is NewEvents for a plugin that calls a web API: it also
// declares the events Observe writes.
func NewAPIEvents(id, title string) *Events {
	e := NewEvents(id, title)
	p := e.Provider
	e.RateLimited = p.Event(2001, event.Warning, "API", title+" rate limit reached on {op}: HTTP {status}", "op", "status", "retry", "path").
		Sensitive("path").
		Doc("The service refused a call because the account used up its request quota.",
			"The agent made many calls in a short time, or something else shares the account's quota.",
			"Wait until the quota resets (retry holds the service's Retry-After, when it sent one). If it keeps happening, use the tool less or raise the account's quota.")
	e.AuthRejected = p.Event(3002, event.Error, "API", title+" refused the credentials on {op}: HTTP {status}", "op", "status", "path").
		Sensitive("path").
		Doc("The service refused a call as unauthorized, so the tool failed.",
			"The token or key expired or was revoked, or it lacks the scope or access this call needs.",
			"Check the plugin's credentials: /configure it again, or paste a new key on the dashboard's Keys page.")
	e.APIFailed = p.Event(3003, event.Error, "API", title+" API call {op} failed: {err}", "op", "status", "err", "path").
		Sensitive("path").
		Doc("A call to the service failed on its side or on the way, so the tool failed. status is 0 when no answer came.",
			"The service had an outage or an error, or the network, DNS or a proxy failed.",
			"Retry later. If it persists, check the service's status page and this machine's connection.")
	return e
}

// Observe returns rt (nil: http.DefaultTransport) with each answer checked,
// logging the failures the operator must act on: a spent quota, refused
// credentials, a server error or an unreachable service. Any other 4xx is the
// call's own mistake, and the agent sees it in the tool result. The request's
// path is kept, masked; its query, which may carry what was searched, is not.
func (e *Events) Observe(rt http.RoundTripper) http.RoundTripper {
	if rt == nil {
		rt = http.DefaultTransport
	}
	return observed{rt: rt, e: e}
}

type observed struct {
	rt http.RoundTripper
	e  *Events
}

func (o observed) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := o.rt.RoundTrip(req)
	ctx := req.Context()
	kv := []any{"op", req.Method + " " + req.URL.Host, "path", req.URL.Path}
	switch {
	case err != nil:
		if ctx.Err() == nil { // a canceled turn is not the service's failure
			o.e.APIFailed.Log(ctx, append(kv, "status", 0, "err", err)...)
		}
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden && quotaSpent(resp):
		o.e.RateLimited.Log(ctx, append(kv, "status", resp.StatusCode, "retry", resp.Header.Get("Retry-After"))...)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		o.e.AuthRejected.Log(ctx, append(kv, "status", resp.StatusCode)...)
	case resp.StatusCode >= 500:
		o.e.APIFailed.Log(ctx, append(kv, "status", resp.StatusCode, "err", resp.Status)...)
	}
	return resp, err
}

// quotaSpent reports whether a 403 is a rate limit rather than a refusal:
// GitHub says so in its headers, Google in the body, whose start is read and
// put back.
func quotaSpent(resp *http.Response) bool {
	if resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != "" {
		return true
	}
	head, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), resp.Body), resp.Body}
	s := strings.ToLower(string(head))
	return strings.Contains(s, "rate limit") || strings.Contains(s, "ratelimit") || strings.Contains(s, "quota")
}

// checkEvents holds a plugin's catalog to its own provider, plugin.<id>,
// writing the log of the same name: every event it names must be that
// provider's. The event package already refuses, at registration, an ID
// declared twice and a template naming a field it does not declare.
func checkEvents(id string, ev *Events) error {
	if ev == nil {
		return nil
	}
	want := "plugin." + id
	if ev.Provider == nil || ev.Provider.Name != want || ev.Provider.Log != want {
		return fmt.Errorf("pluginreg: plugin %q must log through its own provider and log, %q", id, want)
	}
	for _, d := range []*event.Def{ev.Started, ev.Enabled, ev.Disabled, ev.StartFailed, ev.RateLimited, ev.AuthRejected, ev.APIFailed} {
		if d != nil && d.Spec().Provider != want {
			s := d.Spec()
			return fmt.Errorf("pluginreg: plugin %q names event %s/%d, which is not its provider's", id, s.Provider, s.ID)
		}
	}
	if ev.Started == nil || ev.Enabled == nil || ev.Disabled == nil || ev.StartFailed == nil {
		return fmt.Errorf("pluginreg: plugin %q's catalog lacks a lifecycle event; build it with NewEvents", id)
	}
	return nil
}
