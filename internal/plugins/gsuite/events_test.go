package gsuite

import (
	"context"
	"errors"
	"testing"
	"time"

	"tenant/internal/logs/event"
)

type sink []*event.Event

func (s *sink) Emit(_ context.Context, e *event.Event)          { *s = append(*s, e) }
func (s *sink) Audit(ctx context.Context, e *event.Event) error { s.Emit(ctx, e); return nil }

// Every mint of an access token, whatever the auth mode, is logged: a
// refresh, or its failure (TEN-393).
func TestCachedSourceLogsItsRefreshes(t *testing.T) {
	var s sink
	event.Install(&s)
	defer event.Install(nil)
	ctx := context.Background()
	_, _ = newCached(func(context.Context) (string, time.Time, error) {
		return "", time.Time{}, errors.New("invalid_grant")
	}, nil).token(ctx)
	_, _ = newCached(func(context.Context) (string, time.Time, error) {
		return "tok", time.Now().Add(time.Hour), nil
	}, nil).token(ctx)
	if len(s) != 2 || s[0].ID != 3101 || s[1].ID != 1101 || s[0].Provider != "plugin.gsuite" {
		t.Fatalf("logged %+v, want plugin.gsuite 3101 then 1101", s)
	}
}
