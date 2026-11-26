package main

import (
	"context"
	"time"

	"tenant/internal/logs/catalog"
	"tenant/internal/logs/event"
	"tenant/internal/version"
)

// The system log's events the composition root writes (TEN-383,
// docs/EVENTS.md): a serve or TUI run's start-up and stop, parts that did not
// come up, changes that were not saved, the settings and credentials
// watchers, agent profiles' models and Tailscale. The provider and the events
// every package shares are in internal/logs/catalog; the shutdown's are in
// internal/lifecycle.

// Lifecycle (1–999, 3000s).

var sysStartup = catalog.System.Event(10, event.Info, "Lifecycle", "Start-up of {role}", "role").
	Keywords("lifecycle").
	Doc("serve or the TUI starting up: its start, then its stop with the duration once it is ready, or its failure with the error.",
		"The operator or a service manager started Tenant.",
		"None when it stops. On a failure Tenant did not start: read the error, fix its cause and start it again.")

var sysRunFailed = catalog.System.Event(3010, event.Error, "Lifecycle", "{role} stopped with an error: {err}", "role", "err").
	Keywords("lifecycle").
	Doc("Tenant ran, then stopped because of an error rather than being asked to. Its shutdown still ran.",
		"The terminal UI failed: the terminal went away or could not be driven.",
		"Read the error, then start Tenant again. If it repeats, run it in another terminal.")

// Components (2100s).

var sysUnavailable = catalog.System.Event(2110, event.Warning, "Component", "{component} unavailable ({effect}): {err}", "component", "effect", "err").
	Doc("A part of Tenant did not come up, or could not be refreshed, so Tenant runs without it. effect says what is missing.",
		"Its store or file could not be opened (permissions, a full disk, a database another process holds), or what it depends on failed.",
		"Read the error and fix its cause, then restart Tenant, or for the embedder switch the model again.")

// The embedder's watch (embedwatch.go): an error, so in the 3100s, and its
// end in the 1100s.

var sysEmbedderSilent = catalog.System.Event(3110, event.Error, "Component", "The embedder {embedder} has not answered for {for}: {err}", "embedder", "for", "err").
	Doc("Tenant asks its embedder for one small embedding every five minutes, and for this long none came back. Memory recall and wiki search wait for an embedding, so they fail or time out meanwhile. Written again every 30 minutes while it lasts; the built-in alert rule embedder-silent fires on it.",
		"The embedding server is stopped, or it takes connections and does not answer: a model runner stuck at full processor looks like that, and its model list still loads.",
		"Restart the embedding server (for Ollama: restart it, then check `ollama ps`). Tenant goes on by itself once it answers, and system 1110 says so.")

var sysEmbedderBack = catalog.System.Event(1110, event.Info, "Component", "The embedder {embedder} answers again after {after}", "embedder", "after").
	Doc("The embedder that system 3110 reported answered a question again. Memory recall and wiki search work again.",
		"The embedding server was restarted, or it finished what held it.",
		"None.")

// Storage (2200s).

var sysNotSaved = catalog.System.Event(2201, event.Warning, "Storage", "{what} was not saved: {err}", "what", "path", "err").
	Doc("A change applies in the running process but was not written to disk, so a restart will undo it.",
		"The file could not be written: permissions, a full disk, or a read-only config or data directory.",
		"Fix the path's permissions or free space, then make the change again so it is saved.")

// Configuration (5000s).

var sysSettingsApplied = catalog.System.Event(5001, event.Info, "Configuration", "{path} was edited; applied {changed}", "path", "changed").
	Keywords("config").
	Doc("The settings file changed on disk, and the running Tenant applied what differs, with no restart: tool rules, category approval modes or exec profiles.",
		"Someone edited the file by hand or from another window, or a script pushed it.",
		"None. Check that changed lists what you meant to change.")

var sysConfigNotLoaded = catalog.System.Event(5002, event.Warning, "Configuration", "{path} did not load, so {fallback}: {err}", "path", "fallback", "err").
	Keywords("config").
	Doc("A configuration file could not be read or parsed, so Tenant goes on without it; fallback says what it uses instead.",
		"The file has a JSON error, or it cannot be read.",
		"Fix the file (tenant doctor points at a JSON error); an edit is picked up without a restart.")

