package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"tenant/internal/agent"
	"tenant/internal/memory/compress"
	"tenant/internal/memory/skills"
	"tenant/internal/memory/userprofile"
	"tenant/internal/model"
	"tenant/internal/orchestra"
	"tenant/internal/plugins/web"
	"tenant/internal/plugins/wiki"
	"tenant/internal/research"
	"tenant/internal/tui"
)

// Deep Research (Phase A) — an Onyx-style orchestrator-worker pipeline:
//   plan → spawn ≤N researchers in waves of ≤parallel → collapse citations →
//   tools-off synthesis into one cited report.
// It reuses the existing TeamRuntime (concurrent sub-agents, per-agent browser,
// the bus, the live model) — the new bits are the plan/synthesis prompts and
// citation collapse. See docs/DEEP-RESEARCH.md.

type researchOpts struct {
	maxAgents    int           // sub-questions per cycle (plan + each reflection)
	parallel     int           // max concurrent researchers per wave (KV-cache pressure)
	awaitTimeout time.Duration // per-wave wait
	depth        int           // reflection cycles (1 = Phase-A single pass)
	maxTime      time.Duration // wall-clock cap across all cycles (0 = none)
	noClarify    bool          // skip the C2 clarification prompt for vague queries
}

func defaultResearchOpts() researchOpts {
	// awaitTimeout 10m: a researcher doing iterative web research with a
	// thinking model (aeon-ultimate, qwen3-derived merges) routinely needs
	// 5–8m to finish — assemble + plan + tool dispatch + chromedp + the
	// final synthesis call. 5m left it stuck mid-loop.
	return researchOpts{maxAgents: 5, parallel: 3, awaitTimeout: 10 * time.Minute, depth: 2, maxTime: 20 * time.Minute}
}

// ClarifyNeededError is returned by deepResearch (and by extension
// runWithPersistence / researchControl.Research) when the question is too
// vague to plan effectively and a clarifying round-trip is recommended.
// Callers handle it by displaying the questions to the user, getting an
// answer, and re-running with an enriched query + opts.noClarify=true.
type ClarifyNeededError struct {
	Question  string   // the original question as received
	Questions []string // 1-2 clarifying questions to ask the user
}

func (e *ClarifyNeededError) Error() string {
	if len(e.Questions) == 0 {
		return "research: clarification needed"
	}
	return fmt.Sprintf("research: clarification needed (%d question%s)",
		len(e.Questions), plural(len(e.Questions)))
}

// ClarifyQuestions / ClarifyOriginal satisfy tui.ResearchClarifyError so the
// TUI can extract the prompt without importing this package's concrete type.
func (e *ClarifyNeededError) ClarifyQuestions() []string { return e.Questions }
func (e *ClarifyNeededError) ClarifyOriginal() string    { return e.Question }

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// EnrichClarified builds the enriched query the caller passes back on the
// second pass (the user's answer folded into the original question). Same
// shape regardless of where the call comes from (CLI stdin / TUI message)
// so the planner sees consistent input.
func EnrichClarified(original, answer string) string {
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return original
	}
	return strings.TrimSpace(original) + "\n\nAdditional context from user: " + answer
}

// heuristicVague is the cheap pre-check that decides whether to even bother
// asking the LLM clarifier. Returns true for "looks vague, ask the model."
// Errs on the side of "looks specific" — false negatives (skipping clarify
// on a clear-shaped query) are cheap; false positives (asking the user
// unnecessary clarifying questions) waste their time.
func heuristicVague(q string) bool {
	q = strings.TrimSpace(q)
	if q == "" {
		return false
	}
	words := strings.Fields(q)
	// Very short → definitely vague ("nvidia stock", "graphiti", "rust").
	if len(words) < 4 {
		return true
	}
	// Long → assume the user already gave specifics.
	if len(q) > 140 {
		return false
	}
	// Specificity signals: any quoted phrase, any digit, ≥2 proper-noun
	// tokens after the first word.
	if strings.ContainsAny(q, `"0123456789`) {
		return false
	}
	propers := 0
	for i, w := range words {
		if i == 0 || w == "" {
			continue
		}
		if c := w[0]; c >= 'A' && c <= 'Z' {
			propers++
		}
	}
	return propers < 2
}

// clarifyQuestion decides whether to ask the user 1-2 clarifying questions
// before kicking off the research pipeline. Two gates: a cheap heuristic to
// avoid the LLM call on obviously-specific queries, then a tools-off LLM
// call as the real arbiter. Returns nil when the query is clear enough.
// LLM call errors degrade silently ("can't decide, proceed") — never a
// reason to block research.
func clarifyQuestion(ctx context.Context, planner model.LLM, question string) []string {
	if !heuristicVague(question) {
		return nil
	}
	const prompt = `You're triaging a deep-research question. Read it carefully and decide:

Question: %s

If this question is clear enough to research without more context from the user,
output EXACTLY one line: CLEAR

If it's vague (could mean multiple things, missing scope, missing timeframe,
ambiguous about which entity or which angle), output 1-2 sharp clarifying
questions, one per line, that would let a researcher know exactly what to
investigate. Each must end with a question mark.
Do NOT include numbering. Do NOT include preamble. ONLY the questions, one per line.`
	resp, err := planner.Generate(ctx, model.GenerateRequest{
		Messages: []model.Message{{Role: "user", Content: fmt.Sprintf(prompt, question)}},
		// Generous budget because thinking-model variants (aeon-ultimate
		// w/ --reasoning-parser) burn through hundreds of tokens deciding
		// before they emit any visible answer. 200 left us with empty
		// content while the model was still mid-think; 2000 covers the
		// thinking + the actual CLEAR/questions output.
		MaxTokens: 2000,
		// Stop sequences hard-cut any model that starts emitting tool-call
		// markup in a context where we explicitly asked for prose only.
		StopSequences: []string{"<tool_call", "<function=", "<parameter="},
	})
	if err != nil {
		return nil // degrade: skip clarification, let research proceed
	}
	text := strings.TrimSpace(resp.Text)
	if text == "" {
		return nil
	}
	// CLEAR sentinel: search anywhere in the response, not just the prefix.
	// With the reasoning-parser fallback, the salvaged text may have a long
	// thinking prelude followed by "CLEAR" — that should still mean "clear."
	if hasClearSentinel(text) {
		return nil
	}
	return extractClarifyQuestions(text)
}

// hasClearSentinel returns true if the response declares the question clear.
// More lenient than a strict prefix match: searches for the exact sentinel
// "CLEAR" as a standalone word, anywhere in the text. Necessary because the
// reasoning-parser fallback can prepend a long thinking block.
func hasClearSentinel(s string) bool {
	upper := strings.ToUpper(s)
	// Anchor at line boundaries or word boundaries to avoid matching
	// "UNCLEAR" / "CLEARLY" / "CLEARED."
	for _, line := range strings.Split(upper, "\n") {
		line = strings.TrimSpace(line)
		if line == "CLEAR" || strings.HasPrefix(line, "CLEAR ") || strings.HasPrefix(line, "CLEAR:") {
			return true
		}
		if strings.HasSuffix(line, " CLEAR") || strings.HasSuffix(line, ": CLEAR") {
			return true
		}
	}
	return false
}

// extractClarifyQuestions pulls the question-shaped lines from the model's
// response. Tolerates numbered / bulleted / prefixed lines, but requires a
// trailing `?`. Cap at 2 — Onyx's rule, also keeps the user prompt short.
func extractClarifyQuestions(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		// Strip common bullet/number prefixes.
		line = strings.TrimLeft(line, "-*•0123456789. )")
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasSuffix(line, "?") {
			continue
		}
		out = append(out, line)
		if len(out) >= 2 {
			break
		}
	}
	return out
}

