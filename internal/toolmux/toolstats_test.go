package toolmux

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tenant/internal/model"
)

// The counter must see every outcome class through the REAL Dispatch path -
// success, runtime error, config error, and calls on disabled tools - because
// the health pillar treats each differently.
func TestToolStats_CountsThroughDispatch(t *testing.T) {
	dir := t.TempDir()
	mux := New()
	mux.Add("t", stubDispPlugin{})
	stats := NewStats(dir, "test")
	mux.EnableStats(stats)

	ctx := context.Background()
	mux.Dispatch(ctx, model.ToolCall{Name: "ok_tool"})
	mux.Dispatch(ctx, model.ToolCall{Name: "ok_tool"})
	mux.Dispatch(ctx, model.ToolCall{Name: "err_tool"})
	mux.Dispatch(ctx, model.ToolCall{Name: "cfg_tool"})
	if _, _, err := mux.SetEnabled("ok_tool", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	mux.Dispatch(ctx, model.ToolCall{Name: "ok_tool"})    // disabled attempt
	mux.Dispatch(ctx, model.ToolCall{Name: "ghost_tool"}) // unknown: not counted

	stats.flush()
	data, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("toolstats-test-%d.json", os.Getpid())))
	if err != nil {
		t.Fatalf("stats file: %v", err)
	}
	var f toolStatsFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("parse: %v", err)
	}

	okS := f.Tools["ok_tool"]
	if okS == nil || okS.Calls != 2 || okS.Errors != 0 || okS.DisabledCalls != 1 {
		t.Errorf("ok_tool = %+v, want 2 calls, 0 errors, 1 disabled attempt", okS)
	}
	if okS != nil && okS.LastOK == 0 {
		t.Error("ok_tool.LastOK not stamped")
	}
	errS := f.Tools["err_tool"]
	if errS == nil || errS.Errors != 1 || errS.ConfigErrors != 0 {
		t.Errorf("err_tool = %+v, want 1 runtime error", errS)
	}
	if errS != nil && errS.LastErrMsg == "" {
		t.Error("err_tool.LastErrMsg empty - the health page needs the culprit text")
	}
	cfgS := f.Tools["cfg_tool"]
	if cfgS == nil || cfgS.ConfigErrors != 1 || cfgS.Errors != 0 {
		t.Errorf("cfg_tool = %+v, want 1 config error, 0 runtime", cfgS)
	}
	if _, ok := f.Tools["ghost_tool"]; ok {
		t.Error("unknown tool must not be counted - it is not a real tool")
	}
}

// Counts survive a process bounce: a counter that resets on restart can never
// answer "used in the last 30 days".
func TestToolStats_PersistAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s1 := NewStats(dir, "test")
	s1.record("a_tool", statOK)
	s1.record("a_tool", statErr)
	s1.flush()

	s2 := NewStats(dir, "test")
	s2.record("a_tool", statOK)
	s2.flush()

	data, _ := os.ReadFile(filepath.Join(dir, fmt.Sprintf("toolstats-test-%d.json", os.Getpid())))
	var f toolStatsFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := f.Tools["a_tool"].Calls; got != 3 {
		t.Fatalf("calls after restart = %d, want 3 (2 before + 1 after)", got)
	}
	if got := f.Tools["a_tool"].Errors; got != 1 {
		t.Fatalf("errors after restart = %d, want 1", got)
	}
}

// A nil stats set is a no-op - the eval harness relies on that.
func TestToolStats_NilIsOff(t *testing.T) {
	var s *Stats
	s.record("x", statOK) // must not panic
	s.noteError("x", "boom")
	s.flush()
	stop := s.StartPersist(time.Minute)
	stop()
}

