package cron

import "tenant/internal/logs/event"

// Cron is the scheduler's event provider, writing the cron log. IDs follow
// the blocks in internal/logs/event's package doc.
var Cron = event.NewProvider("cron", "cron")

// Lifecycle.

var SchedulerStarted = Cron.Event(1, event.Info, "Scheduler", "Scheduler started with {jobs} jobs, {enabled} enabled", "jobs", "enabled", "tz", "catchup").
	Doc("The cron scheduler loaded its jobs and started checking for due runs.",
		"Serve or the TUI started with cron available.",
		"None. A job that can't be scheduled is reported on its own (cron/2001).")

// Job runs.

// JobRun is the activity of one job run, from its start to its stop or
// failure. A prompt job's run carries its turn's trace, and links to it.
var JobRun = Cron.Event(1001, event.Info, "Job run", "Run of job {job}", "job", "id", "kind", "exec").
	Doc("A scheduled job ran: start, then stop with its duration, or fail with its error.",
		"Its schedule came due, catch-up fired it, or someone ran it now (cron_run, /cron run, the dashboard).",
		"On a failure, read the error; for a prompt job, open its turn (the trace) to see what it did.")

var JobCatchup = Cron.Event(1002, event.Info, "Schedule", "Job {job} missed its run at {due}; it runs once now", "job", "id", "due").
	Doc("A job's run came due while Tenant was down or the job was disabled, and catch-up runs it once.",
		"cron.catchup is on, and the job is a prompt job without exec.",
		"None. Turn cron.catchup off to skip missed runs instead.")

var JobOutput = Cron.Event(9001, event.Verbose, "Job run", "Job {job} printed {bytes} bytes", "job", "bytes", "output").
	Sensitive("output").Diagnostic().
	Doc("The output of a shell job's command.",
		"A shell job ran.",
		"None. The output is masked in viewers; it can hold anything the command printed.")

// Warnings.

var JobUnschedulable = Cron.Event(2001, event.Warning, "Schedule", "Job {job} can't be scheduled and stays disabled: {err}", "job", "id", "err").
	Doc("A saved job has a schedule or timezone that no longer parses, so it is kept but never runs.",
		"config.json was edited by hand, or the timezone database changed.",
		"Fix the job's spec or tz (/cron, the dashboard), or remove it.")

var JobMissed = Cron.Event(2002, event.Warning, "Schedule", "Job {job} missed its run at {due}: {reason}", "job", "id", "due", "reason").
	Doc("A job's run came due while Tenant was down or the job was disabled, and it was skipped.",
		"Catch-up is off (it is held off while the model is degraded at start-up), or the job is a shell or exec job, which never catch up so a destructive job can't fire unexpectedly.",
		"Run it now if the missed run matters (/cron run). Turn cron.catchup on to catch up prompt jobs.")

var HistoryNotSaved = Cron.Event(2003, event.Warning, "Run history", "The run of job {job} was not saved to the run history: {err}", "job", "id", "err").
	Doc("A job ran, but its record did not reach cron-history.json.",
		"The data directory is full, unwritable or locked.",
		"Check the data directory's disk space and permissions. The run's events in this log still hold its outcome.")

var HistoryNotLoaded = Cron.Event(2004, event.Warning, "Run history", "The run history could not be read, so missed runs aren't caught up: {err}", "err").
	Doc("cron-history.json could not be read at start-up, so no job knows when it last ran.",
		"The file is unreadable or locked by another process.",
		"Check the file's permissions in the data directory; the next run writes a new record.")

// Configuration.

var JobAdded = Cron.Event(5001, event.Info, "Job", "Job {job} added: {spec}", "job", "id", "spec", "kind", "exec", "tz", "enabled").
	Doc("A cron job was created.",
		"The agent (cron_add), /cron add or the dashboard added it.",
		"None.")

var JobSwitched = Cron.Event(5002, event.Info, "Job", "Job {job} {state}", "job", "id", "state").
	Doc("A cron job was enabled or disabled.",
		"The agent, /cron or the dashboard changed it.",
		"None.")

var JobRemoved = Cron.Event(5003, event.Info, "Job", "Job {job} removed", "job", "id").
	Doc("A cron job was deleted.",
		"The agent, /cron remove or the dashboard removed it.",
		"None.")

var ExecSwitched = Cron.Event(5004, event.Info, "Job", "cron-exec turned {state}", "state").
	Doc("The global cron-exec switch changed: while off, shell jobs and exec prompt jobs fail instead of running.",
		"/cron exec or the dashboard flipped it.",
		"None when intended.")
