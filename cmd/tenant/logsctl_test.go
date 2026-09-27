package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/datalock"
	"tenant/internal/lifecycle"
	"tenant/internal/logs"
	"tenant/internal/logs/event"
	"tenant/internal/logs/logapi"
	"tenant/internal/logs/store"
	"tenant/internal/runtrace"
)

// Every read command's output in each of the three formats, pinned in
// testdata/logs.<format>.golden (go test ./cmd/tenant -run TestLogsOutputGolden -update).
func TestLogsOutputGolden(t *testing.T) {
	at := time.Date(2026, 9, 27, 14, 3, 11, 123_000_000, time.UTC)
	day := at.Add(-24 * time.Hour)
	evs := []event.Event{
		{Seq: 41, Log: "discord", Provider: "discord", ID: 2001, Time: at, Level: event.Warning, Task: "Connection", Opcode: event.OpFail,
			Msg: "Relay disconnected: gateway reset", Data: []event.Field{{Key: "reason", Value: "reset"}, {Key: "retry", Value: 5 * time.Second}},
			Corr: event.Correlation{ActivityID: "5d2a0000000000000000000000000000"}, Proc: event.Process{PID: 4412, BootID: "7c1e0000000000000000000000000000", Host: "box"}},
		{Seq: 42, Log: "ext.deploy", Provider: "ext.deploy", Time: at.Add(time.Second), Level: event.Info, Keywords: []string{"external"},
			Msg: "build started", Data: []event.Field{{Key: "commit", Value: "abc123"}, {Key: "token", Value: event.Masked, Sensitive: true}}},
	}
	sections := []struct {
		name string
		run  func(o logsOut) error
	}{
		{"logs", func(o logsOut) error {
			return o.logs([]logapi.Log{{Name: "discord", Group: []string{"Subsystems", "Channels"}, Events: 1234, Bytes: 3 << 20,
				Policy: logapi.Policy{MaxBytes: 64 << 20, MaxAgeS: 30 * 86400, OnFull: "overwrite", Enabled: true}, LastWrite: &at, Dropped: 2, DebugUntil: &at},
				{Name: "security", Group: []string{"Tenant"}, Events: 7, Bytes: 2048,
					Policy: logapi.Policy{MaxBytes: 256 << 20, MaxAgeS: 365 * 86400, OnFull: "archive", MinLevel: "", Enabled: true}, Cleared: &day}})
		}},
		{"query", func(o logsOut) error {
			ew, err := o.events()
			if err != nil {
				return err
			}
			for _, e := range evs {
				if err := ew.Write(e); err != nil {
					return err
				}
			}
			return ew.Flush()
		}},
		{"show", func(o logsOut) error {
			return o.detail(logapi.EventDetail{Event: evs[0], Help: "### discord 2001\n\n**What to do.** Nothing; it retries.\n",
				Stats: &logapi.IDStats{Count: 14, First: &day, Last: &at, Trend: [7]int64{0, 0, 1, 2, 3, 4, 4}}})
		}},
		{"trace", func(o logsOut) error {
			return o.timeline(logapi.Timeline{ID: "3f2a0000000000000000000000000000", Events: evs[:1],
				Run: &runtrace.Run{TraceID: "3f2a0000000000000000000000000000", Source: "discord", Model: "m", Provider: "p", LatencyMS: 900,
					PromptTokens: 10, CompletionTokens: 5, Outcome: "ok", ToolCalls: []runtrace.ToolCall{{Name: "web_search", Outcome: "ok"}}}})
		}},
		{"stats", func(o logsOut) error {
			return o.stats(logapi.Stats{Buckets: []logapi.Bucket{{Start: &day, Log: "discord", Level: "warning", N: 3}, {Start: &at, Log: "discord", Level: "error", N: 1}}},
				logapi.Top{Top: []logapi.IDCount{{Provider: "discord", ID: 2001, N: 3}}})
		}},
		{"catalog", func(o logsOut) error {
			return o.catalog(logapi.Catalog{Version: "abc123def456", Markdown: "### system 3101\n\n`{component} failed: {err}`\n",
				Specs: []logapi.Spec{{Provider: "system", Log: "system", ID: 3101, Level: "error", Channel: "operational", Task: "Component",
					Template: "{component} failed: {err}", Filter: "provider:system id:3101"}}})
		}},
		{"components", func(o logsOut) error {
			return o.components([]logapi.Component{{Name: "discord relay", Kind: lifecycle.Connector, Log: "discord", State: lifecycle.Degraded,
				Since: at, Started: day, Error: "gateway reset"}})
		}},
		{"verify", func(o logsOut) error {
			return o.verify(logapi.Verify{Log: "security", OK: false, Chain: &logapi.Chain{Records: 9, Head: 9, Archives: 1, Notes: []string{"no witness"},
				Break: "record 7 (logbook.db): hash mismatch"}})
		}},
	}
	for _, format := range []string{"text", "json", "csv"} {
		var b bytes.Buffer
		for _, s := range sections {
			b.WriteString("== " + s.name + " ==\n")
			if err := s.run(logsOut{w: &b, format: format, loc: time.UTC}); err != nil {
				t.Fatalf("%s %s: %v", format, s.name, err)
			}
		}
		path := filepath.Join("testdata", "logs."+format+".golden")
		if *update {
			if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v (run with -update to create it)", err)
		}
		if got := b.String(); got != strings.ReplaceAll(string(want), "\r\n", "\n") {
			t.Errorf("%s output drifted from %s (run with -update if intended):\n%s", format, path, got)
		}
	}
}

