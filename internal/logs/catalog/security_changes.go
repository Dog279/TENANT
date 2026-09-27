package catalog

import (
	"context"

	"tenant/internal/logs/event"
)

// security: changes the operator (or the agent, or Tenant itself) made to
// what Tenant is and holds (docs/design/logging-v2.md §10.1): configuration
// (4500s), secrets (4600s), scheduled jobs (4700s), data lifecycle (4800s)
// and the log book itself (4900s). Each is written through Audit by the code
// that makes the change, before it returns. actor says who made it
// (operator, agent or tenant) and surface where (tui, dashboard, cli, or the
// surface of the agent's turn).

var ConfigChanged = Security.Event(4501, event.Info, "Configuration", "{file} changed: {changes} settings", "file", "changes", "diff").
	Keywords("audit.success", "config").
	Doc("A configuration file was saved with changes. diff lists each setting that changed, old → new, with every secret value replaced by its name.",
		"The operator changed a setting (the TUI, the dashboard, a tenant command), the agent changed one through a tool, or Tenant saved one it manages.",
		"None when expected. Otherwise read diff, and the process that wrote it, to see what changed and who ran it.")

var SecretStored = Security.Event(4601, event.Info, "Secret", "Secret {secret} stored from {surface}", "secret", "actor", "surface").
	Keywords("audit.success", "config").
	Doc("A secret (an API key, a token, a password) was stored under an ID that had none. The record names the ID, never the value.",
		"The operator entered a key: the dashboard's Keys page, /configure, /model add-cloud, a skill's configure, or tenant setup.",
		"None when expected. Otherwise rotate the secret at its service and remove it (the Keys page, or /skill clear).")

var SecretChanged = Security.Event(4602, event.Info, "Secret", "Secret {secret} changed from {surface}", "secret", "actor", "surface").
	Keywords("audit.success", "config").
	Doc("A stored secret was replaced with a new value. The record names the ID, never the value.",
		"The operator re-entered a key, or a skill's configure or probe stored a fresh token.",
		"None when expected. Otherwise rotate the secret at its service.")

var SecretDeleted = Security.Event(4603, event.Info, "Secret", "Secret {secret} deleted from {surface}", "secret", "actor", "surface").
	Keywords("audit.success", "config").
	Doc("A stored secret was removed. The record names the ID, never the value.",
		"The operator removed a key or a model, cleared a skill field, or forgot a connected service (tenant privacy forget --connector).",
		"None when expected. The integration that used it stops working until a new one is stored.")

var ScheduledJobAdded = Security.Event(4701, event.Info, "Scheduled job", "Scheduled job {job} added by the {actor} from {surface}", "job", "id", "spec", "kind", "exec", "actor", "surface").
	Keywords("audit.success", "config").
	Doc("A cron job was created. A job runs unattended: a prompt job as a turn of the agent, a shell job as a command.",
		"The operator added it (/cron add, the dashboard), or the agent did with cron_add, which asks nobody (audit item D4).",
		"None when expected. Otherwise remove it (/cron remove, the dashboard), and read the turn that added it (its trace).")

var ScheduledJobChanged = Security.Event(4702, event.Info, "Scheduled job", "Scheduled job {job} {state} by the {actor} from {surface}", "job", "id", "state", "actor", "surface").
	Keywords("audit.success", "config").
	Doc("A cron job was enabled or disabled.",
		"The operator changed it (/cron, the dashboard), or the agent did with cron_set_enabled.",
		"None when expected.")

var ScheduledJobRemoved = Security.Event(4703, event.Info, "Scheduled job", "Scheduled job {id} removed by the {actor} from {surface}", "id", "actor", "surface").
	Keywords("audit.success", "config").
	Doc("A cron job was deleted. The cron log's job events name it.",
		"The operator removed it (/cron remove, the dashboard), or the agent did with cron_remove.",
		"None when expected.")

var CronExecSwitched = Security.Event(4704, event.Info, "Scheduled job", "cron-exec turned {state} from {surface}", "state", "actor", "surface").
	Keywords("audit.success", "config").
	Doc("The global cron-exec switch changed. While it is on, shell jobs and exec prompt jobs run commands unattended.",
		"The operator flipped it: /cron exec, or the dashboard.",
		"None when expected. Turn it off if nobody meant to.")

