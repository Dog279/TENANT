package sql_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/model"
	sqlp "tenant/internal/plugins/sql"
)

func mkStore(t *testing.T) *sqlp.Store {
	t.Helper()
	st, err := sqlp.Open(sqlp.Config{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "t.db")})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_, err = st.Exec(context.Background(),
		`CREATE TABLE products (id INTEGER PRIMARY KEY, name TEXT, price REAL)`)
	if err != nil {
		t.Fatalf("seed schema: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO products (name, price) VALUES ('Widget', 9.99)`,
		`INSERT INTO products (name, price) VALUES ('Gadget', 19.50)`,
		`INSERT INTO products (name, price) VALUES ('Gizmo', 4.25)`,
	} {
		if _, err := st.Exec(context.Background(), q); err != nil {
			t.Fatalf("seed rows: %v", err)
		}
	}
	return st
}

func call(name, sql string) model.ToolCall {
	b, _ := json.Marshal(map[string]string{"sql": sql})
	if sql == "" {
		b = []byte(`{}`)
	}
	return model.ToolCall{Name: name, Arguments: b}
}

// --- classifier ---

func TestClassify(t *testing.T) {
	cases := []struct {
		sql  string
		want sqlp.Class
	}{
		{"SELECT * FROM x", sqlp.ClassRead},
		{"  select 1", sqlp.ClassRead},
		{"WITH t AS (SELECT 1) SELECT * FROM t", sqlp.ClassRead},
		{"EXPLAIN SELECT 1", sqlp.ClassRead},
		{"INSERT INTO x VALUES (1)", sqlp.ClassWrite},
		{"update x set a=1", sqlp.ClassWrite},
		{"DELETE FROM x", sqlp.ClassWrite},
		{"WITH t AS (SELECT 1) DELETE FROM x WHERE id IN (SELECT 1)", sqlp.ClassWrite},
		{"DROP TABLE x", sqlp.ClassDDL},
		{"TRUNCATE x", sqlp.ClassDDL},
		{"ALTER TABLE x ADD c INT", sqlp.ClassDDL},
		{"CREATE TABLE x (a int)", sqlp.ClassDDL},
		{"PRAGMA table_info(x)", sqlp.ClassDDL},
		{"VACUUM", sqlp.ClassDDL},
		{"GIBBERISH foo", sqlp.ClassDDL}, // unknown verb → fail closed
	}
	for _, c := range cases {
		got, _, reason := sqlp.Classify(c.sql)
		if reason != "" {
			t.Errorf("%q unexpectedly rejected: %s", c.sql, reason)
			continue
		}
		if got != c.want {
			t.Errorf("Classify(%q) = %v, want %v", c.sql, got, c.want)
		}
	}
}

func TestClassify_InjectionDefenses(t *testing.T) {
	// multi-statement
	if _, _, r := sqlp.Classify("SELECT 1; DROP TABLE x"); r == "" {
		t.Error("multi-statement must be rejected")
	}
	// comment-smuggled DROP — comment stripped, leaves DROP → DDL (not Read)
	c, _, r := sqlp.Classify("SELECT 1 -- harmless\n; DROP TABLE x")
	if r == "" {
		t.Errorf("comment-smuggled multi-statement must be rejected (got class %v)", c)
	}
	// block comment hiding a second statement
	if _, _, r := sqlp.Classify("SELECT 1 /* */ ; DELETE FROM x"); r == "" {
		t.Error("block-comment multi-statement must be rejected")
	}
	// a string containing -- must NOT be treated as a comment
	cl, _, r := sqlp.Classify(`SELECT '--not a comment' AS s`)
	if r != "" || cl != sqlp.ClassRead {
		t.Errorf("string with -- mishandled: class=%v reason=%q", cl, r)
	}
	// trailing semicolon alone is fine
	if cl, _, r := sqlp.Classify("SELECT 1;"); r != "" || cl != sqlp.ClassRead {
		t.Errorf("trailing ; should be OK: class=%v reason=%q", cl, r)
	}
}