// logsBook is a data dir with a log book of a few events, as a serve that
// has run leaves it, and a config dir whose dashboard answers nowhere.
func logsBook(t *testing.T) (data, cfg string, st *store.Store) {
	t.Helper()
	data, cfg = t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(cfg, "config.json"), []byte(`{"dashboard":{"addr":"127.0.0.1:1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(data, "logs", "logbook.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	at := time.Now().UTC().Add(-time.Minute)
	if err := st.Commit(context.Background(), []event.Event{
		{Log: "cron", Provider: "cron", Time: at, Level: event.Info, Msg: "job fired", Data: []event.Field{{Key: "job", Value: "nightly"}}},
		{Log: "system", Provider: "system", ID: 3101, Time: at, Level: event.Error, Task: "Component", Msg: "discord failed: gone",
			Data: []event.Field{{Key: "component", Value: "discord"}, {Key: "err", Value: "gone"}, {Key: "key", Value: "hunter2", Sensitive: true}}},
		{Log: "cron", Provider: "cron", Time: at, Level: event.Warning, Msg: "job slow"},
	}); err != nil {
		t.Fatal(err)
	}
	return data, cfg, st
}

func logsRun(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var b bytes.Buffer
	err := runLogs(context.Background(), args, &b)
	return b.String(), err
}

// Read here or through a dashboard, the answers are the same: one API.
func TestLogsLocalAndRemoteAgree(t *testing.T) {
	data, cfg, _ := logsBook(t)
	root := filepath.Join(data, "logs")
	h, err := logs.Open(root, logs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(root, "logbook.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h.AttachStore(st)
	defer h.Close()
	ts := httptest.NewServer((&logapi.API{Hub: func() *logs.Hub { return h }}).Handler())
	defer ts.Close()
	srv := ts.URL
	for _, args := range [][]string{
		{"--format", "json"},
		{"query", "log:cron", "--format", "json"},
		{"query", "level>=warning", "--format", "csv", "--reverse"},
		{"query", "--format", "text", "--limit", "2"},
		{"show", "2", "--format", "json"},
		{"stats", "log:cron", "--format", "csv"},
		{"trace", "0123456789abcdef0123456789abcdef", "--format", "csv"},
		{"catalog", "system", "--id", "3101"},
		{"config", "cron", "--format", "json"},
	} {
		base := append(args[:len(args):len(args)], "--data", data, "--config", cfg)
		local, err := logsRun(t, base...)
		if err != nil {
			t.Fatalf("local %v: %v", args, err)
		}
		remote, err := logsRun(t, append(base, "--url", srv)...)
		if err != nil {
			t.Fatalf("remote %v: %v", args, err)
		}
		if local != remote {
			t.Errorf("%v differs:\n local:  %s\n remote: %s", args, local, remote)
		}
		if local == "" {
			t.Errorf("%v printed nothing", args)
		}
	}
	out, _ := logsRun(t, "show", "2", "--data", data, "--config", cfg)
	if !strings.Contains(out, "data.key     «sensitive»") || strings.Contains(out, "hunter2") || !strings.Contains(out, "What to do") {
		t.Errorf("show:\n%s", out)
	}
}

// The commands that change the log book, read here: config, debug, clear
// with an export first, export with its header, verify, and the ones that
// can't answer without serve.
func TestLogsCommandsLocally(t *testing.T) {
	data, cfg, st := logsBook(t)
	run := func(args ...string) string {
		t.Helper()
		out, err := logsRun(t, append(args, "--data", data, "--config", cfg)...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return out
	}
	if out := run("config", "cron", "--max-size", "32MB", "--max-age", "7d", "--min-level", "warning"); !strings.Contains(out, "32.0 MB") || !strings.Contains(out, "7d") || !strings.Contains(out, "warning") {
		t.Errorf("config:\n%s", out)
	}
	if p, _ := st.Policy(context.Background(), store.DefaultBudget(), "cron"); p.MaxBytes != 32<<20 || p.MaxAge != 7*24*time.Hour || p.MinLevel != event.Warning {
		t.Errorf("stored policy = %+v", p)
	}
	if out := run("debug", "cron", "--for", "30m"); !strings.Contains(out, "cron: its verbose events are written until") {
		t.Errorf("debug: %s", out)
	}
	if on, _ := st.Debugging(context.Background()); on["cron"].IsZero() {
		t.Error("the debug switch is not on")
	}
	export := filepath.Join(t.TempDir(), "cron.ndjson")
	if out := run("clear", "cron", "--export", export); !strings.Contains(out, "cleared 2 events from cron") {
		t.Errorf("clear: %s", out)
	}
	b, _ := os.ReadFile(export)
	if lines := strings.Split(strings.TrimSpace(string(b)), "\n"); len(lines) != 3 || !strings.HasPrefix(lines[0], `{"export":{"schema":`) {
		t.Errorf("the export before the clear:\n%s", b)
	}
	csvOut := filepath.Join(t.TempDir(), "all.csv")
	run("export", "--out", csvOut, "--format", "csv", "--include-sensitive")
	if b, _ := os.ReadFile(csvOut); !strings.HasPrefix(string(b), "# tenant logbook export: schema=") || !strings.Contains(string(b), "hunter2") {
		t.Errorf("csv export:\n%s", b)
	}
	if fi, err := os.Stat(csvOut); err == nil && fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Errorf("an export is owner-only, not %v", fi.Mode().Perm())
	}
	if out := run("verify"); !strings.Contains(out, "the integrity check passed") {
		t.Errorf("verify: %s", out)
	}
	if out := run("verify", "security"); !strings.Contains(out, "note: the Security log's chain was not checked: no Security log key") {
		t.Errorf("verify security: %s", out)
	}
	for _, args := range [][]string{{"components"}, {"recover"}, {"export"}, {"show"}, {"nosuch"}, {"query", "level>=loud"}} {
		if _, err := logsRun(t, append(args, "--data", data, "--config", cfg)...); err == nil {
			t.Errorf("%v should fail", args)
		}
	}
}

// boot:current, read here, is the running serve's run: its row in the
// boots table whose lock is held; with no serve, the running TUI's.
func TestLogsBootCurrentIsTheServingProcess(t *testing.T) {
	data, cfg, st := logsBook(t)
	root := filepath.Join(data, "logs")
	ctx := context.Background()
	serve, tui := runtrace.NewID(), runtrace.NewID()
	if err := os.MkdirAll(filepath.Join(root, "boots"), 0o700); err != nil {
		t.Fatal(err)
	}
	locks := map[string]*datalock.Lock{}
	for id, role := range map[string]string{serve: "serve", tui: "tui"} {
		l, err := datalock.ExclusiveFile(filepath.Join(root, "boots", id+".lock"))
		if err != nil {
			t.Fatal(err)
		}
		locks[id] = l
		if err := st.BeginBoot(ctx, store.Boot{ID: id, PID: 1, Role: role, Started: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		if err := st.Commit(ctx, []event.Event{{Log: role, Provider: role, Level: event.Info, Msg: "from " + role, Proc: event.Process{PID: 1, BootID: id}}}); err != nil {
			t.Fatal(err)
		}
	}
	defer locks[tui].Release()
	if id, _ := servingBoot(filepath.Join(root, "logbook.db"), root); id != serve {
		t.Errorf("servingBoot = %s, want serve's %s", id, serve)
	}
	if out, err := logsRun(t, "query", "boot:current", "--data", data, "--config", cfg); err != nil || !strings.Contains(out, "from serve") || strings.Contains(out, "from tui") {
		t.Errorf("boot:current = %q, %v", out, err)
	}
	_ = locks[serve].Release()
	if id, _ := servingBoot(filepath.Join(root, "logbook.db"), root); id != tui {
		t.Errorf("with serve gone, servingBoot = %s, want the TUI's %s", id, tui)
	}
}

// query --follow prints the newest events, then each new one as it is
// committed, through the API's live tail run in this process.
func TestLogsQueryFollow(t *testing.T) {
	data, cfg, st := logsBook(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- runLogs(ctx, []string{"query", "log:cron", "--follow", "--data", data, "--config", cfg}, out)
	}()
	deadline := time.Now().Add(10 * time.Second)
	committed := false
	for !strings.Contains(out.String(), "live one") {
		if !committed && strings.Contains(out.String(), "job slow") {
			if err := st.Commit(context.Background(), []event.Event{{Log: "cron", Provider: "cron", Level: event.Info, Msg: "live one"},
				{Log: "system", Provider: "system", Level: event.Info, Msg: "not cron"}}); err != nil {
				t.Fatal(err)
			}
			committed = true
		}
		if time.Now().After(deadline) {
			t.Fatalf("follow printed:\n%s", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("follow = %v", err)
	}
	if s := out.String(); strings.Contains(s, "not cron") || strings.Index(s, "job fired") > strings.Index(s, "live one") {
		t.Errorf("follow printed:\n%s", s)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"0": 0, "512": 512, "64MB": 64 << 20, "1.5 GB": 3 << 29, "10kb": 10 << 10} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v", in, got, err)
		}
	}
	if _, err := parseSize("lots"); err == nil {
		t.Error("parseSize(lots) should fail")
	}
}

// attach with no URL reads the log book here when it can (its dashboard
// answers nowhere in this config, so the URL form would fail).
func TestAttachWithNoURLReadsTheLogBook(t *testing.T) {
	data, cfg, _ := logsBook(t)
	if err := cmdAttach(context.Background(), []string{"--data", data, "--config", cfg, "--source", "cron", "--level", "warn"}); err != nil {
		t.Fatal(err)
	}
	if q := attachFilter("Cron", "warn", `t"1`); q != `channel:operational log:cron level>=warning trace:"t\"1"` {
		t.Errorf("attachFilter = %s", q)
	}
}
