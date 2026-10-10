package main

// The judge in the loop of a turn (TEN-510): a System One decision model
// (Cloudflare's Clef-flash) triages each prompt and checks a task's final
// answer. It implements agent.Judge; /judge loop and the dashboard switch it
// live, and config.json's judge_loop keeps the choice.

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tenant/internal/agent"
	"tenant/internal/config"
	"tenant/internal/model/systemone"
)

const (
	judgeDefaultModel  = "clef-flash"
	judgeDefaultRounds = 3
	judgeMaxRounds     = 10               // each round is a free planner iteration: keep a hand-edited config bounded
	judgeProbeTimeout  = 20 * time.Second // a cold server took ~15 s for its first answer
	judgeKeepOutputs   = 20               // tool calls whose output the judge sees (the rest: name and input)
	judgeMaxCalls      = 100              // tool calls listed at all
	judgeInputChars    = 300              // per tool input
	judgeOutputChars   = 600              // per tool output
	judgeTextChars     = 4000             // the request and the answer
)

// The questions, worded and tested against Clef-flash (docs/plans/turn-judge-plan.md).
var (
	judgeTriageQs = map[string]systemone.Question{
		"kind": {Type: systemone.Choice, Instructions: "What does the user want from an AI agent that can run tools on their computer?",
			Criteria: map[string]string{
				"chat":     "Small talk, thanks, or a greeting; nothing to do",
				"question": "A question answered from knowledge, with nothing to change or look up",
				"task":     "Work to do: look something up, run, change, create, fix or check something",
			}},
	}
	judgeCheckQs = map[string]systemone.Question{
		"done":                {Type: systemone.Noul, Instructions: "Did the assistant fully complete what the user asked?"},
		"every_part":          {Type: systemone.Noul, Instructions: "Did the assistant address every part of the user's request?"},
		"claims_without_tool": {Type: systemone.Noul, Instructions: "Does final_answer say it performed an action (like deleting, writing, sending or changing something) for which there is no matching tool call in tool_calls?"},
		"gave_up":             {Type: systemone.Noul, Instructions: "Did the assistant stop and ask the user, or hand the work back, when it could have done it itself?"},
	}
)

// judgeCheck is one check: the question, whether "true" passes, and what the
// planner is told when it fails.
type judgeCheck struct {
	id    string
	pass  bool // the answer that passes
	nudge string
}

var judgeChecks = []judgeCheck{
	{"done", true, "A check found the request isn't fully done. Re-read what the user asked, finish what's missing, then answer."},
	{"every_part", true, "A check found the request isn't fully done. Re-read what the user asked, finish what's missing, then answer."},
	{"claims_without_tool", false, "Your answer says things were done that no tool call shows. Do them with tools now, or say plainly they weren't done."},
	{"gave_up", false, "You handed the work back to the user. Do it yourself unless you truly need their input; if you do, say exactly what you need."},
}

// judgeLoop is the live judge. Its settings swap whole, so a turn never
// reads a new endpoint with an old on/off.
type judgeLoop struct {
	cfgDir string
	mu     sync.Mutex // serializes Set
	state  atomic.Pointer[judgeLoopState]
}

type judgeLoopState struct {
	on     bool
	client *systemone.Client
	rounds int
}

// judgeLoopFor builds the process's judge from config.json's judge_loop.
func judgeLoopFor(c *commonFlags) *judgeLoop {
	var jc *config.JudgeLoop
	if c.lc != nil {
		jc = c.lc.JudgeLoop
	}
	return newJudgeLoop(c.cfgDir, jc)
}

func newJudgeLoop(cfgDir string, c *config.JudgeLoop) *judgeLoop {
	j := &judgeLoop{cfgDir: cfgDir}
	j.state.Store(judgeStateFrom(c))
	return j
}

func judgeStateFrom(c *config.JudgeLoop) *judgeLoopState {
	if c == nil { // a fresh setup: off, the default model, no server yet
		return &judgeLoopState{client: &systemone.Client{Model: judgeDefaultModel}, rounds: judgeDefaultRounds}
	}
	m := c.Model
	if m == "" {
		m = judgeDefaultModel
	}
	r := c.MaxRounds
	if r <= 0 {
		r = judgeDefaultRounds
	}
	r = min(r, judgeMaxRounds)
	return &judgeLoopState{on: c.Enabled && c.Endpoint != "", client: &systemone.Client{Endpoint: c.Endpoint, Model: m}, rounds: r}
}

func (j *judgeLoop) live() (*judgeLoopState, error) {
	s := j.state.Load()
	if !s.on {
		return nil, agent.ErrJudgeOff
	}
	return s, nil
}

// Triage implements agent.Judge.
func (j *judgeLoop) Triage(ctx context.Context, prompt string) (bool, agent.JudgeUsage, error) {
	s, err := j.live()
	if err != nil {
		return false, agent.JudgeUsage{}, err
	}
	a, use, err := s.client.Decide(ctx, "User: "+cut(prompt, judgeTextChars), judgeTriageQs)
	if err != nil {
		return false, agent.JudgeUsage{}, err
	}
	if t := a["kind"].Type; t != systemone.Choice {
		return false, agent.JudgeUsage{}, fmt.Errorf("the judge answered kind as %q, not a choice: is it a System One decision model?", t)
	}
	return a["kind"].Choice == "task", agent.JudgeUsage{Model: s.client.Model, InputTokens: use.InputTokens}, nil
}

