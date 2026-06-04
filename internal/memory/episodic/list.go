package episodic

import (
	"context"
	"fmt"
	"strings"
)

// ListFilter narrows a chronological List call. Zero-value matches
// everything (capped by Limit). All fields combine with AND.
type ListFilter struct {
	AgentIDs         []string // empty = all
	Visibility       []string // empty = all
	SinceID          int64    // id > SinceID (0 = no lower bound)
	BeforeID         int64    // id < BeforeID (0 = no upper bound)
	Limit            int      // 0 = unlimited (use with care on large stores)
	IncludeTombstoned bool    // default false: skip tombstoned rows
}

// List returns episodes matching filter in id-ascending (i.e.
// chronological) order. Used by the distillation job to scan
// episodes since the last cursor. Search is for relevance retrieval;
// List is for chronological scan.
func (s *Store) List(ctx context.Context, f ListFilter) ([]*Episode, error) {
	clauses := []string{"1=1"}
	var args []any

	if !f.IncludeTombstoned {
		clauses = append(clauses, "tombstoned = 0")
	}
	if len(f.AgentIDs) > 0 {
		clauses = append(clauses, "agent_id IN ("+placeholders(len(f.AgentIDs))+")")
		for _, id := range f.AgentIDs {
			args = append(args, id)
		}
	}
	if len(f.Visibility) > 0 {
		clauses = append(clauses, "visibility IN ("+placeholders(len(f.Visibility))+")")
		for _, v := range f.Visibility {
			args = append(args, v)
		}
	}
	if f.SinceID > 0 {
		clauses = append(clauses, "id > ?")
		args = append(args, f.SinceID)
	}
	if f.BeforeID > 0 {
		clauses = append(clauses, "id < ?")
		args = append(args, f.BeforeID)
	}

	q := `SELECT id, agent_id, visibility, session_id, ts, prompt, response,
              tool_calls, outcome, user_feedback, tags, embedder_id, embedding, tombstoned
          FROM episodes WHERE ` + strings.Join(clauses, " AND ") + ` ORDER BY id ASC`
	if f.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", f.Limit)
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("episodic: list: %w", err)
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
		return nil, fmt.Errorf("episodic: list iter: %w", err)
	}
	return out, nil
}
