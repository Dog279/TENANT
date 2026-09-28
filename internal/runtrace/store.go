package runtrace

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `CREATE TABLE IF NOT EXISTS runs (
 trace_id TEXT PRIMARY KEY, agent_id TEXT, session_id TEXT, source TEXT NOT NULL,
 started_at TEXT NOT NULL, completed_at TEXT, model TEXT, provider TEXT,
 latency_ms INTEGER NOT NULL DEFAULT 0, prompt_tokens INTEGER NOT NULL DEFAULT 0,
 completion_tokens INTEGER NOT NULL DEFAULT 0, cost_estimate_usd REAL,
 retries INTEGER NOT NULL DEFAULT 0, fallbacks TEXT NOT NULL DEFAULT '[]',
 tool_calls TEXT NOT NULL DEFAULT '[]', approvals TEXT NOT NULL DEFAULT '[]',
 outcome TEXT NOT NULL DEFAULT 'running', error_class TEXT
);
CREATE INDEX IF NOT EXISTS idx_runs_started_at ON runs(started_at);
CREATE INDEX IF NOT EXISTS idx_runs_completed_at ON runs(completed_at);`

type Config struct {
	MaxAge       time.Duration
	MaxCount     int
	RedactBodies bool
}

type Store struct {
	db   *sql.DB
	cfg  Config
	stop chan struct{} // closed by Close; ends the prune timer
	wg   sync.WaitGroup
}

// pruneEvery is how often an open store re-applies its bounds: serve and the
// TUI stay up for months, so pruning only at Open would never happen.
var pruneEvery = time.Hour

func Open(path string, cfg Config) (*Store, error) {
	dsn := path
	if path != ":memory:" {
		// _txlock=immediate: a transaction takes the write lock when it begins,
		// so a second process waits on busy_timeout instead of failing to upgrade.
		dsn = fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_txlock=immediate", path)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("runtrace: open: %w", err)
	}
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
	}
	if _, err := db.ExecContext(context.Background(), schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("runtrace: schema: %w", err)
	}
	if err := addColumn(db, "parent_trace_id", "TEXT"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("runtrace: migrate parent_trace_id: %w", err)
	}
	s := &Store{db: db, cfg: cfg, stop: make(chan struct{})}
	if _, err := s.Prune(context.Background(), time.Now().UTC()); err != nil {
		_ = db.Close()
		return nil, err
	}
	s.wg.Add(1)
	go s.pruneOnTimer(pruneEvery)
	return s, nil
}

// addColumn adds a column an older runs.db lacks: CREATE TABLE IF NOT EXISTS
// never alters an existing table.
func addColumn(db *sql.DB, column, definition string) error {
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('runs') WHERE name = ?`, column).Scan(&n); err != nil || n > 0 {
		return err
	}
	_, err := db.Exec(`ALTER TABLE runs ADD COLUMN ` + column + ` ` + definition)
	return err
}

// pruneOnTimer re-applies the bounds every interval until Close. A failed
// prune (the database busy past its timeout) is simply retried next tick.
func (s *Store) pruneOnTimer(interval time.Duration) {
	defer s.wg.Done()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case now := <-t.C:
			_, _ = s.Prune(context.Background(), now.UTC())
		}
	}
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	close(s.stop)
	s.wg.Wait()
	err := s.db.Close()
	s.db = nil
	return err
}

func (s *Store) Start(ctx context.Context, r Run) error {
	if s == nil || s.db == nil {
		return nil
	}
	if r.TraceID == "" {
		return errors.New("runtrace: trace id required")
	}
	if r.StartedAt.IsZero() {
		r.StartedAt = time.Now().UTC()
	}
	if r.Source == "" {
		r.Source = "headless"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO runs
 (trace_id,parent_trace_id,agent_id,session_id,source,started_at,outcome) VALUES (?,?,?,?,?,?,?)`,
		r.TraceID, r.ParentTraceID, r.AgentID, r.SessionID, r.Source, r.StartedAt.UTC().Format(time.RFC3339Nano), "running")
	if err != nil {
		return fmt.Errorf("runtrace: start: %w", err)
	}
	return nil
}

func (s *Store) Finish(ctx context.Context, r Run) error {
	if s == nil || s.db == nil {
		return nil
	}
	if s.cfg.RedactBodies {
		r.ToolCalls = append([]ToolCall(nil), r.ToolCalls...)
		for i := range r.ToolCalls {
			r.ToolCalls[i].Arguments, r.ToolCalls[i].Result = "", ""
		}
	}
	fallbacks, err := json.Marshal(nonNilFallbacks(r.Fallbacks))
	if err != nil {
		return err
	}
	tools, err := json.Marshal(nonNilTools(r.ToolCalls))
	if err != nil {
		return err
	}
	approvals, err := json.Marshal(nonNilApprovals(r.Approvals))
	if err != nil {
		return err
	}
	var completed any
	if r.CompletedAt != nil {
		completed = r.CompletedAt.UTC().Format(time.RFC3339Nano)
	}
	var cost any
	if r.CostEstimateUSD != nil {
		cost = *r.CostEstimateUSD
	}
	res, err := s.db.ExecContext(ctx, `UPDATE runs SET completed_at=?,model=?,provider=?,latency_ms=?,
 prompt_tokens=?,completion_tokens=?,cost_estimate_usd=?,retries=?,fallbacks=?,tool_calls=?,approvals=?,outcome=?,error_class=?
 WHERE trace_id=?`, completed, r.Model, r.Provider, r.LatencyMS, r.PromptTokens, r.CompletionTokens,
		cost, r.Retries, string(fallbacks), string(tools), string(approvals), r.Outcome, r.ErrorClass, r.TraceID)
	if err != nil {
		return fmt.Errorf("runtrace: finish: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("runtrace: finish: trace %s not found", r.TraceID)
	}
	return nil
}

func (s *Store) Get(ctx context.Context, traceID string) (Run, error) {
	row := s.db.QueryRowContext(ctx, selectRuns+` WHERE trace_id=?`, traceID)
	r, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, fmt.Errorf("runtrace: trace %s not found", traceID)
	}
	return r, err
}

