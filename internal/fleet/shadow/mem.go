package shadow

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

// errMemLost is every later answer of an in-memory database whose one
// connection is gone: what it held went with the connection, and a second
// connection would be a new, empty database passing for the old one.
var errMemLost = errors.New("shadow: the in-memory database lost its connection")

// memDSN is a private in-memory database. temp_store keeps SQLite's own
// temporary files (sorts, temporary tables, VACUUM's working copy) in RAM,
// and the rollback journal of an in-memory database is in RAM by nature, so
// nothing the database holds reaches a disk through SQLite.
const memDSN = ":memory:?_pragma=foreign_keys(1)&_pragma=temp_store(2)&_pragma=trusted_schema(0)"

// memPragmas belong to the database, not the connection, so they are set
// after the image is in. secure_delete: a deleted row's bytes are zeroed and
// do not ride along in later snapshots. cache_size, in KiB: SQLite's page
// cache holds copies of pages, and for a database that is itself in RAM a
// copy is cheap to make again and dear to keep, per machine.
const memPragmas = "PRAGMA secure_delete = ON; PRAGMA cache_size = -512"

// sqliteDriver is the driver the stores use, as registered.
var sqliteDriver = func() driver.Driver {
	db, err := sql.Open("sqlite", "")
	if err != nil {
		panic(err)
	}
	defer db.Close()
	return db.Driver()
}()

// memDB is an in-memory SQLite database with exactly one connection.
type memDB struct {
	*sql.DB
	conn *memConnector
}

// openMem opens an in-memory database that starts as image, a serialized
// database, or empty when image is nil. With check, the image must pass
// SQLite's integrity check before the database is returned.
func openMem(ctx context.Context, image []byte, check bool) (*memDB, error) {
	c := &memConnector{image: image}
	db := sql.OpenDB(c)
	db.SetMaxOpenConns(1)
	m := &memDB{DB: db, conn: c}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if check {
		if err := integrityCheck(ctx, db); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return m, nil
}

// serialize returns the database as the bytes of a database file, and the
// connection's count of rows changed so far, which moves with every write.
// It takes the one connection, so it waits for a statement or transaction in
// flight, and the image and the count are of one moment between two of them.
func (m *memDB) serialize(ctx context.Context) (image []byte, changes int64, err error) {
	conn, err := m.DB.Conn(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer conn.Close()
	if err := conn.QueryRowContext(ctx, "SELECT total_changes()").Scan(&changes); err != nil {
		return nil, 0, err
	}
	image, err = m.conn.serialize()
	return image, changes, err
}

// changes is the connection's count of rows changed so far.
func (m *memDB) changes(ctx context.Context) (n int64, err error) {
	err = m.DB.QueryRowContext(ctx, "SELECT total_changes()").Scan(&n)
	return n, err
}

// integrityCheck runs PRAGMA integrity_check and names the first problems.
func integrityCheck(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return fmt.Errorf("%w: integrity check: %v", ErrDamaged, err)
	}
	defer rows.Close()
	var issues []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return fmt.Errorf("%w: integrity check: %v", ErrDamaged, err)
		}
		if line != "ok" && len(issues) < 3 {
			issues = append(issues, line)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w: integrity check: %v", ErrDamaged, err)
	}
	if len(issues) > 0 {
		return fmt.Errorf("%w: integrity check: %s", ErrDamaged, strings.Join(issues, "; "))
	}
	return nil
}

// memConnector makes the one connection of an in-memory database, loading
// the image into it, and refuses to make a second.
type memConnector struct {
	mu    sync.Mutex
	image []byte
	used  bool
	raw   driver.Conn
}

func (c *memConnector) Driver() driver.Driver { return sqliteDriver }

func (c *memConnector) Connect(ctx context.Context) (driver.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.used {
		return nil, errMemLost
	}
	c.used = true
	raw, err := sqliteDriver.Open(memDSN)
	if err != nil {
		return nil, err
	}
	if len(c.image) > 0 {
		err = raw.(interface{ Deserialize([]byte) error }).Deserialize(c.image)
		c.image = nil
	}
	if err == nil {
		_, err = raw.(driver.ExecerContext).ExecContext(ctx, memPragmas, nil)
	}
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	c.raw = raw
	return memConn{
		Conn:               raw,
		ExecerContext:      raw.(driver.ExecerContext),
		QueryerContext:     raw.(driver.QueryerContext),
		ConnPrepareContext: raw.(driver.ConnPrepareContext),
		ConnBeginTx:        raw.(driver.ConnBeginTx),
	}, nil
}

func (c *memConnector) serialize() ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.raw == nil {
		return nil, errMemLost
	}
	return c.raw.(interface{ Serialize() ([]byte, error) }).Serialize()
}

// memConn is the driver's connection without its validity checks. The driver
// reports a connection as unusable while an interrupt (a cancelled context)
// is pending on it, and database/sql then closes it and opens another. For a
// file that is harmless; here it would throw the database away. A pending
// interrupt clears itself when the next statement starts.
type memConn struct {
	driver.Conn
	driver.ExecerContext
	driver.QueryerContext
	driver.ConnPrepareContext
	driver.ConnBeginTx
}
