package sql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"tenant/internal/model"
)

// Policy gates SQL by statement class — same shape as the web
// plugin's gate (kept local; the class sets differ enough that a
// shared abstraction would be premature for two consumers).
//
// Defaults are SAFE: read always; write only if AllowWrite; DDL/
// destructive only if Confirm explicitly approves (nil ⇒ deny all).
// The model cannot change the policy — the operator sets it.
type Policy struct {
	AllowWrite bool
	// Confirm is consulted for every ClassDDL statement. nil ⇒ deny
	// all DDL (the safe default). Receives a human-readable
	// description of exactly what will run.
	Confirm func(ctx context.Context, action, detail string) bool
}

func (p Policy) gate(ctx context.Context, c Class, stmt string) error {
	switch c {
	case ClassRead:
		return nil
	case ClassWrite:
		if p.AllowWrite {
			return nil
		}
		return fmt.Errorf("blocked: write statements (INSERT/UPDATE/DELETE) are disabled by policy " +
			"(enable with the write flag) — this is a data-safety boundary")
	default: // ClassDDL
		if p.Confirm != nil && p.Confirm(ctx, "sql_ddl", clip(stmt, 200)) {
			return nil
		}
		return fmt.Errorf("blocked: DDL/destructive SQL (DROP/TRUNCATE/ALTER/CREATE/PRAGMA/…) " +
			"requires explicit approval and was not confirmed — this is a data-safety boundary, not a bug")
	}
}

// Dispatcher implements agent.ToolDispatcher for SQL.
type Dispatcher struct {
	st      *Store
	policy  Policy
	maxRows int
}

const defaultMaxRows = 200

// NewDispatcher wires a dispatcher over a Store + policy.
func NewDispatcher(st *Store, policy Policy) *Dispatcher {
	return &Dispatcher{st: st, policy: policy, maxRows: defaultMaxRows}
}

// Tools registers sql_schema / sql_query / sql_exec. sql_schema is the
// single most important one — the model writes blind SQL without it.
func (d *Dispatcher) Tools() []model.ToolSpec {
	obj := func(props string, req ...string) json.RawMessage {
		r := ""
		for i, x := range req {
			if i > 0 {
				r += ","
			}
			r += `"` + x + `"`
		}
		return json.RawMessage(`{"type":"object","properties":{` + props + `},"required":[` + r + `]}`)
	}
	return []model.ToolSpec{
		{Name: "sql_schema", Description: "List the database tables and their columns. Call this FIRST so you know what to query.",
			Parameters: obj(``)},
		{Name: "sql_query", Description: "Run a read-only SELECT (or WITH…SELECT / EXPLAIN). Returns rows (capped). Reject for any non-read statement.",
			Parameters: obj(`"sql":{"type":"string","description":"a single SELECT statement"}`, "sql")},
		{Name: "sql_exec", Description: "Run a write (INSERT/UPDATE/DELETE). Gated: writes require approval; DROP/ALTER/etc. require explicit confirmation.",
			Parameters: obj(`"sql":{"type":"string","description":"a single write statement"}`, "sql"), Gated: true},
	}
}

func (d *Dispatcher) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	switch call.Name {
	case "sql_schema":
		return d.schema(ctx)
	case "sql_query":
		return d.query(ctx, call.Arguments)
	case "sql_exec":
		return d.exec(ctx, call.Arguments)
	default:
		return "unknown sql tool: " + call.Name, true, nil
	}
}

func (d *Dispatcher) schema(ctx context.Context) (string, bool, error) {
	tables, err := d.st.Schema(ctx)
	if err != nil {
		return "schema introspection failed: " + err.Error(), true, nil
	}
	if len(tables) == 0 {
		return "(database has no user tables)", false, nil
	}
	var b strings.Builder
	for _, t := range tables {
		fmt.Fprintf(&b, "TABLE %s\n", t.Name)
		for _, c := range t.Columns {
			pk := ""
			if c.PK {
				pk = " PRIMARY KEY"
			}
			fmt.Fprintf(&b, "  %s %s%s\n", c.Name, c.Type, pk)
		}
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

func (d *Dispatcher) query(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		SQL string `json:"sql"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "invalid arguments: " + err.Error(), true, nil
	}
	if strings.TrimSpace(a.SQL) == "" {
		return "sql is required", true, nil
	}
	class, kw, reason := Classify(a.SQL)
	if reason != "" {
		return "rejected: " + reason, true, nil
	}
	// sql_query is READ ONLY. Anything else is refused here even
	// before the policy gate — and Query() only ever calls db.Query,
	// so a non-SELECT physically can't mutate via this path anyway.
	if class != ClassRead {
		return fmt.Sprintf("rejected: sql_query only runs read statements; %q is %s — use sql_exec (gated)",
			kw, class), true, nil
	}
	if err := d.policy.gate(ctx, class, a.SQL); err != nil {
		return err.Error(), true, nil
	}
	res, err := d.st.Query(ctx, a.SQL, d.maxRows)
	if err != nil {
		return "query error: " + err.Error(), true, nil
	}
	return renderResult(res), false, nil
}

func (d *Dispatcher) exec(ctx context.Context, args json.RawMessage) (string, bool, error) {
	var a struct {
		SQL string `json:"sql"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "invalid arguments: " + err.Error(), true, nil
	}
	if strings.TrimSpace(a.SQL) == "" {
		return "sql is required", true, nil
	}
	class, kw, reason := Classify(a.SQL)
	if reason != "" {
		return "rejected: " + reason, true, nil
	}
	if class == ClassRead {
		return fmt.Sprintf("rejected: %q is a read statement — use sql_query for reads", kw), true, nil
	}
	if err := d.policy.gate(ctx, class, a.SQL); err != nil {
		return err.Error(), true, nil
	}
	n, err := d.st.Exec(ctx, a.SQL)
	if err != nil {
		return "exec error: " + err.Error(), true, nil
	}
	return fmt.Sprintf("ok: %s affected %d row(s)", kw, n), false, nil
}

func renderResult(r *QueryResult) string {
	if len(r.Rows) == 0 {
		return fmt.Sprintf("(0 rows; columns: %s) [%s]", strings.Join(r.Columns, ", "), r.Elapsed.Round(1e6))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", strings.Join(r.Columns, " | "))
	const maxBytes = 16000
	for _, row := range r.Rows {
		line := strings.Join(row, " | ")
		if b.Len()+len(line) > maxBytes {
			fmt.Fprintf(&b, "…[result truncated at %d bytes — narrow the query]\n", maxBytes)
			r.Truncated = true
			break
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	tail := fmt.Sprintf("(%d row(s)", len(r.Rows))
	if r.Truncated {
		tail += ", TRUNCATED — add WHERE/LIMIT to see the rest"
	}
	tail += fmt.Sprintf(", %s)", r.Elapsed.Round(1e6))
	return strings.TrimRight(b.String(), "\n") + "\n" + tail
}

func clip(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
