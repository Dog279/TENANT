package main

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"tenant/internal/cmdreg"
	"tenant/internal/model"
	sqlp "tenant/internal/plugins/sql"
)

// sqlPlugin declares the SQL plugin: the agent queries a SQLite database.
// Configured by path (--sql-db, or `tenant setup`).
func sqlPlugin() *pluginDescriptor {
	return &pluginDescriptor{
		ID: "sql",
		External: func(settings map[string]string) map[string]string {
			return map[string]string{settings["db"]: "SQL plugin database"}
		},
		Flags:       bindSQLFlags,
		On:          func(pf *pluginFlags) bool { return sqlFlagsOf(pf).db != "" },
		ApplyConfig: applySQLConfig,
		ExpandPaths: expandSQLPaths,
		Launch:      launchSQL,
		Prepare:     prepareSQL,
		Eval:        evalSQL,
		Title:       "SQL (SQLite database)",
		Version:     1,
		Tools:       func() []model.ToolSpec { return sqlp.NewDispatcher(nil, sqlp.Policy{}).Tools() },
		Configure: func() skillKind {
			return skillKind{ID: "sql", Label: "SQL (SQLite database)", Wired: true, Fields: []skillKindField{
				{Key: "db", Prompt: "SQLite database file path"},
			}}
		},
		SetupOnly:  true,
		SetupOrder: 2,
		Actions:    map[string]string{"sql_ddl": catDestructive},
		StubHint:   "relaunch with --sql-db <file>",
		CLI: []*cliCommand{
			{Names: []string{"sql"}, Section: "plugins", Run: cmdSQL,
				Summary: "the agent queries a SQLite database",
				Rows:    []cmdreg.Row{{Syntax: `sql "<q>" --db FILE`, Desc: "the agent queries a SQLite database (read; --allow-write)"}}},
		},
	}
}

// sqlFlags are the SQL plugin's launch flags.
type sqlFlags struct {
	db         string
	allowWrite bool
}

func sqlFlagsOf(pf *pluginFlags) *sqlFlags { return flagsOf[sqlFlags](pf, "sql") }

func bindSQLFlags(fs *flag.FlagSet) any {
	f := &sqlFlags{}
	fs.StringVar(&f.db, "sql-db", "", "enable the sql plugin over this SQLite file")
	fs.BoolVar(&f.allowWrite, "sql-allow-write", false, "permit SQL INSERT/UPDATE/DELETE")
	return f
}

func applySQLConfig(pf *pluginFlags, s savedConfig) {
	if f := sqlFlagsOf(pf); !s.set["sql-db"] {
		f.db = firstNonEmpty(f.db, s.get("db"))
	}
}

func expandSQLPaths(pf *pluginFlags) { f := sqlFlagsOf(pf); f.db = expandPath(f.db) }

// launchSQL opens the database named by --sql-db. Writes follow the
// --sql-allow-write flag alone (DDL always asks).
func launchSQL(_ context.Context, e *pluginEnv) ([]plugin, func(), error) {
	f := sqlFlagsOf(e.pf)
	if f.db == "" {
		return nil, nil, nil
	}
	db, err := sqlp.Open(sqlp.Config{Driver: "sqlite", DSN: f.db})
	if err != nil {
		return nil, nil, fmt.Errorf("sql plugin: %w", err)
	}
	return []plugin{sqlp.NewDispatcher(db, sqlp.Policy{AllowWrite: f.allowWrite, Confirm: e.confirm})}, func() { _ = db.Close() }, nil
}

func prepareSQL(pf *pluginFlags, ensureDir func(label, dir string), _ *[]string) {
	if f := sqlFlagsOf(pf); f.db != "" {
		ensureDir("sql dir", filepath.Dir(f.db))
	}
}

// evalSQL auto-detects the operator's real database, if they have one.
func evalSQL(pf *pluginFlags, c *commonFlags, explicit map[string]bool) bool {
	f := sqlFlagsOf(pf)
	if explicit["sql-db"] || f.db != "" || c.dataDir == "" {
		return false
	}
	candidate := findFirstExisting(
		filepath.Join(c.dataDir, "tenant.db"),
		filepath.Join(c.dataDir, "eval.db"),
	)
	if candidate == "" {
		return false
	}
	f.db = candidate
	f.allowWrite = true
	return true
}
