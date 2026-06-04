package semantic

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Insert writes a new Fact and returns its assigned ID. Required:
// AgentID, Fact (the claim text), EmbedderID, Embedding. Defaults:
// Visibility = private, Confidence = 1.0 if 0, FirstSeen / LastConfirmed
// = now if zero.
func (s *Store) Insert(ctx context.Context, f *Fact) (int64, error) {
	if f == nil {
		return 0, errors.New("semantic: Insert nil fact")
	}
	if f.AgentID == "" {
		return 0, errors.New("semantic: Insert requires AgentID")
	}
	if f.Fact == "" {
		return 0, errors.New("semantic: Insert requires Fact text")
	}
	if f.EmbedderID == "" {
		return 0, errors.New("semantic: Insert requires EmbedderID")
	}
	if len(f.Embedding) == 0 {
		return 0, errors.New("semantic: Insert requires non-empty Embedding")
	}
	if f.Visibility == "" {
		f.Visibility = VisibilityPrivate
	}
	if f.Confidence == 0 {
		f.Confidence = 1.0
	}
	now := time.Now().UTC()
	if f.FirstSeen.IsZero() {
		f.FirstSeen = now
	}
	if f.LastConfirmed.IsZero() {
		f.LastConfirmed = now
	}

	sourcesJSON, err := marshalSources(f.SourceEpisodes)
	if err != nil {
		return 0, err
	}
	var supersededBy any
	if f.SupersededBy != 0 {
		supersededBy = f.SupersededBy
	}

	res, err := s.db.ExecContext(ctx, `
        INSERT INTO facts
            (agent_id, visibility, fact, source_episodes, confidence,
             first_seen, last_confirmed, superseded_by, embedder_id, embedding, tombstoned)
        VALUES
            (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    `,
		f.AgentID, f.Visibility, f.Fact, nullString(sourcesJSON), f.Confidence,
		f.FirstSeen.UTC().Unix(), f.LastConfirmed.UTC().Unix(),
		supersededBy, f.EmbedderID, encodeEmbedding(f.Embedding),
		boolToInt(f.Tombstoned),
	)
	if err != nil {
		return 0, fmt.Errorf("semantic: insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("semantic: lastinsertid: %w", err)
	}
	f.ID = id
	return id, nil
}

// Get fetches a fact by ID. Returns ErrNotFound if no row matches.
// Tombstoned and superseded facts are returned (audit path); Search
// filters them out.
func (s *Store) Get(ctx context.Context, id int64) (*Fact, error) {
	row := s.db.QueryRowContext(ctx, `
        SELECT id, agent_id, visibility, fact, source_episodes, confidence,
               first_seen, last_confirmed, superseded_by, embedder_id, embedding, tombstoned
        FROM facts WHERE id = ?
    `, id)
	return scanFact(row)
}

// Supersede marks oldID as superseded by newID. Both must exist. The
// old fact stays in the DB for audit; Search treats it as gone.
// Use this when the distill job decides a new fact contradicts an
// older one.
func (s *Store) Supersede(ctx context.Context, oldID, newID int64) error {
	if oldID == newID {
		return errors.New("semantic: cannot supersede with self")
	}
	// Verify both rows exist before mutating; gives clearer errors than
	// a silent UPDATE that affects zero rows.
	if _, err := s.Get(ctx, oldID); err != nil {
		return fmt.Errorf("semantic: supersede old=%d: %w", oldID, err)
	}
	if _, err := s.Get(ctx, newID); err != nil {
		return fmt.Errorf("semantic: supersede new=%d: %w", newID, err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE facts SET superseded_by = ? WHERE id = ?`, newID, oldID); err != nil {
		return fmt.Errorf("semantic: supersede: %w", err)
	}
	return nil
}

// Reaffirm bumps last_confirmed on a fact to now. The distill job
// calls this when an episode re-validates an existing fact instead
// of inserting a duplicate. Resets the confidence-decay clock.
func (s *Store) Reaffirm(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE facts SET last_confirmed = ? WHERE id = ?`,
		time.Now().UTC().Unix(), id)
	if err != nil {
		return fmt.Errorf("semantic: reaffirm: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: id=%d", ErrNotFound, id)
	}
	return nil
}

// Tombstone marks a fact as user-forgotten. Search filters it out;
// Get still returns it. Separate from supersede: tombstone is a user
// privacy act, supersede is a knowledge-update act.
func (s *Store) Tombstone(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE facts SET tombstoned = 1 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("semantic: tombstone: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: id=%d", ErrNotFound, id)
	}
	return nil
}

// Restore clears a fact's tombstone — the inverse of Tombstone, so an
// accidental delete in the curator is recoverable. Resets last_confirmed
// to now as well so the restored fact isn't immediately decayed away.
// Reaffirm alone would NOT bring a fact back: it only touches the decay
// clock, not the tombstone flag Search filters on.
func (s *Store) Restore(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE facts SET tombstoned = 0, last_confirmed = ? WHERE id = ?`,
		time.Now().UTC().Unix(), id)
	if err != nil {
		return fmt.Errorf("semantic: restore: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: id=%d", ErrNotFound, id)
	}
	return nil
}

// Count returns the number of facts. By default excludes tombstoned
// and superseded rows (the "live" count). Pass true/true for full
// audit count.
func (s *Store) Count(ctx context.Context, includeTombstoned, includeSuperseded bool) (int, error) {
	q := `SELECT COUNT(*) FROM facts`
	clauses := []string{}
	if !includeTombstoned {
		clauses = append(clauses, "tombstoned = 0")
	}
	if !includeSuperseded {
		clauses = append(clauses, "superseded_by IS NULL")
	}
	if len(clauses) > 0 {
		q += " WHERE "
		for i, c := range clauses {
			if i > 0 {
				q += " AND "
			}
			q += c
		}
	}
	var n int
	if err := s.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, fmt.Errorf("semantic: count: %w", err)
	}
	return n, nil
}

// --- helpers ---

type rowScanner interface {
	Scan(dest ...any) error
}

func scanFact(row rowScanner) (*Fact, error) {
	var (
		f               Fact
		sourcesJSON     sql.NullString
		supersededBy    sql.NullInt64
		embeddingBytes  []byte
		firstSeenUnix   int64
		lastConfirmUnix int64
		tombstonedInt   int
	)
	err := row.Scan(
		&f.ID, &f.AgentID, &f.Visibility, &f.Fact, &sourcesJSON, &f.Confidence,
		&firstSeenUnix, &lastConfirmUnix, &supersededBy,
		&f.EmbedderID, &embeddingBytes, &tombstonedInt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("semantic: scan: %w", err)
	}
	f.FirstSeen = time.Unix(firstSeenUnix, 0).UTC()
	f.LastConfirmed = time.Unix(lastConfirmUnix, 0).UTC()
	if supersededBy.Valid {
		f.SupersededBy = supersededBy.Int64
	}
	f.Tombstoned = tombstonedInt != 0

	if sourcesJSON.Valid && sourcesJSON.String != "" {
		if err := json.Unmarshal([]byte(sourcesJSON.String), &f.SourceEpisodes); err != nil {
			return nil, fmt.Errorf("semantic: parse source_episodes: %w", err)
		}
	}
	if len(embeddingBytes) > 0 {
		f.Embedding, err = decodeEmbedding(embeddingBytes)
		if err != nil {
			return nil, err
		}
	}
	return &f, nil
}

func marshalSources(ids []int64) (string, error) {
	if len(ids) == 0 {
		return "", nil
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return "", fmt.Errorf("semantic: marshal source_episodes: %w", err)
	}
	return string(b), nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
