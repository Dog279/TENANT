package main

// /goal — Claude-Code-style autonomous loop on a stop condition.
//
// Lifecycle:
//   /goal <condition>            → set + return the first prompt to submit
//   <agent runs one turn>        → assistant response captured
//   judge(condition, response)   → MET / NOT_MET:<reason>
//   if NOT_MET + under cap       → Continue(reason) builds the next prompt
//   if MET                       → finalized, state cleared on Clear()
//   /goal show                   → snapshot (condition, turns, last judge)
//   /goal clear                  → stop the loop
//
// The judge is a small tools-off LLM call against the orchestrator's planner
// (same provider as the main agent). Cheap (~200 tokens) so we can run it
// after every turn without blowing the budget.
//
// Safety: hard turn cap (defaultGoalMaxTurns) so a misbehaving condition or
// stuck agent can't burn an infinite loop. User can /goal clear or Esc.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"tenant/internal/agent"
	"tenant/internal/memory/working"
	"tenant/internal/model"
	"tenant/internal/tui"
)

// defaultGoalMaxTurns caps an autonomous goal loop. 20 is the same ballpark
// as Claude Code — enough for multi-step tasks, low enough that a stuck
// loop self-terminates before chewing through credits.
const defaultGoalMaxTurns = 20

// goalControl holds the active goal's state + drives the judge calls.
// Single-threaded under the TUI's Update goroutine (no mutex needed for the
// goal-state mutations), but Judge can be invoked from a tea.Cmd goroutine —
// hence the mu for state reads/writes.
type goalControl struct {
	mu sync.Mutex
	ag *agent.Agent // for resolving the planner LLM (router can swap mid-run)

	condition string
	startedAt time.Time
	turns     int
	maxTurns  int
	lastJudge string
	lastEval  time.Time
	met       bool

	// loopCeiling is the per-turn planner↔tool iteration budget applied WHILE
	// this goal loop is active, decoupled from the global PlanLoopCeiling
	// (TEN-216). Set once from config at construction; read-only thereafter.
	// >0 override, <0 unlimited, 0 inherit the global ceiling.
	loopCeiling int
}

func newGoalControl(ag *agent.Agent, loopCeiling int) *goalControl {
	return &goalControl{ag: ag, maxTurns: defaultGoalMaxTurns, loopCeiling: loopCeiling}
}

// LoopCeiling reports the per-turn loop ceiling the TUI should apply to turns
// run WHILE this goal is active (TEN-216): >0 overrides the global
// PlanLoopCeiling for goal turns, <0 = unlimited, 0 = inherit. Read-only after
// construction, so no lock is needed.
func (gc *goalControl) LoopCeiling() int { return gc.loopCeiling }

// goalLoopCeilingFromConfig nil-safely reads the persisted goal.loop_ceiling so
// callers can wire newGoalControl without guarding a possibly-nil launchConfig.
func goalLoopCeilingFromConfig(lc *launchConfig) int {
	if lc == nil {
		return 0
	}
	return lc.Goal.LoopCeiling
}

// Set replaces the active goal and returns the prompt the TUI should submit
// as the first agent turn. status is a one-liner for the system chat.
func (gc *goalControl) Set(_ context.Context, condition string) (firstPrompt, status string, err error) {
	condition = strings.TrimSpace(condition)
	if condition == "" {
		return "", "", fmt.Errorf("goal needs a condition (e.g. /goal write a unit test for the new feature)")
	}
	gc.mu.Lock()
	defer gc.mu.Unlock()
	gc.condition = condition
	gc.startedAt = time.Now()
	gc.turns = 0
	gc.lastJudge = ""
	gc.lastEval = time.Time{}
	gc.met = false
	if gc.maxTurns <= 0 {
		gc.maxTurns = defaultGoalMaxTurns
	}
	firstPrompt = fmt.Sprintf("[Goal — autonomous loop active]\n\nGoal: %s\n\n"+
		"DO ONE THING toward this goal RIGHT NOW. Use tools to ACTUALLY do "+
		"the work — call web_navigate / web_read / wiki_search / write the "+
		"file / draft the document. Produce an ARTIFACT, not a plan.\n\n"+
		"Forbidden: meta-commentary about what you'll do, outlines of next "+
		"steps, sentences starting with 'I should' / 'I will' / 'let me' / "+
		"'first I need to'. The judge evaluates your OUTPUT, not your "+
		"intentions. End your response with the artifact or the concrete "+
		"finding, then stop.", condition)
	status = fmt.Sprintf("🎯 goal set: %s   (auto-continuing up to %d turns; /goal clear to stop)",
		clip(oneLineStr(condition), 80), gc.maxTurns)
	return firstPrompt, status, nil
}