// --- gate ---

func TestGate_ReadAlways_WriteFlag_DDLConfirm(t *testing.T) {
	st := mkStore(t)
	ctx := context.Background()

	// read-only policy
	ro := sqlp.NewDispatcher(st, sqlp.Policy{})
	out, isErr, _ := ro.Dispatch(ctx, call("sql_query", "SELECT name FROM products ORDER BY price DESC"))
	if isErr {
		t.Fatalf("read must be allowed: %q", out)
	}
	if !strings.Contains(out, "Gadget") {
		t.Errorf("query result wrong: %q", out)
	}

	// write blocked without AllowWrite
	out, isErr, _ = ro.Dispatch(ctx, call("sql_exec", "DELETE FROM products WHERE id=1"))
	if !isErr || !strings.Contains(out, "disabled by policy") {
		t.Fatalf("write must be blocked read-only: isErr=%v %q", isErr, out)
	}
	// verify nothing deleted
	out, _, _ = ro.Dispatch(ctx, call("sql_query", "SELECT COUNT(*) FROM products"))
	if !strings.Contains(out, "3") {
		t.Fatalf("rows changed despite blocked write: %q", out)
	}

	// write allowed with flag
	rw := sqlp.NewDispatcher(st, sqlp.Policy{AllowWrite: true})
	out, isErr, _ = rw.Dispatch(ctx, call("sql_exec", "DELETE FROM products WHERE name='Gizmo'"))
	if isErr || !strings.Contains(out, "affected 1") {
		t.Fatalf("allowed write failed: isErr=%v %q", isErr, out)
	}

	// DDL blocked even WITH AllowWrite (needs Confirm)
	out, isErr, _ = rw.Dispatch(ctx, call("sql_exec", "DROP TABLE products"))
	if !isErr || !strings.Contains(out, "not confirmed") {
		t.Fatalf("DDL must need confirm even with AllowWrite: isErr=%v %q", isErr, out)
	}
	// table still exists
	out, isErr, _ = rw.Dispatch(ctx, call("sql_query", "SELECT COUNT(*) FROM products"))
	if isErr {
		t.Fatalf("DROP was not actually blocked — table gone: %q", out)
	}
}

func TestGate_DDLAllowedWithConfirm(t *testing.T) {
	st := mkStore(t)
	var sawDetail string
	d := sqlp.NewDispatcher(st, sqlp.Policy{AllowWrite: true,
		Confirm: func(_ context.Context, _, det string) bool { sawDetail = det; return true }})
	out, isErr, _ := d.Dispatch(context.Background(), call("sql_exec", "DROP TABLE products"))
	if isErr {
		t.Fatalf("confirmed DDL should run: %q", out)
	}
	if !strings.Contains(sawDetail, "DROP TABLE products") {
		t.Errorf("Confirm detail should describe the statement: %q", sawDetail)
	}
}

// --- defense in depth: sql_query refuses non-reads regardless ---

func TestQueryToolRefusesNonRead(t *testing.T) {
	st := mkStore(t)
	// Even with AllowWrite=true, sql_query must refuse a DELETE — the
	// read tool is read-only by construction.
	d := sqlp.NewDispatcher(st, sqlp.Policy{AllowWrite: true})
	out, isErr, _ := d.Dispatch(context.Background(), call("sql_query", "DELETE FROM products"))
	if !isErr || !strings.Contains(out, "only runs read") {
		t.Fatalf("sql_query must refuse non-read: isErr=%v %q", isErr, out)
	}
	out, _, _ = d.Dispatch(context.Background(), call("sql_query", "SELECT COUNT(*) FROM products"))
	if !strings.Contains(out, "3") {
		t.Fatalf("rows changed — sql_query executed a DELETE!: %q", out)
	}
}

