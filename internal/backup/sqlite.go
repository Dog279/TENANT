package backup

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"modernc.org/sqlite"
)

// sqliteMagic starts every SQLite 3 database file.
const sqliteMagic = "SQLite format 3\x00"

// isSQLite reports whether path starts with the SQLite header, so stores are
// recognized by content rather than by a list of names that can go stale.
func isSQLite(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	head := make([]byte, len(sqliteMagic))
	if _, err := io.ReadFull(f, head); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return false, nil
		}
		return false, err
	}
	return bytes.Equal(head, []byte(sqliteMagic)), nil
}

// openReadOnly opens a database without the ability to change it: no
// migrations, no journal-mode switch, no permission tightening.
func openReadOnly(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// snapshotSQLite writes a consistent copy of src to dst with VACUUM INTO. It
// reads one snapshot, so it is safe while another process writes to src (the
// stores run in WAL mode), and the copy is a single self-contained file with
// no -wal or -shm companion.
//
// The source is opened read-only. A database left mid-transaction by a crash
// cannot be read that way: its journal or WAL has to be recovered first, which
// needs write access. Only then is it opened read-write, to run the same
// recovery Tenant runs when it next opens the store; VACUUM INTO still only
// reads it.
func snapshotSQLite(ctx context.Context, src, dst string) error {
	err := vacuumInto(ctx, src, dst, "ro")
	if isReadOnly(err) {
		_ = os.Remove(dst)
		err = vacuumInto(ctx, src, dst, "rw")
	}
	if err != nil {
		return fmt.Errorf("snapshot %s: %w", src, err)
	}
	return nil
}

func vacuumInto(ctx context.Context, src, dst, mode string) error {
	db, err := sql.Open("sqlite", "file:"+src+"?mode="+mode+"&_pragma=busy_timeout(10000)")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(ctx, "VACUUM INTO ?", dst)
	return err
}

// isReadOnly reports SQLITE_READONLY or one of its extended codes, such as a
// hot journal that needs rolling back (SQLITE_READONLY_ROLLBACK).
func isReadOnly(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code()&0xff == 8
}

// integrityCheck runs PRAGMA integrity_check and returns an error naming the
// first problems it reports.
func integrityCheck(ctx context.Context, path string) error {
	db, err := openReadOnly(path)
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	var issues []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return err
		}
		if strings.TrimSpace(line) != "ok" {
			issues = append(issues, line)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(issues) > 0 {
		if len(issues) > 3 {
			issues = append(issues[:3], fmt.Sprintf("and %d more", len(issues)-3))
		}
		return fmt.Errorf("integrity_check: %s", strings.Join(issues, "; "))
	}
	return nil
}

// Schema is a database's tables and each table's columns, sorted.
type Schema map[string][]string

// ReadSchema lists the user tables of the database at path and their columns.
// SQLite's own tables are left out.
func ReadSchema(ctx context.Context, path string) (Schema, error) {
	db, err := openReadOnly(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return readSchema(ctx, db)
}

func readSchema(ctx context.Context, db *sql.DB) (Schema, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\'`)
	if err != nil {
		return nil, err
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		tables = append(tables, name)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	s := Schema{}
	for _, t := range tables {
		cols, err := db.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", t)
		if err != nil {
			return nil, err
		}
		var names []string
		for cols.Next() {
			var c string
			if err := cols.Scan(&c); err != nil {
				cols.Close()
				return nil, err
			}
			names = append(names, c)
		}
		if err := cols.Close(); err != nil {
			return nil, err
		}
		sort.Strings(names)
		s[t] = names
	}
	return s, nil
}

// Newer lists what s has that ref, the schema this build of Tenant creates,
// does not: whole tables, or columns of shared tables. A backup made by a
// newer Tenant shows up here; one made by an older Tenant does not, because
// the stores add missing tables and columns when they open.
func (s Schema) Newer(ref Schema) []string {
	var out []string
	for table, cols := range s {
		refCols, ok := ref[table]
		if !ok {
			out = append(out, "table "+table)
			continue
		}
		have := make(map[string]bool, len(refCols))
		for _, c := range refCols {
			have[c] = true
		}
		for _, c := range cols {
			if !have[c] {
				out = append(out, "column "+table+"."+c)
			}
		}
	}
	sort.Strings(out)
	return out
}
