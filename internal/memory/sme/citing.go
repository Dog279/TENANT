package sme

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// CitingFact returns the active SME sections that cite factID as a source:
// one of the ways a fact is used downstream (TEN-297). Superseded section
// versions are left out.
func (s *Store) CitingFact(ctx context.Context, factID int64) ([]Section, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT d.project_id, d.agent_id, d.section, d.body, d.source_fact_ids, d.version, d.updated_at, d.token_estimate
        FROM sme_docs d
        JOIN (
            SELECT project_id, agent_id, section, MAX(version) AS v
            FROM sme_docs GROUP BY project_id, agent_id, section
        ) m ON m.project_id = d.project_id AND m.agent_id = d.agent_id
            AND m.section = d.section AND m.v = d.version
        WHERE d.source_fact_ids IS NOT NULL
        ORDER BY d.section`)
	if err != nil {
		return nil, fmt.Errorf("sme: citing fact: %w", err)
	}
	defer rows.Close()
	var out []Section
	for rows.Next() {
		var (
			sec     Section
			src     sql.NullString
			updated int64
		)
		if err := rows.Scan(&sec.ProjectID, &sec.AgentID, &sec.Section, &sec.Body, &src, &sec.Version, &updated, &sec.TokenEstimate); err != nil {
			return nil, fmt.Errorf("sme: scan section: %w", err)
		}
		if src.String == "" || json.Unmarshal([]byte(src.String), &sec.SourceFactIDs) != nil {
			continue
		}
		for _, id := range sec.SourceFactIDs {
			if id == factID {
				sec.UpdatedAt = time.Unix(updated, 0).UTC()
				out = append(out, sec)
				break
			}
		}
	}
	return out, rows.Err()
}
