package main

import (
	"errors"
	"strings"
	"testing"

	"tenant/internal/dashboard"
)

// The Quality page's words (TEN-211): queuing a check names neither the
// feed nor trend.jsonl, and a Tenant without the improve scheduler says so
// plainly instead of naming a flag.
func TestDashEval_RunNowIsPlain(t *testing.T) {
	d := dashEval{ev: evalTUIControl{sched: newEvalSchedule(nil, "off"), cfgDir: t.TempDir(), dataDir: t.TempDir()}}
	for _, want := range []string{"Quality check queued.", "A quality check is already queued."} {
		msg, err := d.RunNow()
		if err != nil || !strings.HasPrefix(msg, want) || strings.Contains(msg, "trend.jsonl") || strings.Contains(msg, "feed") {
			t.Errorf("RunNow = (%q, %v), want %q in plain words", msg, err, want)
		}
	}
	off := dashEval{ev: evalTUIControl{cfgDir: t.TempDir(), dataDir: t.TempDir()}}
	var pe dashboard.PlainError
	if _, err := off.RunNow(); !errors.As(err, &pe) || strings.Contains(err.Error(), "--self-improve") {
		t.Errorf("no scheduler: %v, want a PlainError", err)
	}
}

func TestDashEval_JudgeStatusIsPlain(t *testing.T) {
	dir := t.TempDir()
	d := dashEval{quality: qualityModelCtl{cfgDir: dir, planner: "local"}}
	if got := d.JudgeStatus(); got != "Your main model grades the checks." {
		t.Errorf("default grader: %q", got)
	}
	if _, err := d.SetJudge("anthropic", "claude-grader", ""); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "")
	got := d.JudgeStatus()
	if !strings.HasPrefix(got, "claude-grader from anthropic grades the checks.") || strings.ContainsAny(got, "`/") {
		t.Errorf("set grader: %q", got)
	}
}

// C-7: the Quality page and the terminal's /eval model read one resolved setting
// (qualityModelCtl.resolved), so they agree on the kind and on the missing key.
func TestDashEval_JudgeStatusFollowsTheTerminal(t *testing.T) {
	d := dashEval{quality: qualityModelCtl{cfgDir: t.TempDir(), planner: "local"}}
	if _, err := d.SetJudge("anthropic", "claude-grader", ""); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", "sk-test"} {
		t.Setenv("ANTHROPIC_API_KEY", key)
		page, term := d.JudgeStatus(), d.quality.Current()
		if missing := key == ""; strings.Contains(page, "isn't set") != missing || strings.Contains(term, "not set") != missing ||
			!strings.Contains(page, "from anthropic") || !strings.Contains(term, "Quality model: anthropic") {
			t.Errorf("key %q: page %q, terminal %q", key, page, term)
		}
	}
}
