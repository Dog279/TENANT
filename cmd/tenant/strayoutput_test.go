package main

import (
	"context"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/agent"
	"tenant/internal/httplimits"
	"tenant/internal/logs"
	"tenant/internal/memory/soul"
	"tenant/internal/runtrace"
)

// keepLogDefaults restores what openLogHub and openOneShotLog change: the
// slog and log package defaults, the installed hub and the For fallback.
func keepLogDefaults(t *testing.T) {
	t.Helper()
	prevSlog, prevHub := slog.Default(), logs.Default()
	prevW, prevF := log.Writer(), log.Flags()
	t.Cleanup(func() {
		slog.SetDefault(prevSlog)
		log.SetOutput(prevW)
		log.SetFlags(prevF)
		logs.Install(prevHub)
		logs.SetFallback(nil)
		commandLog = nil
	})
}

// hasEntry waits briefly: with logbook.db attached, the ring is fed after
// each batch commits (every 100 ms).
func hasEntry(h *logs.Hub, source, msg string) bool {
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		entries, _ := h.Snapshot()
		for _, e := range entries {
			if e.Source == source && strings.Contains(e.Msg, msg) {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// Serve and the TUI point slog.Default, the log package and every
// http.Server's error log at the hub: a library's line is filed, never
// printed over the TUI's screen (TEN-363).
func TestOpenLogHubPointsTheDefaultsAtTheHub(t *testing.T) {
	keepLogDefaults(t)
	hub, _, err := openLogHub(&commonFlags{dataDir: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	if logs.Default() != hub {
		t.Fatal("the hub is not installed")
	}
	log.Printf("chromedp: a library's stray line")
	slog.Warn("dashboard: a slog.Default line")
	srv := &http.Server{}
	httplimits.Configure(srv)
	srv.ErrorLog.Printf("http: TLS handshake error from 10.0.0.9:5555: EOF")
	for _, want := range []struct{ source, msg string }{
		{"system", "a library's stray line"},
		{"dashboard", "a slog.Default line"},
		{"system", "TLS handshake error"},
	} {
		if !hasEntry(hub, want.source, want.msg) {
			t.Errorf("no %q line under %s", want.msg, want.source)
		}
	}
}

// A log book that cannot be opened still leaves an installed memory hub, so
// logs.For loggers keep off the terminal, and says why.
func TestOpenLogHubFallsBackToAnInstalledMemoryHub(t *testing.T) {
	keepLogDefaults(t)
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	hub, _, err := openLogHub(&commonFlags{dataDir: t.TempDir(), logDir: filepath.Join(file, "logs")}, nil)
	if err == nil {
		t.Fatal("no error for a log root under a file")
	}
	if hub.Root() != "" || logs.Default() != hub {
		t.Fatalf("want an installed memory hub, got root %q, installed %v", hub.Root(), logs.Default() == hub)
	}
	logs.For("memory").Warn("memory: kept in memory")
	if !hasEntry(hub, "memory", "kept in memory") {
		t.Error("a For line did not reach the memory hub")
	}
}

// A status line is filed before the feed is offered it: a full feed drops
// only its own copy.
func TestFeedLineFilesALineTheFeedDrops(t *testing.T) {
	hub := logs.NewMemory(10)
	feedLine(hub, make(chan string), "improve", "improve: distill ok") // no reader: always full
	if !hasEntry(hub, "improve", "distill ok") {
		t.Fatal("the line was lost with the feed's copy")
	}
}

// A one-shot command files its warnings and errors (not its info) in the log
// book and still prints them on stderr.
func TestOneShotLogFilesWarningsAndPrintsThem(t *testing.T) {
	keepLogDefaults(t)
	stderr, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	prevStderr := os.Stderr
	os.Stderr = stderr
	defer func() { os.Stderr = prevStderr }()

	c := &commonFlags{dataDir: t.TempDir(), cfgDir: t.TempDir()}
	openOneShotLog(c)
	if commandLog == nil {
		t.Fatal("no log book opened")
	}
	slog.Info("one-shot: info stays out")
	newLogger().Warn("one-shot: warning filed")
	logs.For("memory").Error("memory: error filed")
	closeCommandLog()

	filed := map[string]string{} // source folder → its files
	_ = filepath.WalkDir(filepath.Join(c.dataDir, "logs"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			filed[filepath.Base(filepath.Dir(p))] += string(b)
		}
		return nil
	})
	printed, _ := os.ReadFile(stderr.Name())
	for source, want := range map[string]string{"system": "warning filed", "memory": "error filed"} {
		if !strings.Contains(filed[source], want) {
			t.Errorf("%q not filed under %s: %v", want, source, filed)
		}
		if !strings.Contains(string(printed), want) {
			t.Errorf("%q not printed:\n%s", want, printed)
		}
	}
	if strings.Contains(filed["system"], "info stays out") {
		t.Errorf("info was filed: %v", filed)
	}
}

// Under serve or the TUI the eval harness's notes go to the eval log, not
// stderr.
func TestEvalNoteFilesUnderALongRunningSurface(t *testing.T) {
	prev := logs.Default()
	hub := logs.NewMemory(10)
	logs.Install(hub)
	t.Cleanup(func() { logs.Install(prev) })
	evalNote("judge = planner model %q", "m")
	if !hasEntry(hub, "eval", `eval: judge = planner model "m"`) {
		t.Fatal("the note was not filed under eval")
	}
}

// The reconnect monitor's feed lines reach the log book too, under its logger.
func TestReconnectMonitorFilesItsFeedLines(t *testing.T) {
	hub := logs.NewMemory(10)
	feed := make(chan string, 1)
	rm := &reconnectMonitor{feed: feed, ctx: context.Background(), log: hub.Logger("model")}
	rm.emit("⟳ reconnect attempt 1 failed")
	if !hasEntry(hub, "model", "reconnect attempt 1 failed") || len(feed) != 1 {
		t.Fatal("want the line both filed and fed")
	}
}

// A sub-agent's "agent: …" lines file under subagents, not the main agent's
// turns.
func TestSubAgentLinesFileUnderSubagents(t *testing.T) {
	hub := logs.NewMemory(10)
	r := newTeamRuntime(TeamConfig{Log: slog.New(hub.Handler(nil))})
	r.cfg.Log.Warn("agent: embed query failed")
	if !hasEntry(hub, "subagents", "embed query failed") {
		t.Fatal("the sub-agent's line did not land in subagents")
	}
}

// The iMessage agent mirrors its turns like the Discord relay's: the log book
// and the dashboard see what a texted turn did.
func TestIMessageAgentMirrorsItsTurns(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := &commonFlags{backend: "echo", agent: "main", dataDir: t.TempDir(), cfgDir: t.TempDir()}
	router, err := buildRouter(c, log)
	if err != nil {
		t.Fatal(err)
	}
	live := &soul.Live{}
	live.Store(soul.NewDefault("main"))
	runs, err := runtrace.Open(":memory:", runtrace.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer runs.Close()
	var mu sync.Mutex
	var events []agent.Event
	mux := newToolMux()
	ag, err := buildIMessageAgent(imessageAgentDeps{
		router: router, soulLive: live, runs: runs, fullTools: mux, fullDisp: mux, log: log,
		events: func(e agent.Event) { mu.Lock(); events = append(events, e); mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ag.Turn(context.Background(), agent.TurnRequest{UserQuery: "hi"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	sawFinal := false
	for _, e := range events {
		if e.Agent != "tenant-imessage" || e.Source != "imessage" {
			t.Errorf("event %v attributed to %q/%q", e.Kind, e.Agent, e.Source)
		}
		sawFinal = sawFinal || e.Kind == agent.EventFinal
	}
	if !sawFinal {
		t.Fatal("no EventFinal: the iMessage turn stayed invisible")
	}
}
