package sql

import (
	"tenant/internal/logs/event"
	"tenant/internal/pluginreg"
)

// Events is the SQL plugin's event catalog (TEN-393), declared by its
// descriptor.
var Events = pluginreg.NewEvents("sql", "SQL")

var DatabaseFailed = Events.Provider.Event(3101, event.Error, "Database", "SQL database schema unreadable: {err}", "err").
	Doc("The database's schema could not be read, so the agent can't see its tables.",
		"The file was moved, deleted, locked by another program, or is corrupt.",
		"Check the database file named by --sql-db or /configure sql; `sqlite3 <file> 'PRAGMA integrity_check'` tests it.")
