package improve

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/memory/sme"
	"tenant/internal/model"
)

// ReflectionJob synthesizes the per-project SME document (Phase 3 of
// docs/memory-sme-plan.md) — the durable nuance carrier. It is the
// Generative-Agents "reflection" + Letta sleep-time-compute pattern: a
// BACKGROUND writer (never the live turn loop) that reads a project's
// protected / high-importance / recent facts (+ a window of recent episodes)
// and rewrites a sectioned markdown understanding, PRESERVING every specific
// (paths, versions, ticket IDs, decisions + rationale, gotchas), citing the
// source facts for provenance and reaffirming them (so being load-bearing
// enough to enter the SME resets their decay clock — the longevity backstop).
//
// Discipline (mirrors SoulNudgeJob): OFF by default (registered only when a
// cadence is configured), suppressed while degraded via the scheduler's Paused
// gate, and run on the pinned proposer model (never the daily turn model). It is
// consolidation-by-ADDITION: it NEVER supersedes or deletes the source facts.
// Fails closed — a missing/garbled proposer response writes nothing.
type ReflectionJob struct {
	Semantic *semantic.Store
	Episodic *episodic.Store // optional: recent-episode context
	SME      *sme.Store
	Live     *sme.Live // optional: refreshed with the new render after a run
	Router   *model.Router
	// SummarizerRouter pins synthesis to the proposer model (TEN-195); nil ⇒ Router.
	SummarizerRouter *model.Router
	AgentID          string
	ProjectID        string // "" = global (single-project; multi-project deferred)
	SummarizerRole   model.Role
	// MaxSectionTokens caps each section at synthesis time so the always-present
	// doc can't balloon the system reserve (design §8). 0 ⇒ default.
	MaxSectionTokens int
	Logger           *slog.Logger
}

const (
	reflectMaxFacts         = 60  // facts handed to one synthesis prompt
	reflectImportanceFloor  = 0.7 // pinned/protected OR importance>=this ⇒ "primary"
	reflectRecentEpisodes   = 10  // recent-episode context lines
	defaultMaxSectionTokens = 400 // per-section cap
	approxCharsPerToken     = 4   // crude token estimate (avoids a tokenizer dep)
	reflectMinFactsToSynth  = 2   // below this, nothing worth synthesizing
)

// Name implements Job.
func (j *ReflectionJob) Name() string { return "reflect" }

func (j *ReflectionJob) summarizerRouter() *model.Router {
	if j.SummarizerRouter != nil {
		return j.SummarizerRouter
	}
	return j.Router
}

func (j *ReflectionJob) summarizerRole() model.Role {
	if j.SummarizerRole != "" {
		return j.SummarizerRole
	}
	return model.RoleSummarizer
}

func (j *ReflectionJob) logger() *slog.Logger {
	if j.Logger != nil {
		return j.Logger
	}
	return slog.Default()
}

