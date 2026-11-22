package shadow

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"math"
	"slices"
	"strings"
	"time"
)

// embeddingColumn is each table's vector column. The child's copy carries
// them empty: they are most of the bytes, they do not compress, and a
// rebuilt shadow is re-embedded.
var embeddingColumn = map[string]string{"episodes": "embedding", "facts": "embedding"}

// unhashed is what the content hash leaves out of each table.
//
// The vectors and the name of the model that made them: re-embedding changes
// both and changes nothing a child's copy holds. And the two access columns
// of fact_signals, which recall writes on every read. The rest of that table
// (importance, pins, protection, the validity window) is content and is
// hashed.
var unhashed = map[string][]string{
	"episodes":     {"embedding", "embedder_id"},
	"facts":        {"embedding", "embedder_id"},
	"fact_signals": {"access_count", "last_accessed"},
}

// unhashedRows leaves out whole rows. Recall makes a fact_signals row for a
// fact that had none, holding the two access columns and defaults; the store
// reads such a row exactly as it reads no row.
var unhashedRows = map[string]string{
	"fact_signals": `importance = 0.5 AND confirm_count = 0 AND pinned = 0 AND protected = 0
		AND valid_from IS NULL AND valid_to IS NULL AND project_id IS NULL`,
}

// contentHash is the SHA-256 of what a machine's memory says: every row of
// every table of both stores, except what unhashed and unhashedRows name. A
// recall leaves it as it was and every write of content changes it. It does
// not depend on where SQLite keeps a row, so a copy that went through VACUUM
// hashes the same.
func contentHash(ctx context.Context, episodic, semantic *sql.DB) (string, error) {
	h := sha256.New()
	for _, db := range []*sql.DB{episodic, semantic} {
		if err := hashDB(ctx, h, db); err != nil {
			return "", fmt.Errorf("shadow: content hash: %w", err)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func hashDB(ctx context.Context, h hash.Hash, db *sql.DB) error {
	// One read transaction: it holds the database's one connection, so the
	// tables are hashed as of one moment.
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Ordinary tables only: an FTS index is a virtual table over shadow
	// tables, all derived from the rows hashed here.
	tables, err := queryStrings(ctx, tx, `SELECT name FROM pragma_table_list
		WHERE schema = 'main' AND type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		return err
	}
	for _, table := range tables {
		if err := hashTable(ctx, h, tx, table); err != nil {
			return fmt.Errorf("table %s: %w", table, err)
		}
	}
	hashValue(h, nil) // end of this database
	return nil
}

func hashTable(ctx context.Context, h hash.Hash, tx *sql.Tx, table string) error {
	all, err := queryStrings(ctx, tx, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		return err
	}
	key, err := queryStrings(ctx, tx, `SELECT name FROM pragma_table_info(?) WHERE pk > 0 ORDER BY pk`, table)
	if err != nil {
		return err
	}
	var cols []string
	for _, c := range all {
		if !slices.Contains(unhashed[table], c) {
			cols = append(cols, c)
		}
	}
	hashValue(h, table)
	hashValue(h, strings.Join(cols, ","))
	if len(cols) == 0 {
		return nil
	}
	q := "SELECT " + quoteIdents(cols) + " FROM " + quoteIdents([]string{table})
	if where := unhashedRows[table]; where != "" {
		q += " WHERE NOT (" + where + ")"
	}
	if len(key) > 0 {
		q += " ORDER BY " + quoteIdents(key)
	} else {
		q += " ORDER BY rowid"
	}
	rows, err := tx.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		for _, v := range vals {
			hashValue(h, v)
		}
	}
	return rows.Err()
}

// hashValue writes one value with its type and length, so that no two
// different sequences of values write the same bytes.
func hashValue(h hash.Hash, v any) {
	var head [9]byte
	switch v := v.(type) {
	case nil:
		h.Write(head[:1])
	case int64:
		head[0] = 1
		binary.BigEndian.PutUint64(head[1:], uint64(v))
		h.Write(head[:])
	case float64:
		head[0] = 2
		binary.BigEndian.PutUint64(head[1:], math.Float64bits(v))
		h.Write(head[:])
	case string:
		head[0] = 3
		binary.BigEndian.PutUint64(head[1:], uint64(len(v)))
		h.Write(head[:])
		_, _ = io.WriteString(h, v)
	case []byte:
		head[0] = 4
		binary.BigEndian.PutUint64(head[1:], uint64(len(v)))
		h.Write(head[:])
		h.Write(v)
	case time.Time:
		// The driver turns text in a column declared as a date into a time.
		head[0] = 5
		binary.BigEndian.PutUint64(head[1:], uint64(v.UnixNano()))
		h.Write(head[:])
	default:
		hashValue(h, fmt.Sprintf("%T %v", v, v))
	}
}

func queryStrings(ctx context.Context, tx *sql.Tx, q string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func quoteIdents(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = `"` + strings.ReplaceAll(n, `"`, `""`) + `"`
	}
	return strings.Join(quoted, ", ")
}