// Active reports whether a goal is currently set + not yet met. The TUI
// reads this to know whether to fire the judge after a turnDoneMsg.
func (gc *goalControl) Active() bool {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	return gc.condition != "" && !gc.met
}

// Show returns a snapshot of the current goal state — drives /goal show
// and any future status-bar overlay.
func (gc *goalControl) Show() tui.GoalStatus {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	st := tui.GoalStatus{
		Active:          gc.condition != "",
		Condition:       gc.condition,
		Turns:           gc.turns,
		MaxTurns:        gc.maxTurns,
		LastJudge:       gc.lastJudge,
		LastEval:        gc.lastEval,
		Started:         gc.startedAt,
		Met:             gc.met,
		GoalLoopCeiling: gc.loopCeiling,
	}
	if !st.Started.IsZero() {
		st.ElapsedFmt = time.Since(st.Started).Round(time.Second).String()
	}
	return st
}

// Clear stops the loop and returns a status line for the system chat.
// Idempotent — clearing when nothing's active is a no-op message.
func (gc *goalControl) Clear() string {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	if gc.condition == "" {
		return "no goal is active"
	}
	had := gc.condition
	gc.condition = ""
	gc.met = false
	gc.lastJudge = ""
	return fmt.Sprintf("🎯 goal cleared: %s", clip(oneLineStr(had), 80))
}

// Judge runs one judge call against the active condition and the agent's
// last response. Returns:
//
//	met=true   → goal achieved; the TUI posts "✦ goal met" + clears.
//	met=false  → keep going; reason is the judge's one-line "still needed"
//	             explanation; caller increments turns and decides whether
//	             to continue based on atCap.
//	atCap=true → turn budget exhausted; the TUI posts "⚠ hit cap" + clears.
//
// Always returns met=true with no error when no goal is active — so the TUI
// can call Judge unconditionally after turnDoneMsg and only act when it
// matters.
func (gc *goalControl) Judge(ctx context.Context, lastResponse string) (met bool, reason string, atCap bool, err error) {
	gc.mu.Lock()
	condition := gc.condition
	turns := gc.turns
	max := gc.maxTurns
	gc.mu.Unlock()
	if condition == "" {
		return true, "", false, nil // no goal → nothing to judge
	}

	// Cap check FIRST. If we're already at the cap, don't waste a judge
	// call — surface the cap and stop.
	if turns >= max {
		gc.mu.Lock()
		gc.lastJudge = fmt.Sprintf("turn cap reached (%d) — stopping", max)
		gc.lastEval = time.Now()
		gc.mu.Unlock()
		return false, "turn cap reached", true, nil
	}

	planner, _, perr := gc.ag.Router().LLMForRole(ctx, model.RolePlanner)
	if perr != nil {
		return false, "", false, fmt.Errorf("goal judge: resolve planner: %w", perr)
	}

	prompt := fmt.Sprintf(`You are a goal-completion judge. Decide whether the agent has met its objective.

GOAL: %s

AGENT'S LATEST RESPONSE:
%s

Decision rules:
- MET requires an ARTIFACT or CONCRETE FINDING in this response, not a plan.
  • A drafted document / written file / code / cited research result counts.
  • An outline of what the agent WILL do does NOT count.
  • Sentences like "I should...", "let me...", "I'll draft...", "first I need to..."
    are planning, not delivery — return NOT_MET.
- If the agent has clearly delivered the requested artifact / answered the
  question / completed the task, output EXACTLY one line: MET
- Otherwise output: NOT_MET: <one-line reason that names the specific missing
  artifact or step — be concrete so the agent can act on it>

Output ONLY the decision line. No preamble. No surrounding text.`, condition, clip(lastResponse, 4000))

	resp, gerr := planner.Generate(ctx, model.GenerateRequest{
		Messages: []model.Message{{Role: "user", Content: prompt}},
		// Generous budget because thinking-model variants (aeon-ultimate
		// w/ --reasoning-parser) burn hundreds of tokens deciding before
		// they emit any visible verdict. 200 left us with empty content
		// while the model was still mid-think (same root cause as the C2
		// clarifier bug — see tasks/lessons.md "vLLM reasoning-parser
		// silent failures"). 2000 covers the thinking + the verdict.
		MaxTokens: 2000,
		// Stop on tool-call markup so a thinking model that drifts into
		// tool-call mode doesn't pollute the judgment.
		StopSequences: []string{"<tool_call", "<function="},
	})
	if gerr != nil {
		return false, "", false, fmt.Errorf("goal judge: %w", gerr)
	}
	text := strings.TrimSpace(resp.Text)

	gc.mu.Lock()
	gc.lastEval = time.Now()
	gc.turns = turns + 1
	gc.mu.Unlock()

	met, reason = parseJudgeVerdict(text)
	if met {
		gc.mu.Lock()
		gc.met = true
		gc.lastJudge = "goal achieved"
		gc.mu.Unlock()
		return true, "", false, nil
	}

	gc.mu.Lock()
	gc.lastJudge = reason
	gc.mu.Unlock()
	// Did this turn put us AT the cap? (turns was incremented above.)
	if turns+1 >= max {
		return false, reason, true, nil
	}
	return false, reason, false, nil
}

