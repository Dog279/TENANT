package logs

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

var base = time.Date(2026, 9, 22, 14, 0, 0, 0, time.Local)

func openHub(t *testing.T, opts Options) (*Hub, string, *fakeClock) {
	t.Helper()
	clk := &fakeClock{t: base}
	if opts.Now == nil {
		opts.Now = clk.Now
	}
	root := t.TempDir()
	h, err := Open(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h, root, clk
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// One folder per source, one file per day, our line format.
func TestHub_WritesDailyFilePerSource(t *testing.T) {
	h, root, clk := openHub(t, Options{})
	h.Logf("discord", slog.LevelInfo, "relay connected", "guild", "g1")
	h.Logf("turns", slog.LevelError, "turn error", "text", "boom")
	clk.Add(24 * time.Hour)
	h.Logf("turns", slog.LevelInfo, "next day")

	d1 := readFile(t, filepath.Join(root, "discord", "2026-09-22.log"))
	if !strings.Contains(d1, "INFO  [discord] relay connected\tguild=g1\n") {
		t.Errorf("discord file:\n%s", d1)
	}
	t1 := readFile(t, filepath.Join(root, "turns", "2026-09-22.log"))
	if !strings.Contains(t1, "ERROR [turns] turn error\ttext=boom\n") || strings.Contains(t1, "next day") {
		t.Errorf("turns day-1 file:\n%s", t1)
	}
	t2 := readFile(t, filepath.Join(root, "turns", "2026-09-23.log"))
	if !strings.Contains(t2, "next day") {
		t.Errorf("turns day-2 file:\n%s", t2)
	}
	srcs := h.Sources()
	if len(srcs) != 2 || srcs[0].Name != "turns" || srcs[1].Name != "discord" {
		t.Fatalf("Sources() = %+v (want turns, discord in display order)", srcs)
	}
	if !srcs[0].Native || srcs[0].Count != 2 || srcs[0].Err != "" {
		t.Errorf("turns info = %+v", srcs[0])
	}
}

func TestHub_RingCursorAndEviction(t *testing.T) {
	h := NewMemory(3)
	for i := 0; i < 5; i++ {
		h.Logf("a", slog.LevelInfo, "m")
	}
	all, head := h.Snapshot()
	if head != 5 || len(all) != 3 || all[0].Seq != 3 || all[2].Seq != 5 {
		t.Fatalf("snapshot = %d entries head=%d first=%d", len(all), head, all[0].Seq)
	}
	since, head := h.Since(4)
	if len(since) != 1 || since[0].Seq != 5 || head != 5 {
		t.Fatalf("Since(4) = %+v head=%d", since, head)
	}
	if got, _ := h.Since(5); got != nil {
		t.Fatalf("Since(head) should be empty, got %+v", got)
	}
	if got, _ := h.Since(1); len(got) != 3 {
		t.Fatalf("stale cursor should return the surviving ring, got %d", len(got))
	}
	if s := h.Sources(); s[0].Count != 3 {
		t.Errorf("count after eviction = %d, want 3", s[0].Count)
	}
	// Notify coalesces: at most one pending wake.
	select {
	case <-h.Notify():
	default:
		t.Error("expected a pending wake")
	}
	select {
	case <-h.Notify():
		t.Error("wake must coalesce")
	default:
	}
}

func TestHub_SourceNormalizedAndDefaulted(t *testing.T) {
	h := NewMemory(10)
	h.Log(Entry{Source: "My Script!", Msg: "a"})
	h.Log(Entry{Source: "", Msg: "b"})
	h.Log(Entry{Source: "..", Msg: "c"})
	all, _ := h.Snapshot()
	if all[0].Source != "my-script" || all[1].Source != DefaultSource || all[2].Source != DefaultSource {
		t.Errorf("sources = %s %s %s", all[0].Source, all[1].Source, all[2].Source)
	}
	if h.Sources()[len(h.Sources())-1].Name != "system" {
		t.Error("system sorts last")
	}
}

func TestHub_FieldCapAppliesToDiskAndRing(t *testing.T) {
	h, root, _ := openHub(t, Options{MaxFieldBytes: 8})
	big := strings.Repeat("x", 100)
	fields := []Field{{Key: "result", Value: big}}
	h.Log(Entry{Source: "turns", Msg: "m", Fields: fields})
	if fields[0].Value != big {
		t.Error("Log must not mutate the caller's fields")
	}
	all, _ := h.Snapshot()
	if v := all[0].Field("result"); v != "xxxxxxxx…(+92 bytes)" {
		t.Errorf("ring value = %q", v)
	}
	if !strings.Contains(readFile(t, filepath.Join(root, "turns", "2026-09-22.log")), "(+92 bytes)") {
		t.Error("disk value should be capped too")
	}
}

func TestHub_MinLevelDrops(t *testing.T) {
	h := NewMemory(10)
	h.Logf("a", slog.LevelDebug, "dbg")
	h.Logf("a", slog.LevelInfo, "info")
	if all, _ := h.Snapshot(); len(all) != 1 || all[0].Msg != "info" {
		t.Errorf("default INFO floor should drop DEBUG: %+v", all)
	}
	hd, _ := Open("", Options{RingCap: 10, MinLevel: slog.LevelDebug})
	hd.Logf("a", slog.LevelDebug, "dbg")
	if all, _ := hd.Snapshot(); len(all) != 1 {
		t.Error("MinLevel debug should keep DEBUG")
	}
}

// A restart shows history: the newest file of every folder is re-read (in
// time order across folders), foreign lines that aren't ours are kept raw.
func TestHub_BackfillOnStart(t *testing.T) {
	root := t.TempDir()
	mk := func(dir, name, body string) {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("turns", "2026-09-21.log", "2026-09-21 10:00:00.000 INFO  [turns] old day\n")
	mk("turns", "2026-09-22.log", "2026-09-22 09:00:00.000 INFO  [turns] first\tev=turn_start\n2026-09-22 11:00:00.000 ERROR [turns] third\n")
	mk("myscript", "run.log", "2026-09-22 10:00:00.000 INFO  [whatever] second\nplain script output\n")
	mk("_ignored", "x.log", "2026-09-22 10:30:00.000 INFO  [x] never\n")
	// A newer mtime decides which file is "newest" for turns.
	now := time.Now()
	_ = os.Chtimes(filepath.Join(root, "turns", "2026-09-21.log"), now.Add(-2*time.Hour), now.Add(-2*time.Hour))
	_ = os.Chtimes(filepath.Join(root, "turns", "2026-09-22.log"), now, now)

	clk := &fakeClock{t: base}
	h, err := Open(root, Options{Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.Start(ctx)

	all, _ := h.Snapshot()
	var msgs []string
	for _, e := range all {
		msgs = append(msgs, e.Source+":"+e.Msg)
	}
	got := strings.Join(msgs, "|")
	// "old day" is in the older file → not backfilled; the raw script line is
	// stamped "now" (after the parsed ones); the folder overrides the bracket.
	want := "turns:first|myscript:second|turns:third|myscript:plain script output"
	if got != want {
		t.Fatalf("backfill order = %s\nwant %s", got, want)
	}
	if all[0].Kind() != "turn_start" || !all[2].IsErr() {
		t.Error("parsed fields/levels should survive backfill")
	}
	srcs := h.Sources()
	if len(srcs) != 2 || srcs[0].Name != "turns" || srcs[1].Name != "myscript" || srcs[0].Native || srcs[1].Native {
		t.Errorf("Sources() = %+v", srcs)
	}
}

// A folder another process creates shows up as a source (tab) and its file is
// tailed live; once this process writes to it, tailing stops so our own lines
// are not re-read.
func TestHub_DiscoversForeignFolderAndTails(t *testing.T) {
	h, root, clk := openHub(t, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.Start(ctx)

	dir := filepath.Join(root, "sidecar")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "2026-09-22.log")
	if err := os.WriteFile(p, []byte("2026-09-22 14:00:01.000 INFO  [sidecar] hello from a script\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	clk.Add(3 * time.Second) // past the Sources() rescan gap
	srcs := h.Sources()
	if len(srcs) != 1 || srcs[0].Name != "sidecar" || srcs[0].Native {
		t.Fatalf("Sources() = %+v, want a foreign sidecar", srcs)
	}
	all, _ := h.Snapshot()
	if len(all) != 1 || all[0].Msg != "hello from a script" {
		t.Fatalf("discovery should seed the file's tail: %+v", all)
	}

	// Append → the tail tick ingests it (a partial line waits for its newline).
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString("2026-09-22 14:00:02.000 WARN  [sidecar] second\tk=v\npartial")
	_ = f.Close()
	h.tailTick()
	all, _ = h.Snapshot()
	if len(all) != 2 || all[1].Msg != "second" || all[1].Level != slog.LevelWarn || all[1].Field("k") != "v" {
		t.Fatalf("tail should ingest the new complete line only: %+v", all)
	}
	f, _ = os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString(" line done\n")
	_ = f.Close()
	h.tailTick()
	if all, _ = h.Snapshot(); len(all) != 3 || all[2].Msg != "partial line done" {
		t.Fatalf("partial line should complete: %+v", all)
	}

	// Promotion: we write to it → native; our line is on disk once and in the
	// ring once, and later ticks don't re-read it.
	h.Logf("sidecar", slog.LevelInfo, "ours")
	h.tailTick()
	h.tailTick()
	all, _ = h.Snapshot()
	if len(all) != 4 || all[3].Msg != "ours" {
		t.Fatalf("after promotion: %+v", all)
	}
	if s := h.Sources(); !s[0].Native {
		t.Error("sidecar should be native after we wrote to it")
	}
	if n := strings.Count(readFile(t, p), "ours"); n != 1 {
		t.Errorf("our line written %d times, want 1", n)
	}
}

func TestHub_RetentionPrunesOldFilesOnly(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "turns")
	_ = os.MkdirAll(dir, 0o755)
	for _, n := range []string{"2026-08-01.log", "2026-09-20.log", "2026-09-22.log", "notes.txt", "README"} {
		_ = os.WriteFile(filepath.Join(dir, n), []byte("x\n"), 0o644)
	}
	old := time.Date(2026, 7, 1, 0, 0, 0, 0, time.Local)
	_ = os.Chtimes(filepath.Join(dir, "notes.txt"), old, old)
	clk := &fakeClock{t: base}
	h, err := Open(root, Options{Now: clk.Now, Retention: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.Start(ctx) // prunes once at start
	left, _ := os.ReadDir(dir)
	var names []string
	for _, e := range left {
		names = append(names, e.Name())
	}
	got := strings.Join(names, ",")
	if got != "2026-09-20.log,2026-09-22.log,README" {
		t.Errorf("after prune: %s", got)
	}
	// Retention < 0 never prunes.
	hn, _ := Open(t.TempDir(), Options{Now: clk.Now, Retention: -1})
	_ = os.MkdirAll(filepath.Join(hn.root, "a"), 0o755)
	_ = os.WriteFile(filepath.Join(hn.root, "a", "2020-01-01.log"), []byte("x\n"), 0o644)
	hn.Start(ctx)
	if _, err := os.Stat(filepath.Join(hn.root, "a", "2020-01-01.log")); err != nil {
		t.Error("Retention<0 must never delete")
	}
	_ = hn.Close()
}

func TestHub_SizeRoll(t *testing.T) {
	h, root, _ := openHub(t, Options{MaxFileBytes: 200})
	for i := 0; i < 6; i++ {
		h.Logf("turns", slog.LevelInfo, strings.Repeat("m", 40))
	}
	files, err := h.Files("turns")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "2026-09-22.2.log,2026-09-22.1.log,2026-09-22.log" {
		t.Errorf("rolled files = %v", names)
	}
	for _, f := range files {
		if f.Size > 200 {
			t.Errorf("%s is %d bytes, over the cap", f.Name, f.Size)
		}
	}
	// Re-open resumes on the newest segment rather than the full first one.
	_ = h.Close()
	h2, _ := Open(root, Options{MaxFileBytes: 200, Now: h.opts.Now})
	h2.Logf("turns", slog.LevelInfo, "after reopen")
	files, _ = h2.Files("turns")
	if files[0].Name == "2026-09-22.log" || !strings.Contains(readFile(t, filepath.Join(root, "turns", files[0].Name)), "after reopen") {
		t.Errorf("reopen should continue on the newest segment, wrote to %s", files[0].Name)
	}
	if strings.Contains(readFile(t, filepath.Join(root, "turns", "2026-09-22.log")), "after reopen") {
		t.Error("reopen must not go back to segment 0")
	}
	_ = h2.Close()
}

func TestHub_FilesAndOpenFileArePathSafe(t *testing.T) {
	h, root, _ := openHub(t, Options{})
	h.Logf("turns", slog.LevelInfo, "hi")
	_ = os.WriteFile(filepath.Join(root, "secret.txt"), []byte("no"), 0o644)
	files, err := h.Files("turns")
	if err != nil || len(files) != 1 || files[0].Name != "2026-09-22.log" {
		t.Fatalf("Files = %+v err=%v", files, err)
	}
	rc, fi, err := h.OpenFile("turns", "2026-09-22.log")
	if err != nil || fi.Size == 0 {
		t.Fatalf("OpenFile: %v", err)
	}
	_ = rc.Close()
	for _, bad := range [][2]string{{"turns", "../secret.txt"}, {"..", "secret.txt"}, {"turns", ".hidden.log"}, {"turns", "a/b.log"}, {"turns", "x.db"}} {
		if _, _, err := h.OpenFile(bad[0], bad[1]); err == nil {
			t.Errorf("OpenFile(%q,%q) should be rejected", bad[0], bad[1])
		}
	}
	if _, err := h.Files("../"); err == nil {
		t.Error("Files with a bad source should error")
	}
	if fs, err := h.Files("nothing"); err != nil || fs != nil {
		t.Errorf("Files on an unknown source = %v, %v", fs, err)
	}
}

func TestSourceLess_Order(t *testing.T) {
	names := []string{"zeta", "system", "discord", "alpha", "turns", "subagents"}
	want := "turns,subagents,discord,alpha,zeta,system"
	for i := 0; i < len(names); i++ {
		for j := 0; j < len(names)-1-i; j++ {
			if SourceLess(names[j+1], names[j]) {
				names[j], names[j+1] = names[j+1], names[j]
			}
		}
	}
	if got := strings.Join(names, ","); got != want {
		t.Errorf("order = %s, want %s", got, want)
	}
}

func TestHub_ClosedIsInert(t *testing.T) {
	h, _, _ := openHub(t, Options{})
	_ = h.Close()
	h.Logf("turns", slog.LevelInfo, "after close") // no panic, no write
	if all, _ := h.Snapshot(); len(all) != 0 {
		t.Error("closed hub must not append")
	}
	_ = h.Close() // idempotent
}
