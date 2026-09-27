package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"tenant/internal/logs/event"
)

type eventSink struct {
	mu  sync.Mutex
	got []string
}

func (s *eventSink) Emit(_ context.Context, e *event.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.Provider == "plugin.github" {
		s.got = append(s.got, fmt.Sprintf("%d %s", e.ID, e.Msg))
	}
}

func (s *eventSink) Audit(ctx context.Context, e *event.Event) error { s.Emit(ctx, e); return nil }

// Every plugin but the channels brings its event catalog (TEN-393), and the
// host writes its lifecycle through it: whether it started, and the
// operator's enable and disable.
func TestPluginsLogThroughTheirOwnCatalog(t *testing.T) {
	for _, d := range plugins().All() {
		if channel := d.ID == "discord" || d.ID == "imessage"; (d.Events == nil) != channel {
			t.Errorf("plugin %q: Events = %v; every plugin but the channels declares its catalog", d.ID, d.Events)
		}
	}

	s := &eventSink{}
	event.Install(s)
	defer event.Install(nil)
	ctx := context.Background()
	gh, _ := plugins().Get("github")
	gh.logStart(ctx, fmt.Errorf("github plugin: %w yet", errNotConfigured)) // a state, not a failure
	gh.logStart(ctx, errors.New("bad credentials"))
	gh.logStart(ctx, nil)

	mux := newToolMux()
	mux.Add("github", stubPlugin{Specs: gh.Tools()})
	mux.SetEnabled("github", false) // the launch's own
	mux.SetOnChange(func(map[string]bool) {})
	mux.SetEnabled("github", true)
	mux.SetEnabled("github_create_pr", false)

	want := []string{
		"3001 GitHub could not start: bad credentials",
		"1 GitHub started",
		"2 github enabled",
		"3 github_create_pr disabled",
	}
	if !slices.Equal(s.got, want) {
		t.Fatalf("plugin.github logged %q, want %q", s.got, want)
	}
}