// parseJudgeVerdict is the single entry point for interpreting the judge's
// response. Returns (met, reason). The detection is intentionally lenient
// because thinking models (aeon-ultimate w/ reasoning-parser) don't emit
// the strict "MET" / "NOT_MET:" shape — they narrate the decision in prose
// ("the agent did X but didn't do Z, so the goal is not yet met because…").
//
// Order of detection:
//  1. Clean MET token on its own line → met.
//  2. NOT_MET / NOT MET line with reason → not met with that reason.
//  3. Narrative containing "not met" / "incomplete" / "missing" /
//     "still needs" / "didn't" / "hasn't" → not met, reason = first
//     useful sentence of the response.
//  4. Narrative containing "goal met" / "complete" / "accomplished" /
//     "achieved" without negation nearby → met.
//  5. Total fallback: not met, reason = clipped raw response (still
//     useful continuation signal — better than "unparseable").
//
// Default bias is toward NOT MET: a false-positive "met" stops the loop
// prematurely; a false-negative just costs one more turn (capped by
// maxTurns anyway). Asymmetric cost → conservative detection.
func parseJudgeVerdict(text string) (met bool, reason string) {
	if strings.TrimSpace(text) == "" {
		return false, "judge returned empty response"
	}
	upper := strings.ToUpper(text)

	// 1. Clean MET token: line is exactly "MET", or starts/ends with MET
	// boundary. Excludes "NOT MET" / "UNMET" / "MET WITH CONCERNS".
	for _, line := range strings.Split(upper, "\n") {
		l := strings.TrimSpace(line)
		if l == "MET" {
			return true, ""
		}
		if strings.HasPrefix(l, "MET ") || strings.HasPrefix(l, "MET:") || strings.HasPrefix(l, "MET.") {
			return true, ""
		}
		if strings.HasSuffix(l, " MET") && !strings.Contains(l, "NOT") {
			return true, ""
		}
	}

	// 2. NOT_MET: <reason> shape.
	if r := extractNotMetReason(text); r != "" {
		return false, r
	}

	// 3. Narrative not-met signals — phrases that almost always mean
	// "still working." Pick the first useful sentence as the reason.
	for _, marker := range []string{
		"not met", "not yet met", "not yet accomplished", "not complete",
		"incomplete", "still missing", "still needs", "still pending",
		"didn't", "did not", "hasn't", "has not", "needs to",
		"requires more", "missing the", "missing a",
	} {
		if strings.Contains(strings.ToLower(text), marker) {
			return false, firstUsefulSentence(text)
		}
	}

	// 4. Narrative met signals (with negation guard). The model said
	// something like "the goal has been achieved" without "not" / "isn't"
	// nearby — accept as met. List covers the natural-language ways a
	// thinking model phrases completion.
	for _, marker := range []string{
		"goal met", "goal is met", "goal has been met",
		"goal accomplished", "goal has been accomplished", "goal is accomplished",
		"goal achieved", "goal has been achieved", "goal is achieved",
		"goal complete", "goal is complete", "goal has been completed",
		"fully complete", "fully accomplished", "fully achieved",
		"task complete", "task accomplished", "task achieved",
		"objective met", "objective achieved", "objective accomplished",
	} {
		i := strings.Index(strings.ToLower(text), marker)
		if i < 0 {
			continue
		}
		// Negation check: 30 chars before the marker.
		start := i - 30
		if start < 0 {
			start = 0
		}
		window := strings.ToLower(text[start:i])
		if strings.Contains(window, "not ") || strings.Contains(window, "n't ") ||
			strings.Contains(window, "isn") || strings.Contains(window, "hasn") {
			continue
		}
		return true, ""
	}

	// 5. Fallback: surface the response itself as the reason — gives the
	// agent actual signal to act on, instead of a useless placeholder.
	preview := firstUsefulSentence(text)
	if preview == "" {
		preview = clip(oneLineStr(text), 200)
	}
	if preview == "" {
		preview = "judge response was unparseable"
	}
	return false, preview
}