// A flush that cannot write is logged once per streak of failures (the ticker
// retries every minute), and again after a recovery.
func TestToolStats_FlushFailureLoggedOnce(t *testing.T) {
	var buf bytes.Buffer
	// SetDefault also redirects the log package; put both back.
	prev, prevOut, prevFlags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer func() { slog.SetDefault(prev); log.SetOutput(prevOut); log.SetFlags(prevFlags) }()

	dir := t.TempDir()
	s := NewStats(dir, "test")
	s.path = filepath.Join(dir, "missing", "toolstats.json") // its dir does not exist
	s.flush()
	s.flush()
	if n := strings.Count(buf.String(), "Tool stats not saved"); n != 1 {
		t.Fatalf("two failed flushes logged %d times, want once: %q", n, buf.String())
	}
	s.path = filepath.Join(dir, "toolstats.json")
	s.flush() // recovers
	s.path = filepath.Join(dir, "missing", "toolstats.json")
	s.flush()
	if n := strings.Count(buf.String(), "Tool stats not saved"); n != 2 {
		t.Errorf("a new failure after a recovery must be logged again, got %d lines", n)
	}
}

// stubDispPlugin serves three tools with distinct outcomes.
type stubDispPlugin struct{}

func (stubDispPlugin) Tools() []model.ToolSpec {
	return []model.ToolSpec{{Name: "ok_tool"}, {Name: "err_tool"}, {Name: "cfg_tool"}}
}

func (stubDispPlugin) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	switch call.Name {
	case "err_tool":
		return "command failed: exit status 1", true, nil
	case "cfg_tool":
		return "this plugin is not configured — run /configure", true, nil
	}
	return "done", false, nil
}

// A dead sibling's counts get absorbed exactly once; a live sibling's fresh
// file is left alone. This is what makes per-PID files safe: two concurrent
// bridges never clobber each other, and restarts keep history.
func TestToolStats_AbsorbsDeadSiblings(t *testing.T) {
	dir := t.TempDir()
	stale := toolStatsFile{Schema: 1, Role: "test",
		Updated: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		Tools:   map[string]*toolStat{"a_tool": {Calls: 5, Errors: 2}}}
	staleData, _ := json.Marshal(stale)
	os.WriteFile(filepath.Join(dir, "toolstats-test-99999.json"), staleData, 0o644)

	fresh := stale
	fresh.Updated = time.Now().UTC().Format(time.RFC3339)
	fresh.Tools = map[string]*toolStat{"b_tool": {Calls: 7}}
	freshData, _ := json.Marshal(fresh)
	os.WriteFile(filepath.Join(dir, "toolstats-test-88888.json"), freshData, 0o644)

	s := NewStats(dir, "test")
	s.record("a_tool", statOK)
	s.flush()

	data, _ := os.ReadFile(filepath.Join(dir, fmt.Sprintf("toolstats-test-%d.json", os.Getpid())))
	var f toolStatsFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := f.Tools["a_tool"]; got == nil || got.Calls != 6 || got.Errors != 2 {
		t.Errorf("a_tool = %+v, want dead sibling's 5 calls + 1 new = 6, errors 2", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "toolstats-test-99999.json")); !os.IsNotExist(err) {
		t.Error("dead sibling file should be deleted after absorption")
	}
	if _, err := os.Stat(filepath.Join(dir, "toolstats-test-88888.json")); err != nil {
		t.Error("LIVE sibling file must be left alone")
	}
	if _, ok := f.Tools["b_tool"]; ok {
		t.Error("live sibling's counts must not be absorbed")
	}
}

// Cancellation is not tool failure: an interrupted long call must not raise
// the tool's error rate.
func TestClassifyDispatch_CanceledIsNotError(t *testing.T) {
	if got := classifyDispatch("", true, context.Canceled); got != statCanceled {
		t.Fatalf("classify(context.Canceled) = %q, want canceled", got)
	}
	s := NewStats(t.TempDir(), "test")
	s.record("slow_tool", statCanceled)
	s.mu.Lock()
	st := s.tools["slow_tool"]
	s.mu.Unlock()
	if st.Errors != 0 || st.Canceled != 1 || st.Calls != 1 {
		t.Fatalf("stat = %+v, want 1 call, 1 canceled, 0 errors", st)
	}
}
