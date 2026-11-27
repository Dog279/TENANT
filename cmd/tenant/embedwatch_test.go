package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"tenant/internal/logs/event/eventtest"
)

// The embedder's watch (TEN-555): the launch line comes from a real
// embedding, and an embedder that stops answering is an event after five
// minutes, once, again after half an hour, and an event when it is back.
func TestEmbedWatch(t *testing.T) {
	rec := eventtest.Capture(t)
	clock := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	var said []string
	var answer func(context.Context) error
	w := &embedWatch{what: "nomic @ http://embed", now: func() time.Time { return clock }, lastOK: clock,
		say:   func(s string) { said = append(said, s) },
		embed: func(ctx context.Context) error { return answer(ctx) }}
	ids := func() (ids []uint32) {
		for _, e := range rec.Events() {
			ids = append(ids, e.ID)
		}
		return ids
	}
	// ask runs one question that takes took, and returns what was said for it.
	ask := func(took time.Duration, err error) string {
		t.Helper()
		said = nil
		answer = func(context.Context) error { clock = clock.Add(took); return err }
		w.ask(context.Background())
		if len(said) > 1 {
			t.Fatalf("one question said %d lines: %q", len(said), said)
		}
		return strings.Join(said, "")
	}
	down := errors.New("connection refused")

	if got := ask(300*time.Millisecond, nil); got != "embeddings OK: nomic @ http://embed (answered in 300ms)" {
		t.Errorf("the launch line of an embedder that answers: %q", got)
	}
	if got := ask(time.Second, nil); got != "" {
		t.Errorf("a later answer says nothing, and said %q", got)
	}

	// It stops answering. One missed question is not five minutes.
	if got := ask(embedWatchTimeout, down); got != "" || len(ids()) != 0 {
		t.Errorf("the first question with no answer: said %q, events %v; want neither yet", got, ids())
	}
	clock = clock.Add(embedWatchEvery)
	ask(embedWatchTimeout, down)
	if got := ids(); len(got) != 1 || got[0] != sysEmbedderSilent.Spec().ID {
		t.Fatalf("after five minutes with no answer: events %v, want one system %d", got, sysEmbedderSilent.Spec().ID)
	}
	if e := rec.Events()[0]; !strings.Contains(e.Msg, "nomic @ http://embed") || !strings.Contains(e.Msg, "connection refused") {
		t.Errorf("the event does not name the embedder and the reason: %q", e.Msg)
	}
	clock = clock.Add(embedWatchEvery)
	ask(embedWatchTimeout, down)
	if got := ids(); len(got) != 1 {
		t.Errorf("five minutes later it is written again: %v", got)
	}
	clock = clock.Add(embedSilentAgain)
	ask(embedWatchTimeout, down)
	if got := ids(); len(got) != 2 {
		t.Errorf("half an hour on, still silent: events %v, want it written a second time", got)
	}

	// And back.
	if got := ask(time.Second, nil); !strings.HasPrefix(got, "embeddings answering again: nomic @ http://embed (no answer for ") {
		t.Errorf("back again: said %q", got)
	}
	if got := ids(); len(got) != 3 || got[2] != sysEmbedderBack.Spec().ID {
		t.Errorf("back again: events %v, want a system %d last", got, sysEmbedderBack.Spec().ID)
	}
	if got := ask(time.Second, nil); got != "" {
		t.Errorf("answering as before says nothing, and said %q", got)
	}
}

// What the launch line says of an embedder that is slow, that is up and does
// not answer (the R340's, TEN-555), and that fails outright.
func TestEmbedWatchLaunchLine(t *testing.T) {
	eventtest.Capture(t)
	for _, tc := range []struct {
		name  string
		embed func(context.Context) (time.Duration, error) // how long it took, and how it ended
		want  string
	}{
		{"slow", func(context.Context) (time.Duration, error) { return 7 * time.Second, nil },
			"embeddings SLOW: m @ e answered in 7s; memory and wiki search will be slow"},
		{"up and silent", func(ctx context.Context) (time.Duration, error) {
			<-ctx.Done() // it never answers: the watch's limit ends the question
			return 10 * time.Millisecond, ctx.Err()
		}, "EMBEDDINGS NOT ANSWERING: m @ e: it took the connection and returned no embedding in 10ms. Memory and wiki search will fail"},
		{"refused", func(context.Context) (time.Duration, error) { return 0, errors.New("connection refused") },
			"EMBEDDINGS NOT ANSWERING: m @ e: connection refused. Memory and wiki search will fail until it answers; restart the embedding server"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := time.Now()
			var said []string
			w := &embedWatch{what: "m @ e", now: func() time.Time { return clock }, lastOK: clock, limit: 10 * time.Millisecond,
				say: func(s string) { said = append(said, s) }}
			w.embed = func(ctx context.Context) error {
				took, err := tc.embed(ctx)
				clock = clock.Add(took)
				return err
			}
			w.ask(context.Background())
			if len(said) != 1 || !strings.HasPrefix(said[0], tc.want) {
				t.Errorf("said %q, want %q", said, tc.want)
			}
		})
	}
	// A Tenant that is stopping says nothing of the question it cut short.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var said []string
	w := &embedWatch{what: "m @ e", now: time.Now, lastOK: time.Now(), say: func(s string) { said = append(said, s) },
		embed: func(ctx context.Context) error { return ctx.Err() }}
	w.ask(ctx)
	if len(said) != 0 || w.launched {
		t.Errorf("stopping: said %q, launched %v", said, w.launched)
	}
}
