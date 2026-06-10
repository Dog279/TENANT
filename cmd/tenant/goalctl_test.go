package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Set / Show / Clear lifecycle without invoking the judge (we use a fake
// agent that never fires LLM calls — this test exercises pure state).

func TestGoalControl_Lifecycle(t *testing.T) {
	gc := &goalControl{maxTurns: 5} // no ag — Judge isn't called in this test

	// Empty: not active, Show shows the empty state, Clear is a no-op msg.
	if gc.Active() {
		t.Error("fresh goalControl should not be active")
	}
	if st := gc.Show(); st.Active {
		t.Errorf("fresh Show.Active should be false: %+v", st)
	}
	if msg := gc.Clear(); !strings.Contains(msg, "no goal") {
		t.Errorf("Clear on empty: %q", msg)
	}

	// Set with empty condition errors.
	if _, _, err := gc.Set(context.Background(), "   "); err == nil {
		t.Error("Set(empty) should error")
	}

	// Set + Show.
	first, status, err := gc.Set(context.Background(), "write a unit test for the new feature")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !strings.Contains(first, "Goal: write a unit test") {
		t.Errorf("first prompt missing condition: %q", first)
	}
	if !strings.Contains(status, "🎯 goal set") {
		t.Errorf("status wrong: %q", status)
	}
	if !gc.Active() {
		t.Error("Active should be true after Set")
	}
	st := gc.Show()
	if !st.Active || st.Condition == "" || st.MaxTurns != 5 {
		t.Errorf("Show snapshot wrong: %+v", st)
	}

	// Clear.
	msg := gc.Clear()
	if !strings.Contains(msg, "goal cleared") {
		t.Errorf("Clear msg: %q", msg)
	}
	if gc.Active() {
		t.Error("Active should be false after Clear")
	}
}

// Set REPLACES any prior goal cleanly (turns reset, judge state cleared).
func TestGoalControl_SetReplaces(t *testing.T) {
	gc := &goalControl{maxTurns: 5}
	_, _, _ = gc.Set(context.Background(), "old goal")
	gc.turns = 3
	gc.lastJudge = "stale"
	gc.met = true
	_, _, _ = gc.Set(context.Background(), "new goal")
	st := gc.Show()
	if st.Condition != "new goal" || st.Turns != 0 || st.LastJudge != "" || st.Met {
		t.Errorf("Set didn't reset state: %+v", st)
	}
}

// Continue builds a user-message-shaped prompt; folds the judge's reason
// when non-empty, generic continuation when blank. Both shapes carry the
// "action not narration" pressure (live trigger: agent burned multiple
// turns meta-commenting instead of producing artifacts).
func TestGoalControl_Continue(t *testing.T) {
	gc := &goalControl{}
	cases := []struct {
		reason, mustContain string
	}{
		{"", "DO the next thing"},
		{"   ", "DO the next thing"},
		{"the test is missing an assertion", "the test is missing an assertion"},
	}
	for _, c := range cases {
		p := gc.Continue(c.reason)
		if !strings.Contains(p, c.mustContain) {
			t.Errorf("Continue(%q) missing %q: %q", c.reason, c.mustContain, p)
		}
		// Always carries the "Goal" framing so the agent knows context.
		if !strings.Contains(p, "Goal") {
			t.Errorf("Continue missing 'Goal' framing: %q", p)
		}
		// Anti-narration pressure must be present — this is what stops the
		// agent from looping on meta-commentary instead of producing
		// artifacts. Drift guard: any future refactor must preserve this.
		anyPressure := strings.Contains(p, "ACTION") ||
			strings.Contains(p, "DO ") ||
			strings.Contains(p, "artifact")
		if !anyPressure {
			t.Errorf("Continue lost the action-pressure phrasing: %q", p)
		}
	}
}

// Set's first prompt MUST push for action + an artifact, not "make a step"
// (which the model reads as "plan a step"). Drift guard.
func TestGoalControl_FirstPromptPushesForAction(t *testing.T) {
	gc := &goalControl{maxTurns: 5}
	first, _, err := gc.Set(context.Background(), "draft a one-page proposal")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	for _, want := range []string{
		"DO ONE THING",    // explicit action verb
		"ARTIFACT",        // demands output, not plan
		"Forbidden",       // names the failure mode explicitly
		"meta-commentary", // names the anti-pattern
		"tools",           // tells the agent HOW (use tools)
	} {
		if !strings.Contains(first, want) {
			t.Errorf("first prompt missing anti-narration cue %q:\n%s", want, first)
		}
	}
}

// extractNotMetReason handles the canonical shape + variant casing + extra
// prose before the verdict.
func TestExtractNotMetReason(t *testing.T) {
	cases := []struct{ in, want string }{
		{"NOT_MET: the test file doesn't exist yet", "the test file doesn't exist yet"},
		{"not_met: needs more work", "needs more work"},
		{"NOT MET: missing assertion", "missing assertion"},
		{"Notmet: still pending", "still pending"},
		{"Let me think... NOT_MET: the agent didn't create the file", "the agent didn't create the file"},
		{"MET", ""}, // not a not-met line
		{"completely unparseable", ""},
		{"NOT_MET", ""}, // no reason on the line
	}
	for _, c := range cases {
		if got := extractNotMetReason(c.in); got != c.want {
			t.Errorf("extractNotMetReason(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// parseJudgeVerdict is the load-bearing parser the judge uses. Its job is
// to map ANY judge response shape into (met, reason). Thinking models
// (aeon-ultimate w/ --reasoning-parser) routinely emit narrative prose
// instead of the documented "MET" / "NOT_MET: x" tokens — the parser must
// extract a useful signal anyway so the loop doesn't stall on "unparseable".
//
// LIVE TRIGGER: user reported repeated "judge response was unparseable;
// continuing" with aeon-ultimate. Root cause: MaxTokens=200 burned by
// reasoning + strict line-anchored verdict regex. This test suite covers
// every detection branch + the fallback that NEVER returns
// "unparseable" when the judge said anything at all.
func TestParseJudgeVerdict(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantMet   bool
		mustMatch string // substring the reason must contain (only when !wantMet)
	}{
		// 1. Clean MET token.
		{"bare met", "MET", true, ""},
		{"met colon", "MET: done", true, ""},
		{"met period", "MET.", true, ""},
		{"met after thinking", "Let me think.\nThe agent did everything.\nMET", true, ""},
		{"trailing met no negation", "After analysis: MET", true, ""},
		// NOT_MET token (preserves existing behavior).
		{"strict not_met", "NOT_MET: missing tests", false, "missing tests"},
		{"casing variant", "Not_Met: needs more work", false, "needs more work"},
		{"spaced variant", "NOT MET: incomplete", false, "incomplete"},
		// 3. Narrative not-met (the actual live failure shape).
		{
			"narrative not met",
			"Let me evaluate. The agent has not met the goal because it didn't deliver the proposal document.",
			false,
			"didn't deliver", // skips opener "Let me evaluate", picks the actionable sentence
		},
		{
			"narrative incomplete",
			"Looking at the response, the work is incomplete — the agent only researched but didn't synthesize.",
			false,
			"incomplete",
		},
		{
			"still missing",
			"Goal evaluation: the analysis is still missing the path-to-10k-users section.",
			false,
			"missing",
		},
		// 4. Narrative met signals (with negation guard).
		{
			"narrative met",
			"After review, the goal has been accomplished — the agent delivered the full proposal.",
			true,
			"",
		},
		{
			"negated met stays not-met",
			"The goal has NOT been accomplished because the document is missing.",
			false,
			"", // any reason text is fine
		},
		// 5. Fallback: judge said SOMETHING — never return "unparseable".
		{
			"prose with no verdict words",
			"This is a partial answer that needs more depth on the second requirement.",
			false,
			"needs more depth",
		},
		// Empty response → useful placeholder, NOT silent met.
		{
			"empty response",
			"",
			false,
			"empty",
		},
		{
			"whitespace only",
			"   \n\n  ",
			false,
			"empty",
		},
	}
	for _, c := range cases {
		met, reason := parseJudgeVerdict(c.in)
		if met != c.wantMet {
			t.Errorf("[%s] met = %v, want %v (text=%q, reason=%q)", c.name, met, c.wantMet, c.in, reason)
			continue
		}
		if !c.wantMet && c.mustMatch != "" && !strings.Contains(reason, c.mustMatch) {
			t.Errorf("[%s] reason %q should contain %q", c.name, reason, c.mustMatch)
		}
		// The CRITICAL guarantee: any non-empty input that's not-met MUST
		// produce a non-empty reason — never the old "unparseable" trap.
		if !c.wantMet && strings.TrimSpace(c.in) != "" && reason == "" {
			t.Errorf("[%s] empty reason on non-empty input — drops signal", c.name)
		}
		if !c.wantMet && strings.Contains(reason, "judge response was unparseable") &&
			strings.TrimSpace(c.in) != "" {
			t.Errorf("[%s] fell back to 'unparseable' when response had content — regression to live bug", c.name)
		}
	}
}

// firstUsefulSentence skips throat-clearing openers and returns the first
// substantive sentence — so the agent's continuation prompt carries
// actionable content, not "Let me think...".
func TestFirstUsefulSentence(t *testing.T) {
	cases := []struct{ in, want string }{
		{
			"Let me think about this. The actual issue is the missing test file.",
			"The actual issue is the missing test file.",
		},
		{
			"Looking at the response, the agent did not implement the API endpoint yet.",
			"the agent did not implement the API endpoint yet.",
		},
		{"Just one sentence with the real point.", "Just one sentence with the real point."},
		{"", ""},
		{"...", "..."},
	}
	for _, c := range cases {
		got := firstUsefulSentence(c.in)
		if !strings.Contains(got, strings.TrimSpace(strings.TrimPrefix(c.want, " "))) && c.want != "" {
			t.Errorf("firstUsefulSentence(%q) = %q, want it to contain %q", c.in, got, c.want)
		}
	}
}

// Judge with no active goal: short-circuits to met=true, no error.
func TestGoalControl_JudgeNoGoal(t *testing.T) {
	gc := &goalControl{maxTurns: 5}
	met, reason, atCap, err := gc.Judge(context.Background(), "anything")
	if err != nil {
		t.Errorf("Judge with no goal should not error: %v", err)
	}
	if !met {
		t.Error("Judge with no goal should return met=true (short-circuit)")
	}
	if atCap || reason != "" {
		t.Errorf("Judge with no goal should be neutral: atCap=%v reason=%q", atCap, reason)
	}
}

// Judge with turn count already at cap returns atCap=true WITHOUT making the
// LLM call (we know it's over). Verified by absence of an LLM panic — the
// gc has nil ag so any actual call would crash.
func TestGoalControl_JudgeStopsAtCap(t *testing.T) {
	gc := &goalControl{maxTurns: 3}
	_, _, _ = gc.Set(context.Background(), "x")
	gc.turns = 3 // at cap

	met, _, atCap, err := gc.Judge(context.Background(), "anything")
	if err != nil {
		t.Errorf("at-cap path should not error (no LLM call): %v", err)
	}
	if met {
		t.Error("at-cap should NOT be met")
	}
	if !atCap {
		t.Error("at-cap should be true")
	}
	if !strings.Contains(gc.lastJudge, "turn cap") {
		t.Errorf("lastJudge should mention cap: %q", gc.lastJudge)
	}
}

// Clear is idempotent (calling on already-empty doesn't crash + returns a
// useful message).
func TestGoalControl_ClearIdempotent(t *testing.T) {
	gc := &goalControl{maxTurns: 5}
	_ = gc.Clear() // no-op
	_, _, _ = gc.Set(context.Background(), "x")
	first := gc.Clear()
	second := gc.Clear()
	if first == second {
		t.Error("first Clear after Set + second Clear should produce different messages")
	}
	if !strings.Contains(second, "no goal") {
		t.Errorf("second Clear: %q", second)
	}
}

// errors interaction: ensure errors.Is works as expected on judge errors
// (defensive — caller uses errors.Is on the wrapped chain).
func TestGoalControl_JudgeErrorIsWrapped(t *testing.T) {
	// We can't trigger a real planner error without a real agent; sanity
	// check that the error path we built returns nil for the no-goal case.
	gc := &goalControl{maxTurns: 5}
	_, _, _, err := gc.Judge(context.Background(), "")
	if err != nil {
		t.Errorf("no-goal Judge should not error: %v", err)
	}
	// errors.Is sanity (not exercising real error here).
	if errors.Is(err, context.Canceled) {
		t.Error("err should be nil, not canceled")
	}
}
