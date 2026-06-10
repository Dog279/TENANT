package episodic

import (
	"context"
	"database/sql"
	"fmt"
)

// User-feedback values for Episode.UserFeedback (the real-world success signal,
// TEN-151). Empty string = no feedback given (the default).
const (
	FeedbackAck  = "ack"  // the operator was happy with the turn
	FeedbackUndo = "undo" // the operator was NOT happy / wants it reverted
)

// validFeedback reports whether f is an accepted feedback value ("" clears it).
func validFeedback(f string) bool {
	return f == "" || f == FeedbackAck || f == FeedbackUndo
}

// SetUserFeedback records the operator's ack/undo on a previously-stored episode
// (the column existed but was never written until TEN-151). Passing "" clears any
// prior feedback. Returns ErrNotFound if no live episode has that id.
func (s *Store) SetUserFeedback(ctx context.Context, id int64, feedback string) error {
	if !validFeedback(feedback) {
		return fmt.Errorf("episodic: invalid feedback %q (want %q, %q, or \"\")", feedback, FeedbackAck, FeedbackUndo)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE episodes SET user_feedback=? WHERE id=? AND tombstoned=0`,
		nullString(feedback), id)
	if err != nil {
		return fmt.Errorf("episodic: set feedback: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// LatestID returns the id of the most-recent live episode for EXACTLY agentID
// (no sub-agent glob — mirrors Recent's scoping). Used by `tenant ack`/`/ack` to
// target "my last turn". Returns ErrNotFound when the agent has no episodes.
func (s *Store) LatestID(ctx context.Context, agentID string) (int64, error) {
	if agentID == "" {
		return 0, fmt.Errorf("episodic: empty agentID")
	}
	var id int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM episodes WHERE tombstoned=0 AND agent_id=? ORDER BY id DESC LIMIT 1`,
		agentID).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("episodic: latest id: %w", err)
	}
	return id, nil
}

// FeedbackStats tallies ack/undo over the most-recent n episodes that carry
// feedback for EXACTLY agentID. It powers the "trusted" auto-accept gate
// (TEN-152): auto-accept proceeds only while the operator's recent feedback is
// healthy (enough acks, no undos). n<=0 returns zeroes.
func (s *Store) FeedbackStats(ctx context.Context, agentID string, n int) (acks, undos int, err error) {
	if agentID == "" || n <= 0 {
		return 0, 0, nil
	}
	rows, err := s.db.QueryContext(ctx, `
        SELECT user_feedback FROM episodes
         WHERE tombstoned = 0 AND agent_id = ?
           AND user_feedback IS NOT NULL AND user_feedback != ''
         ORDER BY id DESC
         LIMIT ?`, agentID, n)
	if err != nil {
		return 0, 0, fmt.Errorf("episodic: feedback stats: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var fb string
		if err := rows.Scan(&fb); err != nil {
			return 0, 0, fmt.Errorf("episodic: feedback stats scan: %w", err)
		}
		switch fb {
		case FeedbackAck:
			acks++
		case FeedbackUndo:
			undos++
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("episodic: feedback stats iter: %w", err)
	}
	return acks, undos, nil
}
