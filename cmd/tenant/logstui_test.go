package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tenant/internal/control"
	"tenant/internal/logs"
	"tenant/internal/logs/event"
	"tenant/internal/logs/store"
)

// The TUI's /logs reads the hub's store through the log book's API: the
// newest events oldest first, a parse error, a masked detail with its Help,
// a timeline and a live tail that ends with its context.
func TestTUILogsControl(t *testing.T) {
	data, _, st := logsBook(t)
	root := filepath.Join(data, "logs")
	h, err := logs.Open(root, logs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	own, err := store.Open(filepath.Join(root, "logbook.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h.AttachStore(own)
	defer h.Close()
	const trace = "0123456789abcdef0123456789abcdef"
	if err := st.Commit(context.Background(), []event.Event{{Log: "turns", Provider: "turns", Level: event.Info, Msg: "turn one",
		Corr: event.Correlation{TraceID: trace}}}); err != nil {
		t.Fatal(err)
	}
	var lc control.LogsControl = newTUILogs(h, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	evs, cursor, err := lc.Query(ctx, "log:cron", 10)
	if err != nil || len(evs) != 2 || evs[0].Msg != "job fired" || evs[1].Msg != "job slow" || cursor == "" {
		t.Fatalf("query = %v %q %v", evs, cursor, err)
	}
	if _, _, err := lc.Query(ctx, "bogus:1", 10); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Errorf("a bad filter = %v", err)
	}
	text, err := lc.Event(ctx, 2)
	if err != nil || !strings.Contains(text, "«sensitive»") || strings.Contains(text, "hunter2") || !strings.Contains(text, "What to do") {
		t.Errorf("event = %v\n%s", err, text)
	}
	head, tl, err := lc.Timeline(ctx, trace)
	if err != nil || len(tl) != 1 || tl[0].Msg != "turn one" || !strings.Contains(head, "runs.db") {
		t.Errorf("timeline = %q %v %v", head, tl, err)
	}

	live := lc.Follow(ctx, "log:cron", cursor)
	if err := st.Commit(context.Background(), []event.Event{{Log: "system", Provider: "system", Level: event.Info, Msg: "not cron"},
		{Log: "cron", Provider: "cron", Level: event.Info, Msg: "live one"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-live:
		if e.Msg != "live one" {
			t.Errorf("follow sent %q", e.Msg)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("follow sent nothing")
	}
	cancel()
	for range live { // closes once the context is done
	}
}
