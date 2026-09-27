package logs

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"

	"tenant/internal/logs/event"
	"tenant/internal/logs/store"
)

// A log's timed debug switch lets its verbose events past the INFO floor
// (Log, the slog handler and the emitter), and past its own minimum level,
// once Reload reads it; other logs' verbose events stay out.
func TestDebugSwitchLetsOneLogsVerboseEventsThrough(t *testing.T) {
	h, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := attach(t, h)
	ctx := context.Background()
	if err := r.SetDebug(ctx, "discord", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := r.SetPolicy(ctx, "discord", store.Policy{MinLevel: event.Warning, OnFull: store.Overwrite}); err != nil {
		t.Fatal(err)
	}
	h.Logf("discord", slog.LevelDebug, "before reload") // the switch isn't read yet
	h.Reload(ctx)
	h.Logf("discord", slog.LevelDebug, "via Log")
	h.Logger("discord").Debug("via slog")
	h.Emit(ctx, &event.Event{Log: "discord", Provider: "discord", Level: event.Verbose, Msg: "via the emitter"})
	h.Logf("cron", slog.LevelDebug, "another log")
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range all(t, r) {
		if e.Log != "logbook" { // Reload may note low disk space
			got = append(got, e.Log+": "+e.Msg)
		}
	}
	if want := []string{"discord: via Log", "discord: via slog", "discord: via the emitter"}; !slices.Equal(got, want) {
		t.Errorf("written = %q, want %q", got, want)
	}
}

// An outside line keeps its log, level, message, data, trace and activity,
// and nothing it can't vouch for; it is marked external.
func TestLogExternal(t *testing.T) {
	h, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := attach(t, h)
	h.LogExternal(Entry{Source: "deploy", Level: slog.LevelWarn, Msg: "disk at 91%", Fields: []Field{
		{Key: "event", Value: "discord/2001"}, {Key: "approval", Value: "ap-1"}, {Key: "actor", Value: "operator"},
		{Key: "trace", Value: "0123456789abcdef0123456789abcdef"}, {Key: "host", Value: "build-box"}}})
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	evs := all(t, r)
	if len(evs) != 1 {
		t.Fatalf("got %d events", len(evs))
	}
	e := evs[0]
	if e.Log != "ext.deploy" || e.Provider != "ext.deploy" || e.ID != 0 || e.Corr.ApprovalID != "" || e.Actor != "" ||
		e.Corr.TraceID != "0123456789abcdef0123456789abcdef" || !slices.Equal(e.Keywords, []string{"external"}) ||
		len(e.Data) != 1 || e.Data[0].Key != "host" || e.Level != event.Warning {
		t.Errorf("event = %+v", e)
	}
}
