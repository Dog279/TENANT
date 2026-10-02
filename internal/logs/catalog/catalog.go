// Package catalog holds Tenant's built-in event providers and their
// definitions: each event ID with its template and its documentation,
// declared once and emitted where the action happens:
//
//	catalog.CompFailed.Log(ctx, "component", "discord", "err", err)
//
// IDs follow the blocks in internal/logs/event's package doc.
package catalog

import "tenant/internal/logs/event"

// The built-in providers, each writing its own log.
var (
	System   = event.NewProvider("system", "system")     // the process and its components
	Security = event.NewProvider("security", "security") // the audit trail
	Logbook  = event.NewProvider("logbook", "logbook")   // the log book itself
)

// system: lifecycle (1–999) and components (the 100s, 2100s and 3100s).

var Started = System.Event(1, event.Info, "Lifecycle", "Tenant {version} started as {role}", "version", "role").
	Doc("Tenant started.",
		"The operator or a service manager started serve or the TUI.",
		"None.")

var StoppedOK = System.Event(2, event.Info, "Lifecycle", "Tenant stopped cleanly after {uptime}", "uptime").
	Doc("Tenant finished its shutdown.",
		"It was asked to stop: Ctrl-C, the service manager, or an update.",
		"None.")

var Unexpected = System.Event(3, event.Critical, "Lifecycle", "The previous run ended unexpectedly; its last event was at {last}", "last", "boot").
	Doc("The previous run of Tenant never stopped cleanly.",
		"It crashed or was killed, or the machine lost power or restarted.",
		"Read the events before its last one, and any crash file filed at this start.")

// A component's state changes (internal/lifecycle's registry): each run of a
// component is one activity, so activity:<id> is its history.

var CompStarting = System.Event(101, event.Info, "Component", "{component} is starting", "component", "kind", "log").
	Doc("A component began to start.",
		"Tenant started it, or the operator turned it on.",
		"None; a running or failed event follows.")

var CompRunning = System.Event(102, event.Info, "Component", "{component} is running", "component", "kind", "log").
	Doc("A component is running, or has recovered from a degraded state.",
		"It started, or what it depends on works again.",
		"None.")

var CompStopped = System.Event(103, event.Info, "Component", "{component} stopped", "component", "kind", "log", "uptime").
	Doc("A component stopped in order.",
		"Tenant is shutting down, the operator turned it off, or a child process finished.",
		"None.")

var CompDegraded = System.Event(2101, event.Warning, "Component", "{component} is degraded: {err}", "component", "kind", "log", "err").
	Doc("A component still runs, but not as it should.",
		"Something it depends on failed: a file it reads, a connection, a poll.",
		"Open the component's log for the cause; a running event follows when it recovers.")

var ChildFailed = System.Event(2102, event.Warning, "Component", "Child process {component} failed: {err}", "component", "kind", "log", "err", "uptime").
	Doc("A child process exited with an error, or did not start.",
		"Its command failed, it crashed, or it could not be run. A command's non-zero exit is often its answer, not a fault.",
		"Its exit line in its own log has the code and the last lines it wrote.")

var CompFailed = System.Event(3101, event.Error, "Component", "{component} failed: {err}", "component", "err", "kind", "log", "uptime").
	Doc("A subsystem stopped with an error.",
		"Something it depends on failed: a connection, a file or its configuration.",
		"Open the component's log; the event before this one usually names the cause.")

// system: updates (the 1500s and 2500s), tenant update and serve's daily
// check (TEN-500). The check only reports; nothing is applied unasked.

var UpdateAvailable = System.Event(1501, event.Info, "Update", "A newer release is available: {latest} (running {current})", "latest", "current").
	Doc("A release newer than the running one exists. Nothing was changed.",
		"serve's daily check, or tenant doctor, asked the release source for the latest release.",
		"Run tenant update when convenient; \"update_check\": false in config.json turns the daily check off.")

var UpdateInterrupted = System.Event(2501, event.Warning, "Update", "The update from {from} to {to} was interrupted at {step}; running {running}", "from", "to", "step", "running").
	Doc("tenant update swapped the binary but never confirmed the new version running: its marker is still there.",
		"An OS stop or a kill landed during the update.",
		"Run tenant update: it clears the marker when the new version is what runs, and offers --rollback otherwise.")
