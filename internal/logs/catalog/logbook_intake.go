package catalog

import "tenant/internal/logs/event"

// logbook: intake (1101–1199), what the housekeeper files that no Tenant
// process wrote through its pipeline (docs/design/logging-v2.md §6.4).

var HistoryImported = Logbook.Event(1101, event.Info, "Intake", "Imported {lines} lines of v1 history from {files} files", "lines", "files").
	Doc("The log book filed the text files Tenant wrote before it kept logbook.db, once; the lines are tagged imported.",
		"The first run with the store found v1 log files in the logs folders.",
		"None. The text files are left as they were.")

var SpoolRefused = Logbook.Event(1102, event.Warning, "Intake", "{lines} lines of spool file {file} were not filed: {err}", "file", "lines", "err").
	Doc("Lines of a spool file were dropped: they were not an event, or they named one of Tenant's own logs or providers.",
		"A writer outside Tenant spooled a malformed line, or one claiming a log only Tenant writes (security, tenant.*, a built-in).",
		"Fix the writer: one JSON event per line, into a log of its own.")
