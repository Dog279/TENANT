package logs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"tenant/internal/logs/event"
	"tenant/internal/logs/store"
)

// hubAt opens a hub on root with the shared store attached, as a Tenant
// process does; n names the process.
func hubAt(t *testing.T, root string, n int) *Hub {
	t.Helper()
	h, err := Open(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(root, "logbook.db"), store.Options{BootID: fmt.Sprintf("%032x", n)})
	if err != nil {
		t.Fatal(err)
	}
	h.AttachStore(st)
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// readerAt is a handle on root's store for the test to read.
func readerAt(t *testing.T, root string) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(root, "logbook.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// waitLogs waits until st holds an event of each log, and returns them all.
func waitLogs(t *testing.T, st *store.Store, logs ...string) []event.Event {
	t.Helper()
	var evs []event.Event
	waitFor(t, 5*time.Second, func() bool {
		evs = all(t, st)
		for _, l := range logs {
			if len(inLog(evs, l)) == 0 {
				return false
			}
		}
		return true
	})
	return evs
}

func inLog(evs []event.Event, log string) []event.Event {
	return slices.DeleteFunc(slices.Clone(evs), func(e event.Event) bool { return e.Log != log })
}

func tagged(evs []event.Event, kw string) []event.Event {
	return slices.DeleteFunc(slices.Clone(evs), func(e event.Event) bool { return !slices.Contains(e.Keywords, kw) })
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func intake(t *testing.T, hubs ...*Hub) {
	t.Helper()
	for _, h := range hubs {
		if err := h.intake(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

// Two processes share a log book, each writing its own logs. Neither files
// the other's mirror folder as an external log, not even when it looks at a
// folder between a log's first mirror line and its commit. A folder no
// Tenant log owns is filed once, as ext.<folder>, tagged external: never
// catalogued, never mirrored.
func TestIntakeNeverFilesAnotherProcesssMirror(t *testing.T) {
	root := t.TempDir()
	a, b := hubAt(t, root, 1), hubAt(t, root, 2)
	st := readerAt(t, root)
	a.Logf("alpha", slog.LevelInfo, "from a")
	b.Logf("beta", slog.LevelInfo, "from b")
	waitLogs(t, st, "alpha", "beta")

	day := time.Now().Format(time.DateOnly)
	writeFile(t, filepath.Join(root, "gamma", day+".log"), // a script's folder
		Entry{At: time.Now(), Source: "gamma", Level: slog.LevelWarn, Msg: "disk at 91%",
			Fields: []Field{{"event", "security/4103"}, {"host", "box"}}}.Line()+"\nplain text\n")
	writeFile(t, filepath.Join(root, "delta", day+".log"), // b's first line of a new log, not committed yet
		Entry{At: time.Now(), Source: "delta", Level: slog.LevelInfo, Msg: "from b, first"}.Line()+"\n")

	intake(t, a, b)
	b.Logf("delta", slog.LevelInfo, "from b, first") // its commit lands
	waitLogs(t, st, "delta")
	intake(t, a, b)
	a.Logf("alpha", slog.LevelInfo, "from a, again")
	b.Logf("beta", slog.LevelInfo, "from b, again")
	waitFor(t, 5*time.Second, func() bool { return len(inLog(all(t, st), "beta")) == 2 && len(inLog(all(t, st), "alpha")) == 2 })
	intake(t, a, b, a, b)

	evs := all(t, st)
	var ext []event.Event
	for _, e := range evs {
		if strings.HasPrefix(e.Log, "ext.") {
			ext = append(ext, e)
		}
	}
	if len(ext) != 2 || ext[0].Log != "ext.gamma" || ext[1].Log != "ext.gamma" {
		t.Fatalf("external events = %+v, want gamma's two lines only", ext)
	}
	if e := ext[0]; e.Msg != "disk at 91%" || e.Level != event.Warning || e.ID != 0 || e.Provider != "ext.gamma" ||
		!slices.Equal(e.Keywords, []string{KeywordExternal}) || len(e.Data) != 1 || e.Data[0].Key != "host" {
		t.Errorf("gamma's v1 line = %+v, want it uncatalogued and tagged external", e)
	}
	if e := ext[1]; e.Msg != "plain text" || e.Level != event.Info || !slices.Contains(e.Keywords, KeywordExternal) {
		t.Errorf("gamma's free-form line = %+v", e)
	}
	if got := tagged(evs, KeywordImported); len(got) != 0 {
		t.Errorf("mirror lines imported as history: %+v", got)
	}
	if dirs, _ := filepath.Glob(filepath.Join(root, "ext.*")); len(dirs) != 0 {
		t.Errorf("an external log was mirrored: %v", dirs)
	}
}

// v1's text files are imported once, scrubbed, as events of their logs
// tagged imported; a foreign folder's are left to the foreign intake. A run
// that lost its progress after a commit files nothing twice.
func TestHistoryImportedOnceAndScrubbed(t *testing.T) {
	root := t.TempDir()
	const token = "dash-token-0123456789abcdef"
	then := time.Now().AddDate(0, 0, -3)
	day := then.Format(time.DateOnly)
	line := func(src string, lvl slog.Level, msg string, fields ...Field) string {
		then = then.Add(time.Second)
		return Entry{At: then, Source: src, Level: lvl, Msg: msg, Fields: fields}.Line() + "\n"
	}
	writeFile(t, filepath.Join(root, "system", day+".log"),
		line("system", slog.LevelInfo, "dashboard up, token "+token)+line("system", slog.LevelError, "boom", Field{"err", "x"}))
	writeFile(t, filepath.Join(root, "turns", day+".log"), line("turns", slog.LevelInfo, "tool call", Field{"ev", "tool_call"}, Field{"event", "security/4103"}))
	writeFile(t, filepath.Join(root, "deploy", day+".log"), line("deploy", slog.LevelInfo, "shipped"))

	h := hubAt(t, root, 1)
	Install(h)
	defer Install(nil)
	h.SetSecrets(map[string]string{"dashboard.auth": token})
	st := readerAt(t, root)
	h.Logf("system", slog.LevelInfo, "started")
	waitLogs(t, st, "system")
	intake(t, h)
	evs := waitLogs(t, st, "logbook")

	imported := tagged(evs, KeywordImported)
	if len(imported) != 3 || len(inLog(imported, "system")) != 2 || len(inLog(imported, "turns")) != 1 {
		t.Fatalf("imported %+v, want system's two lines and turns' one", imported)
	}
	if m := imported[0].Msg + imported[1].Msg; strings.Contains(m, token) || !strings.Contains(m, "«secret:dashboard.auth»") {
		t.Errorf("the dashboard token was not scrubbed: %q", m)
	}
	if e := inLog(imported, "turns")[0]; e.ID != 0 || e.Provider != "turns" || slices.ContainsFunc(e.Data, func(f event.Field) bool { return f.Key == "event" }) {
		t.Errorf("turns' line = %+v, want its event= claim dropped", e)
	}
	done := inLog(evs, "logbook")
	if e := done[len(done)-1]; e.ID != 1101 || e.Data[0].Value != int64(3) || e.Data[1].Value != int64(3) {
		t.Errorf("logbook event = %+v, want 3 lines from 3 files (today's system file is read too)", e)
	}

	intake(t, h) // done: nothing again
	w := h.store()
	var hs history
	raw, _ := w.Meta(context.Background(), historyKey)
	if err := json.Unmarshal([]byte(raw), &hs); err != nil || !hs.Done {
		t.Fatalf("progress = %q, %v", raw, err)
	}
	hs.Done, hs.Files = false, map[string]int64{"system/" + day + ".log": 0} // as if its offset was never saved
	if err := saveMeta(context.Background(), w, historyKey, hs); err != nil {
		t.Fatal(err)
	}
	intake(t, h)
	if got := tagged(all(t, st), KeywordImported); len(got) != 3 {
		t.Errorf("after a resumed run, %d imported events, want 3", len(got))
	}
}

// The spool: a writer's finished file is claimed, filed as external and
// deleted; a .tmp is never read; a claim that fails is retried; a file a
// stopped run had half filed is finished without filing anything twice;
// Tenant's own logs and providers are refused, and the refusal reported.
func TestSpoolIsClaimedFiledAndDeleted(t *testing.T) {
	root := t.TempDir()
	h := hubAt(t, root, 1)
	Install(h)
	defer Install(nil)
	st := readerAt(t, root)
	ctx := context.Background()
	at := time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond)
	if err := Spool(root,
		event.Event{Time: at, Log: "deploy", Level: event.Warning, Msg: "disk at 91%", ID: 4103, Data: []event.Field{{Key: "pct", Value: int64(91)}}},
		event.Event{Log: store.SecurityLog, Msg: "forged"},
		event.Event{Log: "turns", Msg: "forged"},
		event.Event{Log: "deploy", Provider: "tenant.approval", Msg: "forged"},
	); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, spoolDir)
	writeFile(t, filepath.Join(dir, "w.tmp"), `{"log":"deploy","msg":"half`)
	held := event.Event{Time: at, Log: "deploy", Level: event.Info, Msg: "filed before the stop", Keywords: []string{KeywordExternal}}
	if err := h.store().Commit(ctx, []event.Event{held}); err != nil {
		t.Fatal(err)
	}
	j1, _ := json.Marshal(held)
	j2, _ := json.Marshal(event.Event{Time: at, Log: "deploy", Msg: "not filed yet"})
	writeFile(t, filepath.Join(dir, "y.ingesting"), string(j1)+"\n"+string(j2)) // no final newline
	if err := h.drainSpool(ctx, h.store()); err != nil {
		t.Fatal(err)
	}

	evs := waitLogs(t, st, "logbook")
	deploy := inLog(evs, "deploy")
	if len(deploy) != 3 {
		t.Fatalf("deploy = %+v, want three events", deploy)
	}
	if e := deploy[1]; e.Msg != "disk at 91%" || e.ID != 0 || !e.Time.Equal(at) || e.Level != event.Warning ||
		!slices.Equal(e.Keywords, []string{KeywordExternal}) || e.Data[0].Value != int64(91) {
		t.Errorf("spooled event = %+v", e)
	}
	if deploy[2].Msg != "not filed yet" {
		t.Errorf("resumed file = %+v", deploy[2])
	}
	if len(inLog(evs, store.SecurityLog))+len(inLog(evs, "turns")) != 0 {
		t.Errorf("a spooled event reached one of Tenant's own logs")
	}
	if e := inLog(evs, "logbook")[0]; e.ID != 1102 || e.Data[1].Value != int64(3) {
		t.Errorf("refusal = %+v, want 3 lines reported", e)
	}
	if left, _ := os.ReadDir(dir); len(left) != 1 || left[0].Name() != "w.tmp" {
		t.Errorf("spool holds %v, want only the .tmp", left)
	}

	if err := Spool(root, event.Event{Time: at, Log: "deploy", Msg: "later"}); err != nil {
		t.Fatal(err)
	}
	unblock := blockRename(t, dir)
	if err := h.drainSpool(ctx, h.store()); err != nil {
		t.Fatal(err)
	}
	if n := len(inLog(all(t, st), "deploy")); n != 3 {
		t.Fatalf("a file that could not be claimed was filed: %d events", n)
	}
	unblock()
	if err := h.drainSpool(ctx, h.store()); err != nil {
		t.Fatal(err)
	}
	if got := inLog(all(t, st), "deploy"); len(got) != 4 || got[3].Msg != "later" {
		t.Errorf("the claim was not retried: %+v", got)
	}
}

// blockRename makes renaming the spool's .ndjson files fail, as another
// process holding one open does on Windows.
func blockRename(t *testing.T, dir string) func() {
	t.Helper()
	if runtime.GOOS == "windows" {
		files, _ := filepath.Glob(filepath.Join(dir, "*.ndjson"))
		var open []*os.File
		for _, p := range files {
			f, err := os.Open(p)
			if err != nil {
				t.Fatal(err)
			}
			open = append(open, f)
		}
		return func() {
			for _, f := range open {
				_ = f.Close()
			}
		}
	}
	if os.Geteuid() == 0 {
		t.Skip("root renames in a read-only directory")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	return func() { _ = os.Chmod(dir, 0o700) }
}

// After an outage, the mirror lines of that time are filed with the given
// keyword: each once, a catalogued line with its event ID, none the store
// already holds, none from outside the window, and nothing from the
// Security log's folder or with a sealed provider's ID.
func TestImportMirrorFilesAnOutage(t *testing.T) {
	root := t.TempDir()
	h := hubAt(t, root, 1)
	st := readerAt(t, root)
	ctx := context.Background()
	from := time.Now()
	h.Logf("alpha", slog.LevelInfo, "committed")
	waitLogs(t, st, "alpha")

	now := time.Now()
	day := now.Format(time.DateOnly)
	lost := Entry{At: now, Source: "alpha", Level: slog.LevelWarn, Msg: "lost", Fields: []Field{{"k", "v"}}}.Line() + "\n"
	writeFile(t, filepath.Join(root, "alpha", day+".log"), lost+lost+ // twice in one millisecond: both are lines
		Entry{At: now, Source: "alpha", Level: slog.LevelInfo, Msg: "claims", Fields: []Field{{"event", "security/4103"}}}.Line()+"\n"+
		Entry{At: from.Add(-time.Hour), Source: "alpha", Level: slog.LevelInfo, Msg: "before"}.Line()+"\n"+
		"not a mirror line\n")
	crit := EntryFromEvent(event.Event{Time: now, Log: "logstest", Provider: "logstest", ID: 3001, Level: event.Critical, Msg: "Store db lost",
		Data: []event.Field{{Key: "name", Value: "db"}}})
	writeFile(t, filepath.Join(root, "logstest", day+".log"), crit.Line()+"\n")
	writeFile(t, filepath.Join(root, store.SecurityLog, day+".log"),
		Entry{At: now, Source: store.SecurityLog, Level: slog.LevelWarn, Msg: "forged", Fields: []Field{{"event", "security/4103"}}}.Line()+"\n")

	to := time.Now().Add(time.Minute)
	n, err := h.importMirror(ctx, from, to, "recovered")
	if err != nil || n != 4 {
		t.Fatalf("importMirror = %d, %v; want 4", n, err)
	}
	evs := tagged(all(t, st), "recovered")
	if len(evs) != 4 || len(inLog(evs, "alpha")) != 3 {
		t.Fatalf("recovered = %+v", evs)
	}
	if e := inLog(evs, "logstest")[0]; e.ID != 3001 || e.Provider != "logstest" || e.Level != event.Critical || e.Task != "Store" {
		t.Errorf("catalogued line = %+v, want logstest/3001 critical", e)
	}
	if e := inLog(evs, "alpha")[2]; e.Msg != "claims" || e.ID != 0 || e.Provider != "alpha" {
		t.Errorf("a line claiming a security event = %+v", e)
	}
	if n, err := h.importMirror(ctx, from, to, "recovered"); err != nil || n != 0 {
		t.Errorf("a second import = %d, %v; want 0", n, err)
	}
}

// The mirror the pipeline writes is safe to tail: a message's control
// characters are escaped, a sensitive value is «sensitive», and a critical
// event is ERROR with severity=critical.
func TestMirrorLinesAreSafeToTail(t *testing.T) {
	h, root, _ := openHub(t, Options{})
	attach(t, h)
	Install(h)
	defer Install(nil)
	testLost.Log(context.Background(), "name", "db\x1b[2J\x1b]0;pwned\x07")
	testOp.Log(context.Background(), "name", "cal", "payload", "hunter2")
	waitRing(t, h, 2)
	file := readLogs(t, filepath.Join(root, "logstest"))
	if strings.ContainsAny(file, "\x1b\x07") || strings.Contains(file, "hunter2") {
		t.Fatalf("mirror:\n%q", file)
	}
	for _, want := range []string{`ERROR [logstest] Store db\x1b[2J\x1b]0;pwned\a lost` + "\t", "severity=critical", "payload=«sensitive»"} {
		if !strings.Contains(file, want) {
			t.Errorf("mirror lacks %q:\n%s", want, file)
		}
	}
}