// Run synthesizes the SME doc for the configured project from its load-bearing
// facts and recent episodes, writing a new version of each section.
func (j *ReflectionJob) Run(ctx context.Context) (JobResult, error) {
	if j.Semantic == nil || j.SME == nil {
		return JobResult{}, fmt.Errorf("reflect: nil Semantic or SME store")
	}
	if j.Router == nil {
		return JobResult{}, fmt.Errorf("reflect: nil Router")
	}
	if j.AgentID == "" {
		return JobResult{}, fmt.Errorf("reflect: empty AgentID")
	}
	log := j.logger()

	facts, err := j.selectFacts(ctx)
	if err != nil {
		return JobResult{}, fmt.Errorf("reflect: select facts: %w", err)
	}
	if len(facts) < reflectMinFactsToSynth {
		return JobResult{Summary: fmt.Sprintf("reflect: only %d load-bearing fact(s), nothing to synthesize", len(facts))}, nil
	}

	summarizer, sumProf, err := j.summarizerRouter().LLMForRole(ctx, j.summarizerRole())
	if err != nil {
		return JobResult{}, fmt.Errorf("reflect: resolve summarizer: %w", err)
	}
	if j.SummarizerRouter != nil && j.SummarizerRouter != j.Router {
		log.Info("reflect: synthesizing on pinned proposer model", "model", sumProf.Model)
	}

	episodes := j.recentEpisodes(ctx)
	sections, err := j.synthesize(ctx, summarizer, facts, episodes)
	if err != nil {
		// Fail closed: a bad proposer response writes nothing.
		log.Warn("reflect: synthesis failed; wrote nothing", "agent", j.AgentID, "err", err)
		return JobResult{Summary: "reflect: synthesis produced no usable sections"}, nil
	}
	if len(sections) == 0 {
		return JobResult{Summary: "reflect: no sections returned"}, nil
	}

	capTok := j.MaxSectionTokens
	if capTok <= 0 {
		capTok = defaultMaxSectionTokens
	}

	written := 0
	cited := map[int64]bool{}
	for _, s := range sections {
		body := strings.TrimSpace(s.Body)
		if body == "" {
			continue
		}
		// Per-section hard cap, enforced at synthesis time (the growth ceiling
		// lives in the writer, NOT the reader — the system block isn't truncated).
		if est := estimateTokens(body); est > capTok {
			body = clipToTokens(body, capTok)
			log.Warn("reflect: section over cap, truncated", "section", s.Section, "est_tokens", est, "cap", capTok)
		}
		ids := mapCitedFactIDs(s.Members, facts)
		ver, werr := j.SME.UpsertSection(ctx, sme.Section{
			ProjectID:     j.ProjectID,
			AgentID:       j.AgentID,
			Section:       s.Section,
			Body:          body,
			SourceFactIDs: ids,
			TokenEstimate: estimateTokens(body),
		})
		if werr != nil {
			log.Warn("reflect: write section failed", "section", s.Section, "err", werr)
			continue
		}
		written++
		_ = ver
		for _, id := range ids {
			cited[id] = true
		}
	}

	// Reaffirm cited source facts: being load-bearing enough to enter the SME
	// resets their decay clock (design §7 longevity backstop). Best-effort.
	for id := range cited {
		if rerr := j.Semantic.Reaffirm(ctx, id); rerr != nil {
			log.Debug("reflect: reaffirm cited fact failed", "fact", id, "err", rerr)
		}
	}

	// Refresh the live render so the next turn sees the new doc (no per-turn DB read).
	if j.Live != nil {
		if rendered, rerr := j.SME.RenderActive(ctx, j.ProjectID, j.AgentID); rerr == nil {
			j.Live.Set(rendered)
		} else {
			log.Warn("reflect: render for live cache failed", "err", rerr)
		}
	}

	return JobResult{
		Changed: written > 0,
		Summary: fmt.Sprintf("reflect: wrote %d section(s) from %d fact(s), reaffirmed %d cited", written, len(facts), len(cited)),
		Details: map[string]any{
			"sections_written": written,
			"facts_considered": len(facts),
			"facts_cited":      len(cited),
			"project":          j.ProjectID,
		},
	}, nil
}

// selectFacts picks the load-bearing input set: pinned / protected / high-
// importance facts first (recent-first within that), topped up with the most
// recent others to reflectMaxFacts.
func (j *ReflectionJob) selectFacts(ctx context.Context) ([]*semantic.Fact, error) {
	all, err := j.Semantic.List(ctx, semantic.ListFilter{AgentIDs: []string{j.AgentID}, Limit: reflectMaxFacts * 4})
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, nil
	}
	ids := make([]int64, len(all))
	for i, f := range all {
		ids[i] = f.ID
	}
	sigs, err := j.Semantic.SignalsBatch(ctx, ids)
	if err != nil {
		return nil, err
	}
	var primary, recent []*semantic.Fact
	for _, f := range all {
		s := sigs[f.ID]
		if s.Pinned || s.Protected || s.Importance >= reflectImportanceFloor {
			primary = append(primary, f)
		} else {
			recent = append(recent, f)
		}
	}
	selected := primary
	if len(selected) < reflectMaxFacts {
		need := reflectMaxFacts - len(selected)
		if need > len(recent) {
			need = len(recent)
		}
		selected = append(selected, recent[:need]...)
	}
	if len(selected) > reflectMaxFacts {
		selected = selected[:reflectMaxFacts]
	}
	return selected, nil
}

func (j *ReflectionJob) recentEpisodes(ctx context.Context) []*episodic.Episode {
	if j.Episodic == nil {
		return nil
	}
	eps, err := j.Episodic.List(ctx, episodic.ListFilter{AgentIDs: []string{j.AgentID}, Limit: reflectRecentEpisodes})
	if err != nil {
		j.logger().Debug("reflect: recent episodes failed", "err", err)
		return nil
	}
	return eps
}