const selectRuns = `SELECT trace_id,parent_trace_id,agent_id,session_id,source,started_at,completed_at,model,provider,
 latency_ms,prompt_tokens,completion_tokens,cost_estimate_usd,retries,fallbacks,tool_calls,approvals,outcome,error_class FROM runs`

func (s *Store) List(ctx context.Context) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, selectRuns+` ORDER BY started_at, trace_id`)
	if err != nil {
		return nil, fmt.Errorf("runtrace: list: %w", err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type scanner interface{ Scan(...any) error }

func scanRun(row scanner) (Run, error) {
	var r Run
	var started string
	var parent, completed, model, provider, errClass sql.NullString
	var cost sql.NullFloat64
	var fallbacks, tools, approvals string
	err := row.Scan(&r.TraceID, &parent, &r.AgentID, &r.SessionID, &r.Source, &started, &completed, &model, &provider,
		&r.LatencyMS, &r.PromptTokens, &r.CompletionTokens, &cost, &r.Retries, &fallbacks, &tools, &approvals, &r.Outcome, &errClass)
	if err != nil {
		return Run{}, err
	}
	r.StartedAt, err = time.Parse(time.RFC3339Nano, started)
	if err != nil {
		return Run{}, fmt.Errorf("runtrace: started_at: %w", err)
	}
	if completed.Valid {
		t, e := time.Parse(time.RFC3339Nano, completed.String)
		if e != nil {
			return Run{}, e
		}
		r.CompletedAt = &t
	}
	r.ParentTraceID, r.Model, r.Provider, r.ErrorClass = parent.String, model.String, provider.String, errClass.String
	if cost.Valid {
		r.CostEstimateUSD = &cost.Float64
	}
	if err := json.Unmarshal([]byte(fallbacks), &r.Fallbacks); err != nil {
		return Run{}, fmt.Errorf("runtrace: fallbacks: %w", err)
	}
	if err := json.Unmarshal([]byte(tools), &r.ToolCalls); err != nil {
		return Run{}, fmt.Errorf("runtrace: tools: %w", err)
	}
	if err := json.Unmarshal([]byte(approvals), &r.Approvals); err != nil {
		return Run{}, fmt.Errorf("runtrace: approvals: %w", err)
	}
	return r, nil
}

// ExportJSONL emits one stable JSON object per run, oldest first.
// TODO(OPS-001): add an OTLP exporter when Tenant grows collector configuration.
func (s *Store) ExportJSONL(ctx context.Context, w io.Writer) error {
	runs, err := s.List(ctx)
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	for _, r := range runs {
		if err := enc.Encode(r); err != nil {
			return fmt.Errorf("runtrace: export: %w", err)
		}
	}
	return bw.Flush()
}

// Prune applies both configured bounds and returns the number removed.
func (s *Store) Prune(ctx context.Context, now time.Time) (int64, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	var removed int64
	if s.cfg.MaxAge > 0 {
		res, err := s.db.ExecContext(ctx, `DELETE FROM runs WHERE started_at < ?`, now.Add(-s.cfg.MaxAge).UTC().Format(time.RFC3339Nano))
		if err != nil {
			return removed, fmt.Errorf("runtrace: prune age: %w", err)
		}
		n, _ := res.RowsAffected()
		removed += n
	}
	if s.cfg.MaxCount > 0 {
		res, err := s.db.ExecContext(ctx, `DELETE FROM runs WHERE trace_id IN (
 SELECT trace_id FROM runs ORDER BY started_at DESC, trace_id DESC LIMIT -1 OFFSET ?)`, s.cfg.MaxCount)
		if err != nil {
			return removed, fmt.Errorf("runtrace: prune count: %w", err)
		}
		n, _ := res.RowsAffected()
		removed += n
	}
	return removed, nil
}

func nonNilFallbacks(v []Fallback) []Fallback {
	if v == nil {
		return []Fallback{}
	}
	return v
}
func nonNilTools(v []ToolCall) []ToolCall {
	if v == nil {
		return []ToolCall{}
	}
	return v
}
func nonNilApprovals(v []Approval) []Approval {
	if v == nil {
		return []Approval{}
	}
	return v
}
