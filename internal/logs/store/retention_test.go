package store

import (
	"bufio"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"tenant/internal/logs/event"
)

const day = 24 * time.Hour

// commitLog commits n sample events to log, in the pipeline's batches.
func commitLog(t testing.TB, s *Store, log string, n int) {
	t.Helper()
	batch := make([]event.Event, 0, 512)
	for i := range n {
		e := sample(i)
		e.Log = log
		batch = append(batch, e)
		if len(batch) == cap(batch) || i == n-1 {
			if err := s.Commit(ctx, batch); err != nil {
				t.Fatal(err)
			}
			batch = batch[:0]
		}
	}
}

func count(t testing.TB, s *Store, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := s.r.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// logBytes checks the log's bytes column still matches its events, and
// returns it.
func logBytes(t testing.TB, s *Store, log string) int64 {
	t.Helper()
	id := count(t, s, `SELECT id FROM names WHERE kind = 'log' AND name = ?`, log)
	col := count(t, s, `SELECT coalesce(bytes, 0) FROM logs WHERE log_id = ?`, id)
	if sum := count(t, s, `SELECT coalesce(sum(bytes), 0) FROM events WHERE log_id = ?`, id); col != sum {
		t.Errorf("logs.bytes for %s = %d, its events sum to %d", log, col, sum)
	}
	return col
}

// Expired events go in transactions of at most 500, with their full-text
// rows and their bytes; a log still inside its age keeps everything. Each
// chunk's hold on the write lock is logged, and checked against §12.3's
// 50 ms with TENANT_LOGBOOK_PERF set.
func TestRetainDeletesInChunks(t *testing.T) {
	s := open(t, tempDB(t), "")
	commitLog(t, s, "turns", 2600)
	commitLog(t, s, "system", 10) // 90 days
	rep, err := s.Retain(ctx, DefaultBudget(), time.Now().Add(31*day))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d chunks, at most %d rows, the longest held the write lock %v", rep.Chunks, rep.MaxRows, rep.MaxLock)
	if rep.Deleted["turns"] != 2600 || rep.Deleted["system"] != 0 || rep.Chunks != 6 || rep.MaxRows != chunkRows {
		t.Errorf("report %+v; want 2600 turns in 6 chunks of at most %d", rep, chunkRows)
	}
	if n := count(t, s, `SELECT count(*) FROM events`); n != 10 {
		t.Errorf("%d events left, want system's 10", n)
	}
	if n := count(t, s, `SELECT count(*) FROM events_fts WHERE events_fts MATCH 'web_search'`); n != 10 {
		t.Errorf("full-text search finds %d events, want 10", n)
	}
	if b := logBytes(t, s, "turns"); b != 0 {
		t.Errorf("turns has %d bytes left", b)
	}
	logBytes(t, s, "system")
	if os.Getenv("TENANT_LOGBOOK_PERF") != "" && rep.MaxLock > 50*time.Millisecond {
		t.Errorf("a chunk held the write lock %v, over the 50 ms budget", rep.MaxLock)
	}
}

// Rollups outlive their events until the longest retention of any log.
func TestRetainPrunesRollups(t *testing.T) {
	s := open(t, tempDB(t), "")
	commitLog(t, s, "turns", 10)
	commitLog(t, s, "system", 10)
	if _, err := s.Retain(ctx, DefaultBudget(), time.Now().Add(31*day)); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM rollup`); n == 0 {
		t.Error("rollups went before system's 90 days")
	}
	if _, err := s.Retain(ctx, DefaultBudget(), time.Now().Add(91*day)); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM rollup`); n != 0 {
		t.Errorf("%d rollups left past every log's retention", n)
	}
}

// share is log's apportioned share of the files: size × its bytes / all bytes.
func share(t testing.TB, s *Store, log string) int64 {
	t.Helper()
	db, wal, err := s.Size(ctx)
	if err != nil {
		t.Fatal(err)
	}
	total := count(t, s, `SELECT sum(bytes) FROM logs`)
	return int64(float64(db+wal) * float64(logBytes(t, s, log)) / float64(total))
}

