package main

// The Judge (TEN-510, TEN-516): a System One decision model in the loop of a
// turn, triaging each prompt and checking a task's final answer. It
// implements agent.Judge. /judge and the dashboard's Judge card set it up and
// switch it live; config.json's judge section keeps the choice, and its key,
// if the server wants one, lives in credentials.json under "judge".

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

// judgeKind is one dialect of System One server the Judge can be: its
// defaults. Every kind speaks the same API; a hosted server differs only in
// its endpoint and key. A new kind is a row here (and the live call that
// verified it, docs/plans/judge-rework-plan.md).
type judgeKind struct {
	label    string
	endpoint string // "" = the operator must give one
	model    string
}

var judgeKinds = map[string]judgeKind{
	"clef": {label: "Clef", model: "clef-flash"},
}

// judgeKindOrder lists the kinds as commands and pages offer them.
var judgeKindOrder = []string{"clef"}

// judgeCredID is the credentials.json entry holding the Judge's key.
const judgeCredID = "judge"

const (
	judgeDefaultKind   = "clef"
	judgeDefaultModel  = "clef-flash"
	judgeDefaultRounds = config.JudgeDefaultRounds
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
	kind   string
	client *systemone.Client // its Key is the saved credential, never shown
	rounds int
	// bad is why the saved judge can't run (an unknown kind in config.json);
	// the loop stays off and Status says so.
	bad string
}

// judgeLoopFor builds the process's judge from config.json's judge section
// and the key saved in credentials.json.
func judgeLoopFor(c *commonFlags) *judgeLoop {
	var jc *config.Judge
	if c.lc != nil {
		jc = c.lc.Judge
	}
	return newJudgeLoop(c.cfgDir, jc)
}

func newJudgeLoop(cfgDir string, c *config.Judge) *judgeLoop {
	j := &judgeLoop{cfgDir: cfgDir}
	j.state.Store(judgeStateFrom(c, judgeSavedKey(cfgDir)))
	return j
}

// judgeSavedKey is the key in credentials.json, "" when none or unreadable.
func judgeSavedKey(cfgDir string) string {
	creds, err := loadCredentials(cfgDir)
	if err != nil {
		return ""
	}
	return creds.Get(judgeCredID)
}

func judgeStateFrom(c *config.Judge, key string) *judgeLoopState {
	if c == nil { // a fresh setup: off, the default kind and model, no server yet
		return &judgeLoopState{kind: judgeDefaultKind, client: &systemone.Client{Model: judgeDefaultModel, Key: key}, rounds: judgeDefaultRounds}
	}
	kind := strings.ToLower(strings.TrimSpace(c.Kind))
	if kind == "" {
		kind = judgeDefaultKind
	}
	st := &judgeLoopState{kind: kind, client: &systemone.Client{Endpoint: c.Endpoint, Model: c.Model, Key: key}, rounds: c.RoundsOrDefault()}
	k, ok := judgeKinds[kind]
	if !ok { // a hand-edited kind this build doesn't know: off, and Status names the valid ones
		st.bad = fmt.Sprintf("unknown kind %q in config.json (known: %s)", c.Kind, strings.Join(judgeKindOrder, ", "))
		return st
	}
	if st.client.Model == "" {
		st.client.Model = k.model
	}
	st.on = c.Enabled && c.Endpoint != ""
	return st
}

