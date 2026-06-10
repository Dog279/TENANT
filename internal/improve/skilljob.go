package improve

import (
	"context"
	"fmt"
	"strings"

	"tenant/internal/memory/episodic"
	"tenant/internal/memory/skills"
	"tenant/internal/model"
)

// SkillInductionJob is the T4 learning loop (Hermes/Voyager pattern):
// it scans recent episodes for tool-call sequences that recurred across
// several SUCCESSFUL turns and proposes them as reusable skills. Proposed
// skills are disabled + status=proposed — a human accepts them (via
// /skills accept) before they go live. Heuristic by design; the human
// review gate is what keeps quality up.
type SkillInductionJob struct {
	Episodic *episodic.Store
	Skills   *skills.Store
	Embedder model.Embedder
	AgentID  string
	MinOccur int // min recurrences to propose (default 3)
	Scan     int // how many recent episodes to scan (default 300)

	// AutoAccept, if set, returns the live auto-accept mode for induced skills
	// (TEN-152): "off" (default — queue for /skills accept), "on" (auto-accept
	// every new skill), or "trusted" (auto-accept ONLY while the operator's
	// recent feedback is healthy — see TrustMinAcks). nil/"" ⇒ off. Read each
	// run so a live toggle takes effect on the next induction.
	AutoAccept func() string
	// TrustMinAcks is the min acks (with zero undos) in the recent feedback
	// window required for "trusted" auto-accept. <=0 ⇒ default 5.
	TrustMinAcks int
	// TrustWindow is how many recent fed-back episodes the gate inspects.
	// <=0 ⇒ default 20.
	TrustWindow int
}

// autoAcceptOK resolves the current auto-accept decision for this run. Returns
// (accept, mode) — accept is whether induced skills should go straight to
// live+enabled; mode is the configured string for logging.
func (j *SkillInductionJob) autoAcceptOK(ctx context.Context) (bool, string) {
	mode := "off"
	if j.AutoAccept != nil {
		mode = strings.ToLower(strings.TrimSpace(j.AutoAccept()))
	}
	switch mode {
	case "on":
		return true, mode
	case "trusted":
		minAcks := j.TrustMinAcks
		if minAcks <= 0 {
			minAcks = 5
		}
		win := j.TrustWindow
		if win <= 0 {
			win = 20
		}
		acks, undos, err := j.Episodic.FeedbackStats(ctx, j.AgentID, win)
		// Healthy = enough acks AND no recent undos. A single undo suspends
		// auto-accept back to manual review until trust is re-earned.
		return err == nil && undos == 0 && acks >= minAcks, mode
	default:
		return false, "off"
	}
}

func (j *SkillInductionJob) Name() string { return "skill-induction" }

func (j *SkillInductionJob) Run(ctx context.Context) (JobResult, error) {
	if j.Episodic == nil || j.Skills == nil {
		return JobResult{}, fmt.Errorf("skilljob: Episodic and Skills required")
	}
	min := j.MinOccur
	if min <= 0 {
		min = 3
	}
	scan := j.Scan
	if scan <= 0 {
		scan = 300
	}
	eps, err := j.Episodic.List(ctx, episodic.ListFilter{AgentIDs: []string{j.AgentID}, Limit: scan})
	if err != nil {
		return JobResult{}, fmt.Errorf("skilljob: list episodes: %w", err)
	}

	type group struct {
		count   int
		tools   []string
		example string
	}
	sigs := map[string]*group{}
	for _, e := range eps {
		if e.Outcome == episodic.OutcomeError || len(e.ToolCalls) < 2 {
			continue // need a successful multi-step turn to be a "skill"
		}
		names := make([]string, 0, len(e.ToolCalls))
		for _, tc := range e.ToolCalls {
			names = append(names, tc.Name)
		}
		sig := strings.Join(names, " → ")
		g := sigs[sig]
		if g == nil {
			g = &group{tools: names, example: e.Prompt}
			sigs[sig] = g
		}
		g.count++
	}

	// Names already taken (live or proposed) — don't clobber.
	existing := map[string]bool{}
	if cur, err := j.Skills.List(ctx, skills.ListFilter{AgentID: j.AgentID, IncludeDisabled: true}); err == nil {
		for _, sk := range cur {
			existing[sk.Name] = true
		}
	}

	// Feedback-gated auto-accept (TEN-152): when on/trusted, induced skills go
	// straight to live+enabled instead of waiting for /skills accept.
	autoAccept, mode := j.autoAcceptOK(ctx)
	status, enabled, source := skills.StatusProposed, false, "induction"
	if autoAccept {
		status, enabled, source = skills.StatusLive, true, "auto-accept"
	}

	proposed, accepted := 0, 0
	for sig, g := range sigs {
		if g.count < min {
			continue
		}
		name := "auto: " + sig
		if existing[name] {
			continue
		}
		desc := fmt.Sprintf("Recurring approach (seen %d×) for tasks like: %q", g.count, clipStr(g.example, 80))
		recipe := "Follow these tools in order: " + strings.Join(g.tools, " → ")
		var embed []float32
		if j.Embedder != nil {
			if v, err := j.Embedder.Embed(ctx, []string{name + ": " + desc}); err == nil && len(v) == 1 {
				embed = v[0]
			}
		}
		if _, err := j.Skills.UpsertWithSource(ctx, &skills.Skill{
			AgentID: j.AgentID, Name: name, Description: desc, Recipe: recipe,
			Status: status, Enabled: enabled, Embedding: embed,
		}, source); err == nil {
			if autoAccept {
				accepted++
			} else {
				proposed++
			}
		}
	}
	var summary string
	if autoAccept {
		summary = fmt.Sprintf("scanned %d episode(s), AUTO-ACCEPTED %d new skill(s) (mode=%s) — live now", len(eps), accepted, mode)
	} else {
		summary = fmt.Sprintf("scanned %d episode(s), proposed %d new skill(s) for review", len(eps), proposed)
	}
	return JobResult{
		Summary: summary,
		Changed: proposed+accepted > 0,
		Details: map[string]any{"scanned": len(eps), "proposed": proposed, "auto_accepted": accepted, "auto_accept_mode": mode},
	}, nil
}

func clipStr(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