// One test per when-full action: overwrite deletes the oldest until the log's
// share fits; archive writes them to a gzip NDJSON file first; keep deletes
// nothing and reports the log.
func TestWhenFull(t *testing.T) {
	const limit = 256 << 10
	for _, action := range []string{Overwrite, Archive, Keep} {
		t.Run(action, func(t *testing.T) {
			path := tempDB(t)
			s := open(t, path, "")
			log := "turns"
			if action == Archive {
				log = SecurityLog // archived by default, whatever its limit
			}
			commitLog(t, s, log, 2000)
			commitLog(t, s, "system", 200)
			first, last := count(t, s, `SELECT min(seq) FROM events`), count(t, s, `SELECT max(seq) FROM events WHERE seq < 2001`)
			b := DefaultBudget()
			b.Logs[log] = Policy{MaxBytes: limit, MaxAge: 365 * day, OnFull: action}
			rep, err := s.Retain(ctx, b, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			left := count(t, s, `SELECT count(*) FROM events WHERE seq <= ?`, last)
			if action == Keep {
				if len(rep.Deleted) != 0 || left != 2000 || !reflect.DeepEqual(rep.Kept, []string{log}) {
					t.Errorf("keep: deleted %v, %d left, kept %v", rep.Deleted, left, rep.Kept)
				}
				return
			}
			if rep.Deleted[log] == 0 || rep.Deleted[log]+left != 2000 || rep.Deleted["system"] != 0 {
				t.Fatalf("deleted %v with %d left", rep.Deleted, left)
			}
			if got := share(t, s, log); got > limit*11/10 {
				t.Errorf("%s's share is %d bytes, over its %d limit", log, got, limit)
			}
			if count(t, s, `SELECT count(*) FROM events WHERE seq = ?`, first) != 0 || count(t, s, `SELECT count(*) FROM events WHERE seq = ?`, last) != 1 {
				t.Error("the oldest event is kept or the newest gone")
			}
			if action == Overwrite {
				if len(rep.Archived) != 0 {
					t.Errorf("overwrite archived %v", rep.Archived)
				}
				return
			}
			// Each step archives to its own file, named for its first and last
			// record, and closed by the last one's (rec, hash).
			var lines [][]byte
			for _, name := range rep.Archived {
				got := readArchive(t, filepath.Join(filepath.Dir(path), "archive", name))
				var a, z, end struct {
					Rec  int64
					Hash string
				}
				_ = json.Unmarshal(got[0], &a)
				_ = json.Unmarshal(got[len(got)-2], &z)
				_ = json.Unmarshal(got[len(got)-1], &end)
				if want := fmt.Sprintf("security-%d-%d.ndjson.gz", a.Rec, z.Rec); name != want || end != z || z.Hash == "" {
					t.Errorf("archive %s holds %s, closed by %+v after %+v", name, want, end, z)
				}
				lines = append(lines, got[:len(got)-1]...)
			}
			if int64(len(lines)) != rep.Deleted[log] {
				t.Fatalf("the archives have %d lines for %d deleted events", len(lines), rep.Deleted[log])
			}
			var rec struct {
				Seq            int64
				Log, Msg, Data string
				BootID         string `json:"boot_id"`
			}
			if err := json.Unmarshal(lines[0], &rec); err != nil {
				t.Fatal(err)
			}
			e := sample(0)
			if rec.Seq != first || rec.Log != SecurityLog || rec.Msg != e.Msg || rec.BootID != e.Proc.BootID {
				t.Errorf("first archived record %s", lines[0])
			}
			if data, _ := encodeData(e.Data); rec.Data != data {
				t.Errorf("archived data %q, stored as %q", rec.Data, data)
			}
		})
	}
}

func readArchive(t *testing.T, path string) [][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	sc := bufio.NewScanner(z)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		out = append(out, append([]byte(nil), sc.Bytes()...))
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Under sustained writes the database and its WAL stay within 10% of the
// whole book's budget, measured on disk after each pass, and retention keeps
// what fits rather than emptying the book.
func TestBudgetHoldsUnderSustainedWrites(t *testing.T) {
	s := open(t, tempDB(t), "")
	b := DefaultBudget()
	b.TotalBytes = 2 << 20
	for round := range 5 {
		commitLog(t, s, "turns", 3000)
		rep, err := s.Retain(ctx, b, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		db, wal, err := s.Size(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("round %d: deleted %d, database %d + WAL %d bytes", round, rep.Deleted["turns"], db, wal)
		if db+wal != rep.Size || db+wal > b.TotalBytes*11/10 || wal != 0 {
			t.Errorf("round %d: database %d + WAL %d, report %d; budget %d", round, db, wal, rep.Size, b.TotalBytes)
		}
		if db+wal < b.TotalBytes/2 {
			t.Errorf("round %d: %d bytes left, well under the %d budget", round, db+wal, b.TotalBytes)
		}
		if rep.Over {
			t.Errorf("round %d: over budget with turns to shed", round)
		}
	}
}

// The whole book's budget sheds diagnostic events first, then overwritable
// logs, and never the Security log: with only security left it reports Over.
func TestBudgetNeverShedsSecurity(t *testing.T) {
	s := open(t, tempDB(t), "")
	commitLog(t, s, SecurityLog, 1000)
	batch := make([]event.Event, 300)
	for i := range batch {
		batch[i] = sample(i)
		batch[i].Channel = event.Diagnostic
	}
	if err := s.Commit(ctx, batch); err != nil {
		t.Fatal(err)
	}
	b := DefaultBudget()
	b.TotalBytes = 64 << 10
	rep, err := s.Retain(ctx, b, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Deleted["turns"] != 300 || rep.Deleted[SecurityLog] != 0 || !rep.Over || len(rep.Archived) != 0 {
		t.Errorf("report %+v; want the 300 diagnostic events shed, security untouched, over budget", rep)
	}
}

// A log's own policy round-trips and wins over the budget's; the zero Policy
// returns it to the budget's; bad policies and a weakened Security log are
// refused, and the Security log's policy is clamped whatever config says.
func TestPolicyRoundTrip(t *testing.T) {
	s := open(t, tempDB(t), "")
	b := DefaultBudget()
	if p, err := s.Policy(ctx, b, "mcp.github"); err != nil || p != b.Operational {
		t.Errorf("an unknown log's policy = %+v, %v; want the operational default", p, err)
	}
	own := Policy{MaxBytes: 32 << 20, MaxAge: 2 * day, OnFull: Keep, MinLevel: event.Warning, Disabled: true}
	if err := s.SetPolicy(ctx, "mcp.github", own); err != nil {
		t.Fatal(err)
	}
	if p, err := s.Policy(ctx, b, "mcp.github"); err != nil || p != own {
		t.Errorf("Policy = %+v, %v; want %+v", p, err, own)
	}
	commitLog(t, s, "mcp.github", 3) // committing keeps the policy
	if ps, err := s.Policies(ctx, b); err != nil || ps["mcp.github"] != own || len(ps) != 1 {
		t.Errorf("Policies = %v, %v", ps, err)
	}
	if err := s.SetPolicy(ctx, "mcp.github", Policy{}); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Policy(ctx, b, "mcp.github"); p != b.Operational {
		t.Errorf("after a reset: %+v", p)
	}
	for _, bad := range []struct {
		log string
		p   Policy
	}{
		{"x", Policy{OnFull: "rotate"}},
		{"x", Policy{OnFull: Overwrite, MaxBytes: -1}},
		{"x", Policy{OnFull: Overwrite, MinLevel: 9}},
		{"", Policy{OnFull: Overwrite}},
		{SecurityLog, Policy{OnFull: Overwrite}},
		{SecurityLog, Policy{OnFull: Archive, Disabled: true}},
		{SecurityLog, Policy{OnFull: Archive, MinLevel: event.Error}},
	} {
		if err := s.SetPolicy(ctx, bad.log, bad.p); err == nil {
			t.Errorf("SetPolicy(%q, %+v) accepted", bad.log, bad.p)
		}
	}
	b.Logs[SecurityLog] = Policy{MaxBytes: 1, OnFull: Overwrite, MinLevel: event.Critical, Disabled: true}
	if p := b.For(SecurityLog); p != (Policy{MaxBytes: 1, OnFull: Archive}) {
		t.Errorf("config weakened the Security log: %+v", p)
	}
}

// A database at schema version 1 is brought up to date: the diagnostic index
// is added and the version recorded.
func TestUpgradeFromSchema1(t *testing.T) {
	path := tempDB(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT);` + migrations[0] +
		`INSERT INTO meta VALUES ('schema_version', '1'), ('store_id', '0123456789abcdef0123456789abcdef');`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	s := open(t, path, "")
	if v, _ := meta(ctx, s.r); v != strconv.Itoa(len(migrations)) {
		t.Errorf("schema_version = %s", v)
	}
	if n := count(t, s, `SELECT count(*) FROM sqlite_master WHERE name = 'events_diag'`); n != 1 {
		t.Error("no events_diag index after the upgrade")
	}
}