// judgeKindOf validates a kind the operator typed or picked.
func judgeKindOf(kind string) (judgeKind, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind == "" {
		kind = judgeDefaultKind
	}
	k, ok := judgeKinds[kind]
	if !ok {
		return judgeKind{}, fmt.Errorf("unknown judge kind %q (known: %s)", kind, strings.Join(judgeKindOrder, ", "))
	}
	return k, nil
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

// Status is the one-line state for /judge and the dashboard.
func (j *judgeLoop) Status() string {
	s := j.state.Load()
	key := "no key"
	if s.client.Key != "" {
		key = "a key is saved"
	}
	if s.bad != "" {
		return "judge: off — " + s.bad + "; /judge use clef sets it up again"
	}
	if s.client.Endpoint == "" {
		return "judge: not set up — /judge use clef to point it at a System One server"
	}
	if !s.on {
		return fmt.Sprintf("judge: off — %s (%s) at %s, %s", s.client.Model, s.kind, s.client.Endpoint, key)
	}
	return fmt.Sprintf("judge: on — %s (%s) at %s, %s; triages each prompt and checks tasks, up to %d rounds", s.client.Model, s.kind, s.client.Endpoint, key, s.rounds)
}

// Saved reports what the setup prompt offers as defaults: the endpoint on
// file and whether a key is saved.
func (j *judgeLoop) Saved() (endpoint string, hasKey bool) {
	s := j.state.Load()
	return s.client.Endpoint, s.client.Key != ""
}

// judgeSetup is one operator choice about the Judge's server, applied by
// apply: the kind's dialect, the address, the model, the key (when setKey;
// blank = none), the switch and the rounds (0 keeps the saved count).
// surface names who made it, for the Security log.
type judgeSetup struct {
	kind, endpoint, model string
	key                   string
	setKey                bool
	on                    bool
	rounds                int
	surface               string
}

// Use points the Judge at a server with the saved key: the kind's dialect,
// the endpoint (given, else the saved one, else the kind's default) and the
// model (given, else the kind's default). It asks the server one triage
// question first, saves, switches the loop on, and returns the status.
func (j *judgeLoop) Use(kind, endpoint, modelID string) (string, error) {
	return j.apply(judgeSetup{kind: kind, endpoint: endpoint, model: modelID, on: true})
}

// Setup implements tui.JudgeControl: Use with a key the operator just typed
// (blank = none). The server is asked with that key before anything is
// saved, so a wrong address or a refused key leaves credentials.json and
// config.json as they were.
func (j *judgeLoop) Setup(kind, endpoint, key, modelID string) (string, error) {
	return j.apply(judgeSetup{kind: kind, endpoint: endpoint, model: modelID, key: key, setKey: true, on: true, surface: surfaceTUI})
}

// apply is Use, Setup and the dashboard card: the server is asked one triage
// question with the key that will be saved (the typed one when setKey, else
// the saved one), and only then are the key, the server, the switch and the
// rounds saved, together. A failed config save puts the key back, so a
// failure of any kind changes nothing.
func (j *judgeLoop) apply(s judgeSetup) (string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	k, err := judgeKindOf(s.kind)
	if err != nil {
		return "", err
	}
	kind := strings.ToLower(strings.TrimSpace(s.kind))
	if kind == "" {
		kind = judgeDefaultKind
	}
	cur := j.state.Load()
	endpoint := strings.TrimSpace(s.endpoint)
	if endpoint == "" {
		endpoint = cur.client.Endpoint
	}
	if endpoint == "" {
		endpoint = k.endpoint
	}
	modelID := strings.TrimSpace(s.model)
	if modelID == "" {
		modelID = k.model
	}
	if u, err := url.Parse(endpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("the judge needs a System One server's address, like http://host:30000")
	}
	rounds := cur.rounds
	if s.rounds != 0 {
		if err := judgeRoundsOK(s.rounds); err != nil {
			return "", err
		}
		rounds = s.rounds
	}
	key := cur.client.Key
	if s.setKey {
		key = strings.TrimSpace(s.key)
	}
	answered, err := judgeProbe(&systemone.Client{Endpoint: endpoint, Model: modelID, Key: key})
	if err != nil {
		return "", err // nothing saved: not the key, not the address, not the rounds
	}
	if key != cur.client.Key {
		if err := j.storeKey(key, s.surface); err != nil {
			return "", err
		}
	}
	next := &config.Judge{Enabled: s.on, Kind: kind, Endpoint: endpoint, Model: modelID, Rounds: rounds}
	if next.Rounds == judgeDefaultRounds {
		next.Rounds = 0
	}
	j.state.Store(judgeStateFrom(next, key))
	if err := persistLaunchConfig(j.cfgDir, nil, func(lc *launchConfig) { lc.Judge = next }); err != nil {
		// Nothing saved means nothing: put the key and the live state back.
		if key != cur.client.Key {
			_ = j.storeKey(cur.client.Key, s.surface)
		}
		j.state.Store(cur)
		return "", fmt.Errorf("judge: nothing changed, config.json couldn't be saved: %w", err)
	}
	return j.Status() + answered, nil
}

// judgeRoundsOK bounds the send-backs per answer (TEN-516: 1 to 5).
func judgeRoundsOK(n int) error {
	if n < 1 || n > config.JudgeMaxRounds {
		return fmt.Errorf("rounds must be between 1 and %d", config.JudgeMaxRounds)
	}
	return nil
}

// setKey stores the Judge's key in credentials.json ("" removes it) and
// applies it live, without asking the server. Setup is the path that asks
// first; this one serves tests and a key change on its own.
func (j *judgeLoop) setKey(key, surface string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.storeKey(key, surface)
}

// storeKey is setKey under j.mu.
func (j *judgeLoop) storeKey(key, surface string) error {
	key = strings.TrimSpace(key)
	creds, err := loadCredentials(j.cfgDir)
	if err != nil {
		return err
	}
	if key == "" {
		delete(creds.Secrets, judgeCredID)
	} else {
		creds.Set(judgeCredID, key)
	}
	if err := saveCredentials(j.cfgDir, creds, surface); err != nil {
		return err
	}
	cur := j.state.Load()
	next := *cur
	client := *cur.client
	client.Key = key
	next.client = &client
	j.state.Store(&next)
	return nil
}

// Rounds sets how many times a turn may be sent back, 1..config.JudgeMaxRounds,
// live and saved.
func (j *judgeLoop) Rounds(n int) (string, error) {
	if err := judgeRoundsOK(n); err != nil {
		return "", err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	cur := j.state.Load()
	next := *cur
	next.rounds = n
	j.state.Store(&next)
	saved := n
	if saved == judgeDefaultRounds {
		saved = 0
	}
	err := persistLaunchConfig(j.cfgDir, nil, func(lc *launchConfig) {
		if lc.Judge == nil {
			lc.Judge = &config.Judge{Kind: cur.kind}
		}
		lc.Judge.Rounds = saved
	})
	if err != nil {
		return "", errNotSaved(j.Status(), err)
	}
	return fmt.Sprintf("judge: up to %d rounds per answer", n), nil
}

// judgeProbe asks the server one triage question, so a wrong address, a
// refused key or a server that isn't a decision model shows before anything
// is saved. It returns how long the answer took, for the status line.
func judgeProbe(c *systemone.Client) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), judgeProbeTimeout)
	defer cancel()
	start := time.Now()
	if _, _, err := c.Decide(ctx, "User: hello", judgeTriageQs); err != nil {
		return "", fmt.Errorf("%s at %s didn't answer: %v", c.Model, c.Endpoint, err)
	}
	return fmt.Sprintf(" (answered in %.1fs)", time.Since(start).Seconds()), nil
}

