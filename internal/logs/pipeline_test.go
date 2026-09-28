package logs

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"tenant/internal/logs/catalog"
	"tenant/internal/logs/event"
	"tenant/internal/logs/store"
)

var testDetail = testProvider.Event(9002, event.Info, "Detail", "detail {n}", "n").Diagnostic()

// startHelper starts the test binary as a Tenant process (TestMain) and,
// for live, waits until it is up.
func startHelper(t *testing.T, mode, root string) (*exec.Cmd, io.WriteCloser) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "TENANT_LOGBOOK_HELPER="+mode, "TENANT_LOGBOOK_ROOT="+root)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if mode == "live" {
		if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || strings.TrimSpace(line) != "ready" {
			t.Fatalf("helper did not come up: %q %v", line, err)
		}
	}
	return cmd, stdin
}

// TestMain lets the test binary stand in for a Tenant process, with the hub
// at TENANT_LOGBOOK_ROOT, its store and a boot record: with
// TENANT_LOGBOOK_HELPER=crash it logs until it is killed; with live it says
// ready and waits to be killed.
func TestMain(m *testing.M) {
	if mode := os.Getenv("TENANT_LOGBOOK_HELPER"); mode != "" {
		h, err := Open(os.Getenv("TENANT_LOGBOOK_ROOT"), Options{})
		if err == nil {
			_, err = h.OpenStore(nil)
		}
		if err != nil {
			panic(err)
		}
		h.BeginBoot("serve")
		if mode == "live" {
			fmt.Println("ready")
			_, _ = io.Copy(io.Discard, os.Stdin)
			os.Exit(0)
		}
		h.lb.lim.set(-1, 0) // one line as fast as it goes: past its rate limit
		for i := 0; ; i++ {
			h.Logf("crashtest", slog.LevelInfo, "event "+strconv.Itoa(i))
			if i%256 == 255 {
				time.Sleep(time.Millisecond) // about the writer's pace, so the queue rarely fills
			}
		}
	}
	os.Exit(m.Run())
}

// attach opens <root>/logbook.db, attaches it to h and returns a second
// handle on it, for the test to read.
func attach(t *testing.T, h *Hub) *store.Store {
	t.Helper()
	path := filepath.Join(h.Root(), "logbook.db")
	st, err := store.Open(path, store.Options{ChainKey: []byte("test chain key")})
	if err != nil {
		t.Fatal(err)
	}
	h.AttachStore(st)
	return readerAt(t, h.Root())
}

func all(t *testing.T, st *store.Store) []event.Event {
	t.Helper()
	var out []event.Event
	var cur store.Cursor
	for {
		evs, next, _, err := st.Since(context.Background(), cur, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(evs) == 0 {
			return out
		}
		out, cur = append(out, evs...), next
	}
}

// readLogs is every day file in a source folder, concatenated.
func readLogs(t *testing.T, dir string) string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "*.log"))
	var b strings.Builder
	for _, f := range files {
		b.WriteString(readFile(t, f))
	}
	return b.String()
}

// waitRing waits for the v1 ring to hold n entries.
func waitRing(t *testing.T, h *Hub, n int) []Entry {
	t.Helper()
	wake, cancel := h.Subscribe()
	defer cancel()
	deadline := time.After(5 * time.Second)
	for {
		if got, _ := h.Snapshot(); len(got) >= n {
			return got
		}
		select {
		case <-wake:
		case <-deadline:
			got, _ := h.Snapshot()
			t.Fatalf("the ring holds %d entries, want %d", len(got), n)
		}
	}
}