var DataDeleted = Security.Event(4801, event.Info, "Data lifecycle", "Privacy deletion ({reason}) by the {actor}: {removed}", "reason", "removed", "actor", "surface", "err").
	Keywords("audit.success").
	Doc("Personal data was deleted for good. removed counts what went, kind by kind; err is set when the deletion stopped part way, after what removed lists.",
		"The operator ran tenant privacy forget or prune, or the daily retention sweep aged a class out (actor tenant).",
		"None when expected. Deleted data can only come back from a backup made before it.")

var DataExported = Security.Event(4802, event.Info, "Data lifecycle", "Personal data exported to {dir}: {files} files", "dir", "files", "bytes", "actor", "surface").
	Keywords("audit.success").
	Doc("Everything Tenant remembers was written out, in plaintext, to a directory.",
		"The operator ran tenant privacy export.",
		"None when expected. The export is plaintext: keep it private, and delete it when done.")

var BackupWritten = Security.Event(4803, event.Info, "Data lifecycle", "Backup written to {path}", "path", "bytes", "encrypted", "actor", "surface").
	Keywords("audit.success").
	Doc("A backup of the configuration and data directories was written. An unencrypted one leaves the credentials out.",
		"The operator ran tenant backup create.",
		"None when expected. Keep the file private: it holds everything Tenant remembers.")

var BackupRestored = Security.Event(4804, event.Info, "Data lifecycle", "Backup {path} restored over this install", "path", "created", "actor", "surface").
	Keywords("audit.success").
	Doc("The configuration and data directories were replaced by a backup's. The previous ones are kept beside them.",
		"The operator ran tenant backup restore.",
		"None when expected. The Security log's re-anchor record, next to this one, says which head the restore replaced.")

var LogCleared = Security.Event(4901, event.Info, "Log book", "Log {log} cleared: {details}", "log", "details", "actor", "surface").
	Keywords("audit.success").
	Doc("Every event of a log was deleted.",
		"The operator cleared it: tenant logs clear, or the Logs page.",
		"None when expected. The events can only come back from a backup.")

var LogReconfigured = Security.Event(4902, event.Info, "Log book", "Log {log} reconfigured: {details}", "log", "details", "actor", "surface").
	Keywords("audit.success", "config").
	Doc("A log's retention, size limit, level or debug switch changed.",
		"The operator changed it: tenant logs config, or the log's Properties.",
		"None when expected.")

var LogExported = Security.Event(4903, event.Info, "Log book", "Log {log} exported: {details}", "log", "details", "actor", "surface").
	Keywords("audit.success").
	Doc("Events were exported from the log book.",
		"The operator exported them: tenant logs export, or the Logs page.",
		"None when expected. An export holds whatever the events held: keep it private.")

var FieldRevealed = Security.Event(4904, event.Info, "Log book", "Sensitive field revealed in {log}: {details}", "log", "details", "actor", "surface").
	Keywords("audit.success").
	Doc("A viewer showed the value of a field the log book masks as sensitive.",
		"The operator chose to reveal it on the Logs page.",
		"None when expected.")

var LogVerified = Security.Event(4905, event.Info, "Log book", "Log {log} verified: {details}", "log", "details", "actor", "surface").
	Keywords("audit.success").
	Doc("An integrity check of the log passed.",
		"The operator ran tenant logs verify, or the log book checked itself.",
		"None.")

var LogVerifyFailed = Security.Event(4906, event.Error, "Log book", "Log {log} failed verification: {details}", "log", "details", "actor", "surface").
	Keywords("audit.failure").
	Doc("An integrity check of the log failed: a record was changed, removed or forged, or the log was cut short.",
		"Something edited logbook.db or its archives, or restored an older copy outside tenant backup restore; or the disk corrupted it.",
		"Keep the database and its archives as they are, and read details: it names the first bad record.")

// AuditLogAdmin files the Security record of an administrative action on the
// log book: d is LogCleared, LogReconfigured, LogExported, FieldRevealed,
// LogVerified or LogVerifyFailed, and details says what was done ("412
// events", "max_age_days 30 → 7", "event 1234, field output"). Call it where
// the action is handled, after it succeeds, as tenant logs and the Logs page
// do.
func AuditLogAdmin(ctx context.Context, d *event.Def, log, details, actor, surface string) error {
	return d.Audit(ctx, "log", log, "details", details, "actor", actor, "surface", surface)
}