// judgeSettings is the Judge's state as a card shows it: never the key.
type judgeSettings struct {
	on             bool
	kind, endpoint string
	model          string
	rounds         int
	hasKey         bool
}

// Settings reports the Judge's state for the dashboard's card.
func (j *judgeLoop) Settings() judgeSettings {
	s := j.state.Load()
	return judgeSettings{on: s.on, kind: s.kind, endpoint: s.client.Endpoint, model: s.client.Model, rounds: s.rounds, hasKey: s.client.Key != ""}
}

// On reports whether the loop is on, and how /model names the judge.
func (j *judgeLoop) On() (bool, string) {
	s := j.state.Load()
	return s.on, s.client.Model + " (" + s.kind + ")"
}

// Set turns the loop on or off live and saves it. Turning it on asks the
// saved server one triage question first with the saved key; a server that
// doesn't answer, or no server yet, is refused.
func (j *judgeLoop) Set(on bool) (string, error) {
	if on {
		if ep, _ := j.Saved(); ep == "" {
			return "", fmt.Errorf("the judge has no server yet: /judge use clef sets one up")
		}
	}
	return j.store(on, "", "")
}

// store turns the loop on or off live and saves it, with an endpoint and
// model that default to the saved ones; switching on asks the server first.
// Set and the dashboard's "off" go through it; Use is the setup path.
func (j *judgeLoop) store(on bool, endpoint, modelID string) (string, error) {
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
			return "", fmt.Errorf("the judge needs a System One server's address, like http://host:30000")
		}
		a, err := judgeProbe(&systemone.Client{Endpoint: endpoint, Model: modelID, Key: cur.client.Key})
		if err != nil {
			return "", fmt.Errorf("%v (judge left %s)", err, onOff(cur.on))
		}
		answered = a
	}
	next := &config.Judge{Enabled: on, Kind: cur.kind, Endpoint: endpoint, Model: modelID, Rounds: cur.rounds}
	if next.Rounds == judgeDefaultRounds {
		next.Rounds = 0
	}
	j.state.Store(judgeStateFrom(next, cur.client.Key))
	err := persistLaunchConfig(j.cfgDir, nil, func(lc *launchConfig) { lc.Judge = next })
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