// promptClarifyStdin is the CLI side of the clarify round-trip: print the
// questions to stderr (stdout is reserved for the report), read one line of
// the user's answer from stdin. An empty answer is allowed — caller folds it
// in as best-effort context. Reads ONE line (newline-delimited); multi-line
// answers must be entered as a single line. EOF (Ctrl-D, pipe closed) is
// returned as an empty string, not an error — non-interactive runs that
// happen to hit clarify shouldn't crash.
func promptClarifyStdin(questions []string) (string, error) {
	fmt.Fprintf(os.Stderr, "\nThis question is vague — quick clarification:\n")
	for i, q := range questions {
		fmt.Fprintf(os.Stderr, "  %d. %s\n", i+1, q)
	}
	fmt.Fprintf(os.Stderr, "Your answer (one line, blank to proceed anyway): ")
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil && line == "" { // EOF with no input → proceed empty, not error
		return "", nil
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// timelineEmitter is the C4 structured-update sink. nil is a valid value —
// callers without a TUI (CLI runs) pass nil and the producer side just skips.
type timelineEmitter func(tui.ResearchTimelineUpdate)

// emitSafe wraps a possibly-nil emitter so producers can call it unconditionally.
func emitSafe(emit timelineEmitter, u tui.ResearchTimelineUpdate) {
	if emit == nil {
		return
	}
	emit(u)
}

// deepResearch runs the full pipeline and returns the final markdown report.
// say streams human-readable progress (stderr for CLI, the feed for the TUI).
// run, if non-nil, persists the audit trail (manifest + findings + report) to
// disk for /research history|show|replay. nil = transient run (legacy path).
// emit, if non-nil, sends structured timeline updates for the TUI's C4 pane.
func deepResearch(ctx context.Context, planner model.LLM, rt *TeamRuntime, question string, opts researchOpts, say func(string, ...any), run *research.Run, emit timelineEmitter) (string, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return "", fmt.Errorf("research: empty question")
	}

	// 0. C2 — Optional clarification step. When the question is vague,
	// surface a typed ClarifyNeededError so the caller (CLI stdin / TUI
	// command handler) can ask the user, fold the answer in, and re-call
	// with opts.noClarify=true. Skipped on the second pass (--no-clarify)
	// and on replays (where the question is exactly as it was last time).
	if !opts.noClarify {
		if qs := clarifyQuestion(ctx, planner, question); len(qs) > 0 {
			return "", &ClarifyNeededError{Question: question, Questions: qs}
		}
	}

	// C4: open the timeline pane immediately so the user sees activity
	// during planning (before any sub-question fires).
	emitSafe(emit, tui.ResearchTimelineUpdate{
		Kind: "started", Total: opts.depth,
		Started: &tui.ResearchStartedData{Question: question},
	})

	// 1. Plan → independent sub-questions.
	subqs, err := researchPlan(ctx, planner, question, opts.maxAgents)
	if err != nil {
		return "", fmt.Errorf("research: plan: %w", err)
	}
	if len(subqs) == 0 {
		subqs = []string{question} // degrade: research the question directly
	}
	say("plan: %d sub-question(s)", len(subqs))
	for i, q := range subqs {
		say("  %d. %s", i+1, q)
	}

	// 2. Reflective deepening loop (Phase B). depth=1 reduces to Phase A.
	depth := opts.depth
	if depth < 1 {
		depth = 1
	}
	var deadline time.Time
	if opts.maxTime > 0 {
		deadline = time.Now().Add(opts.maxTime)
	}
	asked := map[string]bool{} // normalized sub-questions already dispatched
	var mine []string          // ids WE spawned (the runtime may be shared)

	for cycle := 1; cycle <= depth; cycle++ {
		// Dedup against earlier cycles so we only chase NEW angles.
		wave := make([]string, 0, len(subqs))
		for _, q := range subqs {
			k := normalizeQuestion(q)
			if k != "" && !asked[k] {
				asked[k] = true
				wave = append(wave, q)
			}
		}
		if len(wave) == 0 {
			break
		}
		// C4: publish the plan for THIS cycle before we dispatch — the TUI
		// seeds placeholder rows from this so the pane shows what we're
		// about to do, not just what's already in flight.
		emitSafe(emit, tui.ResearchTimelineUpdate{
			Kind: "plan", Cycle: cycle, Total: depth,
			Plan: &tui.ResearchPlanData{SubQuestions: wave},
		})
		mine = append(mine, runResearchWaves(ctx, rt, question, wave, opts.parallel, opts.awaitTimeout, say, emit, cycle, depth)...)
		if run != nil {
			run.SetCycles(cycle)
		}
		if ctx.Err() != nil {
			break
		}
		if cycle >= depth {
			break
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			say("time cap reached — finishing with current findings")
			break
		}
		// Reflect: gap analysis → follow-up sub-questions (or done).
		say("cycle %d done — reflecting on gaps…", cycle)
		emitSafe(emit, tui.ResearchTimelineUpdate{
			Kind: "reflect_done", Cycle: cycle, Total: depth,
			Reflect: &tui.ResearchReflectData{}, // gaps filled below if any
		})
		gaps, rerr := reflect(ctx, planner, question, rt.ResultsFor(mine), opts.maxAgents)
		if rerr != nil {
			say("reflection failed (%v) — finishing", rerr)
			break
		}
		if len(gaps) == 0 {
			say("no further gaps — finishing")
			break
		}
		say("cycle %d: %d follow-up question(s)", cycle+1, len(gaps))
		for i, g := range gaps {
			say("  %d. %s", i+1, g)
		}
		// Re-emit reflect_done with the gaps populated so the timeline
		// shows the count.
		emitSafe(emit, tui.ResearchTimelineUpdate{
			Kind: "reflect_done", Cycle: cycle, Total: depth,
			Reflect: &tui.ResearchReflectData{Gaps: gaps},
		})
		subqs = gaps
	}

	// 3. Collapse OUR researchers' local [n] citations into one global biblio.
	myResults := rt.ResultsFor(mine)
	// Per-agent visibility — silent skips in collapseCitations made it look like
	// "nothing came back" when really individual agents were timing out, hitting
	// loop ceiling with empty synthesis, or producing tool-call dumps. Surface
	// each agent's status + a preview so the failure mode is obvious.
	// Also: strip ANY leaked tool-call markup from the finding text before it
	// reaches the synthesizer. A subagent that bailed mid-call can produce
	// `<tool_call>...` text as its "report"; passing that to synthesis just
	// makes the synthesizer return NO_USABLE_FINDINGS. Strip it first; if
	// nothing's left, the agent gets recorded as empty and skipped cleanly.
	for i, r := range myResults {
		cleanedResult := stripToolCallNoise(r.Result)
		if cleanedResult != r.Result {
			say("  ⚠ [%s] stripped leaked tool-call markup (%d→%d chars)",
				r.ID, len(r.Result), len(cleanedResult))
			myResults[i].Result = cleanedResult
		}
		preview := oneLineStr(strings.TrimSpace(myResults[i].Result))
		say("  · [%s] %s — %d chars: %s", myResults[i].ID, myResults[i].Status,
			len(strings.TrimSpace(myResults[i].Result)), clip(preview, 140))
		// C4: publish each agent's terminal status to the timeline pane so
		// the per-row glyph flips ↺→✓/✗ and the result length shows.
		emitSafe(emit, tui.ResearchTimelineUpdate{
			Kind: "agent_status",
			Agent: &tui.ResearchAgentRow{
				ID:        myResults[i].ID,
				Status:    myResults[i].Status,
				ResultLen: len(strings.TrimSpace(myResults[i].Result)),
			},
		})
		// C3 audit: persist EVERY subagent's raw result (the ORIGINAL, not the
		// stripped one — operators inspecting a failed run need to see what
		// the model actually emitted). The collapse step drops the bad ones
		// for the synthesis input, but they're invaluable for /research show.
		if run != nil {
			_ = run.AppendFinding(research.Finding{
				AgentID: r.ID, Role: r.Role, Task: r.Task,
				Status: r.Status, Result: r.Result,
				Finished: time.Now().UTC(),
			})
		}
	}
	// C5: embedding-based rerank + near-duplicate drop. Best-effort —
	// passes through unchanged when the embedder is nil/erroring or there
	// are <2 done findings. Dropped near-dups are excluded from synthesis
	// input but remain on disk via run.AppendFinding above (so /research
	// show still surfaces every sub-agent's raw output).
	myResults = rerankAndDedupFindings(ctx, rt.cfg.Embedder, question, myResults, say)
	combined, references := collapseCitations(myResults)
	if strings.TrimSpace(combined) == "" {
		return "", fmt.Errorf("research: no usable findings from sub-agents (see per-agent status above)")
	}

	// 4. Synthesize the final report (tools OFF — text only).
	say("synthesizing final report…")
	emitSafe(emit, tui.ResearchTimelineUpdate{
		Kind: "synth", Synth: &tui.ResearchSynthData{Starting: true},
	})
	body, err := synthesizeReport(ctx, planner, question, combined)
	if err != nil {
		return "", fmt.Errorf("research: synthesize: %w", err)
	}
	// Short-circuit on the synthesis model's explicit "findings too thin"
	// sentinel — surface a clean failure instead of saving waffle.
	if strings.HasPrefix(strings.TrimSpace(body), noUsableFindingsSentinel) {
		why := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(body), noUsableFindingsSentinel))
		return "", fmt.Errorf("research: no usable findings — %s", why)
	}
	report := strings.TrimRight(body, "\n")
	if references != "" {
		report += "\n\n" + references
	}
	return report, nil
}

