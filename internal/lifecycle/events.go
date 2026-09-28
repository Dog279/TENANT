package lifecycle

import (
	"tenant/internal/logs/catalog"
	"tenant/internal/logs/event"
)

// The shutdown's events, in the system log (docs/EVENTS.md): the shutdown and
// each of its stages is an activity, so the steps' failures carry their
// stage's activity id.

var shutdownRun = catalog.System.Event(20, event.Info, "Shutdown", "Shutdown of {steps} steps", "steps").
	Keywords("lifecycle").
	Doc("Tenant shutting down, stage by stage: its start, then its stop with the duration, or its failure naming every step that failed.",
		"serve or the TUI was asked to stop (Ctrl-C, the service manager, an update), or it is leaving after an error.",
		"None when it stops. On a failure, read the steps' events under it; the shutdown still ran every other step.")

var stageRun = catalog.System.Event(21, event.Info, "Shutdown", "Shutdown stage {stage}: {steps} steps", "stage", "steps").
	Keywords("lifecycle").
	Doc("One stage of the shutdown: unblock (deny pending approvals), intake (stop taking new work), drain (let running work finish) or release (close stores). Its start, then its stop with the duration, or its failure.",
		"The shutdown reached the stage.",
		"None when it stops. A slow drain is a job or turn still running; a failure names the steps that failed.")

var stepFailed = catalog.System.Event(2020, event.Warning, "Shutdown", "Shutdown step {stage}/{step} failed: {err}", "stage", "step", "err", "took").
	Doc("A component did not stop cleanly. The shutdown went on with the other steps.",
		"Closing it returned an error: a store that could not flush, a listener already gone, a timeout.",
		"Read the error. A store that failed to close may have lost its last writes; tenant doctor checks the databases.")

var stepPanicked = catalog.System.Event(3020, event.Error, "Shutdown", "Shutdown step {stage}/{step} panicked: {panic}", "stage", "step", "panic", "stack", "took").
	Doc("A component's stop crashed. The panic was contained and the shutdown went on with the other steps.",
		"A bug in that component's stop.",
		"Report it with the stack. The component may not have released what it held; check its store with tenant doctor.")
