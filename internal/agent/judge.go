package agent

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Judge is the judge in the loop of a turn (TEN-510): a model in front of
// the planner that triages each prompt and checks a task's final answer
// before it goes out. The agent knows nothing of how it decides; the
// composition root supplies one (a System One decision model).
type Judge interface {
	// Triage reports whether prompt asks for work to be done. It runs while
	// the planner works, so it adds no wait. ErrJudgeOff: the loop is off.
	Triage(ctx context.Context, prompt string) (task bool, use JudgeUsage, err error)
	// Check judges ex. ErrJudgeOff: the loop is off.
	Check(ctx context.Context, ex Exchange) (Verdict, error)
	// MaxRounds is how many times a turn may be sent back.
	MaxRounds() int
}

// ErrJudgeOff is what a Judge returns while the loop is switched off, so a
// live toggle applies to the next turn without rebuilding the agent.
var ErrJudgeOff = errors.New("judge loop is off")

// Exchange is what the judge sees: the ask, the work and the answer.
type Exchange struct {
	Request string // this turn's message, plus anything interjected
	Tools   []ToolCallResult
	Answer  string
}

// Verdict is the judge's ruling on an Exchange.
type Verdict struct {
	Done    bool
	Summary string // what decided it, e.g. "every_part 0.09"
	Nudge   string // what to tell the planner when it isn't done
	Use     JudgeUsage
}

// JudgeUsage is what one judge call cost, counted like a model call's.
type JudgeUsage struct {
	Model       string
	InputTokens int
}

// judgeCooldown is how long a judge outage skips judging before trying
// again, so an unreachable judge doesn't stall every turn on its timeout.
const judgeCooldown = time.Minute

// judgeTurn is one turn's judging: the triage in flight, the rounds used,
// and the ask as it grew.
type judgeTurn struct {
	triage  chan judgeTriage
	task    *bool
	rounds  int
	request strings.Builder
}

type judgeTriage struct {
	task bool
	use  JudgeUsage
	err  error
}

// startJudge begins this turn's triage, or returns nil when the turn isn't
// judged: no judge, a sub-agent's turn, or the caller asked not to (/goal
// judges its own runs).
func (a *Agent) startJudge(ctx context.Context, req TurnRequest) *judgeTurn {
	if a.cfg.Judge == nil || req.NoJudge || req.ParentTraceID != "" || strings.TrimSpace(req.UserQuery) == "" {
		return nil
	}
	// Another agent talking (a peer, research's workers) isn't someone's ask.
	if req.RouteSurface == RouteSurfacePeer || req.RouteSurface == RouteSurfaceResearch {
		return nil
	}
	if time.Now().UnixNano() < a.judgeRetryAt.Load() {
		return nil // in an outage's cooldown
	}
	jt := &judgeTurn{triage: make(chan judgeTriage, 1)}
	jt.request.WriteString(req.UserQuery)
	go func() {
		task, use, err := a.cfg.Judge.Triage(ctx, req.UserQuery)
		jt.triage <- judgeTriage{task, use, err}
	}()
	return jt
}

// judgeIsTask waits for the triage, which its own client timeout bounds (it
// started with the turn, so it is normally done). A triage that failed
// counts as not a task: the judge fails open.
func (a *Agent) judgeIsTask(ctx context.Context, jt *judgeTurn, iter int, span spanUsage) bool {
	if jt.task != nil {
		return *jt.task
	}
	no := false
	jt.task = &no
	select {
	case t := <-jt.triage:
		a.judgeUsage(iter, span, t.use)
		if t.err != nil {
			a.judgeFailed(ctx, t.err)
			return false
		}
		jt.task = &t.task
		if t.task {
			a.emit(Event{Kind: EventJudge, Iter: iter, Text: "task: checking the answer"})
		}
		return t.task
	case <-ctx.Done():
	}
	return false
}

