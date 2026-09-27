package catalog

import "tenant/internal/logs/event"

// logbook: the log book's own events (docs/design/logging-v2.md §12.4).
// 1101–1199 are the v1 history import's (TEN-372).

var LogbookOpened = Logbook.Event(1, event.Info, "Lifecycle", "The log book opened: logbook.db holds {bytes} bytes, schema version {schema}, store {store}", "bytes", "schema", "store").
	Doc("This process opened logbook.db, the system of record.",
		"Serve or the TUI started.",
		"None.")

var MirrorOnly = Logbook.Event(2, event.Critical, "Lifecycle", "logbook.db can't be written; the log book runs on its text files alone: {err}", "err", "since").
	Doc("Events are written to the text files only. Nothing is lost: the text files are written before the store.",
		"logbook.db did not open, or three commits in a row failed: a full disk, a lock held past its timeout, an I/O error.",
		"Fix the cause the error names. The process keeps retrying; once a commit succeeds, the text files' lines from the outage are imported.")

var MirrorOnlyEnded = Logbook.Event(3, event.Info, "Lifecycle", "logbook.db is written again after {outage} on the text files alone", "outage", "from", "to").
	Doc("The store accepts commits again.",
		"Whatever failed the commits cleared.",
		"None; the outage's lines are imported next.")

var Quarantined = Logbook.Event(4, event.Critical, "Lifecycle", "logbook.db was corrupt; it was moved aside to {file} and a new store {store} started", "file", "store", "err").
	Doc("The database was damaged, so it and its -wal and -shm files were moved aside and an empty store started. Cursors held by viewers restart.",
		"A crash of the machine or the disk, a bad sector, or the file was changed by another program.",
		"Run tenant logs recover to copy what can still be read into the new store.")

var CorruptInUse = Logbook.Event(5, event.Critical, "Lifecycle", "logbook.db is corrupt, and other Tenant processes have it open; this process runs on the text files alone: {err}", "err").
	Doc("The database is damaged, but it can only be moved aside while no other process has it open.",
		"Moving a database other processes have open fails on Windows and corrupts it on POSIX.",
		"Stop every Tenant process, then start one again: it moves the file aside. Or run tenant logs recover.")

var RecoveryImport = Logbook.Event(1001, event.Info, "Recovery", "Imported {count} lines the text files took while logbook.db was down", "count", "from", "to").
	Keywords("recovered").
	Doc("The events written only to the text files during an outage are in the store now, keyword recovered.",
		"logbook.db accepted commits again after running on the text files alone.",
		"None.")

var IntegrityOK = Logbook.Event(1002, event.Info, "Integrity", "logbook.db passed its integrity check in {took}", "took").
	Doc("The daily quick_check found no damage.",
		"The housekeeper checks the database once a day.",
		"None.")

var Archived = Logbook.Event(1003, event.Info, "Retention", "The oldest records were archived to {file}", "file").
	Doc("A log past its size or age limit had its oldest records moved to an archive file before they were deleted.",
		"The log's policy is archive (the Security log's always is).",
		"None; the archive is gzip, one JSON object per line.")

var Overwritten = Logbook.Event(1004, event.Info, "Retention", "Log {log} was full; its oldest {count} events were deleted", "log", "count").
	Doc("A log reached its size limit and its oldest events were deleted to make room.",
		"The log's policy is overwrite.",
		"Raise the log's max_mb, or set on_full to archive, if these events matter.")

var WALCheckpoint = Logbook.Event(1005, event.Info, "Retention", "The WAL passed its checkpoint threshold at {wal_mb} MB; checkpointing it", "wal_mb").
	Doc("logbook.db's write-ahead log grew past 64 MB and is copied into the database.",
		"Heavy writing, or a reader kept a checkpoint from finishing.",
		"None.")

var DiskRecovered = Logbook.Event(1006, event.Info, "Disk", "Disk space recovered ({free_mb} MB free); every event is written again", "free_mb").
	Doc("The data volume has room again, so no events are shed.",
		"Space was freed.",
		"None.")

var LeaseTaken = Logbook.Event(1007, event.Info, "Housekeeping", "This process now does the log book's housekeeping", "boot").
	Doc("This process holds the housekeeper lease: retention, checkpoints, imports and the dead-process check run here.",
		"It started first, or the previous holder stopped or died.",
		"None.")

var LeaseLost = Logbook.Event(1008, event.Info, "Housekeeping", "This process no longer does the log book's housekeeping", "boot", "err").
	Doc("Another process holds the housekeeper lease now.",
		"This process could not renew it in time, or the store failed.",
		"None, unless err names a failure.")

var QueueHigh = Logbook.Event(2001, event.Warning, "Write path", "The log book's queue reached {depth} of {cap} events in the last {window}", "depth", "cap", "window").
	Doc("The writer fell behind; events wait in memory before they are committed.",
		"A burst of events, or slow commits (a busy disk, another process holding the write lock).",
		"If it recurs, find the chatty provider in the logbook log's suppressed and dropped reports.")

