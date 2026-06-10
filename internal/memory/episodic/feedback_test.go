package episodic_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"tenant/internal/memory/episodic"
)

func TestSetUserFeedbackAndLatest(t *testing.T) {
	s, err := episodic.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	base := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	id1 := recInsert(t, s, "main", "first", base, false)
	id2 := recInsert(t, s, "main", "second", base.Add(time.Minute), false)

	// LatestID returns the newest live episode.
	got, err := s.LatestID(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if got != id2 {
		t.Fatalf("LatestID=%d want %d", got, id2)
	}

	// Set + read back via Recent.
	if err := s.SetUserFeedback(ctx, id2, episodic.FeedbackAck); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserFeedback(ctx, id1, episodic.FeedbackUndo); err != nil {
		t.Fatal(err)
	}
	eps, _ := s.Recent(ctx, "main", 10)
	byID := map[int64]string{}
	for _, e := range eps {
		byID[e.ID] = e.UserFeedback
	}
	if byID[id2] != episodic.FeedbackAck || byID[id1] != episodic.FeedbackUndo {
		t.Fatalf("feedback not persisted: %+v", byID)
	}

	// Clearing works.
	if err := s.SetUserFeedback(ctx, id1, ""); err != nil {
		t.Fatal(err)
	}

	// Invalid value rejected.
	if err := s.SetUserFeedback(ctx, id1, "meh"); err == nil {
		t.Error("invalid feedback should error")
	}

	// Unknown id → ErrNotFound.
	if err := s.SetUserFeedback(ctx, 99999, episodic.FeedbackAck); !errors.Is(err, episodic.ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestFeedbackStats(t *testing.T) {
	s, err := episodic.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	base := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	// 3 acks, 1 undo, 1 no-feedback for "main"; 1 ack for "other" (must not leak).
	a := recInsert(t, s, "main", "a", base, false)
	b := recInsert(t, s, "main", "b", base.Add(1*time.Minute), false)
	c := recInsert(t, s, "main", "c", base.Add(2*time.Minute), false)
	d := recInsert(t, s, "main", "d", base.Add(3*time.Minute), false)
	recInsert(t, s, "main", "e", base.Add(4*time.Minute), false) // no feedback
	o := recInsert(t, s, "other", "o", base.Add(5*time.Minute), false)
	for _, id := range []int64{a, b, c} {
		s.SetUserFeedback(ctx, id, episodic.FeedbackAck)
	}
	s.SetUserFeedback(ctx, d, episodic.FeedbackUndo)
	s.SetUserFeedback(ctx, o, episodic.FeedbackAck)

	acks, undos, err := s.FeedbackStats(ctx, "main", 50)
	if err != nil {
		t.Fatal(err)
	}
	if acks != 3 || undos != 1 {
		t.Fatalf("stats main = (%d ack, %d undo) want (3,1)", acks, undos)
	}

	// Window caps the lookback (most-recent-2 fed-back = c(ack) + d(undo)).
	acks2, undos2, _ := s.FeedbackStats(ctx, "main", 2)
	if acks2 != 1 || undos2 != 1 {
		t.Fatalf("windowed stats = (%d,%d) want (1,1)", acks2, undos2)
	}

	// n<=0 returns zeroes.
	if a0, u0, _ := s.FeedbackStats(ctx, "main", 0); a0 != 0 || u0 != 0 {
		t.Errorf("n=0 should be zero, got (%d,%d)", a0, u0)
	}
}
