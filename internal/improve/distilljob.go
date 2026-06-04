package improve

import (
	"context"
	"fmt"

	"tenant/internal/memory/distill"
)

// DistillJob is the first self-improvement job: it runs the T2 → T3
// distillation, persisting the episode cursor in the Meta store so
// each run picks up exactly where the last left off (Hermes' "remember
// across sessions" property, Go-native).
//
// This is the concrete realization of the Hermes "closed learning
// loop with periodic nudges" — the scheduler is the nudge, distill is
// the loop body, Meta is the cross-session memory of how far we got.
type DistillJob struct {
	Distiller *distill.Distiller
	Meta      *Meta

	// AgentID scopes the cursor key so multiple agents sharing one
	// Meta store don't clobber each other's progress.
	AgentID string
}

// NewDistillJob wires a DistillJob. The cursor key is derived from
// AgentID so concurrent agents stay isolated.
func NewDistillJob(d *distill.Distiller, m *Meta, agentID string) *DistillJob {
	return &DistillJob{Distiller: d, Meta: m, AgentID: agentID}
}

// Name implements Job.
func (j *DistillJob) Name() string { return "distill" }

func (j *DistillJob) cursorKey() string {
	return "distill_cursor:" + j.AgentID
}

// Run implements Job. Reads the persisted cursor, runs distillation
// from there, persists the new cursor. Cursor is advanced even when
// individual batches fail (distill.Run already handles that) so a
// poison batch never wedges the loop.
func (j *DistillJob) Run(ctx context.Context) (JobResult, error) {
	if j.Distiller == nil {
		return JobResult{}, fmt.Errorf("distilljob: nil Distiller")
	}
	if j.Meta == nil {
		return JobResult{}, fmt.Errorf("distilljob: nil Meta")
	}

	cursor, _, err := j.Meta.GetInt64(ctx, j.cursorKey())
	if err != nil {
		return JobResult{}, fmt.Errorf("distilljob: read cursor: %w", err)
	}

	res, err := j.Distiller.Run(ctx, cursor)
	if err != nil {
		// A hard error (e.g. ctx cancelled, store unreachable).
		// Do NOT advance the cursor — we genuinely didn't process.
		return JobResult{
			Summary: fmt.Sprintf("distill failed at cursor %d: %v", cursor, err),
		}, err
	}

	// Persist the new cursor (advances even past failed batches —
	// distill.Run's contract).
	if res.LastEpisodeID > cursor {
		if setErr := j.Meta.SetInt64(ctx, j.cursorKey(), res.LastEpisodeID); setErr != nil {
			return JobResult{
				Summary: fmt.Sprintf("distilled %d episodes but failed to persist cursor: %v",
					res.EpisodesProcessed, setErr),
			}, setErr
		}
	}

	changed := res.FactsInserted > 0 || res.FactsReaffirmed > 0
	summary := fmt.Sprintf(
		"processed %d episodes, %d facts (%d new, %d reaffirmed), %d batch errors; cursor %d → %d",
		res.EpisodesProcessed, res.FactsExtracted, res.FactsInserted, res.FactsReaffirmed,
		len(res.BatchErrors), cursor, res.LastEpisodeID,
	)
	return JobResult{
		Summary: summary,
		Changed: changed,
		Details: map[string]any{
			"episodes_processed": res.EpisodesProcessed,
			"facts_extracted":    res.FactsExtracted,
			"facts_inserted":     res.FactsInserted,
			"facts_reaffirmed":   res.FactsReaffirmed,
			"batch_errors":       len(res.BatchErrors),
			"cursor_from":        cursor,
			"cursor_to":          res.LastEpisodeID,
		},
	}, nil
}
