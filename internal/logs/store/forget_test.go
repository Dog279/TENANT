package store

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/logs/event"
)

// secretWord is what a forgotten session said; after Forget no byte of the
// database or its WAL holds it.
const secretWord = "zanzibarstreet"

func inSession(e event.Event, session, trace string) event.Event {
	e.Corr.SessionID, e.Corr.TraceID = session, trace
	return e
}

func fileBytes(t *testing.T, path string) []byte {
	t.Helper()
	var all []byte
	for _, p := range []string{path, path + "-wal"} {
		b, err := os.ReadFile(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		all = append(all, b...)
	}
	return all
}

// Forgetting a session deletes its events and its runs' events from every log
// but security, where the record is tombstoned; the full-text index, the
// rollups and the log sizes follow, and no copy of the text stays in the
// database or the WAL.
func TestForgetSessionFollowsTheRecipe(t *testing.T) {
	path := tempDB(t)
	s := open(t, path, "")
	hexTrace := strings.Repeat("ab", 16)
	sensitive := ev("turns", "tool call web_search", "args", "near 221B "+secretWord)
	sensitive.Data[0].Sensitive = true
	batch := []event.Event{
		inSession(sensitive, "sess_a", ""),
		inSession(ev("turns", "reply mentions "+secretWord), "", "run-1"),
		inSession(ev("model", "call for "+secretWord), "", hexTrace),
		inSession(ev("security", "approval granted for "+secretWord, "detail", secretWord), "sess_a", ""),
		inSession(ev("turns", "keep this turn"), "sess_b", "run-9"),
		inSession(ev("security", "keep this record"), "sess_b", ""),
	}
	if err := s.Commit(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(fileBytes(t, path), []byte(secretWord)) {
		t.Fatal("fixture: the word is not in the files before the forget")
	}
	m := Match{Sessions: map[string]bool{"sess_a": true}, Traces: map[string]bool{"run-1": true, hexTrace: true}}

	var got []event.Event
	if err := s.Matching(ctx, m, func(e event.Event) error { got = append(got, e); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0].Seq != batch[0].Seq || got[3].Seq != batch[3].Seq {
		t.Fatalf("Matching = %d events, want the first four in order: %+v", len(got), got)
	}
	if got[0].Data[0].Value != "near 221B "+secretWord || !got[0].Data[0].Sensitive {
		t.Errorf("Matching masked or lost a sensitive field: %+v", got[0].Data)
	}

	res, err := s.Forget(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted["turns"] != 2 || res.Deleted["model"] != 1 || res.Tombstoned != 1 || res.Busy {
		t.Fatalf("Forget = %+v", res)
	}
	if b := fileBytes(t, path); bytes.Contains(b, []byte(secretWord)) {
		t.Fatalf("%q is still in logbook.db or its WAL after the forget", secretWord)
	}
	for _, e := range batch[:3] {
		if _, err := s.Get(ctx, e.Seq); err != ErrNotFound {
			t.Errorf("event %d (%q) survived: %v", e.Seq, e.Msg, err)
		}
	}
	tomb, err := s.Get(ctx, batch[3].Seq)
	if err != nil || tomb.Msg != Tombstone || tomb.Data != nil || tomb.Corr.SessionID != "sess_a" {
		t.Errorf("security record after the forget = %+v, %v; want a tombstone", tomb, err)
	}
	for _, e := range batch[4:] {
		if kept, err := s.Get(ctx, e.Seq); err != nil || kept.Msg != e.Msg {
			t.Errorf("event %q of another session: %+v, %v", e.Msg, kept, err)
		}
	}
	var hits int
	if err := s.r.QueryRow(`SELECT count(*) FROM events_fts WHERE events_fts MATCH ?`, secretWord).Scan(&hits); err != nil || hits != 0 {
		t.Errorf("full-text search still finds the word: %d, %v", hits, err)
	}
	if err := s.r.QueryRow(`SELECT count(*) FROM events_fts WHERE events_fts MATCH 'keep'`).Scan(&hits); err != nil || hits != 2 {
		t.Errorf("full-text search lost the kept events: %d, %v", hits, err)
	}
	var turns, turnBytes int64
	if err := s.r.QueryRow(`SELECT coalesce(sum(r.n), 0), (SELECT l.bytes FROM logs l WHERE l.log_id = n.id)
		FROM names n LEFT JOIN rollup r ON r.log_id = n.id WHERE n.kind = 'log' AND n.name = 'turns'`).Scan(&turns, &turnBytes); err != nil {
		t.Fatal(err)
	}
	var keptBytes int64
	_ = s.r.QueryRow(`SELECT bytes FROM events WHERE seq = ?`, batch[4].Seq).Scan(&keptBytes)
	if turns != 1 || turnBytes != keptBytes {
		t.Errorf("turns rollup %d and bytes %d after the forget, want 1 and %d", turns, turnBytes, keptBytes)
	}

	again, err := s.Forget(ctx, m)
	if err != nil || len(again.Deleted) != 0 || again.Tombstoned != 0 {
		t.Errorf("a second forget changed something: %+v, %v", again, err)
	}
	if err := s.Commit(ctx, []event.Event{ev("turns", "after")}); err != nil {
		t.Errorf("commit after the forget: %v", err)
	}
}

func writeTestArchive(t *testing.T, path string, lines ...string) {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	for _, l := range lines {
		zw.Write([]byte(l + "\n"))
	}
	zw.Close()
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readArchiveText(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if _, err := b.ReadFrom(zr); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// Archives are rewritten too: another log's events go, a security record
// becomes a tombstone that keeps its hash, a closing line stays, and a file
// left empty is removed. A dry run only counts.
func TestForgetArchivedRewritesTheArchives(t *testing.T) {
	dir := t.TempDir()
	turns := filepath.Join(dir, "turns-1-3.ndjson.gz")
	security := filepath.Join(dir, "security-1-2.ndjson.gz")
	only := filepath.Join(dir, "model-5-5.ndjson.gz")
	writeTestArchive(t, turns,
		`{"seq":1,"session_id":"sess_a","msg":"`+secretWord+`"}`,
		`{"seq":2,"session_id":"sess_b","msg":"keep"}`,
		`{"seq":3,"trace_id":"run-1","msg":"`+secretWord+` again"}`)
	writeTestArchive(t, security,
		`{"rec":1,"session_id":"sess_a","msg":"`+secretWord+`","data":{"d":"`+secretWord+`"},"dtypes":"s","salt":"c2FsdA==","data_hash":"aGFzaA==","hash":"aA=="}`,
		`{"rec":2,"session_id":"sess_b","msg":"other"}`,
		`{"last_rec":2,"last_hash":"aA=="}`)
	writeTestArchive(t, only, `{"seq":5,"trace_id":"run-1","msg":"x"}`)
	m := Match{Sessions: map[string]bool{"sess_a": true}, Traces: map[string]bool{"run-1": true}}

	before := readArchiveText(t, turns)
	d, tb, err := ForgetArchived(dir, m, false)
	if err != nil || d != 3 || tb != 1 {
		t.Fatalf("dry run = %d dropped, %d tombstoned, %v; want 3, 1", d, tb, err)
	}
	if readArchiveText(t, turns) != before {
		t.Fatal("a dry run changed an archive")
	}
	if d, tb, err = ForgetArchived(dir, m, true); err != nil || d != 3 || tb != 1 {
		t.Fatalf("apply = %d dropped, %d tombstoned, %v", d, tb, err)
	}
	if got := readArchiveText(t, turns); got != `{"seq":2,"session_id":"sess_b","msg":"keep"}`+"\n" {
		t.Errorf("turns archive after the forget:\n%s", got)
	}
	sec := strings.Split(strings.TrimSpace(readArchiveText(t, security)), "\n")
	var rec map[string]any
	if len(sec) != 3 || json.Unmarshal([]byte(sec[0]), &rec) != nil {
		t.Fatalf("security archive after the forget:\n%s", strings.Join(sec, "\n"))
	}
	if rec["msg"] != Tombstone || rec["data"] != nil || rec["salt"] != nil || rec["dtypes"] != nil || rec["data_hash"] != "aGFzaA==" || rec["hash"] != "aA==" {
		t.Errorf("tombstoned record = %v", rec)
	}
	if !strings.Contains(sec[1], `"other"`) || sec[2] != `{"last_rec":2,"last_hash":"aA=="}` {
		t.Errorf("the rest of the security archive changed:\n%s", strings.Join(sec[1:], "\n"))
	}
	if _, err := os.Stat(only); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an archive left with no events is still there: %v", err)
	}
	if d, tb, err = ForgetArchived(dir, m, true); err != nil || d+tb != 0 {
		t.Errorf("a second forget = %d, %d, %v; want nothing", d, tb, err)
	}
}
