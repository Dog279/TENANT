package semantic

import (
	"context"
	"fmt"
	"strings"
)

// ListFilter narrows a List call. Zero-value lists all live (non-
// tombstoned, non-superseded) facts. All fields combine with AND.
type ListFilter struct {
	AgentIDs          []string // empty = all
	Visibility        []string // empty = all
	Limit             int      // 0 = unlimited (use with care)
	IncludeTombstoned bool     // default false
	IncludeSuperseded bool     // default false
}

// List returns facts matching filter ordered by last_confirmed DESC
// (most recently relevant first). Used by the MCP memory server's
// `memory://facts` resource — "what does the agent currently know".
// Search is for relevance retrieval; List is for enumeration.
func (s *Store) List(ctx context.Context, f ListFilter) ([]*Fact, error) {
	clauses := []string{"1=1"}
	var args []any

	if !f.IncludeTombstoned {
		clauses = append(clauses, "tombstoned = 0")
	}
	if !f.IncludeSuperseded {
		clauses = append(clauses, "superseded_by IS NULL")
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

	q := `SELECT id, agent_id, visibility, fact, source_episodes, confidence,
              first_seen, last_confirmed, superseded_by, embedder_id, embedding, tombstoned
          FROM facts WHERE ` + strings.Join(clauses, " AND ") + ` ORDER BY last_confirmed DESC`
	if f.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", f.Limit)
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("semantic: list: %w", err)
	}
	defer rows.Close()

	var out []*Fact
	for rows.Next() {
		fct, err := scanFact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, fct)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("semantic: list iter: %w", err)
	}
	return out, nil
}

// ListPage returns one keyset page of live facts for agentID ordered by
// id DESC (newest first, stable for pagination — id is monotonic and
// never reshuffles under Reaffirm the way last_confirmed would). afterID
// of 0 starts at the first page; otherwise rows with id < afterID are
// returned. limit caps the page (<=0 defaults to 50). The caller paginates
// with the last returned fact's ID as the next afterID.
func (s *Store) ListPage(ctx context.Context, agentID string, afterID int64, limit int) ([]*Fact, error) {
	if limit <= 0 {
		limit = 50
	}
	clauses := []string{"tombstoned = 0", "superseded_by IS NULL", "agent_id = ?"}
	args := []any{agentID}
	if afterID > 0 {
		clauses = append(clauses, "id < ?")
		args = append(args, afterID)
	}
	q := `SELECT id, agent_id, visibility, fact, source_episodes, confidence,
	             first_seen, last_confirmed, superseded_by, embedder_id, embedding, tombstoned
	         FROM facts WHERE ` + strings.Join(clauses, " AND ") +
		fmt.Sprintf(" ORDER BY id DESC LIMIT %d", limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("semantic: listpage: %w", err)
	}
	defer rows.Close()

	var out []*Fact
	for rows.Next() {
		fct, err := scanFact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, fct)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("semantic: listpage iter: %w", err)
	}
	return out, nil
}
