package privacy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"

	_ "modernc.org/sqlite"
)

// queryer is what both *sql.DB and *sql.Tx offer, so the same queries serve
// a preview and the transaction that applies it.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// openStore opens a store for deletion: foreign keys on, so a fact's signals
// and removed-source records go with it, and secure_delete on, so the pages a
// deletion frees are zeroed instead of keeping the old text. A missing file is
// reported as fs.ErrNotExist and never created.
func openStore(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=rw&_pragma=busy_timeout(10000)&_pragma=foreign_keys(ON)&_pragma=secure_delete(ON)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// openReadOnly opens a store for a preview or an export.
func openReadOnly(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func missing(err error) bool { return errors.Is(err, fs.ErrNotExist) }

// hasTable reports whether a table exists; stores add side tables as they
// grow, and an older database may lack one.
func hasTable(ctx context.Context, q queryer, name string) (bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?`, name)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	return rows.Next(), rows.Err()
}

// int64s parses a JSON array of ids as the stores write them. NULL, "" and
// "null" are empty.
func int64s(s sql.NullString) ([]int64, error) {
	if !s.Valid || s.String == "" || s.String == "null" {
		return nil, nil
	}
	var out []int64
	if err := json.Unmarshal([]byte(s.String), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func strs(s sql.NullString) ([]string, error) {
	if !s.Valid || s.String == "" || s.String == "null" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s.String), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// jsonList encodes a non-empty list, and NULL for an empty one, matching how
// the stores write a list with nothing in it.
func jsonList[T any](v []T) any {
	if len(v) == 0 {
		return nil
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// lowerASCII folds ASCII letters only, the same folding SQLite's lower()
// does, so matching in Go and in SQL agrees.
func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

func containsFold(haystack, lowerNeedle string) bool {
	return lowerNeedle != "" && strings.Contains(lowerASCII(haystack), lowerNeedle)
}

// inClause renders "?, ?, ?" for n parameters.
func inClause(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?, ", n-1) + "?"
}

func anys[T any](v []T) []any {
	out := make([]any, len(v))
	for i, x := range v {
		out[i] = x
	}
	return out
}

// chunks splits ids so an IN list stays well under SQLite's parameter limit.
func chunks[T any](v []T, size int) [][]T {
	var out [][]T
	for len(v) > size {
		out = append(out, v[:size])
		v = v[size:]
	}
	if len(v) > 0 {
		out = append(out, v)
	}
	return out
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}
