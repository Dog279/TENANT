package main

import (
	"tenant/internal/cmdreg"
	"tenant/internal/model"
	sqlp "tenant/internal/plugins/sql"
)

// sqlPlugin declares the SQL plugin: the agent queries a SQLite database.
// Configured by path (--sql-db, or `tenant setup`).
func sqlPlugin() *pluginDescriptor {
	return &pluginDescriptor{
		ID:      "sql",
		Title:   "SQL (SQLite database)",
		Version: 1,
		Tools:   func() []model.ToolSpec { return sqlp.NewDispatcher(nil, sqlp.Policy{}).Tools() },
		Configure: func() skillKind {
			return skillKind{ID: "sql", Label: "SQL (SQLite database)", Wired: true, Fields: []skillKindField{
				{Key: "db", Prompt: "SQLite database file path"},
			}}
		},
		SetupOnly: true,
		Setup:     2,
		Actions:   map[string]string{"sql_ddl": catDestructive},
		StubHint:  "relaunch with --sql-db <file>",
		CLI: []*cliCommand{
			{Names: []string{"sql"}, Section: "plugins", Run: cmdSQL,
				Summary: "the agent queries a SQLite database",
				Rows:    []cmdreg.Row{{Syntax: `sql "<q>" --db FILE`, Desc: "the agent queries a SQLite database (read; --allow-write)"}}},
		},
	}
}
