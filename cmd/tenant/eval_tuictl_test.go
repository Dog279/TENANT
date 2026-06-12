package main

// eval_tuictl_test.go covers the /eval TUI control (TEN-196): live re-arming
// through the evalSchedule holder, config persistence, and the force-once
// path. The scenario being protected: an operator flips the schedule from
// the TUI and it both takes effect on the running scheduler AND survives a
// relaunch via config.json.

import (
	"strings"
	"testing"
	"time"
)

// The dynamic predicate honors live schedule swaps: off never fires, an
// armed interval fires on a stale clock, and ForceOnce fires exactly once
// even while the schedule is off.
func TestEvalSchedule_DynamicPredicate(t *testing.T) {
	s := newEvalSchedule(nil, "off")
	due := s.DueFunc()
	now := time.Now()
	stale := now.Add(-48 * time.Hour)

	if due(stale, now) {
		t.Error("off schedule must never fire")
	}
	s.set(evalEveryDue(24*time.Hour), "every 24h")
	if !due(stale, now) {
		t.Error("armed 24h schedule must fire on a 48h-old clock")
	}
	if due(now.Add(-time.Hour), now) {
		t.Error("armed 24h schedule must NOT fire on a 1h-old clock")
	}
	s.set(nil, "off")
	s.ForceOnce()
	if !due(now, now) {
		t.Error("ForceOnce must fire even while off")
	}
	if due(now, now) {
		t.Error("ForceOnce must be consumed by exactly one fire")
	}
}

// Schedule mutations persist to launchConfig (the relaunch half) and re-arm
// the live holder (the in-session half). SetEvery clears eval_at — the
// anchor would otherwise win again at next launch.
func TestEvalTUIControl_PersistAndRearm(t *testing.T) {
	tmp := t.TempDir()
	sched := newEvalSchedule(nil, "off")
	ctl := evalTUIControl{sched: sched, cfgDir: tmp, dataDir: tmp}

	if _, err := ctl.SetAt("3:15"); err != nil {
		t.Fatalf("SetAt: %v", err)
	}
	lc, _ := loadLaunchConfig(tmp)
	if lc.Improve.EvalAt != "03:15" {
		t.Fatalf("EvalAt = %q, want canonical 03:15", lc.Improve.EvalAt)
	}
	if sched.Desc() != "daily at 03:15" {
		t.Fatalf("live desc = %q, want daily at 03:15", sched.Desc())
	}

	if _, err := ctl.SetEvery("6h"); err != nil {
		t.Fatalf("SetEvery: %v", err)
	}
	lc, _ = loadLaunchConfig(tmp)
	if lc.Improve.EvalEvery != "6h" || lc.Improve.EvalAt != "" {
		t.Fatalf("after SetEvery: EvalEvery=%q EvalAt=%q, want 6h and cleared anchor", lc.Improve.EvalEvery, lc.Improve.EvalAt)
	}
	if sched.Desc() != "every 6h0m0s" {
		t.Fatalf("live desc = %q, want every 6h0m0s", sched.Desc())
	}

	if _, err := ctl.Off(); err != nil {
		t.Fatalf("Off: %v", err)
	}
	lc, _ = loadLaunchConfig(tmp)
	if lc.Improve.EvalEvery != "" || lc.Improve.EvalAt != "" {
		t.Fatalf("after Off: EvalEvery=%q EvalAt=%q, want both empty", lc.Improve.EvalEvery, lc.Improve.EvalAt)
	}
	if sched.Desc() != "off" {
		t.Fatalf("live desc = %q, want off", sched.Desc())
	}

	if _, err := ctl.SetEvery("soon"); err == nil {
		t.Error("garbage interval must error")
	}
	if _, err := ctl.SetAt("25:99"); err == nil {
		t.Error("garbage anchor must error")
	}
}

// Without a scheduler this session (--self-improve=false), mutations still
// persist for the next launch and say so; RunNow is refused.
func TestEvalTUIControl_PersistOnlySession(t *testing.T) {
	tmp := t.TempDir()
	ctl := evalTUIControl{sched: nil, cfgDir: tmp, dataDir: tmp}

	msg, err := ctl.SetAt("03:15")
	if err != nil {
		t.Fatalf("SetAt: %v", err)
	}
	if !strings.Contains(msg, "next launch") {
		t.Errorf("persist-only message should say it applies next launch, got %q", msg)
	}
	if lc, _ := loadLaunchConfig(tmp); lc.Improve.EvalAt != "03:15" {
		t.Error("persist-only SetAt must still write config")
	}
	if !strings.Contains(ctl.Status(), "daily at 03:15") {
		t.Errorf("Status should show the persisted schedule, got %q", ctl.Status())
	}
	if _, err := ctl.RunNow(); err == nil {
		t.Error("RunNow without a scheduler must be refused")
	}
}

// RunNow queues exactly one forced fire on the live holder.
func TestEvalTUIControl_RunNow(t *testing.T) {
	sched := newEvalSchedule(nil, "off")
	ctl := evalTUIControl{sched: sched, cfgDir: t.TempDir(), dataDir: t.TempDir()}
	if _, err := ctl.RunNow(); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	due := sched.DueFunc()
	now := time.Now()
	if !due(now, now) {
		t.Error("RunNow must arm a one-shot fire")
	}
	if due(now, now) {
		t.Error("the one-shot must not fire twice")
	}
}

// Status surfaces the last recorded run from the trend log.
func TestEvalTUIControl_StatusShowsLastRun(t *testing.T) {
	tmp := t.TempDir()
	appendEvalTrend(tmp+"/eval-artifacts", evalTrendEntry{
		TS: time.Now().UTC().Format(time.RFC3339), Subset: "full",
		Overall: 91.5, Passed: 11, Total: 12, HasBaseline: true, Regressed: false,
	}, nil)
	ctl := evalTUIControl{sched: newEvalSchedule(nil, "off"), cfgDir: tmp, dataDir: tmp}
	st := ctl.Status()
	if !strings.Contains(st, "full 91.5 (11/12)") || !strings.Contains(st, "ok") {
		t.Errorf("Status should show the last run with verdict, got:\n%s", st)
	}
}
