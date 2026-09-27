package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"tenant/internal/logs/event"
)

type sink []*event.Event

func (s *sink) Emit(_ context.Context, e *event.Event)          { *s = append(*s, e) }
func (s *sink) Audit(ctx context.Context, e *event.Event) error { s.Emit(ctx, e); return nil }

// Open sends every API call through the plugin's catalog, the injected client
// too: a refused token reaches the plugin.github log (TEN-393).
func TestOpenLogsARefusedToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	var s sink
	event.Install(&s)
	defer event.Install(nil)
	if _, err := openTest(t, srv).Validate(context.Background()); err == nil {
		t.Fatal("a 401 validated")
	}
	if len(s) != 1 || s[0].Provider != "plugin.github" || s[0].ID != 3002 {
		t.Fatalf("logged %+v, want plugin.github/3002", s)
	}
}
