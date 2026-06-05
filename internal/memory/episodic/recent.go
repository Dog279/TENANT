package episodic

import (
	"context"
	"fmt"
)

// Recent returns the most-recently-inserted non-tombstoned episodes for EXACTLY
// agentID, newest-first by id then reversed to chronological (oldest-first) for
// natural rendering. n<=0 or empty agentID returns nil.
//
// Unlike retrieval (Search via filterAgent, which globs `<id>-*` to include an
// orchestrator's sub-agents), Recent is deliberately EXACT-match: a "resume your
// last session" recap is the operator's own thread, not a spawned sub-agent's
// intermediate scratch work — pulling `main-researcher-1` turns into the recap
// would leak content the operator never saw. Tombstoned (operator-deleted) rows
// are always excluded, with no override: a deleted turn must not resurface.
func (s *Store) Recent(ctx context.Context, agentID string, n int) ([]*Episode, error) {
	if agentID == "" || n <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, agent_id, visibility, session_id, ts, prompt, response,
               tool_calls, outcome, user_feedback, tags, embedder_id, embedding, tombstoned
          FROM episodes
         WHERE tombstoned = 0 AND agent_id = ?
         ORDER BY id DESC
         LIMIT ?`, agentID, n)
	if err != nil {
		return nil, fmt.Errorf("episodic: recent: %w", err)
	}
	defer rows.Close()

	var out []*Episode
	for rows.Next() {
		ep, err := scanEpisode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ep)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("episodic: recent iter: %w", err)
	}
	// Reverse newest-first → chronological so callers render oldest→newest.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}
