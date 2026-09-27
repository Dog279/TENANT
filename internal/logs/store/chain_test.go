package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"tenant/internal/logs/event"
)

// secRecords commits n security records, two to a batch beside a turns
// event (a hundred when there are many); every seventh belongs to session
// sess_z.
func secRecords(t testing.TB, s *Store, n int) {
	t.Helper()
	per := 2
	if n > 100 {
		per = 100
	}
	for i := 0; i < n; i += per {
		batch := []event.Event{ev("turns", "turn "+strconv.Itoa(i))}
		for j := i; j < min(i+per, n); j++ {
			e := ev(SecurityLog, "audit "+strconv.Itoa(j), "n", int64(j))
			if j%7 == 0 {
				e.Corr.SessionID = "sess_z"
			}
			batch = append(batch, e)
		}
		if err := s.Commit(ctx, batch); err != nil {
			t.Fatal(err)
		}
	}
}

func verify(t testing.TB, s *Store, opts VerifyOptions) Verification {
	t.Helper()
	v, err := s.Verify(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func chainHeadOf(t testing.TB, s *Store) Head {
	t.Helper()
	h, err := s.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func run(t testing.TB, s *Store, qs ...string) {
	t.Helper()
	for _, q := range qs {
		if _, err := s.w.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func wantBreak(t testing.TB, v Verification, rec int64, where, why string) {
	t.Helper()
	if b := v.Break; b == nil || b.Rec != rec || !strings.HasPrefix(b.Where, where) || !strings.Contains(b.Reason, why) {
		t.Errorf("break %v; want record %d in %s, %q", b, rec, where, why)
	}
}

// Every security record is linked as it commits: record numbers without
// gaps, the head in meta, and a chain that verifies with the key that wrote
// it and with no other. Every field of the record is covered.
func TestEveryLinkIsKeyed(t *testing.T) {
	path := tempDB(t)
	s := open(t, path, "")
	secRecords(t, s, 9)
	full := event.Event{Log: SecurityLog, Provider: "tenant.approval", ID: 4103, Version: 2, Level: event.Warning, Channel: event.Diagnostic,
		Task: "Approval", Opcode: event.OpStop, Keywords: []string{"audit.failure", "config"}, Msg: "Approval ap-1 denied",
		Data:  []event.Field{{Key: "args", Value: "rm -rf /", Sensitive: true}, {Key: "payload", Value: json.RawMessage(`{"a":[1,2]}`)}},
		Corr:  event.Correlation{TraceID: strings.Repeat("ab", 16), ActivityID: "act", RelatedID: "rel", SessionID: "s", ApprovalID: "ap-1", ToolCallID: "c"},
		Actor: "operator", Surface: "dashboard", Proc: event.Process{PID: 42, Role: "serve", BootID: strings.Repeat("0f", 16), Host: "mac", Version: "v1"}}
	if err := s.Commit(ctx, []event.Event{full}); err != nil {
		t.Fatal(err)
	}
	h := chainHeadOf(t, s)
	var recs string
	if err := s.r.QueryRow(`SELECT group_concat(rec) FROM (SELECT rec FROM events WHERE rec IS NOT NULL ORDER BY rec)`).Scan(&recs); err != nil ||
		recs != "1,2,3,4,5,6,7,8,9,10" || h.Rec != 10 {
		t.Fatalf("records %q (%v), head %+v", recs, err, h)
	}
	if n := count(t, s, `SELECT count(*) FROM events e JOIN names n ON n.id = e.log_id WHERE (n.name = 'security') != (e.rec IS NOT NULL)`); n != 0 {
		t.Errorf("%d rows are chained outside security or unchained in it", n)
	}
	v := verify(t, s, VerifyOptions{Anchor: &h})
	if v.Break != nil || v.Records != 10 || v.Head != h || len(v.Notes) != 0 {
		t.Errorf("verify = %+v", v)
	}
	run(t, s, `CREATE TABLE saved AS SELECT * FROM events WHERE rec = 10`)
	for _, col := range []string{"actor", "surface", "keywords", "trace_id", "activity_id", "related_id", "session_id", "approval_id",
		"tool_call_id", "pid", "role", "boot_id", "host", "proc_version", "version", "channel", "event_id", "time_ns", "level", "task_id", "opcode"} {
		t.Run(col, func(t *testing.T) {
			run(t, s, `UPDATE events SET `+col+` = CASE typeof(`+col+`) WHEN 'null' THEN 1 WHEN 'integer' THEN `+col+` + 1 END WHERE rec = 10`)
			wantBreak(t, verify(t, s, VerifyOptions{}), 10, "logbook.db", "hash does not match")
			run(t, s, `UPDATE events SET `+col+` = (SELECT `+col+` FROM saved) WHERE rec = 10`)
		})
	}
	if v := verify(t, s, VerifyOptions{}); v.Break != nil {
		t.Fatalf("after putting every column back: %v", v.Break)
	}

	other, err := Open(path, Options{ChainKey: []byte("another key")})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	v, err = other.Verify(ctx, VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wantBreak(t, v, 1, "logbook.db", "hash does not match")
}

// Without the key no security record is written: the whole batch is
// refused, and the Audit caller gets ErrNoChainKey. Other logs still commit.
func TestNoKeyNoSecurityRecord(t *testing.T) {
	s, err := Open(tempDB(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Commit(ctx, []event.Event{ev("turns", "a"), ev(SecurityLog, "b")}); !errors.Is(err, ErrNoChainKey) {
		t.Fatalf("commit without a key = %v, want ErrNoChainKey", err)
	}
	if n := count(t, s, `SELECT count(*) FROM events`); n != 0 {
		t.Errorf("the refused batch left %d rows", n)
	}
	if err := s.Commit(ctx, []event.Event{ev("turns", "c")}); err != nil {
		t.Errorf("a batch with no security record: %v", err)
	}
	if _, err := s.Verify(ctx, VerifyOptions{}); !errors.Is(err, ErrNoChainKey) {
		t.Errorf("verify without a key = %v", err)
	}
}

// forge inserts a copy of record 3 as record rec with a message of its own,
// salted and linked to record rec-1 as the chain would, but with key.
func forge(t *testing.T, s *Store, rec int64, key []byte) {
	t.Helper()
	var raw, prev []byte
	var l line
	if err := s.r.QueryRow(archiveSelect+` WHERE e.rec = 3`).Scan(new(int64), new(int64), &raw); err != nil || json.Unmarshal(raw, &l) != nil {
		t.Fatal(err)
	}
	if err := s.r.QueryRow(`SELECT hash FROM events WHERE rec = ?`, rec-1).Scan(&prev); err != nil {
		t.Fatal(err)
	}
	salt := make([]byte, saltSize)
	dh := dataHash(salt, "forged", l.Data, l.Dtypes)
	h := link(key, prev, l.header(rec), dh)
	const cols = `log_id, provider_id, event_id, version, time_ns, level, channel, task_id, opcode, keywords, %s, data, dtypes,
		trace_id, activity_id, related_id, session_id, approval_id, tool_call_id, actor, surface, pid, role, boot_id, host, proc_version, bytes`
	q := `INSERT INTO events (` + strings.Replace(cols, "%s", "msg", 1) + `, rec, salt, data_hash, hash)
		SELECT ` + strings.Replace(cols, "%s", "'forged'", 1) + `, ?, ?, ?, ? FROM events WHERE rec = 3`
	if _, err := s.w.Exec(q, rec, salt, dh, h); err != nil {
		t.Fatal(err)
	}
}

// Without the key, an edit, a deletion, a reordering or a forged record is
// found, at its record number.
func TestTamperingIsDetected(t *testing.T) {
	notKey := []byte("not the key")
	for _, tc := range []struct {
		name   string
		tamper func(t *testing.T, s *Store)
		rec    int64
		why    string
	}{
		{"edited message", func(t *testing.T, s *Store) { run(t, s, `UPDATE events SET msg = 'nothing to see' WHERE rec = 4`) }, 4, "changed"},
		{"edited data", func(t *testing.T, s *Store) { run(t, s, `UPDATE events SET data = '{"n":40}' WHERE rec = 4`) }, 4, "changed"},
		{"edited field types", func(t *testing.T, s *Store) { run(t, s, `UPDATE events SET dtypes = 'I' WHERE rec = 4`) }, 4, "changed"},
		{"edited actor", func(t *testing.T, s *Store) { run(t, s, `UPDATE events SET actor = 'mallory' WHERE rec = 4`) }, 4, "hash does not match"},
		{"moved to another log", func(t *testing.T, s *Store) {
			run(t, s, `UPDATE events SET log_id = (SELECT id FROM names WHERE kind = 'log' AND name = 'turns') WHERE rec = 4`)
		}, 4, "hash does not match"},
		{"unchained", func(t *testing.T, s *Store) { run(t, s, `UPDATE events SET rec = NULL WHERE rec = 4`) }, 1, "never chained"},
		{"deleted", func(t *testing.T, s *Store) { run(t, s, `DELETE FROM events WHERE rec = 4`) }, 4, "records 4 to 4 are missing"},
		{"reordered", func(t *testing.T, s *Store) {
			run(t, s, `UPDATE events SET rec = -5 WHERE rec = 5`, `UPDATE events SET rec = 5 WHERE rec = 4`, `UPDATE events SET rec = 4 WHERE rec = -5`)
		}, 4, "hash does not match"},
		{"salt removed", func(t *testing.T, s *Store) { run(t, s, `UPDATE events SET salt = NULL, msg = 'edited' WHERE rec = 4`) }, 4, "not a privacy tombstone"},
		{"unreadable", func(t *testing.T, s *Store) { run(t, s, `UPDATE events SET msg = x'00ff' WHERE rec = 4`) }, 4, "can't be read"},
		{"keywords garbled", func(t *testing.T, s *Store) { run(t, s, `UPDATE events SET keywords = '{' WHERE rec = 4`) }, 4, "hash does not match"},
		{"forged at the end", func(t *testing.T, s *Store) { forge(t, s, 11, notKey) }, 11, "hash does not match"},
		{"forged in the middle", func(t *testing.T, s *Store) {
			run(t, s, `UPDATE events SET rec = rec + 100 WHERE rec >= 6`, `UPDATE events SET rec = rec - 99 WHERE rec >= 106`)
			forge(t, s, 6, notKey)
		}, 6, "hash does not match"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := open(t, tempDB(t), "")
			secRecords(t, s, 10)
			tc.tamper(t, s)
			wantBreak(t, verify(t, s, VerifyOptions{}), tc.rec, "logbook.db", tc.why)
		})
	}
	// The control: the same forgery with the key verifies, so it is the key
	// the tests above lack, not an encoding the forger got wrong.
	s := open(t, tempDB(t), "")
	secRecords(t, s, 10)
	forge(t, s, 11, testKey)
	if v := verify(t, s, VerifyOptions{}); v.Break != nil || v.Records != 11 {
		t.Errorf("a record linked with the key: %+v", v)
	}
}

// replaceDB puts the database file at from in place of s's, as an operator
// restoring an older copy by hand would, and reopens it.
func replaceDB(t *testing.T, s *Store, path, from string) *Store {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return open(t, path, "")
}

// A truncated database, or an older copy put back, is a valid prefix of the
// chain: the anchor finds it, and so does the anchor task's Contains, which
// then refuses to move the anchor on. An older copy written on afterwards
// forks the chain, and the anchor finds that too.
func TestAnchorFindsTruncationAndRollback(t *testing.T) {
	path := tempDB(t)
	s := open(t, path, "")
	secRecords(t, s, 5)
	older := filepath.Join(t.TempDir(), "older.db")
	run(t, s, `VACUUM INTO '`+older+`'`)
	secRecords(t, s, 5)
	anchor := chainHeadOf(t, s)
	if ok, err := s.Contains(ctx, anchor); !ok || err != nil {
		t.Fatalf("contains its own head: %v %v", ok, err)
	}

	run(t, s, `DELETE FROM events WHERE rec >= 9`)
	if v := verify(t, s, VerifyOptions{}); v.Break != nil || len(v.Notes) != 1 {
		t.Errorf("a truncated chain without an anchor: %+v; want no break and a note", v)
	}
	wantBreak(t, verify(t, s, VerifyOptions{Anchor: &anchor}), 9, "anchor", "truncated or rolled back")
	if ok, err := s.Contains(ctx, anchor); ok || err != nil {
		t.Errorf("a truncated chain contains the anchor: %v %v", ok, err)
	}

	s = replaceDB(t, s, path, older)
	wantBreak(t, verify(t, s, VerifyOptions{Anchor: &anchor}), 6, "anchor", "truncated or rolled back")
	secRecords(t, s, 6)
	wantBreak(t, verify(t, s, VerifyOptions{Witness: &anchor}), 10, "witness", "differs")
	if ok, err := s.Contains(ctx, anchor); ok || err != nil {
		t.Errorf("a forked chain contains the anchor: %v %v", ok, err)
	}
}

// A legitimate restore writes a re-anchor record, so the restored chain
// verifies, reports the restore, and accepts a witness still naming the
// head the restore replaced.
func TestReanchorReadsAsARestore(t *testing.T) {
	path := tempDB(t)
	s := open(t, path, "")
	secRecords(t, s, 5)
	older := filepath.Join(t.TempDir(), "older.db")
	run(t, s, `VACUUM INTO '`+older+`'`)
	restored := chainHeadOf(t, s)
	secRecords(t, s, 5)
	anchor := chainHeadOf(t, s)
	s = replaceDB(t, s, path, older)
	h, err := s.Reanchor(ctx, anchor, "tenant-backup-x.tar.gz")
	if err != nil || h.Rec != 6 {
		t.Fatalf("reanchor = %+v, %v", h, err)
	}
	v := verify(t, s, VerifyOptions{Anchor: &h, Witness: &anchor})
	want := []Restore{{Rec: 6, Backup: "tenant-backup-x.tar.gz", Restored: restored, Replaced: anchor}}
	if v.Break != nil || !slices.Equal(v.Restores, want) || len(v.Notes) != 1 || !strings.Contains(v.Notes[0], "restore at record 6 replaced") {
		t.Errorf("verify after a restore = %+v", v)
	}
	// The anchor itself moves with the restore: one left behind is a break,
	// so a re-anchor record can't excuse a rollback the anchor caught.
	wantBreak(t, verify(t, s, VerifyOptions{Anchor: &anchor}), 7, "anchor", "truncated or rolled back")
	secRecords(t, s, 2)
	if v := verify(t, s, VerifyOptions{Anchor: &h}); v.Break != nil || v.Records != 8 {
		t.Errorf("the chain after the re-anchor: %+v", v)
	}
}

// A privacy tombstone keeps data_hash, so the chain still verifies; it can't
// be used to rewrite a record, since a tombstone's message is fixed.
func TestTombstoneStillVerifies(t *testing.T) {
	s := open(t, tempDB(t), "")
	secRecords(t, s, 10)
	res, err := s.Forget(ctx, Match{Sessions: map[string]bool{"sess_z": true}})
	if err != nil || res.Tombstoned != 2 {
		t.Fatalf("forget = %+v, %v", res, err)
	}
	if v := verify(t, s, VerifyOptions{}); v.Break != nil || v.Records != 10 || v.Tombstones != 2 {
		t.Errorf("verify after a forget = %+v", v)
	}
	run(t, s, `UPDATE events SET msg = 'rewritten' WHERE rec = 8`)
	wantBreak(t, verify(t, s, VerifyOptions{}), 8, "logbook.db", "not a privacy tombstone")
}

// rewriteArchive rewrites an archive's lines through fn.
func rewriteArchive(t *testing.T, path string, fn func([][]byte) [][]byte) {
	t.Helper()
	if err := writeArchive(path, fn(readArchive(t, path))); err != nil {
		t.Fatal(err)
	}
}

// The Security log's archives carry the chain on: verification walks them,
// then the database. An archive written again over rows still in the
// database is read as the same records twice, and a changed copy is caught
// wherever it is. A privacy rewrite of the archives verifies; an edited or
// cut archive doesn't.
func TestArchivesCarryTheChain(t *testing.T) {
	path := tempDB(t)
	s := open(t, path, "")
	secRecords(t, s, 1200)
	b := DefaultBudget()
	b.Logs[SecurityLog] = Policy{MaxBytes: 64 << 10, MaxAge: 365 * day, OnFull: Archive}
	rep, err := s.Retain(ctx, b, time.Now())
	if err != nil || len(rep.Archived) == 0 || rep.Deleted[SecurityLog] == 0 {
		t.Fatalf("retain = %+v, %v", rep, err)
	}
	h := chainHeadOf(t, s)
	v := verify(t, s, VerifyOptions{Anchor: &h})
	if v.Break != nil || v.Records != 1200 || v.Archives != len(rep.Archived) {
		t.Fatalf("verify across %v = %+v", rep.Archived, v)
	}
	dir := filepath.Join(filepath.Dir(path), "archive")
	if ok, err := s.Contains(ctx, Head{Rec: 1, Hash: first(t, dir)}); !ok || err != nil {
		t.Errorf("Contains misses an archived record: %v %v", ok, err)
	}

	// A pass that archived the oldest rows still in the database, then stopped.
	low := count(t, s, `SELECT min(rec) FROM events`)
	rows, err := s.r.Query(archiveSelect+` WHERE e.rec BETWEEN ? AND ? ORDER BY e.rec`, low, low+99)
	if err != nil {
		t.Fatal(err)
	}
	var seqs []int64
	var lines [][]byte
	for rows.Next() {
		var seq int64
		var l []byte
		if err := rows.Scan(&seq, new(int64), &l); err != nil {
			t.Fatal(err)
		}
		seqs, lines = append(seqs, seq), append(lines, l)
	}
	rows.Close()
	r := &retention{s: s, rep: &Report{}, archiveDir: dir}
	if err := r.archive(SecurityLog, seqs, lines); err != nil {
		t.Fatal(err)
	}
	if v := verify(t, s, VerifyOptions{Anchor: &h}); v.Break != nil || v.Records != 1200 {
		t.Errorf("verify with an overlapping archive = %+v", v)
	}
	at := strconv.FormatInt(low+10, 10)
	run(t, s, `UPDATE events SET msg = 'changed' WHERE rec = `+at)
	wantBreak(t, verify(t, s, VerifyOptions{}), low+10, "logbook.db", "changed")
	run(t, s, `UPDATE events SET msg = 'audit ' || (rec - 1) WHERE rec = `+at)

	// A privacy request tombstones the records in the archives too.
	m := Match{Sessions: map[string]bool{"sess_z": true}}
	if _, _, err := ForgetArchived(dir, m, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Forget(ctx, m); err != nil {
		t.Fatal(err)
	}
	if v := verify(t, s, VerifyOptions{Anchor: &h}); v.Break != nil || v.Tombstones != 172 {
		t.Errorf("verify after a forget = %+v; want 172 tombstones", v)
	}

	oldest := filepath.Join(dir, rep.Archived[0])
	saved := readArchive(t, oldest)
	rewriteArchive(t, oldest, func(ls [][]byte) [][]byte {
		ls[5] = bytes.Replace(ls[5], []byte(`"audit 5"`), []byte(`"audit five"`), 1)
		return ls
	})
	wantBreak(t, verify(t, s, VerifyOptions{}), 6, "archive "+rep.Archived[0], "changed")
	rewriteArchive(t, oldest, func([][]byte) [][]byte { return saved[:len(saved)-1] })
	wantBreak(t, verify(t, s, VerifyOptions{}), int64(len(saved)), "archive "+rep.Archived[0], "cut short")
}

// first is the hash of record 1, from its archive.
func first(t *testing.T, dir string) string {
	t.Helper()
	files, err := securityArchives(dir)
	if err != nil || len(files) == 0 {
		t.Fatal(files, err)
	}
	var l line
	if err := json.Unmarshal(readArchive(t, files[0].path)[0], &l); err != nil || l.rec() != 1 {
		t.Fatalf("first archived record %+v, %v", l, err)
	}
	return l.Hash
}

// Records past their age leave the Security log as a prefix of the chain,
// up to the first still young, so the archives and the database stay
// contiguous whatever order the records' times are in.
func TestSecurityAgesOutAsAPrefix(t *testing.T) {
	s := open(t, tempDB(t), "")
	now := time.Now()
	for _, age := range []int{400, 400, 1, 400, 400} {
		e := ev(SecurityLog, "aged")
		e.Time = now.Add(-time.Duration(age) * day)
		if err := s.Commit(ctx, []event.Event{e}); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := s.Retain(ctx, DefaultBudget(), now)
	if err != nil || rep.Deleted[SecurityLog] != 2 || !slices.Equal(rep.Archived, []string{"security-1-2.ndjson.gz"}) {
		t.Fatalf("retain = %+v, %v; want records 1 and 2 archived", rep, err)
	}
	if v := verify(t, s, VerifyOptions{}); v.Break != nil || v.Records != 5 {
		t.Errorf("verify = %+v", v)
	}
}

// An audit export bundle is self-describing and verifies offline with the
// key, whole or as a stretch from the middle; a wrong key, an edit or a cut
// is found.
func TestExportBundleVerifiesOffline(t *testing.T) {
	s := open(t, tempDB(t), "")
	base := time.Now().Add(-time.Hour).UTC()
	for i := range 10 {
		e := ev(SecurityLog, "audit "+strconv.Itoa(i))
		e.Time = base.Add(time.Duration(i) * time.Minute)
		if err := s.Commit(ctx, []event.Event{e}); err != nil {
			t.Fatal(err)
		}
	}
	h := chainHeadOf(t, s)
	var buf bytes.Buffer
	if err := s.ExportSecurity(ctx, &buf, ExportOptions{Host: "mac", Version: "v1", Anchor: &h}); err != nil {
		t.Fatal(err)
	}
	bundle := buf.Bytes()
	hdr, v, err := VerifyBundle(bytes.NewReader(bundle), testKey)
	if err != nil || v.Break != nil || v.Records != 10 || v.Head != h || hdr.Host != "mac" || hdr.Schema != strconv.Itoa(len(migrations)) || hdr.Prev.Rec != 0 {
		t.Fatalf("bundle %+v: %+v, %v", hdr, v, err)
	}
	_, v, _ = VerifyBundle(bytes.NewReader(bundle), []byte("wrong"))
	wantBreak(t, v, 1, "bundle", "hash does not match")
	_, v, _ = VerifyBundle(bytes.NewReader(bytes.Replace(bundle, []byte(`"audit 3"`), []byte(`"audit three"`), 1)), testKey)
	wantBreak(t, v, 4, "bundle", "changed")
	cut := bundle[:bytes.LastIndexByte(bundle[:len(bundle)-1], '\n')+1]
	_, v, _ = VerifyBundle(bytes.NewReader(cut), testKey)
	wantBreak(t, v, 11, "bundle", "cut short")

	buf.Reset()
	if err := s.ExportSecurity(ctx, &buf, ExportOptions{Since: base.Add(270 * time.Second), Until: base.Add(510 * time.Second), Anchor: &h}); err != nil {
		t.Fatal(err)
	}
	hdr, v, err = VerifyBundle(&buf, testKey)
	if err != nil || v.Break != nil || v.Records != 4 || hdr.Prev.Rec != 5 || v.Head.Rec != 9 || len(v.Notes) != 1 {
		t.Errorf("a stretch from the middle %+v: %+v, %v", hdr, v, err)
	}
}

// §12.3's budget for a security write: p99 at most 25 ms idle, each record
// its own commit. Opt in with TENANT_LOGBOOK_PERF=1.
func TestSecurityWriteLatency(t *testing.T) {
	if os.Getenv("TENANT_LOGBOOK_PERF") == "" {
		t.Skip("set TENANT_LOGBOOK_PERF=1 to measure security writes")
	}
	s := open(t, tempDB(t), "")
	took := make([]time.Duration, 300)
	for i := range took {
		start := time.Now()
		if err := s.Commit(ctx, []event.Event{ev(SecurityLog, "audit", "n", int64(i))}); err != nil {
			t.Fatal(err)
		}
		took[i] = time.Since(start)
	}
	slices.Sort(took)
	p99 := took[len(took)*99/100]
	t.Logf("security write: p50 %v, p99 %v", took[len(took)/2], p99)
	if p99 > 25*time.Millisecond {
		t.Errorf("security write p99 %v, over the 25 ms budget", p99)
	}
}