// judgeFinal judges a final answer. It returns the nudge to send the work
// back with (room: the loop has an iteration left for it), or the note to
// add to an answer it couldn't confirm; both empty when the answer stands.
func (a *Agent) judgeFinal(ctx context.Context, jt *judgeTurn, iter int, room bool, tools []ToolCallResult, answer string, span spanUsage) (nudge, note string) {
	if !a.judgeIsTask(ctx, jt, iter, span) {
		return "", ""
	}
	v, err := a.cfg.Judge.Check(ctx, Exchange{Request: jt.request.String(), Tools: tools, Answer: answer})
	if err != nil {
		a.judgeFailed(ctx, err)
		return "", ""
	}
	a.judgeRetryAt.Store(0) // a check answered: any outage is over
	a.judgeUsage(iter, span, v.Use)
	max := a.cfg.Judge.MaxRounds()
	// A safety boundary or the operator said no this turn: sending the work
	// back would only ask them again, so an unconfirmed answer goes out as is.
	if hitBoundary(tools) {
		room = false
	}
	switch {
	case v.Done:
		Judged.Log(a.ev(ctx), "agent", a.cfg.AgentID, "verdict", "done", "round", jt.rounds, "scores", v.Summary)
		a.emit(Event{Kind: EventJudge, Iter: iter, Text: "done ✓ " + v.Summary})
		return "", ""
	case jt.rounds < max && room:
		jt.rounds++
		Judged.Log(a.ev(ctx), "agent", a.cfg.AgentID, "verdict", "sent back", "round", jt.rounds, "scores", v.Summary)
		a.emit(Event{Kind: EventJudge, Iter: iter, Text: "sent back (" + strconv.Itoa(jt.rounds) + "/" + strconv.Itoa(max) + "): " + v.Summary})
		return v.Nudge, ""
	default:
		JudgeNotConfirmed.Log(a.ev(ctx), "agent", a.cfg.AgentID, "rounds", jt.rounds, "scores", v.Summary)
		a.emit(Event{Kind: EventJudge, Iter: iter, Text: "not confirmed: " + v.Summary})
		return "", "\n\n— judge: not confirmed (" + v.Summary + ")"
	}
}

// hitBoundary reports a tool call this turn that a safety boundary refused
// or the operator denied: the tools report those as "blocked: …".
func hitBoundary(tools []ToolCallResult) bool {
	for _, t := range tools {
		if strings.HasPrefix(t.Result, "blocked:") {
			return true
		}
	}
	return false
}

// judgeUsage counts a judge call like a model call: the run record and the
// usage event (token totals, a team's governor).
func (a *Agent) judgeUsage(iter int, span spanUsage, use JudgeUsage) {
	if use.InputTokens <= 0 {
		return
	}
	span.AddUsage(use.InputTokens, 0)
	a.emit(Event{Kind: EventUsage, Iter: iter, PromptTokens: use.InputTokens})
}

// judgeFailed fails open: the answer goes out unjudged, judging pauses for
// judgeCooldown, and the outage is logged once until a check succeeds again
// (a triage succeeding alone doesn't end it, or a check that keeps failing
// would log every time).
func (a *Agent) judgeFailed(ctx context.Context, err error) {
	if errors.Is(err, ErrJudgeOff) || ctx.Err() != nil {
		return
	}
	if a.judgeRetryAt.Swap(time.Now().Add(judgeCooldown).UnixNano()) == 0 {
		JudgeUnavailable.Log(a.ev(ctx), "agent", a.cfg.AgentID, "err", err)
		a.emit(Event{Kind: EventJudge, Text: "unavailable, answers go out unchecked: " + err.Error(), IsErr: true})
	}
}

// judgeForced checks an answer the loop forced (ceiling, repeat guard, an
// empty final): those are often the unfinished ones, so a failing check adds
// the note, though there's no round left to send it back in.
func (a *Agent) judgeForced(ctx context.Context, jt *judgeTurn, iter int, tools []ToolCallResult, answer string, span spanUsage) string {
	if jt == nil {
		return ""
	}
	_, note := a.judgeFinal(ctx, jt, iter, false, tools, answer, span)
	return note
}

// spanUsage is the part of a run span the judge needs.
type spanUsage interface{ AddUsage(prompt, completion int) }
