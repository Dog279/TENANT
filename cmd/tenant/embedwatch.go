package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"tenant/internal/model"
)

// The embedder's watch (TEN-555). An embedder that takes a connection and
// never answers passes every check that only asks whether it is there: its
// model list still loads. On the R340 one sat like that for hours behind a
// running Tenant, and the next start could not get past it.
//
// So the watch asks for a real embedding: once at start, which is where the
// launch line "embeddings OK" comes from, and then every embedWatchEvery for
// as long as Tenant runs. An embedder that has not answered for
// embedSilentAfter is an event (system 3110), which the built-in alert rule
// embedder-silent turns into a banner and an alert, within minutes and not
// at the next restart.
//
// ponytail: asking keeps the embedding model loaded for as long as Tenant
// runs (Ollama unloads a model nobody has used for five minutes). If that
// memory matters, watch the real embedding calls and ask only when there
// were none.
const (
	embedWatchTimeout = 30 * time.Second // one question; the wiki gives a search the same
	embedWatchEvery   = 5 * time.Minute
	embedWatchSlow    = 5 * time.Second  // an answer that took longer is said to be slow
	embedSilentAfter  = 5 * time.Minute  // no answer for this long is the event
	embedSilentAgain  = 30 * time.Minute // written again this often while it lasts
)

type embedWatch struct {
	what  string                      // nomic-embed-text @ http://localhost:11434
	embed func(context.Context) error // one small embedding
	say   func(string)                // the status feed
	now   func() time.Time
	limit time.Duration // one question's; embedWatchTimeout when zero

	launched bool      // the launch line is said
	failing  bool      // the last question got no answer
	lastOK   time.Time // the last answer, or when the watch began
	wrote    time.Time // when the event was last written; zero while it answers
}

// startEmbedWatch starts the watch when there is a real embedder to ask: not
// the hash stand-in a Tenant on echo, or one started with --allow-no-memory,
// runs on. It ends with ctx.
func startEmbedWatch(ctx context.Context, router *model.Router, say func(string)) {
	if router == nil {
		return
	}
	emb, prof, err := router.EmbedderForRole(ctx, model.RoleEmbedder)
	if err != nil || prof.Backend == "echo" {
		return
	}
	w := &embedWatch{what: prof.Model + " @ " + prof.Endpoint, say: say, now: time.Now,
		embed: func(ctx context.Context) error {
			v, err := emb.Embed(ctx, []string{"tenant embedder watch"})
			if err == nil && (len(v) != 1 || len(v[0]) == 0) {
				err = errors.New("it returned no vector")
			}
			return err
		}}
	go w.run(ctx)
}

func (w *embedWatch) run(ctx context.Context) {
	w.lastOK = w.now()
	t := time.NewTicker(embedWatchEvery)
	defer t.Stop()
	for {
		w.ask(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ask puts one question to the embedder and says what changed.
func (w *embedWatch) ask(ctx context.Context) {
	limit := w.limit
	if limit == 0 {
		limit = embedWatchTimeout
	}
	pctx, cancel := context.WithTimeout(ctx, limit)
	start := w.now()
	err := w.embed(pctx)
	timedOut := errors.Is(pctx.Err(), context.DeadlineExceeded)
	cancel()
	if ctx.Err() != nil {
		return // Tenant is stopping: no answer is not the embedder's doing
	}
	now := w.now()
	if err == nil {
		took := now.Sub(start).Round(10 * time.Millisecond)
		switch {
		case !w.launched && took > embedWatchSlow:
			w.say(fmt.Sprintf("embeddings SLOW: %s answered in %s; memory and wiki search will be slow", w.what, took))
		case !w.launched:
			w.say(fmt.Sprintf("embeddings OK: %s (answered in %s)", w.what, took))
		case w.failing:
			silent := now.Sub(w.lastOK).Round(time.Second)
			w.say(fmt.Sprintf("embeddings answering again: %s (no answer for %s)", w.what, silent))
			if !w.wrote.IsZero() {
				sysEmbedderBack.Log(ctx, "embedder", w.what, "after", silent)
			}
		}
		w.launched, w.failing, w.lastOK, w.wrote = true, false, now, time.Time{}
		return
	}
	why := err.Error()
	if timedOut {
		why = "it took the connection and returned no embedding in " + limit.String()
	}
	if !w.launched {
		w.say(fmt.Sprintf("EMBEDDINGS NOT ANSWERING: %s: %s. Memory and wiki search will fail until it answers; restart the embedding server", w.what, why))
	}
	w.launched, w.failing = true, true
	if silent := now.Sub(w.lastOK); silent >= embedSilentAfter && (w.wrote.IsZero() || now.Sub(w.wrote) >= embedSilentAgain) {
		sysEmbedderSilent.Log(ctx, "embedder", w.what, "for", silent.Round(time.Second), "err", why)
		w.wrote = now
	}
}
