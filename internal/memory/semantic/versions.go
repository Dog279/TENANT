package semantic

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"tenant/internal/memory"
)

// Origin is what created a fact (TEN-297).
type Origin struct {
	Job        string // distill, consolidate, import, remember, mcp, edit, restore, …
	Model      string // the model that did the work, when one did
	RecordedAt time.Time
}

// Origin returns what created fact id. ok is false for a fact from before
// origins were recorded.
func (s *Store) Origin(ctx context.Context, id int64) (Origin, bool, error) {
	var o Origin
	var model sql.NullString
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT job, model, recorded_at FROM fact_origin WHERE fact_id = ?`, id).Scan(&o.Job, &model, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return Origin{}, false, nil
	}
	if err != nil {
		return Origin{}, false, fmt.Errorf("semantic: origin: %w", err)
	}
	o.Model = model.String
	o.RecordedAt = time.Unix(at, 0).UTC()
	return o, true, nil
}

// Versions returns every version of the fact id belongs to, oldest first: the
// facts it superseded (and they superseded, and so on) and the facts that
// superseded it. A fact that was never superseded or superseding is its only
// version.
func (s *Store) Versions(ctx context.Context, id int64) ([]*Fact, error) {
	seen := map[int64]*Fact{}
	queue := []int64{id}
	for len(queue) > 0 && len(seen) < 1000 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] != nil {
			continue
		}
		f, err := s.Get(ctx, cur)
		if err != nil {
			if errors.Is(err, ErrNotFound) && cur != id {
				continue
			}
			return nil, err
		}
		seen[cur] = f
		if f.SupersededBy != 0 {
			queue = append(queue, f.SupersededBy)
		}
		rows, err := s.db.QueryContext(ctx, `SELECT id FROM facts WHERE superseded_by = ?`, cur)
		if err != nil {
			return nil, fmt.Errorf("semantic: versions: %w", err)
		}
		for rows.Next() {
			var prev int64
			if err := rows.Scan(&prev); err != nil {
				rows.Close()
				return nil, err
			}
			queue = append(queue, prev)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	out := make([]*Fact, 0, len(seen))
	for _, f := range seen {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].FirstSeen.Equal(out[j].FirstSeen) {
			return out[i].FirstSeen.Before(out[j].FirstSeen)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// Edit replaces a fact's text with a new version: a new fact with the same
// agent, visibility, sources and confidence, recorded as made by job
// ("edit" or "restore"), which supersedes the old one. The old version stays,
// so the change can be compared and undone. Pins, protection and importance
// carry over. It returns the new version's id.
//
// It all happens in one transaction, so two edits of the same version cannot
// both become current: an edit that starts after the other commits finds a
// newer version, and one that started before cannot commit its write over the
// other's (SQLite refuses a stale write), so it fails and leaves nothing
// behind.
func (s *Store) Edit(ctx context.Context, id int64, text, job, embedderID string, embedding []float32) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("semantic: edit: %w", err)
	}
	defer tx.Rollback()
	old, err := getFact(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	if old.Tombstoned {
		return 0, fmt.Errorf("semantic: fact %d is removed; restore it before editing", id)
	}
	if old.SupersededBy != 0 {
		return 0, fmt.Errorf("semantic: fact %d has a newer version (#%d); edit that one", id, old.SupersededBy)
	}
	if text == old.Fact {
		return 0, errors.New("semantic: the text is unchanged")
	}
	now := time.Now().UTC()
	nf := &Fact{
		AgentID: old.AgentID, Visibility: old.Visibility, Fact: text,
		SourceEpisodes: old.SourceEpisodes, SourceRunIDs: old.SourceRunIDs,
		Confidence: old.Confidence, FirstSeen: now, LastConfirmed: now,
		EmbedderID: embedderID, Embedding: embedding, OriginJob: job,
	}
	newID, err := insertFact(ctx, tx, nf)
	if err != nil {
		return 0, err
	}
	sig, err := getSignals(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	sig.FactID = newID
	sig.AccessCount, sig.LastAccessed = 0, time.Time{}
	if err := upsertSignals(ctx, tx, sig); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE facts SET superseded_by = ? WHERE id = ?`, newID, id); err != nil {
		return 0, fmt.Errorf("semantic: edit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("semantic: edit: %w", err)
	}
	nf.ID = newID
	s.logAdded(ctx, nf)
	s.log(ctx, memory.FactSuperseded, "fact_id", id, "by", newID)
	return newID, nil
}

// RestoreVersion makes an earlier version current again, as a new version at
// the head of the chain, the way a revert does: history only grows.
func (s *Store) RestoreVersion(ctx context.Context, versionID int64, embedderID string, embedding []float32) (int64, error) {
	versions, err := s.Versions(ctx, versionID)
	if err != nil {
		return 0, err
	}
	var want, head *Fact
	for _, v := range versions {
		if v.ID == versionID {
			want = v
		}
		if v.SupersededBy == 0 {
			head = v
		}
	}
	if want == nil {
		return 0, fmt.Errorf("%w: id=%d", ErrNotFound, versionID)
	}
	if head == nil {
		return 0, errors.New("semantic: no current version to restore over")
	}
	if head.Tombstoned {
		return 0, fmt.Errorf("semantic: the current version (#%d) was removed; restore it first", head.ID)
	}
	if head.ID == want.ID {
		return 0, errors.New("semantic: that version is already current")
	}
	return s.Edit(ctx, head.ID, want.Fact, "restore", embedderID, embedding)
}