// runWithPersistence wraps deepResearch with the C3 run lifecycle: open a Run
// in the store, execute, finalize with the right status + reference list +
// error message. Storage failures degrade gracefully — a missing audit log
// must never block a research pass from completing. Returns the run id (for
// TUI display) and the report.
func runWithPersistence(ctx context.Context, planner model.LLM, rt *TeamRuntime, store *research.Store,
	question string, opts researchOpts, modelID, backend, replayOf string,
	say func(string, ...any), emit timelineEmitter) (id, report string, err error) {

	startedAt := time.Now()
	var run *research.Run
	if store != nil {
		mf := research.Manifest{
			Question: question, Model: modelID, Backend: backend,
			Depth: opts.depth, MaxAgents: opts.maxAgents, Parallel: opts.parallel,
			AwaitTimeout: opts.awaitTimeout, MaxTime: opts.maxTime,
			ReplayOf: replayOf,
		}
		r, oerr := store.Create(mf)
		if oerr != nil {
			say("⚠ couldn't open run record: %v — running without audit log", oerr)
		} else {
			run = r
			id = r.ID()
			say("📋 run id: %s", id)
		}
	}

	report, err = deepResearch(ctx, planner, rt, question, opts, say, run, emit)

	if run != nil {
		status := research.StatusDone
		errMsg := ""
		if err != nil {
			// "no usable findings" with at least one finding on disk is a
			// partial — distinguishable in /research history from a hard
			// error (HTTP failure, ctx cancel, etc.).
			status = research.StatusError
			errMsg = err.Error()
			if strings.Contains(errMsg, "no usable findings") && len(run.Manifest().Findings) > 0 {
				status = research.StatusPartial
			}
		}
		refs := parseReferencesURIs(report)
		if ferr := run.Finalize(report, refs, status, errMsg); ferr != nil {
			say("⚠ couldn't finalize run record: %v", ferr)
		}
	}

	// C4: emit terminal timeline update so the TUI flips the pane to
	// done/error and shows the final ref/finding counts before the pane
	// is torn down by the researchDoneMsg handler.
	if emit != nil {
		doneStatus := "done"
		if err != nil {
			doneStatus = "error"
		}
		errMsg := ""
		if err != nil {
			errMsg = err.Error()
		}
		emit(tui.ResearchTimelineUpdate{
			Kind: "done",
			Done: &tui.ResearchDoneData{
				Status:   doneStatus,
				Error:    errMsg,
				NumRefs:  len(parseReferencesURIs(report)),
				NumFinds: len(rt.Results()),
				Duration: time.Since(startedAt),
			},
		})
	}
	return id, report, err
}

// stripToolCallNoise removes leaked tool-call markup from a finding so the
// synthesizer doesn't trip its NO_USABLE_FINDINGS sentinel on text that's
// purely junk. Handles:
//   - well-formed <tool_call>{...}</tool_call> JSON shape
//   - Hermes XML <function=NAME>...</function> blocks
//   - malformed variants with missing < (e.g. `function=web_read>` from
//     truncated model output — the live aeon-ultimate failure mode)
//   - ```tool_code fenced blocks (gemma style)
//
// Strips ONLY the markup. Surrounding prose survives. If the input was 100%
// markup, returns empty — caller's per-agent log surfaces the strip.
var (
	leakedToolCallJSON = regexp.MustCompile(`(?s)<tool_call>.*?</tool_call>`)
	leakedToolCallXML  = regexp.MustCompile(`(?s)<?\s*function=[^>\s]+>.*?<\s*/\s*function\s*>`)
	leakedToolFenced   = regexp.MustCompile("(?s)```tool_code\\s*\\n.*?\\n```")
	// Truncated/malformed (no close tag): strip from <tool_call> or
	// `function=` to the end of the string. Last-resort cleanup.
	leakedToolTrailing = regexp.MustCompile(`(?s)<?tool_call>?[\s\S]*$|(?:^|\n)[^a-zA-Z0-9]*function=[^>\s]+>[\s\S]*$`)
)

func stripToolCallNoise(s string) string {
	if s == "" {
		return s
	}
	s = leakedToolCallJSON.ReplaceAllString(s, "")
	s = leakedToolCallXML.ReplaceAllString(s, "")
	s = leakedToolFenced.ReplaceAllString(s, "")
	// Apply trailing cleanup only if there's clear evidence of leakage.
	if strings.Contains(s, "<tool_call") || strings.Contains(s, "function=") || strings.Contains(s, "<parameter") {
		s = leakedToolTrailing.ReplaceAllString(s, "")
	}
	return strings.TrimSpace(s)
}

// parseReferencesURIs extracts the URI list from a rendered "## References"
// block, in their global [n] order. Used to persist the reference list on the
// manifest so /research history and /research show can render it.
func parseReferencesURIs(report string) []string {
	idx := strings.LastIndex(report, "## References")
	if idx < 0 {
		return nil
	}
	tail := report[idx+len("## References"):]
	var out []string
	for _, line := range strings.Split(tail, "\n") {
		if m := sourceLine.FindStringSubmatch(line); m != nil {
			out = append(out, strings.TrimSpace(m[2]))
		}
	}
	return out
}

