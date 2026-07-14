package episodic

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// AddTags appends missing tags to an existing episode. It is used by
// asynchronous observers whose result may arrive just after Insert; updates
// are idempotent so a close race can safely write the same tags twice.
func (s *Store) AddTags(ctx context.Context, id int64, tags []string) error {
	if s == nil || s.db == nil || id == 0 || len(tags) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("episodic: add tags begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var raw sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT tags FROM episodes WHERE id = ?`, id).Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("%w: id=%d", ErrNotFound, id)
		}
		return fmt.Errorf("episodic: add tags read: %w", err)
	}
	var current []string
	if raw.Valid && raw.String != "" {
		if err := json.Unmarshal([]byte(raw.String), &current); err != nil {
			return fmt.Errorf("episodic: add tags decode: %w", err)
		}
	}
	seen := make(map[string]struct{}, len(current)+len(tags))
	for _, tag := range current {
		seen[tag] = struct{}{}
	}
	changed := false
	for _, tag := range tags {
		if tag == "" {
			continue
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		current = append(current, tag)
		changed = true
	}
	if !changed {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("episodic: add tags commit: %w", err)
		}
		return nil
	}
	b, err := json.Marshal(current)
	if err != nil {
		return fmt.Errorf("episodic: add tags encode: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE episodes SET tags = ? WHERE id = ?`, string(b), id); err != nil {
		return fmt.Errorf("episodic: add tags update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("episodic: add tags commit: %w", err)
	}
	return nil
}