func TestQueryToolRejectsInjection(t *testing.T) {
	st := mkStore(t)
	d := sqlp.NewDispatcher(st, sqlp.Policy{})
	out, isErr, _ := d.Dispatch(context.Background(),
		call("sql_query", "SELECT 1; DROP TABLE products"))
	if !isErr || !strings.Contains(out, "multiple statements") {
		t.Fatalf("injection must be rejected: isErr=%v %q", isErr, out)
	}
	out, isErr, _ = d.Dispatch(context.Background(), call("sql_query", "SELECT COUNT(*) FROM products"))
	if isErr || !strings.Contains(out, "3") {
		t.Fatalf("table should be intact: %q", out)
	}
}

// --- the engine holds the line, not the text classifier ---

// Quoting must be read the way SQLite reads it, or a comment marker inside
// a quoted name is taken for a comment and what follows it goes unseen.
func TestClassify_AllQuotingForms(t *testing.T) {
	// A comment marker inside each kind of quoted run is not a comment.
	for _, q := range []string{
		"SELECT 'a--b' AS x",
		`SELECT "a--b" FROM t`,
		"SELECT `a--b` FROM t",
		"SELECT [a--b] FROM t",
		"SELECT `a/*b` FROM t",
		"SELECT [a/*b] FROM t",
	} {
		if cl, _, r := sqlp.Classify(q); r != "" || cl != sqlp.ClassRead {
			t.Errorf("Classify(%q) = %v, reason %q; want read", q, cl, r)
		}
	}
	// So a statement separator after such a name is still seen.
	for _, q := range []string{
		"SELECT 1 AS `a--b`; SELECT 2",
		"SELECT 1 AS [a--b]; SELECT 2",
		"SELECT 1 AS `a/*b`; SELECT 2",
		"SELECT 1 AS [a/*b]; SELECT 2",
	} {
		if _, _, r := sqlp.Classify(q); r == "" {
			t.Errorf("Classify(%q) accepted two statements", q)
		}
	}
}

// Store.Query is read-only in the engine: a statement that changes the
// database fails there, whatever a caller classified it as.
func TestQuery_EngineRefusesAChange(t *testing.T) {
	st := mkStore(t)
	ctx := context.Background()
	for _, stmt := range []string{
		"DELETE FROM products",
		"INSERT INTO products (name, price) VALUES ('X', 1)",
		"DROP TABLE products",
	} {
		if _, err := st.Query(ctx, stmt, 10); err == nil {
			t.Errorf("Query(%q) succeeded; the engine must refuse a change", stmt)
		}
	}
	res, err := st.Query(ctx, "SELECT COUNT(*) FROM products", 10)
	if err != nil || len(res.Rows) != 1 || res.Rows[0][0] != "3" {
		t.Fatalf("rows changed through Query: %v %v", res, err)
	}
	// The connection goes back to the pool writable.
	if _, err := st.Exec(ctx, "INSERT INTO products (name, price) VALUES ('Y', 2)"); err != nil {
		t.Fatalf("Exec after Query: %v", err)
	}
}

// The plugin serves one database: nothing reaches another file, with or
// without an approval, and no approval is asked for it.
func TestExec_NeverLeavesTheDatabase(t *testing.T) {
	st := mkStore(t)
	ctx := context.Background()
	other := filepath.Join(t.TempDir(), "other.db")
	asked := 0
	d := sqlp.NewDispatcher(st, sqlp.Policy{AllowWrite: true,
		Confirm: func(context.Context, string, string) bool { asked++; return true }})
	for _, stmt := range []string{
		"ATTACH DATABASE '" + other + "' AS o",
		"DETACH DATABASE o",
		"VACUUM INTO '" + other + "'",
		"vacuum main into '" + other + "'",
	} {
		out, isErr, _ := d.Dispatch(ctx, call("sql_exec", stmt))
		if !isErr || !strings.Contains(out, "hard boundary") {
			t.Errorf("%q: isErr=%v %q; want the hard-boundary refusal", stmt, isErr, out)
		}
	}
	if asked != 0 {
		t.Errorf("asked for approval %d times for statements that are never allowed", asked)
	}
	if _, err := os.Stat(other); err == nil {
		t.Error("a second database file was created")
	}
	// And the connection itself refuses ATTACH, under the dispatcher.
	if _, err := st.Exec(ctx, "ATTACH DATABASE '"+other+"' AS o"); err == nil {
		t.Error("Store.Exec ran ATTACH; the connection limit must refuse it")
	}
	if _, err := st.Exec(ctx, "VACUUM INTO '"+other+"'"); err == nil {
		t.Error("Store.Exec ran VACUUM INTO; the connection limit must refuse it")
	}
	if _, err := os.Stat(other); err == nil {
		t.Error("a second database file was created under the dispatcher")
	}
	// A plain VACUUM is still an ordinary confirmed statement.
	if out, isErr, _ := d.Dispatch(ctx, call("sql_exec", "VACUUM")); isErr {
		t.Errorf("plain VACUUM should run once confirmed: %q", out)
	}
}