// runResearchWaves spawns the given sub-questions in waves of ≤parallel
// concurrent researchers (Onyx's ≤3 cap), awaiting each wave. Returns the ids
// it spawned (for scoped result collection).
func runResearchWaves(ctx context.Context, rt *TeamRuntime, goal string, subqs []string, parallel int, timeout time.Duration, say func(string, ...any), emit timelineEmitter, cycle, total int) []string {
	if parallel < 1 {
		parallel = 1
	}
	var ids []string
	for i := 0; i < len(subqs); i += parallel {
		end := min(i+parallel, len(subqs))
		for _, sq := range subqs[i:end] {
			id, err := rt.Spawn(ctx, "researcher", researcherTask(goal, sq))
			if err != nil {
				say("  ! spawn failed: %v", err)
				continue
			}
			ids = append(ids, id)
			// C4: pair the spawned id with its sub-question so the timeline
			// pane's per-row glyph and SubQ caption line up correctly.
			emitSafe(emit, tui.ResearchTimelineUpdate{
				Kind:  "agent_status",
				Cycle: cycle, Total: total,
				Agent: &tui.ResearchAgentRow{ID: id, SubQ: sq, Status: "running"},
			})
		}
		say("dispatched %d–%d of %d; waiting…", i+1, end, len(subqs))
		emitSafe(emit, tui.ResearchTimelineUpdate{
			Kind: "wave", Cycle: cycle, Total: total,
			Wave: &tui.ResearchWaveData{From: i + 1, To: end, Total: len(subqs)},
		})
		rt.Await(ctx, timeout)
		if ctx.Err() != nil {
			break
		}
		// Salvage: any agent still RUNNING when the wave timeout fires gets
		// preempted so its ctx-cancel salvage path kicks in (a short tools-
		// off synthesize). Without this, a still-working agent that had
		// gathered real page text would be left with "(no result)" — its
		// working memory is full of usable content, we just never asked it
		// to write the answer. Grace window of 60s for the salvage call.
		if n := rt.CancelStuck(60 * time.Second); n > 0 {
			say("⏱  wave timed out — salvaged %d stuck agent(s)", n)
		}
	}
	return ids
}

// reflect runs one tools-off gap-analysis call over the findings so far,
// returning up to maxQ NEW follow-up sub-questions — or nil when the model
// judges the question sufficiently answered (the loop's "done" signal).
func reflect(ctx context.Context, planner model.LLM, question string, results []TeamResult, maxQ int) ([]string, error) {
	var found strings.Builder
	for _, r := range results {
		if r.Status == "done" && strings.TrimSpace(r.Result) != "" {
			fmt.Fprintf(&found, "- %s\n", clip(oneLineStr(r.Result), 280))
		}
	}
	if strings.TrimSpace(found.String()) == "" {
		return nil, nil // nothing came back — nothing to deepen
	}
	prompt := fmt.Sprintf(`You are reviewing a deep-research investigation in progress.

Question: %s

Findings so far (one line each):
%s
If these findings already answer the question well, output exactly: DONE
Otherwise output ONLY a numbered list of up to %d NEW, specific sub-questions
targeting the most important GAPS or unresolved contradictions still missing.
Do not repeat angles already covered above.`, question, found.String(), maxQ)
	resp, err := planner.Generate(ctx, model.GenerateRequest{
		Messages:  []model.Message{{Role: "user", Content: prompt}},
		MaxTokens: 600,
	})
	if err != nil {
		return nil, err
	}
	// C5: explicit DONE sentinel wins over a chatty model that ALSO emits
	// a list. The pre-C5 behavior relied on parseNumberedList returning
	// empty for any non-list response, which let a thinking model
	// over-spawn (the parser would happily grab "1." lines buried in
	// reasoning even when the model concluded DONE).
	if hasReflectDoneSentinel(resp.Text) {
		return nil, nil
	}
	return parseNumberedList(resp.Text, maxQ), nil
}

// hasReflectDoneSentinel matches a standalone "DONE" token on a line by
// itself (case-insensitive, tolerates leading/trailing non-letter
// decoration like "**DONE**", "[DONE]", "DONE."). Substrings like
// "WELLDONE" or "DONELY" must NOT trigger. Numbered-list items ARE
// skipped — "1. DONE" stays with parseNumberedList (where looksLikeQuestion
// will drop it as not-a-question). Mirrors the clarifier's hasClearSentinel
// pattern logged in lessons.md — match the verdict word line-bounded,
// never via HasPrefix, never via raw Contains.
func hasReflectDoneSentinel(text string) bool {
	for _, raw := range strings.Split(text, "\n") {
		if numberedRE.MatchString(raw) {
			// List items belong to parseNumberedList; don't poach them.
			continue
		}
		stripped := strings.TrimFunc(raw, func(r rune) bool { return !unicode.IsLetter(r) })
		if strings.EqualFold(stripped, "DONE") {
			return true
		}
	}
	return false
}

// normalizeQuestion canonicalizes a sub-question for cross-cycle dedup.
func normalizeQuestion(q string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(q))), " ")
}

// --- output (wiki deposit) ---

// researchDoc wraps the report with a self-describing title + provenance line,
// so a saved wiki page explains itself.
func researchDoc(question, report string) string {
	return fmt.Sprintf("# Research: %s\n_Generated %s by tenant deep-research._\n\n%s\n",
		question, time.Now().Format("2006-01-02 15:04"), strings.TrimSpace(report))
}

// researchFilename derives a stable, readable wiki filename from the question.
func researchFilename(question string) string {
	var parts []string
	for _, w := range strings.Fields(strings.ToLower(question)) {
		clean := strings.Map(func(r rune) rune {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				return r
			}
			return -1
		}, w)
		if clean != "" {
			parts = append(parts, clean)
		}
		if len(parts) >= 8 {
			break
		}
	}
	slug := strings.Join(parts, "-")
	if slug == "" {
		slug = "topic"
	}
	return fmt.Sprintf("research-%s-%s.md", slug, time.Now().Format("2006-01-02"))
}

