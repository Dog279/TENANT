package privacy

import "tenant/internal/logs/event"

// Privacy is the privacy provider (docs/EVENTS.md): each retention sweep and
// each deletion is an activity, and a deletion counts what it removed, kind
// by kind. The Security log's records of deletions are separate (TEN-398).
var Privacy = event.NewProvider("privacy", "privacy")

var Sweep = Privacy.Event(1001, event.Info, "Retention", "Retention sweep", "classes").
	Doc("The daily retention sweep of a running Tenant: its start, then its stop with the duration, or its failure with the error. classes counts the data classes that have a retention period; a sweep with none removes nothing.",
		"Serve and the TUI sweep a few minutes after they start, then daily.",
		"None when it stops. On a failure, read the error: usually config.json could not be read, and nothing was removed.")

var Deletion = Privacy.Event(1002, event.Info, "Deletion", "Privacy deletion ({reason})", "reason").
	Doc("Data was deleted for good: its start, the counts removed, then its stop with the duration, or its failure with the error. reason is forget (the operator asked) or retention (a class aged out).",
		"tenant privacy forget or prune, or the retention sweep.",
		"None when it stops. After a failure, the stores done before it stay deleted and the rest are unchanged; run the command again.")

var Removed = Privacy.Event(1003, event.Info, "Deletion", "{action} {count} {data}", "data", "action", "count", "reason").
	Doc("One kind of data a deletion changed: rows or files deleted, archive files rewritten, or records unlinked from what was removed.",
		"A privacy deletion ran.",
		"None.")

var Unfinished = Privacy.Event(2001, event.Warning, "Deletion", "A privacy deletion did not fully finish: {warning}", "warning", "reason").
	Doc("The data was deleted, but a clean-up step after it did not finish, so copies may linger until later.",
		"A running Tenant was reading the store, so its WAL still holds old pages, or its search index could not be compacted.",
		"None usually: the store's next checkpoint clears its WAL. A deletion run while Tenant is stopped leaves no copies.")
