package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/logs/event"
)

var ctx = context.Background()

// testKey is the chain key the tests' stores are opened with.
var testKey = []byte("0123456789abcdef0123456789abcdef")

func open(t testing.TB, path, boot string) *Store {
	t.Helper()
	s, err := Open(path, Options{BootID: boot, ChainKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func tempDB(t testing.TB) string { return filepath.Join(t.TempDir(), "logs", "logbook.db") }

// The schema is §6.1's, and the writer connection carries §6.1's pragmas:
// fullfsync on macOS only, and _txlock=immediate, so a transaction holds the
// write lock from BEGIN.
func TestSchemaAndPragmas(t *testing.T) {
	path := tempDB(t)
	s := open(t, path, "")
	want := map[string]int64{"busy_timeout": 5000, "synchronous": 2, "secure_delete": 1, "auto_vacuum": 2, "fullfsync": 0, "checkpoint_fullfsync": 0}
	if runtime.GOOS == "darwin" {
		want["fullfsync"], want["checkpoint_fullfsync"] = 1, 1
	}
	for p, v := range want {
		var got int64
		if err := s.w.QueryRow("PRAGMA " + p).Scan(&got); err != nil || got != v {
			t.Errorf("PRAGMA %s = %d (%v), want %d", p, got, err, v)
		}
	}
	var mode string
	if err := s.w.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal_mode = %q (%v), want wal", mode, err)
	}
	for _, table := range []string{"names", "events", "events_fts", "rollup", "logs", "boots", "leases", "views", "meta"} {
		var n int
		if err := s.r.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = ?`, table).Scan(&n); err != nil || n != 1 {
			t.Errorf("table %s: %d (%v)", table, n, err)
		}
	}
	var v string
	if err := s.r.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v); err != nil || v != strconv.Itoa(len(migrations)) {
		t.Errorf("schema_version = %q (%v)", v, err)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("logbook.db: %v (%v), want mode 0600", fi, err)
		}
	}

	// _txlock=immediate: an open transaction already holds the write lock,
	// so another connection's BEGIN IMMEDIATE is refused.
	conn, err := open(t, path, "").w.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "PRAGMA busy_timeout = 0"); err != nil {
		t.Fatal(err)
	}
	tx, err := s.w.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Errorf("a second writer's BEGIN IMMEDIATE while the first held a transaction = %v, want locked", err)
	}
	_ = tx.Rollback()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Errorf("BEGIN IMMEDIATE once the first transaction ended = %v", err)
	}
	_, _ = conn.ExecContext(ctx, "ROLLBACK")
}

// The store ID is minted once, kept across opens, and differs per database;
// a newer schema than this build knows is refused.
func TestStoreIDPersists(t *testing.T) {
	path := tempDB(t)
	a, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	id := a.StoreID()
	if _, err := a.w.Exec(`UPDATE meta SET value = '99' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	_ = a.Close()
	if _, err := Open(path, Options{}); err == nil {
		t.Error("opened a database of a newer schema version")
	}
	fresh := open(t, tempDB(t), "")
	if fresh.StoreID() == id || id == ([16]byte{}) {
		t.Errorf("store IDs %x and %x", id, fresh.StoreID())
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE meta SET value = ? WHERE key = 'schema_version'`, len(migrations)); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if c := open(t, path, ""); c.StoreID() != id {
		t.Errorf("store ID changed across opens: %x then %x", id, c.StoreID())
	}
}

func TestCursorText(t *testing.T) {
	c := Cursor{StoreID: [16]byte{0xde, 0xad, 15: 0x01}, Seq: 42}
	s := c.String()
	if s != "dead0000000000000000000000000001.42" {
		t.Errorf("String = %q", s)
	}
	if got, err := ParseCursor(s); err != nil || got != c {
		t.Errorf("ParseCursor(%q) = %v, %v", s, got, err)
	}
	for _, bad := range []string{"", "42", "dead.42", "dead0000000000000000000000000001", "dead0000000000000000000000000001.-1", "zz000000000000000000000000000001.1"} {
		if _, err := ParseCursor(bad); err == nil {
			t.Errorf("ParseCursor(%q) accepted", bad)
		}
	}
}

func ev(log, msg string, kv ...any) event.Event {
	e := event.Event{Log: log, Provider: log, Time: time.Now().UTC(), Level: event.Info, Msg: msg}
	for i := 0; i+1 < len(kv); i += 2 {
		e.Data = append(e.Data, event.Field{Key: kv[i].(string), Value: kv[i+1]})
	}
	return e
}

// A cursor from another store restarts from the beginning and says so; the
// zero cursor is the beginning; the returned cursor resumes after the last row.
func TestSinceAndForeignCursor(t *testing.T) {
	s := open(t, tempDB(t), "")
	batch := []event.Event{ev("a", "one"), ev("a", "two"), ev("b", "three")}
	if err := s.Commit(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if batch[0].Seq == 0 || batch[1].Seq != batch[0].Seq+1 || batch[2].Seq != batch[1].Seq+1 {
		t.Fatalf("seqs %d %d %d", batch[0].Seq, batch[1].Seq, batch[2].Seq)
	}
	got, next, restarted, err := s.Since(ctx, Cursor{}, 0)
	if err != nil || restarted || len(got) != 3 || next != (Cursor{StoreID: s.StoreID(), Seq: batch[2].Seq}) {
		t.Fatalf("Since(zero) = %d events, %v, restarted %v, %v", len(got), next, restarted, err)
	}
	got, _, restarted, _ = s.Since(ctx, Cursor{StoreID: [16]byte{1}, Seq: batch[1].Seq}, 0)
	if !restarted || len(got) != 3 {
		t.Errorf("foreign cursor: %d events, restarted %v; want all 3, restarted", len(got), restarted)
	}
	got, _, _, _ = s.Since(ctx, Cursor{StoreID: s.StoreID(), Seq: batch[0].Seq}, 1)
	if len(got) != 1 || got[0].Msg != "two" {
		t.Errorf("Since(first, limit 1) = %+v", got)
	}
	if got, again, _, _ := s.Since(ctx, next, 0); len(got) != 0 || again != next {
		t.Errorf("Since(head) = %d events, %v", len(got), again)
	}
	logs, err := s.Logs(ctx)
	if err != nil || len(logs) != 2 || logs[0].Name != "a" || logs[1].Name != "b" || logs[0].Bytes < int64(len("onetwo")) {
		t.Errorf("Logs = %+v, %v", logs, err)
	}
	if _, err := s.Get(ctx, 999); err != ErrNotFound {
		t.Errorf("Get(999) = %v, want ErrNotFound", err)
	}
}

// Every field survives Commit and Get: typed and sensitive data, keywords,
// correlation IDs in both forms, the process. The message is in the
// full-text index, the rollup counts it, and names are interned once.
func TestCommitRoundTrip(t *testing.T) {
	s := open(t, tempDB(t), "")
	at := time.Date(2026, 9, 27, 10, 11, 12, 123456789, time.UTC)
	full := event.Event{
		Log: "security", Provider: "tenant.approval", ID: 4103, Version: 2, Time: at, Level: event.Warning,
		Channel: event.Diagnostic, Task: "Approval", Opcode: event.OpStop, Keywords: []string{"audit.failure", "config"},
		Msg: "Approval ap-1 denied: os_exec",
		Data: []event.Field{
			{Key: "approval", Value: "ap-1"},
			{Key: "args", Value: "rm -rf /", Sensitive: true},
			{Key: "n", Value: int64(-7)},
			{Key: "ratio", Value: 3.0},
			{Key: "ok", Value: true},
			{Key: "none", Value: nil},
			{Key: "payload", Value: json.RawMessage(`{"a":[1,2]}`), Sensitive: true},
			{Key: "at", Value: at},
			{Key: "took", Value: 1500 * time.Millisecond},
			{Key: "n", Value: int64(8)}, // a repeated key keeps both, in order
		},
		Corr: event.Correlation{TraceID: "0123456789abcdef0123456789abcdef", ActivityID: "fedcba9876543210fedcba9876543210",
			RelatedID: "not-a-runtrace-id", SessionID: "s-1", ApprovalID: "ap-1", ToolCallID: "call_9"},
		Actor: "operator", Surface: "dashboard",
		Proc: event.Process{PID: 4242, Role: "serve", BootID: "00112233445566778899aabbccddeeff", Host: "mac", Version: "v1.2.3"},
	}
	batch := []event.Event{full, ev("security", "second")}
	if err := s.Commit(ctx, batch); err != nil {
		t.Fatal(err)
	}
	want := full
	want.Seq = batch[0].Seq
	got, err := s.Get(ctx, want.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip:\n got %+v\nwant %+v", got, want)
	}

	var trace, related, data, types string
	var rec, salt, hash any
	if err := s.r.QueryRow(`SELECT typeof(trace_id) || length(trace_id), typeof(related_id), data, dtypes, rec, salt, hash FROM events WHERE seq = ?`, want.Seq).
		Scan(&trace, &related, &data, &types, &rec, &salt, &hash); err != nil {
		t.Fatal(err)
	}
	if trace != "blob16" || related != "text" {
		t.Errorf("trace_id stored as %s, related_id as %s; want blob16, text", trace, related)
	}
	if types != "sSifbnJtdi" {
		t.Errorf("dtypes = %q", types)
	}
	var n int64
	if err := s.r.QueryRow(`SELECT json_extract(?, '$.approval') = 'ap-1' AND json_extract(?, '$.payload.a[1]') = 2`, data, data).Scan(&n); err != nil || n != 1 {
		t.Errorf("json_extract over data %s: %d, %v", data, n, err)
	}
	b1, _ := salt.([]byte)
	b2, _ := hash.([]byte)
	if rec != int64(1) || len(b1) != 16 || len(b2) != 32 {
		t.Errorf("security chain columns: rec %v, salt %v, hash %v; want record 1, 16 and 32 bytes", rec, salt, hash)
	}

	var hit uint64
	if err := s.r.QueryRow(`SELECT rowid FROM events_fts WHERE events_fts MATCH 'denied'`).Scan(&hit); err != nil || hit != want.Seq {
		t.Errorf("full-text search for 'denied' = %d, %v; want %d", hit, err, want.Seq)
	}

	// A second batch in the same minute adds to the rollups.
	if err := s.Commit(ctx, []event.Event{{Log: "security", Provider: "security", Time: at, Level: event.Warning, Msg: "third"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.r.QueryRow(`SELECT n FROM rollup WHERE minute = ? AND log_id = (SELECT id FROM names WHERE kind = 'log' AND name = 'security') AND level = 3`,
		at.UnixNano()/int64(time.Minute)).Scan(&n); err != nil || n != 2 {
		t.Errorf("rollup(minute, security, warning) = %d, %v; want 2", n, err)
	}
	if err := s.r.QueryRow(`SELECT count(*) FROM names WHERE kind = 'log' AND name = 'security'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("security interned %d times, %v", n, err)
	}
}

// A failed commit leaves no rows, no Seq, and no names in the cache that the
// database doesn't hold.
func TestFailedCommitLeavesNothing(t *testing.T) {
	s := open(t, tempDB(t), "")
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	batch := []event.Event{ev("ghost", "never")}
	if err := s.Commit(cctx, batch); err == nil {
		t.Fatal("Commit with a cancelled context succeeded")
	}
	if batch[0].Seq != 0 || len(s.names) != 0 {
		t.Errorf("after a failed commit: Seq %d, cached names %v", batch[0].Seq, s.names)
	}
	if err := s.Commit(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get(ctx, batch[0].Seq); err != nil || got.Log != "ghost" {
		t.Errorf("Get = %+v, %v", got, err)
	}
}

// Each subscriber has its own channel, woken after a local commit at once and
// after another process's commit by the poll.
func TestSubscribersWake(t *testing.T) {
	path := tempDB(t)
	s, other := open(t, path, ""), open(t, path, "")
	a, cancelA := s.Subscribe()
	b, cancelB := s.Subscribe()
	defer cancelA()
	if err := s.Commit(ctx, []event.Event{ev("x", "local")}); err != nil {
		t.Fatal(err)
	}
	for name, ch := range map[string]<-chan struct{}{"a": a, "b": b} {
		select {
		case <-ch:
		default:
			t.Errorf("subscriber %s was not woken by a local commit", name)
		}
	}
	cancelB()
	if err := other.Commit(ctx, []event.Event{ev("x", "remote")}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a:
	case <-time.After(3 * pollEvery):
		t.Fatal("no wake-up for another process's commit")
	}
	select {
	case <-b:
		t.Error("a cancelled subscriber was woken")
	default:
	}
}

// Exactly one process holds a lease; renewing keeps it; once it expires
// another takes over.
func TestLease(t *testing.T) {
	path := tempDB(t)
	const ttl = 300 * time.Millisecond
	stores := make([]*Store, 6)
	for i := range stores {
		stores[i] = open(t, path, "0000000000000000000000000000000"+string(rune('a'+i)))
	}
	var wg sync.WaitGroup
	held := make([]bool, len(stores))
	for i, s := range stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.TryLease(ctx, "housekeeper", ttl)
			if err != nil {
				t.Error(err)
			}
			held[i] = ok
		}()
	}
	wg.Wait()
	holder := -1
	for i, ok := range held {
		if ok {
			if holder >= 0 {
				t.Fatalf("stores %d and %d both hold the lease", holder, i)
			}
			holder = i
		}
	}
	if holder < 0 {
		t.Fatal("nobody took the lease")
	}
	next := stores[(holder+1)%len(stores)]
	if ok, _ := stores[holder].TryLease(ctx, "housekeeper", ttl); !ok {
		t.Error("the holder could not renew")
	}
	if ok, _ := next.TryLease(ctx, "housekeeper", ttl); ok {
		t.Error("another process took an unexpired lease")
	}
	if ok, _ := next.TryLease(ctx, "other", ttl); !ok {
		t.Error("leases are not independent by name")
	}
	time.Sleep(ttl + 50*time.Millisecond)
	if ok, _ := next.TryLease(ctx, "housekeeper", ttl); !ok {
		t.Error("an expired lease was not taken over")
	}
	if ok, _ := stores[holder].TryLease(ctx, "housekeeper", ttl); ok {
		t.Error("the old holder kept the lease after another took it over")
	}
	if _, err := open(t, path, "").TryLease(ctx, "housekeeper", ttl); err == nil {
		t.Error("TryLease without a boot ID succeeded")
	}
}

// A restored store gets a new ID, so a cursor from before the restore
// restarts instead of skipping the rows committed after it.
func TestReidentifyRestartsCursors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logbook.db")
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	old := s.StoreID()
	_ = s.Close()
	if err := Reidentify(path); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.StoreID() == old {
		t.Fatal("the store kept its ID")
	}
	if _, _, restarted, err := s.Since(context.Background(), Cursor{StoreID: old, Seq: 5}, 10); err != nil || !restarted {
		t.Fatalf("a pre-restore cursor: restarted=%v err=%v", restarted, err)
	}
}
