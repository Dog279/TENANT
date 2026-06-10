package improve

// souljob.go is SoulNudgeJob (TEN-16): the first eval-GATED self-improvement
// job. It reviews recent rewarded/penalized turns, asks the model to propose a
// refined set of operating instructions for the agent's soul, A/B-scores the
// candidate against the fitness suite, and — only if it does NOT regress —
// queues it via soul.ProposeEdit for HUMAN review. Soul edits hit every turn, so
// (per the soul package's deliberate rule) there is NO auto-apply: two gates in
// series — eval (automated) then the operator's Accept/Reject. Deny-by-default:
// no scorer (model offline) ⇒ nothing is proposed.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"tenant/internal/memory/episodic"
	"tenant/internal/memory/soul"
	"tenant/internal/model"
)

// SoulSignal is the evidence handed to a proposer: recent episodes plus the
// ack/undo tallies over the inspected window.
type SoulSignal struct {
	Episodes []*episodic.Episode // recent, chronological (oldest-first)
	Acks     int
	Undos    int
}

// SoulProposer turns the signal into a candidate set of operating instructions.
// changed=false means "no worthwhile/safe change". Abstracted so the job is
// unit-testable offline without a real model.
type SoulProposer interface {
	Propose(ctx context.Context, current *soul.Soul, sig SoulSignal) (changed bool, reason string, instructions []string, err error)
}

// SoulScorer A/B-tests a candidate soul against the committed fitness baseline
// (captured with the current soul). regressed=true ⇒ discard. An error means
// "couldn't score" — the job FAILS CLOSED (proposes nothing). delta is
// candidate-minus-baseline overall.
type SoulScorer interface {
	Score(ctx context.Context, candidate *soul.Soul) (regressed bool, delta float64, err error)
}

// SoulNudgeJob implements improve.Job.
type SoulNudgeJob struct {
	Episodic *episodic.Store
	AgentID  string
	BaseDir  string // soul lives at <BaseDir>/soul; proposals at <BaseDir>/soul/proposed
	Proposer SoulProposer
	Scorer   SoulScorer // nil ⇒ can't gate ⇒ never proposes (deny-by-default)
	MinAcks  int        // evidence bar: need ≥ this many acks in the window (0 ⇒ 3)
	Scan     int        // recent episodes to inspect (0 ⇒ 100)
	Logger   *slog.Logger
}

func (j *SoulNudgeJob) Name() string { return "soul-nudge" }

func (j *SoulNudgeJob) Run(ctx context.Context) (JobResult, error) {
	log := j.Logger
	if log == nil {
		log = slog.Default()
	}
	if j.Episodic == nil || j.Proposer == nil {
		return JobResult{}, fmt.Errorf("soul-nudge: Episodic and Proposer required")
	}
	if j.AgentID == "" || j.BaseDir == "" {
		return JobResult{}, fmt.Errorf("soul-nudge: AgentID and BaseDir required")
	}
	minAcks := j.MinAcks
	if minAcks <= 0 {
		minAcks = 3
	}
	scan := j.Scan
	if scan <= 0 {
		scan = 100
	}

	// Evidence bar: don't burn a fitness run on a weak hunch. Need a base of
	// rewarded turns before considering a soul change.
	acks, undos, err := j.Episodic.FeedbackStats(ctx, j.AgentID, scan)
	if err != nil {
		return JobResult{}, fmt.Errorf("soul-nudge: feedback stats: %w", err)
	}
	if acks < minAcks {
		return JobResult{Summary: fmt.Sprintf("soul-nudge: insufficient signal (%d/%d acks) — skip", acks, minAcks)}, nil
	}

	cur, err := soul.Load(j.BaseDir, j.AgentID)
	if err != nil {
		// No soul yet → nothing to refine. Not a scheduler error.
		return JobResult{Summary: "soul-nudge: no soul to refine yet"}, nil
	}

	eps, err := j.Episodic.List(ctx, episodic.ListFilter{AgentIDs: []string{j.AgentID}, Limit: scan})
	if err != nil {
		return JobResult{}, fmt.Errorf("soul-nudge: list episodes: %w", err)
	}

	changed, reason, instrs, err := j.Proposer.Propose(ctx, cur, SoulSignal{Episodes: eps, Acks: acks, Undos: undos})
	if err != nil {
		return JobResult{Summary: "soul-nudge: propose failed: " + err.Error()}, err
	}
	if !changed || len(instrs) == 0 {
		return JobResult{Summary: "soul-nudge: no worthwhile change proposed"}, nil
	}

	// Candidate = current soul with refined operating instructions (v1 changes
	// only the Instructions block; the rest is carried over verbatim).
	cand := *cur
	cand.Instructions = soul.Instructions{Items: append([]string(nil), instrs...)}

	// Eval gate (fail closed): no scorer or a scoring error ⇒ propose nothing.
	if j.Scorer == nil {
		return JobResult{Summary: "soul-nudge: no fitness scorer (model offline) — not proposing"}, nil
	}
	regressed, delta, serr := j.Scorer.Score(ctx, &cand)
	if serr != nil {
		log.Warn("soul-nudge: scoring failed; not proposing", "agent", j.AgentID, "err", serr)
		return JobResult{Summary: "soul-nudge: scoring unavailable — not proposing (" + serr.Error() + ")"}, nil
	}
	if regressed {
		return JobResult{
			Summary: fmt.Sprintf("soul-nudge: candidate regressed (Δ %+.1f) — discarded", delta),
			Details: map[string]any{"regressed": true, "delta": delta},
		}, nil
	}

	// Passed the eval gate → queue for HUMAN review. Never auto-applied.
	id, perr := soul.ProposeEdit(j.BaseDir, j.AgentID, reason, &cand)
	if perr != nil {
		return JobResult{Summary: "soul-nudge: propose-edit failed: " + perr.Error()}, perr
	}
	return JobResult{
		Summary: fmt.Sprintf("soul-nudge: queued soul proposal %q (Δ %+.1f) for review — accept via /memory soul", id, delta),
		Changed: true,
		Details: map[string]any{"proposal_id": id, "delta": delta, "reason": reason, "instructions": len(instrs)},
	}, nil
}

