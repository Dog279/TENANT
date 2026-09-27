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

// system: lifecycle (1–999) and components (3100s).

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

var CompFailed = System.Event(3101, event.Error, "Component", "{component} failed: {err}", "component", "err").
	Doc("A subsystem stopped with an error.",
		"Something it depends on failed: a connection, a file or its configuration.",
		"Open the component's log; the event before this one usually names the cause.")