// writeWikiReport writes the report into the wiki dir as a self-describing
// markdown page, returning the path. The wiki plugin re-indexes it on next
// search, so future turns can wiki_search the findings.
func writeWikiReport(wikiDir, question, report string) (string, error) {
	if strings.TrimSpace(wikiDir) == "" {
		return "", fmt.Errorf("no wiki directory configured")
	}
	if err := os.MkdirAll(wikiDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(wikiDir, researchFilename(question))
	if err := os.WriteFile(path, []byte(researchDoc(question, report)), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// --- prompts ---

func researchPlan(ctx context.Context, planner model.LLM, question string, maxQ int) ([]string, error) {
	prompt := fmt.Sprintf(`You are planning a deep-research investigation.

Break the question below into up to %d INDEPENDENT sub-questions that can each be
researched on their own, in parallel. Each must be a complete, standalone question
(no references to "the above"). Cover distinct angles; avoid overlap.

Output ONLY a numbered list — one sub-question per line, nothing else.

Question: %s`, maxQ, question)
	resp, err := planner.Generate(ctx, model.GenerateRequest{
		Messages:  []model.Message{{Role: "user", Content: prompt}},
		MaxTokens: 1024,
	})
	if err != nil {
		return nil, err
	}
	return parseNumberedList(resp.Text, maxQ), nil
}

func researcherTask(goal, subq string) string {
	return fmt.Sprintf(`Overall research goal: %s

YOUR sub-question to investigate (focus only on this): %s

How to investigate (FOLLOW THIS SEQUENCE EXACTLY):
1. Run ONE web_search for the sub-question.
2. From those results, web_navigate into 2–4 of the most promising URLs and
   web_read each. The reading is where the findings come from.
3. STOP gathering after you have read 2–4 pages with usable content. Do not
   keep searching for "just one more source." Do not redo the same search
   with different keywords. You have enough.
4. (Optional) wiki_search / memory_search for anything internal — at MOST
   one call.
5. WRITE THE REPORT. Your VERY NEXT response after that final tool call must
   be the findings report itself, in plain markdown prose, with NO further
   tool calls.

Hard limits: at most 8 tool calls total. After the 6th, you MUST stop
gathering and write up what you have, even if incomplete — say "I could not
find X" rather than continuing to search.

Your final response IS THE REPORT itself — concise findings written as prose
paragraphs, not a transcript. Do NOT include tool_call JSON, raw search-result
listings, raw HTML, or your reasoning steps in the final response. Write what
you LEARNED from the pages you read.

Requirements:
- Ground every claim in a page you actually read. If you couldn't find it, say
  so plainly in one short sentence — never fabricate.
- Cite inline with [1], [2] markers.
- END with a "## Sources" section listing each marker, one per line.
  Use the URI that matches WHERE you got the claim:
    [1] https://example.com/page             (web — from web_read)
    [2] wiki:notes/path/to/file.md           (internal wiki — from wiki_search)
  Internal sources are first-class — cite them the same way as web pages.
  For a wiki citation, copy the EXACT file path that wiki_search returned (the
  bracketed value, before any " › heading").

Start your final response directly with the findings (no "Here is my report"
preamble).`, goal, subq)
}

// noUsableFindingsSentinel — the synthesis model emits this when the findings
// are too thin to write a real report. We surface that as a clean error
// instead of dumping waffle.
const noUsableFindingsSentinel = "NO_USABLE_FINDINGS:"

func synthesizeReport(ctx context.Context, planner model.LLM, question, findings string) (string, error) {
	prompt := fmt.Sprintf(`You are writing a FINAL research report. Output ONLY the markdown report —
nothing else. No preamble. No meta-commentary. No numbered analysis steps. No
"Self-Correction" / "Mental Draft" / "Drafting Response" sections. No chain-of-
thought. No quotes from this prompt. START your output with a markdown heading
(# Title) and write the report directly.

Using ONLY the sub-agent findings below, write a comprehensive, well-structured
markdown report answering the question.

Requirements:
- Use ONLY information present in the findings — never invent facts or sources.
- PRESERVE the [n] citation markers exactly as they appear in the findings.
- If findings conflict, or a sub-agent reported it couldn't find something, say
  so plainly in ONE short paragraph — do not pad with speculation.
- Do NOT write a "References"/"Sources" section — it is appended automatically.

If the findings contain ONLY tool-call logs / a search query with no actual
content, output EXACTLY one line:
%s <one-sentence explanation of what was missing>

Question: %s

=== Sub-agent findings ===
%s`, noUsableFindingsSentinel, question, findings)
	resp, err := planner.Generate(ctx, model.GenerateRequest{
		Messages:  []model.Message{{Role: "user", Content: prompt}},
		MaxTokens: 8000, // long report; no tools
	})
	if err != nil {
		return "", err
	}
	return sanitizeReport(resp.Text), nil
}

// sanitizeReport is the safety net for sloppy model output: collapses runaway
// repeated lines ("..." floods etc.) and trims leading reasoning preambles
// when the model ignored the prompt and dumped its chain-of-thought.
func sanitizeReport(s string) string {
	// 1. Collapse degeneration. 3+ consecutive lines that are just dots / a
	//    single repeated short char → one truncation note. Catches the "...×N"
	//    flood without touching legitimate ellipses inside prose.
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	run := 0
	var last string
	flush := func() {
		if run >= 3 {
			out = append(out, "…[truncated repeated output]")
		} else {
			for i := 0; i < run; i++ {
				out = append(out, last)
			}
		}
		run = 0
	}
	for _, l := range lines {
		t := strings.TrimSpace(l)
		degenerate := t != "" && (strings.TrimLeft(t, ".") == "" ||
			(len(t) <= 8 && strings.Count(t, string(t[0])) == len(t)))
		if degenerate && (run == 0 || strings.TrimSpace(last) == t) {
			last = l
			run++
			continue
		}
		flush()
		out = append(out, l)
	}
	flush()
	cleaned := strings.Join(out, "\n")

	// 2. If output starts with model "reasoning" before any markdown heading,
	//    strip the preamble up to the first heading. Heuristic — only triggers
	//    when explicit reasoning markers are present, so we don't truncate
	//    legitimate intros.
	if looksLikeReasoningPreamble(cleaned) {
		if idx := firstMarkdownHeading(cleaned); idx > 0 {
			cleaned = cleaned[idx:]
		}
	}
	return strings.TrimSpace(cleaned)
}

func looksLikeReasoningPreamble(s string) bool {
	// Strong tells of chain-of-thought leakage (we've seen each of these live).
	first := s
	if len(first) > 600 {
		first = first[:600]
	}
	for _, marker := range []string{"*Self-Correction", "Mental Draft", "*Drafting Response",
		"*Final Polish", "*Refining based", "Self-Correction:", "Constraint Check:"} {
		if strings.Contains(first, marker) {
			return true
		}
	}
	return false
}

func firstMarkdownHeading(s string) int {
	// Find a line starting with one or more '#' followed by a space.
	for i := 0; i < len(s); i++ {
		if i == 0 || s[i-1] == '\n' {
			j := i
			for j < len(s) && s[j] == '#' {
				j++
			}
			if j > i && j < len(s) && s[j] == ' ' {
				return i
			}
		}
	}
	return -1
}

// --- parsing + citation collapse ---

var (
	numberedRE = regexp.MustCompile(`^\s*(?:\d+[.)]|[-*•])\s+(.+\S)\s*$`)
	citeMarker = regexp.MustCompile(`\[(\d+)\]`)
	// sourceLine: a Sources-block entry. Accepts any RFC-3986 scheme so internal
	// citations (wiki:<file>, memory:<fact-id>, skill:<name>) are first-class
	// alongside http(s). Pattern: optional [N] / N) / N. prefix + URI starting
	// with scheme: + non-whitespace tail. The first matching pattern wins, so
	// dedup keys naturally on full URI (no scheme normalization needed).
	sourceLine  = regexp.MustCompile(`^\s*\[?(\d+)\]?[.)]?\s+([a-z][a-z0-9+.-]*:\S+)`)
	sourcesHead = regexp.MustCompile(`(?im)^\s*#{1,6}\s*sources\s*:?\s*$`)
)

// parseNumberedList extracts up to maxQ items from a numbered/bulleted list.
func parseNumberedList(s string, maxQ int) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if m := numberedRE.FindStringSubmatch(line); m != nil {
			item := strings.TrimSpace(m[1])
			if item != "" && looksLikeQuestion(item) {
				out = append(out, item)
			}
		}
		if len(out) >= maxQ {
			break
		}
	}
	return out
}

// looksLikeQuestion filters out bulleted CoT/reasoning that the model
// occasionally smuggles into a "numbered list of sub-questions." Live trigger:
// reflect() got back items like "*Correction:* The prompt says ... I need to
// dig deeper" — a bullet-shaped reasoning paragraph that researchers then
// dutifully tried to "investigate", producing garbage. We accept anything
// reasonable as a question; we ONLY reject the unambiguous reasoning shapes.
//
//   - >300 chars: real sub-questions are short. CoT paragraphs aren't.
//   - Starts with a reasoning marker (`*Correction`, `*Self-Correction`,
//     `*Drafting`, `Mental Draft`, `Let me`, `I need to`, `Wait,`, `Actually,`)
//     — these are tells of CoT leakage, never a real question.
func looksLikeQuestion(item string) bool {
	if len(item) > 300 {
		return false
	}
	lower := strings.ToLower(item)
	for _, marker := range []string{
		"*correction", "*self-correction", "self-correction:", "*drafting",
		"mental draft", "*refining", "let me ", "i need to ", "wait,",
		"actually,", "the prompt says", "as i mentioned",
	} {
		if strings.HasPrefix(lower, marker) {
			return false
		}
	}
	return true
}

// collapseCitations renumbers each researcher's local [n] markers into one
// global, deduplicated bibliography. Returns the concatenated rewritten bodies
// (for synthesis input) and a "## References" block. Robust to a researcher
// emitting no/garbled sources (its markers are left as-is).
func collapseCitations(results []TeamResult) (combined, references string) {
	globalByURL := map[string]int{}
	var order []string // URLs in global numbering order
	var cb strings.Builder
	finding := 0
	for _, res := range results {
		body := strings.TrimSpace(res.Result)
		if res.Status != "done" || body == "" || body == "(no result)" {
			continue
		}
		text, srcs := splitSources(body)
		rewritten := citeMarker.ReplaceAllStringFunc(text, func(mk string) string {
			n, _ := strconv.Atoi(mk[1 : len(mk)-1])
			url, ok := srcs[n]
			if !ok {
				return mk // unknown local marker — leave it
			}
			g, seen := globalByURL[url]
			if !seen {
				order = append(order, url)
				g = len(order)
				globalByURL[url] = g
			}
			return "[" + strconv.Itoa(g) + "]"
		})
		finding++
		fmt.Fprintf(&cb, "### Finding %d\n%s\n\n", finding, strings.TrimSpace(rewritten))
	}
	if len(order) > 0 {
		var rb strings.Builder
		rb.WriteString("## References\n")
		for i, url := range order {
			fmt.Fprintf(&rb, "[%d] %s\n", i+1, url)
		}
		references = strings.TrimRight(rb.String(), "\n")
	}
	return strings.TrimRight(cb.String(), "\n"), references
}

// splitSources separates a report body from its trailing "## Sources" block and
// parses that block into a local marker→URL map.
func splitSources(report string) (body string, srcs map[int]string) {
	srcs = map[int]string{}
	loc := sourcesHead.FindStringIndex(report)
	if loc == nil {
		return strings.TrimRight(report, "\n"), srcs
	}
	body = strings.TrimRight(report[:loc[0]], "\n")
	for _, line := range strings.Split(report[loc[1]:], "\n") {
		if m := sourceLine.FindStringSubmatch(line); m != nil {
			n, _ := strconv.Atoi(m[1])
			srcs[n] = strings.TrimSpace(m[2])
		}
	}
	return body, srcs
}

// --- TUI control ---

// researchControl implements tui.ResearchControl: runs the Phase-A pipeline
// against the TUI's live agent + shared TeamRuntime, streaming progress to the
// system feed. The final report comes back to the chat pane. C3: the optional
// store persists each pass as an auditable, replayable run.
type researchControl struct {
	ag      *agent.Agent
	rt      *TeamRuntime
	say     func(string, ...any) // → the TUI system feed channel
	wikiDir string               // auto-deposit destination (empty = chat-only)
	opts    researchOpts
	store   *research.Store // nil = no persistence (legacy)
	// emitTimeline, if set, publishes C4 structured updates to the TUI's
	// research-timeline pane. nil = no live pane (CLI runs).
	emitTimeline timelineEmitter
	// wikiIndex, if set, is the wiki plugin's live *Index. After a
	// successful writeWikiReport, runOne calls wikiIndex.Reindex(ctx) so
	// the just-deposited file is immediately searchable — without this,
	// the wiki index lags the disk by N turns and the agent can't
	// surface its own research until the next wiki_search happens to
	// fire (the 2026-05-26 lost-context bug, TEN-44). nil = no auto-
	// reindex (legacy / CLI without wired index).
	wikiIndex *wiki.Index
}

func (rc *researchControl) Research(ctx context.Context, question string) (string, error) {
	return rc.runOne(ctx, question, "", false)
}

// ResearchAfterClarify continues a research pass after the user answered the
// clarifier's questions. Bypasses the clarifier check on this second pass so
// we don't loop back to the user. Caller passes the ENRICHED question
// (original + " Additional context from user: <answer>"), built via
// EnrichClarified. Used by the TUI's pending-clarification state machine
// and the CLI's stdin-answer path.
func (rc *researchControl) ResearchAfterClarify(ctx context.Context, enrichedQuestion string) (string, error) {
	return rc.runOne(ctx, enrichedQuestion, "", true)
}

// runOne is the shared internal driver used by Research + Replay +
// ResearchAfterClarify. replayOf is stamped onto the manifest when non-
// empty so /research history can show the chain ("replay of <prior-id>").
// skipClarify=true bypasses the C2 vague-question gate (the second pass of
// a clarify round-trip, or a replay where the question is already known).
func (rc *researchControl) runOne(ctx context.Context, question, replayOf string, skipClarify bool) (string, error) {
	planner, profile, err := rc.ag.Router().LLMForRole(ctx, model.RolePlanner)
	if err != nil {
		return "", fmt.Errorf("resolve planner: %w", err)
	}
	opts := rc.opts
	if skipClarify {
		opts.noClarify = true
	}
	_, report, err := runWithPersistence(ctx, planner, rc.rt, rc.store,
		question, opts, profile.Model, profile.Backend, replayOf, rc.say, rc.emitTimeline)
	if err != nil {
		return "", err
	}
	// Auto-deposit into the wiki when configured. TEN-44: force an
	// immediate reindex so the just-written file is searchable on the
	// next turn — the prior "lazy reindex on next wiki_search" path
	// allowed deposit-to-retrieval gaps measured in turns (the proximate
	// cause of the 2026-05-26 lost-context bug).
	if rc.wikiDir != "" {
		if path, werr := writeWikiReport(rc.wikiDir, question, report); werr != nil {
			rc.say("⚠ couldn't save to wiki: %v", werr)
		} else {
			rc.say("📄 saved to wiki: %s", path)
			if rc.wikiIndex != nil {
				if _, _, rerr := rc.wikiIndex.Reindex(ctx); rerr != nil {
					// Non-fatal: file is on disk; next wiki_search still triggers
					// the lazy reindex path (wiki.go:340-344) as a fallback.
					rc.say("⚠ wiki indexed file but reindex failed: %v", rerr)
				}
			}
		}
	}
	return report, nil
}

// --- C3: history / show / replay / delete ---

// ResearchHistory returns the last `limit` runs, newest first, mapped to the
// TUI-facing summary row (no leak of the research.Manifest type across the
// package boundary).
func (rc *researchControl) ResearchHistory(limit int) ([]tui.ResearchHistoryRow, error) {
	if rc.store == nil {
		return nil, fmt.Errorf("research history is unavailable (no store configured)")
	}
	ms, err := rc.store.List(limit)
	if err != nil {
		return nil, err
	}
	out := make([]tui.ResearchHistoryRow, 0, len(ms))
	for _, m := range ms {
		row := tui.ResearchHistoryRow{
			ID: m.ID, Question: m.Question, Status: string(m.Status),
			Started: m.Started, Model: m.Model, Cycles: m.Cycles,
			NumFinds: len(m.Findings), NumRefs: len(m.References),
			ReplayOf: m.ReplayOf,
		}
		if !m.Finished.IsZero() {
			row.Duration = m.Finished.Sub(m.Started)
		}
		out = append(out, row)
	}
	return out, nil
}

// ResearchShow returns a rendered text representation of a past run: a short
// metadata header followed by the report body. This is what the TUI prints to
// the chat pane.
func (rc *researchControl) ResearchShow(id string) (string, error) {
	if rc.store == nil {
		return "", fmt.Errorf("research history is unavailable")
	}
	m, body, err := rc.store.Get(id)
	if err != nil {
		return "", err
	}
	return renderResearchShow(m, body), nil
}

// renderResearchShow formats a run for /research show <id>. Header carries the
// question + model + status + duration; the body is the persisted report.md.
func renderResearchShow(m research.Manifest, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "── %s ──\n", m.ID)
	fmt.Fprintf(&b, "Question: %s\n", m.Question)
	fmt.Fprintf(&b, "Status:   %s   Model: %s   Cycles: %d   Findings: %d   Refs: %d\n",
		m.Status, m.Model, m.Cycles, len(m.Findings), len(m.References))
	if m.ReplayOf != "" {
		fmt.Fprintf(&b, "Replay of: %s\n", m.ReplayOf)
	}
	dur := ""
	if !m.Finished.IsZero() {
		dur = m.Finished.Sub(m.Started).Round(time.Second).String()
	}
	fmt.Fprintf(&b, "Started: %s   Duration: %s\n", m.Started.Format("2006-01-02 15:04:05"), dur)
	if m.ErrorMessage != "" {
		fmt.Fprintf(&b, "Error:    %s\n", m.ErrorMessage)
	}
	b.WriteString("──\n\n")
	if strings.TrimSpace(body) == "" {
		b.WriteString("(no report — the run finished without a synthesized report)")
	} else {
		b.WriteString(body)
	}
	return b.String()
}