var sysKeysReloaded = catalog.System.Event(5003, event.Info, "Configuration", "credentials.json changed; the active model provider's key was reloaded").
	Keywords("config").
	Doc("A key was rotated outside the settings page, and the running Tenant now uses it, with no restart. The model log records the swap.",
		"A secrets manager or a hand edit changed credentials.json.",
		"None.")

var sysKeyNotReloaded = catalog.System.Event(5004, event.Warning, "Configuration", "A changed key did not reload into the live model provider: {err}", "provider", "source", "err").
	Keywords("config").
	Doc("A model provider's key changed, but the running Tenant still uses the old one. source says where the change came from.",
		"The provider's configuration could not be read, or the provider could not be rebuilt with the new key.",
		"Read the error, then switch to the provider again with /model use or the dashboard's model page.")

var sysProfileModelCased = catalog.System.Event(5010, event.Warning, "Agent profile", "Agent profile {profile}: model {configured} is served as {served}; using that", "profile", "configured", "served").
	Keywords("config").
	Doc("An agent profile names its model in a different letter case than the endpoint serves it, so Tenant uses the served name.",
		"A typo in the profile's model name. The endpoint's model lookup is case-sensitive.",
		"Correct the model name in the agent profile.")

var sysProfileModelMissing = catalog.System.Event(5011, event.Warning, "Agent profile", "Agent profile {profile}: the endpoint does not serve model {configured}; its calls may fail", "profile", "configured", "served").
	Keywords("config").
	Doc("An agent profile names a model its endpoint does not list, so the sub-agents that use it may fail every call.",
		"The model name is wrong, or the server loaded another model.",
		"Set the profile's model to one of served, or load the model on the server.")

// Tailscale (1300s).

var sysTailnetServe = catalog.System.Event(1301, event.Info, "Tailscale", "Publish the dashboard to the tailnet").
	Doc("tailscale serve publishing the loopback dashboard to the operator's tailnet: its start, then its stop with the duration, or its failure with the error.",
		"The operator ran /tailscale serve, or Tenant re-applied that choice at start.",
		"None when it stops. On a failure, install Tailscale or run tailscale up, then try again.")

var sysTailnetReset = catalog.System.Event(1302, event.Info, "Tailscale", "Withdraw the dashboard from the tailnet").
	Doc("tailscale serve reset withdrawing the dashboard from the tailnet: its start, then its stop with the duration, or its failure with the error.",
		"The operator ran /tailscale unserve.",
		"None when it stops. On a failure, check tailscale serve status and reset it by hand.")

// sysRun is a serve or TUI process in the system log: Started when it
// begins, its start-up as an activity until it is ready, and how it stopped.
type sysRun struct {
	role    string
	began   time.Time
	startup *event.Activity
	up      bool
}

// beginRun files the start of a serve or TUI process.
func beginRun(ctx context.Context, role string) *sysRun {
	catalog.Started.Log(ctx, "version", version.Version, "role", role)
	// Not the activity's context: goroutines started during start-up outlive it.
	_, act := event.Start(ctx, sysStartup, "role", role)
	return &sysRun{role: role, began: time.Now(), startup: act}
}

// ready ends the start-up.
func (r *sysRun) ready() {
	r.up = true
	r.startup.End(nil)
}

// failed files a start-up that ended with *errp. Defer it; it does nothing
// once the run is ready, or when *errp is nil (a panic, reported by the boot
// record instead).
func (r *sysRun) failed(errp *error) {
	if !r.up && *errp != nil {
		r.startup.End(errp)
	}
}

// stopped files how a ready run ended, once its shutdown has run.
func (r *sysRun) stopped(ctx context.Context, err error) {
	if err != nil {
		sysRunFailed.Log(ctx, "role", r.role, "err", err)
		return
	}
	catalog.StoppedOK.Log(ctx, "uptime", time.Since(r.began).Round(time.Second))
}

// Child processes (1400s).

var sysChromeProfilesSwept = catalog.System.Event(1401, event.Info, "Component", "Removed {n} leftover browser profile folder(s)", "n", "dir").
	Doc("Tenant removed web-bridge Chrome profile folders that a previous run left behind when it was killed before it could close its browser.",
		"An earlier Tenant ended without a clean shutdown (a service stop, Task Manager, taskkill /F).",
		"None. A folder a running Chrome still holds is left alone.")