// Check implements agent.Judge.
func (j *judgeLoop) Check(ctx context.Context, ex agent.Exchange) (agent.Verdict, error) {
	s, err := j.live()
	if err != nil {
		return agent.Verdict{}, err
	}
	a, use, err := s.client.Decide(ctx, judgeState(ex), judgeCheckQs)
	if err != nil {
		return agent.Verdict{}, err
	}
	for _, c := range judgeChecks { // a wrong-typed answer would read as P(true)=0: fail open instead
		if t := a[c.id].Type; t != systemone.Noul {
			return agent.Verdict{}, fmt.Errorf("the judge answered %s as %q, not yes/no: is it a System One decision model?", c.id, t)
		}
	}
	v := judgeVerdict(a)
	v.Use = agent.JudgeUsage{Model: s.client.Model, InputTokens: use.InputTokens}
	return v, nil
}

// MaxRounds implements agent.Judge.
func (j *judgeLoop) MaxRounds() int { return j.state.Load().rounds }

// judgeVerdict rules on the answers: every check must pass. The summary
// names the failing checks with their P(true), or the done score.
func judgeVerdict(a map[string]systemone.Answer) agent.Verdict {
	var failed, nudges []string
	for _, c := range judgeChecks {
		p := a[c.id].Noul
		if (p >= 0.5) != c.pass {
			failed = append(failed, fmt.Sprintf("%s %.2f", c.id, p))
			if !containsStr(nudges, c.nudge) {
				nudges = append(nudges, c.nudge)
			}
		}
	}
	if len(failed) == 0 {
		return agent.Verdict{Done: true, Summary: fmt.Sprintf("done %.2f", a["done"].Noul)}
	}
	return agent.Verdict{Summary: strings.Join(failed, ", "), Nudge: strings.Join(nudges, " ")}
}

// judgeState is what the judge reads, structured: the claim check separates
// cleanly only when the tool calls are a list beside the answer. Every call
// is listed by name and input (an early action the answer mentions stays
// visible); outputs only for the last judgeKeepOutputs.
//
// It holds what the planner model already saw (tool output included), sent to
// the judge server the operator configured, so it is a model call like the
// planner's, not egress the sensitive-data gate reviews.
func judgeState(ex agent.Exchange) map[string]any {
	type call struct {
		Tool   string `json:"tool"`
		Input  string `json:"input"`
		Output string `json:"output,omitempty"`
		Failed bool   `json:"failed,omitempty"`
	}
	tools := ex.Tools
	skipped := 0
	if len(tools) > judgeMaxCalls {
		skipped = len(tools) - judgeMaxCalls
		tools = tools[skipped:]
	}
	calls := make([]call, 0, len(tools))
	for i, t := range tools {
		c := call{Tool: t.Call.Name, Input: cut(string(t.Call.Arguments), judgeInputChars), Failed: t.IsError}
		if i >= len(tools)-judgeKeepOutputs {
			c.Output = cut(t.Result, judgeOutputChars)
		}
		calls = append(calls, c)
	}
	st := map[string]any{
		"user_request": cut(ex.Request, judgeTextChars),
		"tool_calls":   calls,
		"final_answer": cut(ex.Answer, judgeTextChars),
	}
	if skipped > 0 {
		st["earlier_tool_calls_not_listed"] = skipped
	}
	return st
}

// Status is the one-line state for /judge loop and the dashboard.
func (j *judgeLoop) Status() string {
	s := j.state.Load()
	if !s.on {
		if s.client.Endpoint != "" {
			return fmt.Sprintf("judge loop: off (last: %s at %s)", s.client.Model, s.client.Endpoint)
		}
		return "judge loop: off"
	}
	return fmt.Sprintf("judge loop: on — %s at %s triages each prompt and checks tasks (up to %d rounds)", s.client.Model, s.client.Endpoint, s.rounds)
}

// Settings reports on/off, the endpoint and the model.
func (j *judgeLoop) Settings() (bool, string, string) {
	s := j.state.Load()
	return s.on, s.client.Endpoint, s.client.Model
}

// On reports whether the loop is on, and its model.
func (j *judgeLoop) On() (bool, string) {
	s := j.state.Load()
	return s.on, s.client.Model
}

// Set turns the loop on or off live and saves it. Turning it on asks the
// server one triage question first and reports how it answered; a server
// that doesn't answer is refused. endpoint and modelID default to the saved
// ones.
func (j *judgeLoop) Set(on bool, endpoint, modelID string) (string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	cur := j.state.Load()
	if endpoint == "" {
		endpoint = cur.client.Endpoint
	}
	if modelID == "" {
		modelID = cur.client.Model
	}
	answered := ""
	if on {
		if u, err := url.Parse(endpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return "", fmt.Errorf("the judge needs a System One server: /judge loop on http://host:port [model]")
		}
		ctx, cancel := context.WithTimeout(context.Background(), judgeProbeTimeout)
		defer cancel()
		start := time.Now()
		c := &systemone.Client{Endpoint: endpoint, Model: modelID}
		if _, _, err := c.Decide(ctx, "User: hello", judgeTriageQs); err != nil {
			return "", fmt.Errorf("%s at %s didn't answer: %v (judge loop left %s)", modelID, endpoint, err, onOff(cur.on))
		}
		answered = fmt.Sprintf(" (answered in %.1fs)", time.Since(start).Seconds())
	}
	next := &config.JudgeLoop{Enabled: on, Endpoint: endpoint, Model: modelID, MaxRounds: cur.rounds}
	if next.MaxRounds == judgeDefaultRounds {
		next.MaxRounds = 0
	}
	j.state.Store(judgeStateFrom(next))
	err := persistLaunchConfig(j.cfgDir, nil, func(lc *launchConfig) { lc.JudgeLoop = next })
	if err != nil {
		return "", errNotSaved(j.Status(), err)
	}
	return j.Status() + answered, nil
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// cut keeps the first n bytes of s on a rune boundary, marking the cut.
// (clip flattens newlines for one-line display; the judge reads the text.)
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