// With a store attached, Log and the emitter go through the pipeline: the
// store gets every event, typed and with the process; the text files and the
// ring get the operational ones, as v1 wrote them, so the Activity feed
// carries on; values are capped.
func TestPipelineFeedsStoreFilesAndRing(t *testing.T) {
	h, root, _ := openHub(t, Options{MaxFieldBytes: 16})
	st := attach(t, h)
	Install(h)
	defer Install(nil)

	h.Logf("turns", slog.LevelInfo, "tool call", "args", strings.Repeat("x", 40))
	testOp.Log(context.Background(), "name", "calendar", "payload", "hunter2-secret")
	testDetail.Log(context.Background(), "n", 7)

	ring := waitRing(t, h, 2)
	if len(ring) != 2 || ring[0].Msg != "tool call" || ring[1].Msg != "Sync calendar" || ring[1].Field("payload") != event.Masked {
		t.Fatalf("ring = %+v", ring)
	}
	if !strings.HasPrefix(ring[0].Field("args"), strings.Repeat("x", 16)+"…(+24 bytes)") {
		t.Errorf("args not capped in the ring: %q", ring[0].Field("args"))
	}
	file := readLogs(t, filepath.Join(root, "logstest"))
	if !strings.Contains(file, "Sync calendar") || strings.Contains(file, "hunter2") || strings.Contains(file, "detail") {
		t.Errorf("logstest file:\n%s", file)
	}
	if !strings.Contains(readLogs(t, filepath.Join(root, "turns")), "tool call") {
		t.Error("the turns file lacks the line")
	}

	evs := all(t, st)
	if len(evs) != 3 {
		t.Fatalf("store holds %d events, want 3: %+v", len(evs), evs)
	}
	if evs[0].Log != "turns" || evs[0].Data[0].Value != strings.Repeat("x", 16)+"…(+24 bytes)" {
		t.Errorf("logged entry: %+v", evs[0])
	}
	if evs[1].ID != 1001 || evs[1].Data[1].Value != "hunter2-secret" || !evs[1].Data[1].Sensitive {
		t.Errorf("emitted event: %+v", evs[1])
	}
	if evs[2].Channel != event.Diagnostic || evs[2].Data[0].Value != int64(7) {
		t.Errorf("diagnostic event: %+v", evs[2])
	}
	for _, e := range evs {
		if e.Proc.PID != os.Getpid() || e.Proc.BootID == "" {
			t.Errorf("event %d has no process: %+v", e.Seq, e.Proc)
		}
	}
}

// The writer scrubs every event before the text files and the store see it:
// the message, every string value (sensitive ones too, and a field named
// like a credential whole), the attributes; a Log call itself does not.
func TestPipelineScrubsBeforeFilesAndStore(t *testing.T) {
	h, root, _ := openHub(t, Options{})
	st := attach(t, h)
	Install(h)
	defer Install(nil)
	const secret = "sekrit-discord-bot-value"
	h.SetSecrets(map[string]string{"discord": secret})

	h.Log(Entry{Source: "turns", Level: slog.LevelInfo, Msg: "posting with " + secret,
		Fields: []Field{{Key: "session", Value: secret}, {Key: "auth", Value: "Bearer " + secret}}})
	testOp.Log(context.Background(), "name", "cal", "payload", `{"token":"`+secret+`"}`, "api_key", "short-key-1")
	waitRing(t, h, 2)

	evs := all(t, st)
	if len(evs) != 2 {
		t.Fatalf("store holds %d events", len(evs))
	}
	for _, e := range evs {
		text := fmt.Sprintf("%+v", e)
		if strings.Contains(text, secret) || strings.Contains(text, "short-key-1") {
			t.Errorf("a secret reached the store: %s", text)
		}
	}
	if !strings.Contains(evs[0].Msg, "«secret:discord»") || evs[0].Corr.SessionID != "«secret:discord»" {
		t.Errorf("logged entry not scrubbed as expected: %+v", evs[0])
	}
	if evs[1].Data[2].Value != "«secret:api_key»" {
		t.Errorf("api_key = %v, want its whole value replaced", evs[1].Data[2].Value)
	}
	for _, dir := range []string{"turns", "logstest"} {
		if f := readLogs(t, filepath.Join(root, dir)); strings.Contains(f, secret) || strings.Contains(f, "short-key-1") {
			t.Errorf("a secret reached the %s file:\n%s", dir, f)
		}
	}
}

