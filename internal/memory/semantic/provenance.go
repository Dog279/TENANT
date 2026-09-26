package semantic

import (
	"context"
	"fmt"
	"time"
)

// RemovedSource is a source a fact was distilled from that has since been
// deleted, by a privacy deletion or a retention prune (TEN-309).
type RemovedSource struct {
	Kind      string // "episode" or "run"
	ID        string
	RemovedAt time.Time
	Reason    string // "forget" or "retention"
}

// RemovedSources lists the deleted sources of fact id, oldest removal first.
func (s *Store) RemovedSources(ctx context.Context, id int64) ([]RemovedSource, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT source_kind, source_id, removed_at, reason
        FROM fact_source_tombstones WHERE fact_id = ?
        ORDER BY removed_at, source_kind, source_id`, id)
	if err != nil {
		return nil, fmt.Errorf("semantic: removed sources: %w", err)
	}
	defer rows.Close()
	var out []RemovedSource
	for rows.Next() {
		var r RemovedSource
		var at int64
		if err := rows.Scan(&r.Kind, &r.ID, &at, &r.Reason); err != nil {
			return nil, err
		}
		r.RemovedAt = time.Unix(at, 0).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}