// --- schema + result shaping ---

func TestSchema(t *testing.T) {
	st := mkStore(t)
	d := sqlp.NewDispatcher(st, sqlp.Policy{})
	out, isErr, _ := d.Dispatch(context.Background(), call("sql_schema", ""))
	if isErr {
		t.Fatalf("schema failed: %q", out)
	}
	for _, want := range []string{"TABLE products", "id INTEGER PRIMARY KEY", "name TEXT", "price REAL"} {
		if !strings.Contains(out, want) {
			t.Errorf("schema missing %q:\n%s", want, out)
		}
	}
}

func TestQuery_RowCapTruncates(t *testing.T) {
	st, _ := sqlp.Open(sqlp.Config{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "big.db")})
	defer st.Close()
	ctx := context.Background()
	_, _ = st.Exec(ctx, "CREATE TABLE n (i INTEGER)")
	for i := 0; i < 500; i++ {
		_, _ = st.Exec(ctx, "INSERT INTO n VALUES ("+itoa(i)+")")
	}
	res, err := st.Query(ctx, "SELECT i FROM n", 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 200 || !res.Truncated {
		t.Fatalf("expected 200 rows + Truncated, got %d trunc=%v", len(res.Rows), res.Truncated)
	}
}

func TestOpen_RejectsNonSQLite(t *testing.T) {
	if _, err := sqlp.Open(sqlp.Config{Driver: "postgres", DSN: "x"}); err == nil {
		t.Fatal("postgres should be rejected in v1")
	}
	if _, err := sqlp.Open(sqlp.Config{Driver: "sqlite"}); err == nil {
		t.Fatal("missing DSN should error")
	}
}

func TestOpen_CreatesMissingParentDirs(t *testing.T) {
	// Pointing --db at a path whose parent dirs don't exist used to fail with
	// "unable to open database file"; Open now creates the tree first.
	dbPath := filepath.Join(t.TempDir(), "nested", "deep", "fresh.db")
	st, err := sqlp.Open(sqlp.Config{Driver: "sqlite", DSN: dbPath})
	if err != nil {
		t.Fatalf("Open with missing parent dirs: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("db file not created: %v", err)
	}
}

func TestTools(t *testing.T) {
	d := sqlp.NewDispatcher(nil, sqlp.Policy{})
	names := map[string]bool{}
	for _, sp := range d.Tools() {
		names[sp.Name] = true
		if (sp.Name == "sql_schema" || sp.Name == "sql_query") &&
			(len(sp.ResultLabels) != 1 || sp.ResultLabels[0].Provenance != model.ProvenanceLocalService) {
			t.Errorf("%s labels = %+v", sp.Name, sp.ResultLabels)
		}
		if !json.Valid(sp.Parameters) {
			t.Errorf("%s invalid params schema", sp.Name)
		}
	}
	for _, w := range []string{"sql_schema", "sql_query", "sql_exec"} {
		if !names[w] {
			t.Errorf("missing tool %s", w)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
