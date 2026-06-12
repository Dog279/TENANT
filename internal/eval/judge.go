package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"tenant/internal/model"
)

// Rubric is the anchored grading scheme attached to a Task. v0 used a
// free-form natural-language prompt; v1 uses anchors at three reference
// points (1 / 3 / 5) so the judge has concrete grounding rather than a
// raw Likert scale. Literature is clear that anchored rubrics materially
// outperform free-form scoring for LLM-as-judge (Galileo, Monte Carlo,
// "Silent Judge" arXiv 2509.26072 length-bias work).
type Rubric struct {
	Criterion        string         `yaml:"criterion"`         // what the judge is asking
	Anchors          map[int]string `yaml:"anchors,omitempty"` // {1: "bad example", 3: "ok", 5: "ideal"}
	LengthNormalized bool           `yaml:"length_normalized,omitempty"`
}

// JudgeResult is one judge invocation's outcome. Score is 1-5 on the
// anchored scale; Reasoning is the judge's one-sentence justification
// (recorded for diagnostics; future analytics may surface it).
type JudgeResult struct {
	Score     int    `json:"score"`
	Reasoning string `json:"reasoning"`
}

// Judge is the interface every grading backend implements. v1 ships
// LLMJudge (real model call) and FixtureJudge (deterministic, for the
// harness's own tests).
type Judge interface {
	Grade(ctx context.Context, t *Task, response string, calls []FixtureToolCall) (JudgeResult, error)
}

// JudgePrompt is the anchored-rubric prompt template. Embeds the
// length-bias countermeasure ("Silent Judge" mitigation) so judges
// don't over-weight surface fluency once the deterministic gate passes.
//
// The {{TASK_PROMPT}} / {{RESPONSE}} / etc placeholders are substituted
// at runtime; we deliberately do NOT use text/template to keep the
// substitution surface minimal and auditable.
const JudgePrompt = `You are grading an agent's response against an anchored rubric.

TASK PROMPT TO THE AGENT:
{{TASK_PROMPT}}

AGENT RESPONSE TO GRADE:
{{RESPONSE}}

TOOL CALLS THE AGENT MADE (for context):
{{TOOL_CALLS}}

RUBRIC CRITERION:
{{CRITERION}}

ANCHORED REFERENCE SCORES:
{{ANCHORS}}

INSTRUCTIONS:
- Return a single integer score from 1 to 5 inclusive, matching the closest anchor.
- Penalize answers that pad without adding information from the tool results. Brevity matching the tool output's information content is preferred.
- Output strictly JSON: {"score": <1-5>, "reasoning": "<one sentence>"}
- Do not include markdown fences, prefixes, or commentary outside the JSON.`

// LLMJudge invokes a model.LLM (resolved by router as RoleJudge) to
// produce an anchored score. Enforces different-family default at the
// caller layer — this struct just calls whatever LLM it's handed.
type LLMJudge struct {
	LLM     model.LLM
	Profile model.Profile // for diagnostics; logged on grading errors
}

// Grade renders the prompt + calls the LLM + parses the JSON, retrying
// ONCE on a failed call or unusable output before giving up (TEN-197).
// Thinking models can burn the whole completion budget on internal
// reasoning and emit nothing — observed live: z.ai GLM returned "" on
// 18/57 graded tasks — so the budget is generous and a fresh retry
// covers the transient cases. An error return means the judge was
// unusable even after the retry; the harness records the task UNGRADED
// rather than scoring the agent zero for the grader's failure.
func (j *LLMJudge) Grade(ctx context.Context, t *Task, response string, calls []FixtureToolCall) (JudgeResult, error) {
	if j.LLM == nil {
		return JudgeResult{}, fmt.Errorf("judge: nil LLM")
	}
	prompt := renderJudgePrompt(t, response, calls)
	req := model.GenerateRequest{
		Messages: []model.Message{{Role: "user", Content: prompt}},
		// Generous on purpose: thinking models spend invisible reasoning
		// tokens BEFORE the JSON; 200 was observed truncating verdicts
		// mid-string and returning empty text entirely (TEN-197).
		MaxTokens:   1000,
		Temperature: 0,
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 && ctx.Err() != nil {
			break // don't burn a retry on a dead context
		}
		resp, err := j.LLM.Generate(ctx, req)
		if err != nil {
			lastErr = fmt.Errorf("judge: LLM call: %w", err)
			continue
		}
		jr, perr := parseJudgeOutput(resp.Text)
		if perr != nil {
			lastErr = perr
			continue
		}
		return jr, nil
	}
	return JudgeResult{}, lastErr
}