// --- synthesis ---

type reflectSection struct {
	Section string `json:"section"`
	Body    string `json:"body"`
	Members []int  `json:"source_fact_ids"` // 1-based indices into the numbered fact list
}

const reflectSystemPrompt = `You maintain a concise SUBJECT-MATTER-EXPERT brief about a software project, synthesized from the user's long-term memory. You are given a numbered list of FACTS (and optionally recent conversation snippets). Write a small set of sections that capture the project's durable knowledge.

Rules:
- PRESERVE every specific: file paths, versions, ticket IDs, command names, decisions AND their rationale, gotchas, dead-ends. Specificity is the whole point — do NOT generalize into vague summaries.
- Organize into these sections when content exists (omit empty ones): "Architecture & Decisions", "Conventions & Gotchas", "Open Threads", "Glossary", "History".
- Each section: a few tight bullet points or short sentences. Be dense, not chatty.
- For each section, list source_fact_ids — the numbers of the facts it draws from.
- This is REFERENCE knowledge, not instructions. Do not invent facts not supported by the input.

Respond with JSON only:
{"sections":[{"section":"<title>","body":"<markdown body>","source_fact_ids":[1,4,9]}]}`

const reflectJSONSchema = `{"type":"object","properties":{"sections":{"type":"array","items":{"type":"object","properties":{"section":{"type":"string"},"body":{"type":"string"},"source_fact_ids":{"type":"array","items":{"type":"integer"}}},"required":["section","body"]}}},"required":["sections"]}`

func (j *ReflectionJob) synthesize(ctx context.Context, llm model.LLM, facts []*semantic.Fact, episodes []*episodic.Episode) ([]reflectSection, error) {
	var b strings.Builder
	b.WriteString("FACTS:\n")
	for i, f := range facts {
		fmt.Fprintf(&b, "%d. %s\n", i+1, f.Fact)
	}
	if len(episodes) > 0 {
		b.WriteString("\nRECENT CONVERSATION (context only, do not cite):\n")
		for _, e := range episodes {
			fmt.Fprintf(&b, "- %s -> %s\n", clip(e.Prompt, 120), clip(e.Response, 120))
		}
	}
	b.WriteString("\nWrite the SME sections as JSON.")

	resp, err := llm.Generate(ctx, model.GenerateRequest{
		Messages: []model.Message{
			{Role: "system", Content: reflectSystemPrompt},
			{Role: "user", Content: b.String()},
		},
		JSONSchema:  []byte(reflectJSONSchema),
		Temperature: 0.2,
	})
	if err != nil {
		return nil, fmt.Errorf("summarizer: %w", err)
	}
	if resp.Text == "" {
		return nil, fmt.Errorf("summarizer returned empty")
	}
	var parsed struct {
		Sections []reflectSection `json:"sections"`
	}
	if err := json.Unmarshal([]byte(firstJSONObject(resp.Text)), &parsed); err != nil {
		return nil, fmt.Errorf("parse reflect JSON: %w (%q)", err, clip(resp.Text, 200))
	}
	return parsed.Sections, nil
}

// mapCitedFactIDs converts 1-based indices from the proposer into real fact IDs,
// dropping out-of-range / duplicate references.
func mapCitedFactIDs(members []int, facts []*semantic.Fact) []int64 {
	seen := map[int]bool{}
	var out []int64
	for _, idx := range members {
		if idx < 1 || idx > len(facts) || seen[idx] {
			continue
		}
		seen[idx] = true
		out = append(out, facts[idx-1].ID)
	}
	return out
}

func estimateTokens(s string) int { return len(s) / approxCharsPerToken }

// clipToTokens trims s to roughly maxTokens (by the char estimate), on a word
// boundary where possible, and notes the truncation.
func clipToTokens(s string, maxTokens int) string {
	// Guard against a nonsensically large cap overflowing maxTokens*4 (a >1M-token
	// section cap is absurd — just return unclipped).
	if maxTokens <= 0 || maxTokens > 1<<20 {
		return s
	}
	maxChars := maxTokens * approxCharsPerToken
	if len(s) <= maxChars {
		return s
	}
	cut := s[:maxChars]
	if i := strings.LastIndexAny(cut, " \n"); i > maxChars/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " \n") + " …(truncated)"
}
