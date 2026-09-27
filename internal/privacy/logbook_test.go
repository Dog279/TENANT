package privacy

import (
	"bytes"
	"compress/gzip"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"tenant/internal/logs"
	"tenant/internal/logs/event"
	"tenant/internal/logs/store"
)

// seedLogbook gives the fixture a log book: sess_a's events (by session, and
// by its runs' traces), one in the security log, sess_b's and sess_c's, and a
// system event; their text lines; and an archive holding sess_a and sess_b.
func (fx *fixture) seedLogbook(t *testing.T) {
	t.Helper()
	root := fx.d.logsDir()
	st, err := store.Open(filepath.Join(root, "logbook.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	at := time.Date(2026, 5, 10, 9, 0, 0, 0, time.UTC)
	e := func(log, msg, session, trace string, data ...event.Field) event.Event {
		return event.Event{Log: log, Provider: log, Time: at, Level: event.Info, Msg: msg, Data: data,
			Corr: event.Correlation{SessionID: session, TraceID: trace}}
	}
	evs := []event.Event{
		e("turns", "turn started", "sess_a", "run-1", event.Field{Key: "prompt", Value: "my address is 221B Baker Street", Sensitive: true}),
		e("model", "model call", "", "run-2"),
		e("security", "approval granted: os_exec near Baker Street", "sess_a", ""),
		e("turns", "turn in another session", "sess_b", "run-3"),
		e("turns", "researcher turn", "sess_c", "run-4"),
		e("system", "hub started", "", ""),
	}
	if err := st.Commit(context.Background(), evs); err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		p := filepath.Join(root, ev.Log, "2026-05-10.log")
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(logs.EntryFromEvent(ev).Line() + "\n")
		f.Close()
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte(`{"seq":1,"session_id":"sess_a","msg":"archived Baker Street"}` + "\n" + `{"seq":2,"session_id":"sess_b","msg":"kept"}` + "\n"))
	zw.Close()
	if err := os.MkdirAll(filepath.Join(root, "archive"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "archive", "turns-1-2.ndjson.gz"), gz.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (fx *fixture) logbookMsgs(t *testing.T) []string {
	t.Helper()
	st, err := store.Open(filepath.Join(fx.d.logsDir(), "logbook.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	evs, _, _, err := st.Since(context.Background(), store.Cursor{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		out = append(out, e.Msg)
	}
	return out
}

// logsText is every byte under the logs root, archives decompressed.
func (fx *fixture) logsText(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	_ = filepath.WalkDir(fx.d.logsDir(), func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.HasSuffix(p, ".gz") {
			zr, err := gzip.NewReader(bytes.NewReader(data))
			if err != nil {
				return err
			}
			var un bytes.Buffer
			un.ReadFrom(zr)
			data = un.Bytes()
		}
		b.Write(data)
		return nil
	})
	return b.String()
}

// Forgetting a session previews, then removes, its events and its runs'
// events from the log book: the store, the text files and the archive. Its
// security record is tombstoned, other sessions stay, and no copy of its text
// is left in any file.
func TestForgetSessionClearsTheLogBook(t *testing.T) {
	fx := newFixture(t)
	fx.seedLogbook(t)
	all := fx.logbookMsgs(t)
	p, err := Forget(context.Background(), fx.d, Selector{Sessions: []string{"sess_a"}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{logbookEvents + "turns": 1, logbookEvents + "model": 1, logbookTombstones: 1, logbookLines: 3, logbookArchived: 1}
	for what, n := range want {
		if got := itemCount(p.Items, what); got != n {
			t.Errorf("preview %q = %d, want %d", what, got, n)
		}
	}
	for _, it := range p.Items {
		if it.What == logbookEvents+"turns" && (len(it.Samples) != 1 || !strings.Contains(it.Samples[0], "turn started")) {
			t.Errorf("preview samples of turns = %q", it.Samples)
		}
	}
	if got := fx.logbookMsgs(t); !slices.Equal(got, all) || !strings.Contains(fx.logsText(t), "Baker Street") {
		t.Fatalf("the preview changed the log book: %q", got)
	}

	res, err := p.Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for what, n := range want {
		if got := itemCount(res.Items, what); got != n {
			t.Errorf("result %q = %d, want %d", what, got, n)
		}
	}
	if got := fx.logbookMsgs(t); !slices.Equal(got, []string{store.Tombstone, "turn in another session", "researcher turn", "hub started"}) {
		t.Errorf("log book after the forget = %q", got)
	}
	text := fx.logsText(t)
	if strings.Contains(text, "Baker") || strings.Contains(text, "turn started") || strings.Contains(text, "model call") {
		t.Errorf("the forgotten session's text is still in a file under logs/")
	}
	for _, kept := range []string{"turn in another session", "researcher turn", "hub started", `"kept"`} {
		if !strings.Contains(text, kept) {
			t.Errorf("%s was lost from the log book's files", kept)
		}
	}
	if last, _, err := BackupsBeforeForget(fx.d); err != nil || !last.Equal(now) {
		t.Errorf("the forget was not recorded: %v, %v", last, err)
	}
}

// An export carries the log book's events with their sensitive fields; one
// agent's export carries only its sessions' and runs' events.
func TestExportIncludesTheLogBook(t *testing.T) {
	fx := newFixture(t)
	fx.seedLogbook(t)
	out := filepath.Join(t.TempDir(), "all")
	res, err := Export(context.Background(), fx.d, ExportOptions{Out: out})
	if err != nil {
		t.Fatal(err)
	}
	evs := readJSONL(t, filepath.Join(out, "logbook.jsonl"))
	if len(evs) != 6 || res.Rows["logbook.jsonl"] != 6 {
		t.Fatalf("exported %d log book events (%d counted), want 6", len(evs), res.Rows["logbook.jsonl"])
	}
	// The event's wire form: data as an object, dtypes upper case for a
	// sensitive field (internal/logs/event/json.go).
	data, _ := evs[0]["data"].(map[string]any)
	dtypes, _ := evs[0]["dtypes"].(string)
	var sensitive bool
	for _, v := range data {
		sensitive = sensitive || v == "my address is 221B Baker Street"
	}
	if !sensitive || !strings.ContainsAny(dtypes, "SIFBNJTD") {
		t.Errorf("the sensitive field was not exported as it is: data %v, dtypes %q", evs[0]["data"], dtypes)
	}
	one := filepath.Join(t.TempDir(), "one")
	if _, err := Export(context.Background(), fx.d, ExportOptions{Out: one, Agent: "researcher"}); err != nil {
		t.Fatal(err)
	}
	if evs := readJSONL(t, filepath.Join(one, "logbook.jsonl")); len(evs) != 1 || evs[0]["msg"] != "researcher turn" {
		t.Errorf("researcher's export = %v", evs)
	}
}

// Status counts the backups made before the last forget that are still
// where they were written.
func TestBackupsBeforeTheLastForget(t *testing.T) {
	fx := newFixture(t)
	dir := t.TempDir()
	kept, gone, later := filepath.Join(dir, "kept.tar.gz"), filepath.Join(dir, "gone.tar.gz"), filepath.Join(dir, "later.tar.gz")
	for _, b := range []string{kept, later} {
		if err := os.WriteFile(b, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, b := range []struct {
		path string
		at   time.Time
	}{{kept, now.Add(-2 * time.Hour)}, {gone, now.Add(-time.Hour)}} {
		if err := NoteBackup(fx.d, b.path, b.at); err != nil {
			t.Fatal(err)
		}
	}
	if last, bs, err := BackupsBeforeForget(fx.d); err != nil || !last.IsZero() || bs != nil {
		t.Fatalf("before any forget: %v, %v, %v", last, bs, err)
	}
	forget(t, fx, Selector{Sessions: []string{"sess_b"}})
	if err := NoteBackup(fx.d, later, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	last, bs, err := BackupsBeforeForget(fx.d)
	if err != nil || !last.Equal(now) || !slices.Equal(bs, []string{kept}) {
		t.Errorf("BackupsBeforeForget = %v, %v, %v; want %v, [%s]", last, bs, err, now, kept)
	}
}