// firstUsefulSentence picks the first non-throat-clearing sentence from
// the judge's narrative. Skips boilerplate openers ("Let me think...",
// "Looking at this...") so the reason the agent receives is actionable.
func firstUsefulSentence(text string) string {
	// Split on common sentence boundaries.
	sentences := splitSentencesGoal(text)
	for _, s := range sentences {
		s = strings.TrimSpace(s)
		if len(s) < 12 {
			continue
		}
		lc := strings.ToLower(s)
		// Skip throat-clearing openers.
		skip := false
		for _, opener := range []string{
			"let me ", "looking at", "i need to", "first,", "let's ",
			"to determine", "okay,", "alright,",
		} {
			if strings.HasPrefix(lc, opener) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		return clip(s, 280)
	}
	// Nothing useful — fall back to a clip of the whole thing.
	return clip(oneLineStr(text), 200)
}

// splitSentencesGoal is a cheap sentence splitter. Doesn't handle every
// edge case (abbreviations, decimals) — that's fine; we just want
// reasonable-shaped fragments.
func splitSentencesGoal(text string) []string {
	// Replace newlines with spaces, then split on period/!/?.
	flat := strings.ReplaceAll(text, "\n", " ")
	var out []string
	start := 0
	for i, r := range flat {
		if r == '.' || r == '!' || r == '?' {
			out = append(out, flat[start:i+1])
			start = i + 1
		}
	}
	if start < len(flat) {
		out = append(out, flat[start:])
	}
	return out
}

// extractNotMetReason pulls the one-line reason from a `NOT_MET: <reason>`
// shape, tolerating casing + extra preamble on the same line. Searches any
// line for a NOT_MET / NOT MET / NOTMET marker — thinking models routinely
// prepend "Let me think... NOT_MET: ..." which would miss a strict prefix
// match. Returns the first matching reason; "" when none.
func extractNotMetReason(text string) string {
	for _, line := range strings.Split(text, "\n") {
		l := strings.TrimSpace(line)
		uc := strings.ToUpper(l)
		// Search anywhere in the line for the marker. Try the spaced variant
		// FIRST so "NOT MET" doesn't fall to "NOT" + "MET" hits.
		for _, marker := range []string{"NOT_MET", "NOTMET", "NOT MET"} {
			idx := strings.Index(uc, marker)
			if idx < 0 {
				continue
			}
			rest := l[idx+len(marker):]
			rest = strings.TrimLeft(rest, " :-")
			rest = strings.TrimSpace(rest)
			if rest != "" {
				return rest
			}
			break // matched but no reason on this line — try the next
		}
	}
	return ""
}

// Continue builds the user prompt for the next auto-loop turn, folding the
// judge's reason in so the agent knows what's still missing. Kept as a
// separate method (not coupled to Judge) so the TUI can decide WHEN to
// auto-submit — e.g. skipping when a /research or clarify pause is active.
//
// Prompt phrasing matters a lot here: a soft "make a step" reads as PLAN a
// step, and the agent fills the turn with meta-commentary ("I should...
// let me..."). A hard "DO it now, don't explain it" gets actual tool calls
// + artifacts. Live trigger: user reported the agent narrating instead of
// doing across multiple turns on a /goal research-and-propose task.
func (gc *goalControl) Continue(reason string) string {
	if strings.TrimSpace(reason) == "" {
		return "[Goal — keep going]\n\nDO the next thing toward the goal NOW. Use tools to produce real output. Forbidden: meta-commentary, outlines, 'I should' / 'I will' / 'let me' sentences. End with the artifact."
	}
	return fmt.Sprintf("[Goal — judge: %s]\n\nAddress that gap with ACTION, not narration. Use tools to actually do the work. Produce the artifact (file written, document drafted, finding cited). The judge evaluates what you DID this turn, not what you planned. No meta-commentary.",
		strings.TrimSpace(reason))
}

// --- CLI: `tenant goal <condition>` ------------------------------------

// cmdGoal is the headless equivalent of /goal in the TUI: spins up an
// agent, kicks off the first turn with the condition, runs the judge after
// each response, and auto-continues until the judge says MET (or the cap
// is hit). Prints each turn to stdout; the final result is the agent's
// last response. Useful for scripting: `tenant goal "write README"`.
//
// `--max-turns N` overrides the default cap. `--verbose` shows the judge's
// reason between turns.
func cmdGoal(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("goal", flag.ContinueOnError)
	c := bindCommon(fs)
	maxTurns := fs.Int("max-turns", defaultGoalMaxTurns, "max agent turns before the loop bails out (safety cap)")
	verbose := fs.Bool("verbose", false, "show the judge's verdict + auto-continue prompts between turns")
	if err := fs.Parse(args); err != nil {
		return err
	}
	condition := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if condition == "" {
		return fmt.Errorf("usage: tenant goal [--max-turns N] [--verbose] \"<condition>\"")
	}
	if err := c.resolve(); err != nil {
		return err
	}
	log := newLogger() // stderr; stdout is the agent's response
	router, err := buildRouter(c, log)
	if err != nil {
		return err
	}
	st, closeStores, err := openStores(c)
	if err != nil {
		return err
	}
	defer closeStores()

	ag, err := agent.New(agent.Config{
		AgentID:    c.agent,
		Router:     router,
		Soul:       st.soul,
		Working:    working.New(),
		Archive:    st.archive,
		Episodic:   st.episodic,
		Semantic:   st.semantic,
		Tools:      agent.NewStaticRegistry(),
		Dispatcher: noopDispatcher{},
		Logger:     log,
	})
	if err != nil {
		return err
	}
	gc := newGoalControl(ag, goalLoopCeilingFromConfig(c.lc)) // honor goal.loop_ceiling (TEN-216)
	gc.maxTurns = *maxTurns

	firstPrompt, status, err := gc.Set(ctx, condition)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, status)
	prompt := firstPrompt

	for {
		res, terr := ag.Turn(ctx, agent.TurnRequest{UserQuery: prompt, LoopCeiling: gc.LoopCeiling()})
		if terr != nil {
			return fmt.Errorf("turn %d: %w", gc.Show().Turns+1, terr)
		}
		response := ""
		if res != nil {
			response = res.Response
		}
		fmt.Println(response) // stdout — the agent's text answer

		// Judge → met / not-met / cap.
		met, reason, atCap, jerr := gc.Judge(ctx, response)
		if jerr != nil {
			return fmt.Errorf("judge failed: %w — stopping (last response above)", jerr)
		}
		if met {
			fmt.Fprintln(os.Stderr, "\n🎯 ✦ goal met — autonomous loop complete.")
			return nil
		}
		if atCap {
			fmt.Fprintf(os.Stderr, "\n🎯 ⚠ goal hit turn cap (%d) — stopping. Last judge: %s\n", gc.maxTurns, reason)
			return fmt.Errorf("goal: turn cap reached")
		}
		if *verbose {
			fmt.Fprintf(os.Stderr, "\n--- judge: %s --- continuing ---\n", reason)
		}
		prompt = gc.Continue(reason)
	}
}