// Audit forces its batch to commit, after what was queued before it, and
// returns once it is committed. With the batch timer an hour away, an Audit
// that returns at all forced its commit (a wall-clock bound flaked on slow CI
// disks). A Security event through Log is never queued either.
func TestAuditForcesCommit(t *testing.T) {
	prev := batchEvery
	batchEvery = time.Hour
	t.Cleanup(func() { batchEvery = prev }) // after the hub's Close, registered below
	h, _, _ := openHub(t, Options{})
	st := attach(t, h)
	Install(h)
	defer Install(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	testOp.Log(ctx, "name", "before")
	for i := range 5 {
		if err := testOp.Audit(ctx, "name", "audit"+strconv.Itoa(i)); err != nil {
			t.Fatalf("audit %d: %v (it waited for the batch timer instead of forcing a commit)", i, err)
		}
	}
	evs := all(t, st)
	if len(evs) != 6 || evs[0].Msg != "Sync before" || evs[5].Msg != "Sync audit4" {
		t.Fatalf("store after Audit returned: %+v", evs)
	}

	security := event.Event{Log: "security", Provider: "security", Level: event.Info, Msg: "sec"}
	if !h.pipe.Load().enqueue(ctx, &security) {
		t.Fatal("a security event was refused")
	}
	if evs := all(t, st); evs[len(evs)-1].Msg != "sec" {
		t.Errorf("a security event was queued, not committed: last is %q", evs[len(evs)-1].Msg)
	}
}

// Audit returns the commit's error when the store fails; the text file has
// the record either way, and the writer files the failure with its next batch.
func TestAuditReportsAFailedCommit(t *testing.T) {
	h, root, _ := openHub(t, Options{})
	attach(t, h)
	Install(h)
	defer Install(nil)
	ctx := context.Background()

	_ = h.pipe.Load().st.Close() // the store fails
	if err := testOp.Audit(ctx, "name", "lost"); err == nil {
		t.Error("Audit returned nil for a failed commit")
	}
	if !strings.Contains(readLogs(t, filepath.Join(root, "logstest")), "Sync lost") {
		t.Error("the text file lacks a record the store failed to commit")
	}
	waitFor(t, 5*time.Second, func() bool { // the writer files it with the next batch
		return strings.Contains(readLogs(t, filepath.Join(root, "logbook")), "could not commit")
	})
}

// A full queue makes an operational event wait a millisecond and a
// diagnostic one not at all, then drops and counts them; a Security event
// waits for the writer instead of being dropped.
func TestQueueFullDrops(t *testing.T) {
	h, _, _ := openHub(t, Options{})
	st, err := store.Open(filepath.Join(h.Root(), "logbook.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := newPipeline(h, st) // no writer goroutine: nothing drains the queue
	p.q = make(chan *event.Event, 1)
	op := func() *event.Event { return &event.Event{Log: "x", Level: event.Info} }
	if !p.enqueue(context.Background(), op()) || len(p.q) != 1 {
		t.Fatal("the first event was not queued")
	}
	start := time.Now()
	p.enqueue(context.Background(), op())
	if d := time.Since(start); d < time.Millisecond {
		t.Errorf("an operational event was dropped after %v, want a 1 ms wait", d)
	}
	p.enqueue(context.Background(), &event.Event{Log: "x", Level: event.Info, Channel: event.Diagnostic})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	p.enqueue(ctx, &event.Event{Log: "security", Level: event.Info})
	if p.recent[lossKey{log: "x"}] != 2 || len(p.recent) != 1 {
		t.Errorf("dropped = %v, want x: 2 and no security", p.recent)
	}
}

// Close flushes the queue into the files and the store, then the hub
// behaves as a closed hub: nothing panics and nothing reaches the store.
func TestCloseFlushes(t *testing.T) {
	prev := closeWait
	closeWait = time.Minute // 2,000 events under -race on a 2-core runner come close to 2 s
	t.Cleanup(func() { closeWait = prev })
	h, root, _ := openHub(t, Options{})
	st := attach(t, h)
	Install(h)
	defer Install(nil)
	h.lb.lim.set(-1, 0) // one line 2,000 times: past its rate limit
	for i := range 2000 {
		h.Logf("flush", slog.LevelInfo, "line "+strconv.Itoa(i))
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if n := len(all(t, st)); n != 2000 {
		t.Errorf("store holds %d events after Close, want 2000", n)
	}
	if n := strings.Count(readLogs(t, filepath.Join(root, "flush")), "\n"); n != 2000 {
		t.Errorf("text file holds %d lines, want 2000", n)
	}
	h.Logf("flush", slog.LevelInfo, "after close")
	testOp.Log(context.Background(), "name", "after close")
	_ = testOp.Audit(context.Background(), "name", "after close")
	if n := len(all(t, st)); n != 2000 {
		t.Errorf("store holds %d events, want 2000: something was written after Close", n)
	}
}

// A writer killed mid-batch loses at most the batch it had not committed,
// which the text files still hold, and the database passes its integrity
// check. The next process to look reports it dead, with the time of the
// last event it committed.
func TestCrashLosesAtMostTheBatchInFlight(t *testing.T) {
	root := t.TempDir()
	cmd, _ := startHelper(t, "crash", root)
	path := filepath.Join(root, "logbook.db")
	deadline := time.Now().Add(30 * time.Second)
	for {
		if st, err := store.Open(path, store.Options{}); err == nil {
			_, next, _, _ := st.Since(context.Background(), store.Cursor{StoreID: st.StoreID(), Seq: 5000}, 1)
			_ = st.Close()
			if next.Seq > 5000 {
				break
			}
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("the writer never committed 5,000 events")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var check string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&check); err != nil || check != "ok" {
		t.Fatalf("integrity_check = %q, %v", check, err)
	}
	if _, err := db.Exec(`INSERT INTO events_fts (events_fts) VALUES ('integrity-check')`); err != nil {
		t.Errorf("full-text integrity check: %v", err)
	}

	st, err := store.Open(path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	stored := map[int]bool{}
	last := -1
	for _, e := range all(t, st) {
		n, _ := strconv.Atoi(strings.TrimPrefix(e.Msg, "event "))
		stored[n], last = true, max(last, n)
	}
	mirrored := map[int]bool{}
	for _, line := range strings.Split(strings.TrimSpace(readLogs(t, filepath.Join(root, "crashtest"))), "\n") {
		en, ok := ParseLine(line)
		if !ok {
			t.Fatalf("a torn line in the text file: %q", line)
		}
		n, _ := strconv.Atoi(strings.TrimPrefix(en.Msg, "event "))
		mirrored[n] = true
	}
	for n := range stored {
		if !mirrored[n] {
			t.Fatalf("event %d is in the store but not in the text file", n)
		}
	}
	lost := 0
	for n := range mirrored {
		if !stored[n] {
			if n < last {
				t.Fatalf("event %d, older than the last committed %d, is missing from the store", n, last)
			}
			lost++
		}
	}
	if lost > batchMax {
		t.Errorf("%d events in the text file never reached the store; at most one batch (%d) may", lost, batchMax)
	}
	t.Logf("%d events committed, %d in the text file only (the batch in flight)", len(stored), lost)

	var lastAt time.Time
	for _, e := range all(t, st) {
		lastAt = e.Time
	}
	h, err := Open(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.OpenStore(nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool { _ = h.CheckBoots(context.Background()); return has(t, st, catalog.Unexpected) })
	_ = h.Close()
	for _, e := range all(t, st) {
		if e.Provider == "system" && e.ID == catalog.Unexpected.Spec().ID && !e.Data[0].Value.(time.Time).Equal(lastAt) {
			t.Errorf("unexpected end at %v, want the last committed event's %v", e.Data[0].Value, lastAt)
		}
	}
}

// BenchmarkEmit is an enabled emit up to the moment it is queued (§12.3: p99
// under 20 µs): building the event, enriching it and queueing it. A stand-in
// drains the queue, so the benchmark never measures a full one.
func BenchmarkEmit(b *testing.B) {
	h, err := Open(b.TempDir(), Options{})
	if err != nil {
		b.Fatal(err)
	}
	defer h.Close()
	st, err := store.Open(filepath.Join(h.Root(), "logbook.db"), store.Options{})
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	p := newPipeline(h, st)
	go func() {
		for range p.q {
		}
	}()
	h.pipe.Store(p)
	defer h.pipe.Store(nil)
	Install(h)
	defer Install(nil)
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		testOp.Log(ctx, "name", "calendar", "payload", "hunter2")
	}
	b.StopTimer()
	h.pipe.Store(nil)
	close(p.q)
}
