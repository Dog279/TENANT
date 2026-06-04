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

	proposed := 0
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
		if _, err := j.Skills.Upsert(ctx, &skills.Skill{
			AgentID: j.AgentID, Name: name, Description: desc, Recipe: recipe,
			Status: skills.StatusProposed, Enabled: false, Embedding: embed,
		}); err == nil {
			proposed++
		}
	}
	return JobResult{
		Summary: fmt.Sprintf("scanned %d episode(s), proposed %d new skill(s) for review", len(eps), proposed),
		Changed: proposed > 0,
		Details: map[string]any{"scanned": len(eps), "proposed": proposed},
	}, nil
}

func clipStr(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