// ResearchReplay re-runs a past question against the CURRENT code + model,
// stamping the new run as replay_of:<id>. Returns the NEW report.
func (rc *researchControl) ResearchReplay(ctx context.Context, id string) (string, error) {
	if rc.store == nil {
		return "", fmt.Errorf("research history is unavailable")
	}
	m, _, err := rc.store.Get(id)
	if err != nil {
		return "", fmt.Errorf("replay: %w", err)
	}
	rc.say("⟳ replaying %s: %s", id, clip(m.Question, 80))
	// Replays skip the C2 clarifier — the question already produced findings
	// once (good or bad). Re-asking would surprise the user expecting an
	// identical re-run for diffing.
	return rc.runOne(ctx, m.Question, id, true)
}

// ResearchDelete purges a past run from disk.
func (rc *researchControl) ResearchDelete(id string) error {
	if rc.store == nil {
		return fmt.Errorf("research history is unavailable")
	}
	return rc.store.Delete(id)
}

// --- CLI command ---

// cmdResearch is `tenant research "<question>"` — the Phase-A Deep Research.
// C3 sub-commands (list/show/replay/delete) are NEW first-token forms — `tenant
// research list`, `tenant research show <id>`, etc. — and don't take the
// generation flags. We dispatch them before the flag parser to avoid those
// flag-vs-positional conflicts the standard `flag` package can't handle.
func cmdResearch(ctx context.Context, args []string) error {
	if len(args) >= 1 {
		switch strings.ToLower(args[0]) {
		case "list", "history":
			return cmdResearchList(ctx, args[1:])
		case "show":
			return cmdResearchShow(ctx, args[1:])
		case "delete", "rm":
			return cmdResearchDelete(ctx, args[1:])
		}
	}
	fs := flag.NewFlagSet("research", flag.ContinueOnError)
	c := bindCommon(fs)
	pf := bindPluginFlags(fs)
	maxAgents := fs.Int("agents", 5, "sub-questions per cycle (plan + each reflection)")
	parallel := fs.Int("parallel", 3, "max concurrent researchers per wave")
	awaitTO := fs.Duration("await-timeout", 10*time.Minute, "how long to wait for each wave")
	depth := fs.Int("depth", 2, "reflection cycles (1 = single pass; >1 = iterative deepening)")
	maxTime := fs.Duration("max-time", 20*time.Minute, "wall-clock cap across all cycles (0 = none)")
	out := fs.String("out", "", "write the report to this exact path (overrides --wiki/stdout)")
	toStdout := fs.Bool("stdout", false, "force-print the report to stdout (skips wiki auto-deposit)")
	noClarify := fs.Bool("no-clarify", false, "skip the up-front clarification prompt for vague queries")
	if err := fs.Parse(args); err != nil {
		return err
	}
	question := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if question == "" {
		return fmt.Errorf("usage: tenant research [flags] \"<question>\"\n       tenant research list [N]\n       tenant research show <id>\n       tenant research delete <id>")
	}
	if err := c.resolve(); err != nil {
		return err
	}
	applyPluginConfig(c, pf)
	pf.wikiDir = expandPath(pf.wikiDir)
	pf.sqlDB = expandPath(pf.sqlDB)
	pf.gsuiteSAJSON = expandPath(pf.gsuiteSAJSON)
	log := newLogger() // stderr; stdout is reserved for the report

	router, err := buildRouter(c, log)
	if err != nil {
		return err
	}
	st, closeStores, err := openStores(c)
	if err != nil {
		return err
	}
	defer closeStores()

	// Web research needs a browser per researcher (concurrent navigation can't
	// share one tab). Force it on; keep it OUT of the shared mux.
	teamWeb := true
	webCfg := webConfig(c.cfgDir, pf)
	webPolicy := web.Policy{AllowInteract: pf.webAllowInteract}
	pf.web = false

	shared, _, cleanupMux, err := buildToolMux(ctx, c, router, pf, nil, log)
	if err != nil {
		return err
	}
	defer cleanupMux()

	skillStore, serr := skills.Open(filepath.Join(c.dataDir, "skills.db"))
	if serr != nil {
		return serr
	}
	defer skillStore.Close()
	skEmb, embProfile, _ := router.EmbedderForRole(ctx, model.RoleEmbedder)
	prof, _ := userprofile.Load(c.dataDir, c.agent)
	bus := orchestra.NewBus()
	bus.Register(c.agent)

	var outMu sync.Mutex
	say := func(format string, a ...any) {
		outMu.Lock()
		fmt.Fprintf(os.Stderr, "· "+format+"\n", a...)
		outMu.Unlock()
	}
	observe := func(id string, e agent.Event) {
		switch e.Kind {
		case agent.EventToolCall:
			say("  [%s] → %s %s", id, e.Tool, clip(e.Args, 70))
		case agent.EventToolResult:
			// Tool RESULTS were previously invisible — so a web_search that came
			// back empty or a web_navigate that errored looked identical to a
			// successful one from outside. Show result size + a short preview.
			tag := "✓"
			if e.IsErr {
				tag = "✗"
			}
			say("  [%s] %s %s ← %d chars: %s", id, tag, e.Tool, len(e.Result), clip(oneLineStr(e.Result), 100))
		case agent.EventFinal:
			say("  [%s] ✦ done", id)
		case agent.EventError:
			say("  [%s] ✗ %s", id, clip(e.Text, 100))
		case agent.EventTruncated:
			say("  [%s] ! loop ceiling — synthesized", id)
		}
	}

	var researchAgents map[string]*agentProfile
	if lcInit, err := loadLaunchConfig(c.cfgDir); err == nil {
		researchAgents = effectiveAgents(lcInit) // built-ins + config (TEN-132)
	} else {
		researchAgents = effectiveAgents(nil)
	}
	rt := newTeamRuntime(TeamConfig{
		Bus: bus, Router: router, Stores: st, Shared: shared, Skills: skillStore,
		Embedder: skEmb, EmbedderID: string(embProfile.ID), Compressor: &compress.Compressor{Router: router, Logger: log},
		Profile: prof, OrchID: c.agent, Log: log, Observe: observe,
		Web: teamWeb, WebConfig: webCfg, WebPolicy: webPolicy,
		Shots:         filepath.Join(c.dataDir, "screenshots"),
		AgentProfiles: researchAgents,
		CfgDir:        c.cfgDir,
		EmbedProfile:  embProfile,
	})
	defer rt.Close()

	planner, profile, err := router.LLMForRole(ctx, model.RolePlanner)
	if err != nil {
		return fmt.Errorf("resolve planner: %w", err)
	}

	// C3 audit: open a research store under <data>/research. A store-open
	// failure degrades gracefully — runWithPersistence handles a nil store.
	rstore, serr := research.New(c.dataDir)
	if serr != nil {
		say("⚠ couldn't open research store: %v — running without audit log", serr)
		rstore = nil
	}

	mkOpts := func(skipClarify bool) researchOpts {
		return researchOpts{
			maxAgents: *maxAgents, parallel: *parallel, awaitTimeout: *awaitTO,
			depth: *depth, maxTime: *maxTime,
			noClarify: *noClarify || skipClarify,
		}
	}
	_, report, err := runWithPersistence(ctx, planner, rt, rstore, question, mkOpts(false),
		profile.Model, profile.Backend, "", say, nil)
	// C2: clarifier asked the user something. Prompt on stdin, fold the
	// answer into the question, retry with --no-clarify so we don't loop.
	var clarErr *ClarifyNeededError
	if errors.As(err, &clarErr) {
		answer, prerr := promptClarifyStdin(clarErr.Questions)
		if prerr != nil {
			return fmt.Errorf("clarify: %w", prerr)
		}
		enriched := EnrichClarified(clarErr.Question, answer)
		say("→ proceeding with: %s", clip(oneLineStr(enriched), 120))
		_, report, err = runWithPersistence(ctx, planner, rt, rstore, enriched, mkOpts(true),
			profile.Model, profile.Backend, "", say, nil)
	}
	if err != nil {
		return err
	}

	// Output precedence: --out explicit path > --stdout > wiki dir > stdout.
	switch {
	case *out != "":
		if werr := os.WriteFile(*out, []byte(researchDoc(question, report)), 0o644); werr != nil {
			return fmt.Errorf("write report: %w", werr)
		}
		say("report written to %s", *out)
	case *toStdout:
		fmt.Println(report)
	case pf.wikiDir != "":
		path, werr := writeWikiReport(pf.wikiDir, question, report)
		if werr != nil {
			say("⚠ couldn't write to wiki (%v) — printing instead", werr)
			fmt.Println(report)
			return nil
		}
		say("📄 saved to wiki: %s", path)
	default:
		fmt.Println(report)
	}
	return nil
}