// FixtureJudge returns a pre-set score regardless of input. Used by
// internal eval-harness tests so we can exercise the judge wiring path
// deterministically without a real LLM endpoint. Also useful for
// operators who want to validate task YAML / rubric structure offline.
type FixtureJudge struct {
	Score     int
	Reasoning string
}

func (f *FixtureJudge) Grade(_ context.Context, _ *Task, _ string, _ []FixtureToolCall) (JudgeResult, error) {
	return JudgeResult{Score: f.Score, Reasoning: f.Reasoning}, nil
}

// renderJudgePrompt does the placeholder substitution. Anchors are
// rendered in sorted key order for deterministic prompt output (so
// caching, regression diffing, and audit logs stay stable).
func renderJudgePrompt(t *Task, response string, calls []FixtureToolCall) string {
	rubric := Rubric{}
	if t.Expected.Rubric != nil {
		rubric = *t.Expected.Rubric
	}
	out := JudgePrompt
	out = strings.ReplaceAll(out, "{{TASK_PROMPT}}", t.Prompt)
	out = strings.ReplaceAll(out, "{{RESPONSE}}", response)
	out = strings.ReplaceAll(out, "{{TOOL_CALLS}}", renderCalls(calls))
	out = strings.ReplaceAll(out, "{{CRITERION}}", rubric.Criterion)
	out = strings.ReplaceAll(out, "{{ANCHORS}}", renderAnchors(rubric.Anchors))
	return out
}

func renderCalls(calls []FixtureToolCall) string {
	if len(calls) == 0 {
		return "(none)"
	}
	var b strings.Builder
	for _, c := range calls {
		fmt.Fprintf(&b, "- %s %s\n", c.Tool, c.Args)
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderAnchors(anchors map[int]string) string {
	if len(anchors) == 0 {
		return "(no anchors; use 1=poor, 3=acceptable, 5=ideal as defaults)"
	}
	// Sorted-keys render keeps the prompt deterministic.
	keys := []int{1, 2, 3, 4, 5}
	var b strings.Builder
	for _, k := range keys {
		if v, ok := anchors[k]; ok {
			fmt.Fprintf(&b, "  %d: %s\n", k, v)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// jsonObjectRe finds the first {...} block in the judge's response.
// Some judges still wrap output in markdown fences despite the
// instruction; this strips that noise before json.Unmarshal.
var jsonObjectRe = regexp.MustCompile(`(?s)\{.*?\}`)

// parseJudgeOutput extracts the verdict from the judge's raw text. An
// error return means the text is UNUSABLE (empty, or no score found
// anywhere) — the caller retries once, then marks the task ungraded.
// This used to return (score 0, nil) for unusable text, which the
// scorer read as a legitimate grade of zero: the agent was punished for
// the grader's failure (TEN-197). Salvage order: direct JSON → first
// {...} block → score scrape. The scrape is also the only path that
// rescues TRUNCATED JSON — a verdict cut off mid-reasoning never closes
// its brace, so jsonObjectRe can't see it (one live truncation carried
// "score": 5, a PASS that was thrown away as a 0).
func parseJudgeOutput(raw string) (JudgeResult, error) {
	cleaned := strings.TrimSpace(raw)
	if cleaned == "" {
		return JudgeResult{}, fmt.Errorf("judge: empty output (thinking models can burn the whole token budget before emitting text)")
	}
	// Try direct parse first.
	var jr JudgeResult
	if err := json.Unmarshal([]byte(cleaned), &jr); err == nil {
		return clampScore(jr), nil
	}
	// Fall back to extracting the first complete JSON object.
	if m := jsonObjectRe.FindString(cleaned); m != "" {
		if err := json.Unmarshal([]byte(m), &jr); err == nil {
			return clampScore(jr), nil
		}
	}
	// Last resort: scrape an integer score (rescues truncated JSON too).
	if n, ok := scrapeScore(cleaned); ok {
		return clampScore(JudgeResult{Score: n, Reasoning: trunc(cleaned, 200)}), nil
	}
	return JudgeResult{}, fmt.Errorf("judge: output unparseable: %q", trunc(raw, 100))
}

func clampScore(jr JudgeResult) JudgeResult {
	if jr.Score < 0 {
		jr.Score = 0
	}
	if jr.Score > 5 {
		jr.Score = 5
	}
	return jr
}

var scoreRe = regexp.MustCompile(`"?score"?\s*[:=]\s*(\d)`)

func scrapeScore(s string) (int, bool) {
	m := scoreRe.FindStringSubmatch(s)
	if len(m) < 2 {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
