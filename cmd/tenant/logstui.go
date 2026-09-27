package main

// logstui.go: the TUI's /logs (TEN-381) reads the log book through its API
// (internal/logs/logapi), run in this process over the hub's store with the
// client tenant logs uses (logsctl.go): one filter language, masking and
// Help for every viewer.

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"tenant/internal/logs"
	"tenant/internal/logs/event"
	"tenant/internal/logs/logapi"
	"tenant/internal/runtrace"
)

// tuiLogs is control.LogsControl over one hub.
type tuiLogs struct{ lb *logbook }

// newTUILogs serves /logs from hub's store; runs, when not nil, adds a
// trace's run record to its timeline.
func newTUILogs(hub *logs.Hub, runs *runtrace.Store) tuiLogs {
	api := &logapi.API{Hub: func() *logs.Hub { return hub }, Surface: surfaceTUI}
	if runs != nil {
		api.Runs = runs.Get
	}
	return tuiLogs{&logbook{base: "http://logbook", http: &http.Client{Transport: inProcess{api.Handler()}}, close: func() {}}}
}

func (t tuiLogs) Query(ctx context.Context, filter string, limit int) ([]event.Event, string, error) {
	return logsNewest(ctx, t.lb, filter, limit)
}

func (t tuiLogs) Follow(ctx context.Context, filter, cursor string) <-chan event.Event {
	ch := make(chan event.Event, 64)
	go func() {
		defer close(ch)
		_ = logsFollow(ctx, t.lb, filter, cursor, func(e event.Event) error {
			select {
			case ch <- e:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	return ch
}

func (t tuiLogs) Event(ctx context.Context, seq uint64) (string, error) {
	var d logapi.EventDetail
	if err := t.lb.do(ctx, http.MethodGet, "/api/events/"+strconv.FormatUint(seq, 10), nil, &d); err != nil {
		return "", err
	}
	var b bytes.Buffer
	err := logsOut{w: &b, format: "text", loc: time.Local}.detail(d)
	return b.String(), err
}

func (t tuiLogs) Timeline(ctx context.Context, id string) (string, []event.Event, error) {
	var tl logapi.Timeline
	if err := t.lb.do(ctx, http.MethodGet, "/api/events/timeline/"+url.PathEscape(id), nil, &tl); err != nil {
		return "", nil, err
	}
	return timelineHead(tl), tl.Events, nil
}