// cmdResearchList — `tenant research list [N]` — print the run history.
// Plain text, columnar; matches the TUI table for muscle-memory parity.
func cmdResearchList(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("research list", flag.ContinueOnError)
	c := bindCommon(fs)
	limit := fs.Int("limit", 20, "max entries (0 = all)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// A bare positional like `research list 50` overrides --limit; convenient.
	if rest := fs.Args(); len(rest) == 1 {
		if n, err := strconv.Atoi(rest[0]); err == nil {
			*limit = n
		}
	}
	if err := c.resolve(); err != nil {
		return err
	}
	store, err := research.New(c.dataDir)
	if err != nil {
		return fmt.Errorf("open research store: %w", err)
	}
	ms, err := store.List(*limit)
	if err != nil {
		return err
	}
	if len(ms) == 0 {
		fmt.Println("no past research runs yet — try `tenant research \"<question>\"`")
		return nil
	}
	fmt.Printf("%d run(s):\n", len(ms))
	for _, m := range ms {
		dur := "—"
		if !m.Finished.IsZero() {
			dur = m.Finished.Sub(m.Started).Round(time.Second).String()
		}
		extra := ""
		if m.ReplayOf != "" {
			extra = "  (replay of " + m.ReplayOf + ")"
		}
		fmt.Printf("  %s  %-8s  %2dF/%dR  %6s  %s — %s%s\n",
			m.Started.Local().Format("01-02 15:04"),
			m.Status, len(m.Findings), len(m.References), dur,
			m.ID, clip(m.Question, 60), extra)
	}
	return nil
}

// cmdResearchShow — `tenant research show <id>` — emit a past run's report
// to stdout (the metadata header + report body).
func cmdResearchShow(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("research show", flag.ContinueOnError)
	c := bindCommon(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	id := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if id == "" {
		return fmt.Errorf("usage: tenant research show <id>")
	}
	if err := c.resolve(); err != nil {
		return err
	}
	store, err := research.New(c.dataDir)
	if err != nil {
		return fmt.Errorf("open research store: %w", err)
	}
	m, body, err := store.Get(id)
	if err != nil {
		return fmt.Errorf("show %s: %w", id, err)
	}
	fmt.Println(renderResearchShow(m, body))
	return nil
}

// cmdResearchDelete — `tenant research delete <id>` — purge a past run.
func cmdResearchDelete(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("research delete", flag.ContinueOnError)
	c := bindCommon(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	id := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if id == "" {
		return fmt.Errorf("usage: tenant research delete <id>")
	}
	if err := c.resolve(); err != nil {
		return err
	}
	store, err := research.New(c.dataDir)
	if err != nil {
		return fmt.Errorf("open research store: %w", err)
	}
	if err := store.Delete(id); err != nil {
		return err
	}
	fmt.Printf("deleted %s\n", id)
	return nil
}

// --- C5: rerank + near-duplicate drop (best-effort, embedder-driven) ---

// rerankDupThreshold — pairwise cosine ≥ this on two FULL finding bodies
// means "same coverage." Tight on purpose: these are multi-paragraph
// reports, not snippets, so 0.92 is the floor for "another agent said
// the same thing differently." Lower would start eating distinct angles.
const rerankDupThreshold = 0.92

// rerankAndDedupFindings reorders done findings by descending embedding
// similarity to the question and drops near-duplicates whose body sim
// to an already-kept finding exceeds rerankDupThreshold. Failures
// (status != done OR empty body) keep their spawn-order position at the
// tail — collapseCitations skips them anyway, but their presence
// preserves the per-agent audit shape for any downstream consumer.
//
// Best-effort: returns input unchanged when embedder is nil, when the
// embed batch errors, or when fewer than 2 done findings exist. Drops
// are surfaced via say() so operators see why a sub-agent's report
// didn't reach synthesis. The dropped reports stay on disk via
// run.AppendFinding (called upstream) — only the synthesizer's input
// is trimmed.
func rerankAndDedupFindings(
	ctx context.Context, embedder model.Embedder, question string,
	results []TeamResult, say func(string, ...any),
) []TeamResult {
	if embedder == nil || len(results) < 2 {
		return results
	}
	texts := []string{question}
	indexed := make([]int, 0, len(results))
	for i, r := range results {
		if r.Status != "done" || strings.TrimSpace(r.Result) == "" {
			continue
		}
		texts = append(texts, r.Result)
		indexed = append(indexed, i)
	}
	if len(indexed) < 2 {
		return results
	}
	vecs, err := embedder.Embed(ctx, texts)
	if err != nil || len(vecs) != len(texts) {
		return results
	}
	qv := vecs[0]
	type scored struct {
		idx int
		sim float64
		vec []float32
	}
	rows := make([]scored, len(indexed))
	for j, i := range indexed {
		rows[j] = scored{idx: i, sim: cosineSimF32(qv, vecs[j+1]), vec: vecs[j+1]}
	}
	sort.SliceStable(rows, func(a, b int) bool { return rows[a].sim > rows[b].sim })

	type keptRow struct {
		idx int
		vec []float32
	}
	kept := make([]keptRow, 0, len(rows))
	dropped := map[int]int{} // dropped-idx → kept-idx that subsumed it
	for _, r := range rows {
		dup := -1
		for _, k := range kept {
			if cosineSimF32(r.vec, k.vec) >= rerankDupThreshold {
				dup = k.idx
				break
			}
		}
		if dup >= 0 {
			dropped[r.idx] = dup
			continue
		}
		kept = append(kept, keptRow{idx: r.idx, vec: r.vec})
	}

	if say != nil {
		for di, ki := range dropped {
			say("  ⊘ [%s] near-duplicate of [%s] — dropping from synthesis input",
				results[di].ID, results[ki].ID)
		}
	}

	out := make([]TeamResult, 0, len(results))
	keptIdx := make(map[int]bool, len(kept))
	for _, k := range kept {
		out = append(out, results[k.idx])
		keptIdx[k.idx] = true
	}
	// Tail: every original result that wasn't kept AND wasn't dropped as a
	// near-dup. These are the failures / empties; collapseCitations skips
	// them but keeping them in the slice preserves the audit shape.
	for i, r := range results {
		if keptIdx[i] {
			continue
		}
		if _, isDup := dropped[i]; isDup {
			continue
		}
		out = append(out, r)
	}
	return out
}

// cosineSimF32 — local copy of the formula used in distill/cosine.go,
// inlined here to keep cmd/tenant from importing a memory-tier package
// for one 13-line function. Returns 0 on length mismatch or zero norm.
func cosineSimF32(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		fa, fb := float64(a[i]), float64(b[i])
		dot += fa * fb
		na += fa * fa
		nb += fb * fb
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