// --- production proposer (LLM-backed) ---

const soulNudgeSystem = `You refine an AI agent's OPERATING INSTRUCTIONS — the short rules it follows on every turn. You are given the current instructions and a sample of recent turns labelled with the operator's feedback (✓ = they were happy, ✗ = they were not). Propose a REFINED instruction list that better matches what earns ✓ and avoids what earns ✗.

Rules:
- Only propose a change if there is CLEAR, SAFE evidence for it. If the current instructions are already fine, return {"change": false}.
- Keep the list tight (≤ 8 items), each a single imperative rule. Preserve good existing rules; add/refine sparingly.
- Never add rules that weaken safety, bypass approvals, or contradict the agent's role.

Respond with JSON only:
{"change": true, "reason": "<one line: what you changed and why>", "instructions": ["rule 1", "rule 2", ...]}
{"change": false}`

const soulNudgeSchema = `{"type":"object","properties":{"change":{"type":"boolean"},"reason":{"type":"string"},"instructions":{"type":"array","items":{"type":"string"}}},"required":["change"]}`

// NewLLMSoulProposer builds the production proposer (one structured model call
// per run, routed through RoleSummarizer).
func NewLLMSoulProposer(r *model.Router) SoulProposer {
	return llmSoulProposer{Router: r}
}

// llmSoulProposer is the production proposer: one structured model call.
type llmSoulProposer struct {
	Router *model.Router
	Role   model.Role // "" ⇒ RoleSummarizer
	// MaxSamples caps how many recent fed-back turns go into the prompt (0 ⇒ 16).
	MaxSamples int
}

func (p llmSoulProposer) Propose(ctx context.Context, current *soul.Soul, sig SoulSignal) (bool, string, []string, error) {
	if p.Router == nil {
		return false, "", nil, fmt.Errorf("soul-nudge: nil Router")
	}
	role := p.Role
	if role == "" {
		role = model.RoleSummarizer
	}
	llm, _, err := p.Router.LLMForRole(ctx, role)
	if err != nil {
		return false, "", nil, fmt.Errorf("soul-nudge: resolve model: %w", err)
	}

	maxN := p.MaxSamples
	if maxN <= 0 {
		maxN = 16
	}
	var b strings.Builder
	b.WriteString("Current operating instructions:\n")
	if len(current.Instructions.Items) == 0 {
		b.WriteString("  (none)\n")
	}
	for i, it := range current.Instructions.Items {
		fmt.Fprintf(&b, "  %d. %s\n", i+1, it)
	}
	b.WriteString("\nRecent turns with feedback (most recent last):\n")
	shown := 0
	for _, e := range sig.Episodes { // chronological; show the fed-back ones
		if e.UserFeedback == "" {
			continue
		}
		mark := "✓"
		if e.UserFeedback == episodic.FeedbackUndo {
			mark = "✗"
		}
		fmt.Fprintf(&b, "  %s %s\n", mark, clip(strings.Join(strings.Fields(e.Prompt), " "), 100))
		shown++
		if shown >= maxN {
			break
		}
	}
	if shown == 0 {
		b.WriteString("  (no labelled turns)\n")
	}

	resp, err := llm.Generate(ctx, model.GenerateRequest{
		Messages: []model.Message{
			{Role: "system", Content: soulNudgeSystem},
			{Role: "user", Content: b.String()},
		},
		JSONSchema:  []byte(soulNudgeSchema),
		Temperature: 0.2,
	})
	if err != nil {
		return false, "", nil, fmt.Errorf("soul-nudge: model: %w", err)
	}
	if strings.TrimSpace(resp.Text) == "" {
		return false, "", nil, fmt.Errorf("soul-nudge: empty model response")
	}
	var out struct {
		Change       bool     `json:"change"`
		Reason       string   `json:"reason"`
		Instructions []string `json:"instructions"`
	}
	if jerr := json.Unmarshal([]byte(firstJSONObject(resp.Text)), &out); jerr != nil {
		return false, "", nil, fmt.Errorf("soul-nudge: parse: %w (%q)", jerr, clip(resp.Text, 160))
	}
	if !out.Change {
		return false, "", nil, nil
	}
	// Sanitize: trim, drop empties.
	clean := make([]string, 0, len(out.Instructions))
	for _, it := range out.Instructions {
		if s := strings.TrimSpace(it); s != "" {
			clean = append(clean, s)
		}
	}
	if len(clean) == 0 {
		return false, "", nil, nil
	}
	reason := strings.TrimSpace(out.Reason)
	if reason == "" {
		reason = "refined operating instructions from recent feedback"
	}
	return true, reason, clean, nil
}
