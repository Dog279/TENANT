package improve

import (
	"context"
	"fmt"
	"log/slog"

	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
)

// FeedbackProtectionJob turns the operator's real-world ack signal (TEN-151)
// into learned protection (TEN-255 Phase 4): a fact distilled from a turn the
// operator ACKed is load-bearing by revealed preference, so it is promoted to
// merge-protected — shielded from the holistic consolidation merge alongside
// pinned / high-importance-and-used facts (semantic.MergeProtected).
//
// PROMOTE-ONLY by design. Undo is deliberately NOT wired to auto-UNprotect:
// removing protection is the one direction that can lose nuance, and a fact may
// be protected for several reasons (explicit memory_remember, another acked
// turn). Feedback can only ADD protection — consistent with the no-data-loss
// invariant. Cheap (no LLM): a bounded scan + flag writes, idempotent.
type FeedbackProtectionJob struct {
	Semantic *semantic.Store
	Episodic *episodic.Store
	AgentID  string
	// MaxFacts bounds the scan. 0 ⇒ default.
	MaxFacts int
	Logger   *slog.Logger
}

const defaultFeedbackProtectMaxFacts = 2000

// Name implements Job.
func (j *FeedbackProtectionJob) Name() string { return "feedback-protect" }

// Run promotes facts whose source episodes include an acked episode to
// protected, where they aren't already protected.
func (j *FeedbackProtectionJob) Run(ctx context.Context) (JobResult, error) {
	if j.Semantic == nil || j.Episodic == nil {
		return JobResult{}, fmt.Errorf("feedback-protect: nil store")
	}
	if j.AgentID == "" {
		return JobResult{}, fmt.Errorf("feedback-protect: empty AgentID")
	}
	log := j.Logger
	if log == nil {
		log = slog.Default()
	}

	ackedIDs, err := j.Episodic.AckedEpisodeIDs(ctx, j.AgentID)
	if err != nil {
		return JobResult{}, fmt.Errorf("feedback-protect: acked ids: %w", err)
	}
	if len(ackedIDs) == 0 {
		return JobResult{Summary: "feedback-protect: no acked episodes"}, nil
	}
	acked := make(map[int64]bool, len(ackedIDs))
	for _, id := range ackedIDs {
		acked[id] = true
	}

	limit := j.MaxFacts
	if limit <= 0 {
		limit = defaultFeedbackProtectMaxFacts
	}
	facts, err := j.Semantic.List(ctx, semantic.ListFilter{AgentIDs: []string{j.AgentID}, Limit: limit})
	if err != nil {
		return JobResult{}, fmt.Errorf("feedback-protect: list facts: %w", err)
	}

	// Candidate facts: those with at least one acked source episode.
	var candidateIDs []int64
	candidate := map[int64]bool{}
	for _, f := range facts {
		for _, ep := range f.SourceEpisodes {
			if acked[ep] {
				candidateIDs = append(candidateIDs, f.ID)
				candidate[f.ID] = true
				break
			}
		}
	}
	if len(candidateIDs) == 0 {
		return JobResult{Summary: "feedback-protect: no facts sourced from acked turns"}, nil
	}

	// Only write to facts not already protected (idempotent, avoids churn).
	sigs, err := j.Semantic.SignalsBatch(ctx, candidateIDs)
	if err != nil {
		return JobResult{}, fmt.Errorf("feedback-protect: signals: %w", err)
	}
	promoted := 0
	for _, id := range candidateIDs {
		if sigs[id].Protected {
			continue
		}
		if serr := j.Semantic.SetProtected(ctx, id, true); serr != nil {
			log.Warn("feedback-protect: set protected failed", "fact", id, "err", serr)
			continue
		}
		promoted++
	}

	return JobResult{
		Changed: promoted > 0,
		Summary: fmt.Sprintf("feedback-protect: promoted %d fact(s) from %d acked turn(s)", promoted, len(ackedIDs)),
		Details: map[string]any{
			"promoted":       promoted,
			"acked_episodes": len(ackedIDs),
			"candidates":     len(candidateIDs),
		},
	}, nil
}
