package sql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store wraps a database/sql handle. v1 ships SQLite (modernc, pure
// Go — already a Tenant dep). The driver is selected by Config.Driver
// so Postgres (jackc/pgx) slots in next without touching the
// Dispatcher; that's deliberately deferred (no live PG to verify
// against here — same echo-then-vllm staging used elsewhere).
type Store struct {
	db     *sql.DB
	driver string
}

// Config opens a Store. Driver: "sqlite" (v1). DSN: a file path for
// SQLite (":memory:" for tests).
type Config struct {
	Driver string // "sqlite"
	DSN    string
}

// Open validates + opens the database. SQLite gets a busy timeout so
// concurrent agent turns don't error on a transient lock.
func Open(cfg Config) (*Store, error) {
	if cfg.Driver == "" {
		cfg.Driver = "sqlite"
	}
	if cfg.Driver != "sqlite" {
		return nil, fmt.Errorf("sql: driver %q not supported in v1 (sqlite only; postgres next)", cfg.Driver)
	}
	if cfg.DSN == "" {
		return nil, errors.New("sql: DSN required (a sqlite file path or :memory:)")
	}
	dsn := cfg.DSN
	if dsn != ":memory:" {
		dsn = fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", cfg.DSN)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sql: open: %w", err)
	}
	if dsn == ":memory:" || strings.Contains(dsn, ":memory:") {
		db.SetMaxOpenConns(1) // shared in-memory DB across queries
	}
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sql: ping: %w", err)
	}
	return &Store{db: db, driver: cfg.Driver}, nil
}

// Close closes the DB. Idempotent.
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// Column + Table describe schema for the model. Without this the
// model writes blind SQL; sql_schema is the single most important
// tool for query quality.
type Column struct {
	Name string `json:"name"`
	Type string `json:"type"`
	PK   bool   `json:"pk,omitempty"`
}
type Table struct {
	Name    string   `json:"name"`
	Columns []Column `json:"columns"`
}

// Schema introspects tables + columns. Uses parameterized internal
// queries / PRAGMA on names read from sqlite_master — the model never
// supplies SQL here, so this path is injection-free by construction.
func (s *Store) Schema(ctx context.Context) ([]Table, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("sql: list tables: %w", err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []Table
	for _, name := range names {
		// PRAGMA table_info can't be parameterized; name comes from
		// sqlite_master (not the model) and is quoted defensively.
		q := fmt.Sprintf("PRAGMA table_info(%s)", quoteIdent(name))
		crows, err := s.db.QueryContext(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("sql: columns of %s: %w", name, err)
		}
		t := Table{Name: name}
		for crows.Next() {
			var cid int
			var cname, ctype string
			var notnull, pk int
			var dflt any
			if err := crows.Scan(&cid, &cname, &ctype, &notnull, &dflt, &pk); err != nil {
				crows.Close()
				return nil, err
			}
			t.Columns = append(t.Columns, Column{Name: cname, Type: ctype, PK: pk > 0})
		}
		crows.Close()
		if err := crows.Err(); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// QueryResult is a capped result set. Truncated=true means the model
// must narrow its query (add WHERE/LIMIT) — surfaced in the tool text
// so it adjusts instead of assuming it saw everything.
type QueryResult struct {
	Columns   []string
	Rows      [][]string
	Truncated bool
	Elapsed   time.Duration
}

// Query runs a SELECT-class statement. Caller MUST have classified it
// Read; Query additionally only ever calls db.Query (a SELECT cannot
// mutate even if classification were somehow fooled — defense in
// depth). Bounded by ctx timeout + maxRows.
func (s *Store) Query(ctx context.Context, statement string, maxRows int) (*QueryResult, error) {
	if maxRows <= 0 {
		maxRows = 200
	}
	start := time.Now()
	rows, err := s.db.QueryContext(ctx, statement)
	if err != nil {
		return nil, fmt.Errorf("sql: query: %w", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	res := &QueryResult{Columns: cols}
	for rows.Next() {
		if len(res.Rows) >= maxRows {
			res.Truncated = true
			break
		}
		raw := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		out := make([]string, len(cols))
		for i, v := range raw {
			out[i] = renderCell(v)
		}
		res.Rows = append(res.Rows, out)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	res.Elapsed = time.Since(start)
	return res, nil
}

// Exec runs a write/DDL statement. Caller MUST have gated it. Returns
// rows affected.
func (s *Store) Exec(ctx context.Context, statement string) (int64, error) {
	r, err := s.db.ExecContext(ctx, statement)
	if err != nil {
		return 0, fmt.Errorf("sql: exec: %w", err)
	}
	n, _ := r.RowsAffected()
	return n, nil
}

func renderCell(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return string(x)
	case string:
		return x
	case time.Time:
		return x.Format(time.RFC3339)
	default:
		return fmt.Sprintf("%v", x)
	}
}

// quoteIdent double-quotes an identifier and escapes embedded quotes —
// only used for schema names read from sqlite_master, never model input.
func quoteIdent(id string) string {
	return `"` + strings.ReplaceAll(id, `"`, `""`) + `"`
}