var Suppressed = Logbook.Event(2002, event.Warning, "Write path", "Suppressed {count} × {provider} {event} in the last {window}", "count", "provider", "event", "log", "window").
	Doc("A provider wrote one event faster than its rate limit, so the extra ones were not kept.",
		"Each provider and event ID may write 100 a second, in bursts of 500 (logs.rate_limit); an uncatalogued line counts by its text with the numbers and quoted values taken out.",
		"Find why the provider repeats the event; raise logs.rate_limit only if every copy matters.")

var Dropped = Logbook.Event(2003, event.Warning, "Write path", "Dropped {count} events of {provider} in the last {window}", "count", "provider", "log", "window").
	Doc("Events were not written.",
		"The queue was full (64k events), or the disk is low and less important events are shed.",
		"Free disk space, or find what floods the log book.")

var NearFull = Logbook.Event(2004, event.Warning, "Retention", "Log {log} is at {percent}% of its size limit", "log", "percent", "limit_mb").
	Doc("A log is close to its max_mb; at the limit its policy overwrites, archives or keeps its oldest records.",
		"The log is busy, or its limit is small.",
		"None if its policy is what you want; otherwise raise its max_mb.")

var KeptFull = Logbook.Event(2005, event.Warning, "Retention", "Log {log} is past its size limit; its policy keeps every record, so nothing is deleted", "log").
	Doc("A log whose policy is keep grew past its max_mb.",
		"Its policy is keep, so retention never deletes from it.",
		"Raise its max_mb, clear it, or change its policy.")

var LowDisk = Logbook.Event(2006, event.Warning, "Disk", "Low disk space ({free_mb} MB free); diagnostic events are not written until space is freed", "free_mb").
	Doc("Diagnostic events are shed to save space.",
		"Under 1 GB, or 5%, is free on the data volume.",
		"Free disk space.")

var VeryLowDisk = Logbook.Event(2007, event.Warning, "Disk", "Very low disk space ({free_mb} MB free); only warnings, errors and the security log are written until space is freed", "free_mb").
	Doc("Informational and diagnostic events are shed.",
		"Under 256 MB is free on the data volume.",
		"Free disk space now.")

var NoBootRecord = Logbook.Event(2008, event.Warning, "Boot records", "No boot record for this process; an unexpected exit will go unreported: {err}", "err").
	Doc("This process could not start its boot record, so a crash or kill would not be reported later.",
		"The boots folder or logbook.db could not be written.",
		"Check the log root's permissions and free space.")

var CommitFailed = Logbook.Event(3001, event.Error, "Write path", "logbook.db could not commit {count} events (attempt {attempt}): {err}", "count", "attempt", "err").
	Doc("A batch failed to commit; the text files have it. After three failures in a row the log book runs on the text files alone.",
		"A full disk, a lock held past its timeout, or an I/O error.",
		"Fix the cause the error names.")

var MirrorFailed = Logbook.Event(3002, event.Error, "Write path", "The text files could not be written: {err}", "err").
	Doc("Lines meant for the text files were lost; logbook.db still has the events.",
		"A full disk, a permission change, or the log root is gone.",
		"Fix the cause the error names.")

var OverBudget = Logbook.Event(3003, event.Critical, "Retention", "The log book is over its disk budget, and only records retention may not delete are left", "size_mb", "budget_mb").
	Doc("logbook.db and its WAL are past logs.budget_mb, and what is left is the Security log, archived logs and kept logs.",
		"Those logs are never shed to meet the budget.",
		"Raise budget_mb, or clear or re-policy a kept log.")

var IntegrityFailed = Logbook.Event(3004, event.Critical, "Integrity", "logbook.db failed its integrity check: {err}", "err").
	Doc("The database is damaged.",
		"A crash of the machine or the disk, or a bad sector.",
		"Stop every Tenant process and run tenant logs recover.")

var ChoreFailed = Logbook.Event(3005, event.Error, "Housekeeping", "Housekeeping task {task} failed: {err}", "task", "err").
	Doc("One of the log book's housekeeping tasks failed; the next run tries again.",
		"The store failed, or the task's own work did.",
		"Read the error; repeated failures need the cause fixed.")

var RecoveryFailed = Logbook.Event(3006, event.Error, "Recovery", "The text files' lines from the outage were not imported: {err}", "err", "from", "to").
	Doc("After an outage, the lines written only to the text files could not be imported into logbook.db.",
		"The import failed; the text files still have them.",
		"Read the error; the lines stay in the text files.")

// system: the process as the log book sees it.

var Crashed = System.Event(4, event.Critical, "Lifecycle", "The previous run crashed: {panic}", "panic", "last", "boot", "pid", "role", "file", "stack").
	Doc("The previous run of Tenant crashed, and the runtime's dump of every goroutine was read in.",
		"An unrecovered panic or a fatal runtime error.",
		"Read stack, or the whole dump in the file field; report it with the version.")

var ClockJump = System.Event(2001, event.Warning, "Clock", "The wall clock moved {delta} against the monotonic clock", "delta").
	Doc("The system time changed by more than 2 seconds relative to elapsed time. Events keep their order (by seq), but their times jump.",
		"The clock was set or synced, or the machine slept.",
		"None, unless it recurs without a sleep: then check time sync.")
