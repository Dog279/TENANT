package logs

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"tenant/internal/logs/event"
	"tenant/internal/runtrace"
)

var (
	testProvider = event.NewProvider("logstest", "logstest")
	testOp       = testProvider.Event(1001, event.Info, "Sync", "Sync {name}", "name", "payload").Sensitive("payload")
	testLost     = testProvider.Event(3001, event.Critical, "Store", "Store {name} lost", "name")
	testDiag     = testProvider.Event(9001, event.Info, "Sync", "Sync {name} body", "name", "body").
			Sensitive("body").Keywords("payload").Diagnostic()
)

// A v1 line that names a catalogued event (agent.EntryFor writes them) reads
// back with what the line can't carry: its channel, keywords and which fields
// are sensitive.
func TestEventFromEntryTakesTheDefinitionsChannelAndSensitivity(t *testing.T) {
	e := EventFromEntry(Entry{Source: "turns", Msg: "Sync a body", Fields: []Field{
		{"event", "logstest/9001"}, {"name", "a"}, {"body", "secret"}, {"other", "x"}}})
	if e.Channel != event.Diagnostic || e.Task != "Sync" || !reflect.DeepEqual(e.Keywords, []string{"payload"}) || e.Log != "turns" {
		t.Errorf("identity: %+v", e)
	}
	want := []event.Field{{Key: "name", Value: "a"}, {Key: "body", Value: "secret", Sensitive: true}, {Key: "other", Value: "x"}}
	if !reflect.DeepEqual(e.Data, want) {
		t.Errorf("Data = %+v, want %+v", e.Data, want)
	}
}

// Install makes the hub the event emitter: events land in the hub as v1
// lines, correlated with the turn and activity in ctx, sensitive values
// masked.
func TestEventsFileIntoTheInstalledHub(t *testing.T) {
	h := NewMemory(20)
	Install(h)
	defer Install(nil)

	trace := runtrace.NewID()
	ctx := runtrace.WithSpan(context.Background(), runtrace.NewSpan(trace))
	ctx, act := event.Start(ctx, testOp, "name", "calendar")
	testOp.Log(ctx, "name", "item", "payload", "hunter2-secret")
	testLost.Log(ctx, "name", "runs.db")
	if err := testOp.Audit(ctx, "name", "audited"); err != nil {
		t.Fatalf("Audit = %v", err)
	}
	act.End(nil)

	all, _ := h.Snapshot()
	if len(all) != 5 {
		t.Fatalf("got %d entries, want 5: %+v", len(all), all)
	}
	start, logged, lost, stop := all[0], all[1], all[2], all[4]
	activity := event.ActivityFromContext(ctx)
	if start.Source != "logstest" || start.Msg != "Sync calendar" || start.Field("event") != "logstest/1001" ||
		start.Field("task") != "Sync" || start.Field("opcode") != "start" || start.Field("trace") != trace ||
		start.Field("activity") != activity || start.Field("related") != trace {
		t.Errorf("start line: %s", start.Line())
	}
	if logged.Field("payload") != event.Masked || logged.Field("activity") != activity || logged.Field("related") != "" {
		t.Errorf("logged line: %s", logged.Line())
	}
	if lost.Level != LevelCritical || lost.Field("severity") != "critical" || !strings.HasPrefix(lost.Line()[24:], "ERROR [logstest] Store runs.db lost") {
		t.Errorf("critical line: %s", lost.Line())
	}
	if stop.Field("opcode") != "stop" || stop.Field("duration") == "" {
		t.Errorf("stop line: %s", stop.Line())
	}
	for _, e := range all {
		if strings.Contains(e.Line(), "hunter2") {
			t.Errorf("a sensitive value reached the log: %s", e.Line())
		}
	}

	// The emitter fills in the process.
	var got event.Event
	em := hubEmitter{h}
	em.Emit(ctx, &got)
	host, _ := os.Hostname()
	if got.Proc.PID != os.Getpid() || got.Proc.BootID != runtrace.BootID() || got.Proc.Host != host || got.Proc.Version == "" {
		t.Errorf("Proc = %+v", got.Proc)
	}

	Install(nil)
	if err := testOp.Audit(ctx, "name", "x"); !errors.Is(err, event.ErrNoEmitter) {
		t.Errorf("Audit after Install(nil) = %v, want ErrNoEmitter", err)
	}
}

// v1 entries (Hub.Log, the slog handler, agent.EntryFor) read as events and
// back with nothing lost, and an event survives its v1 line except for its
// sensitive values.
func TestEntriesAndEventsRoundTrip(t *testing.T) {
	at := time.Date(2026, 9, 27, 14, 3, 11, 123e6, time.Local)
	trace := runtrace.NewID()
	for _, v1 := range []Entry{
		{At: at, Source: "turns", Level: slog.LevelWarn, Msg: "tool retry web_search",
			Fields: []Field{{"trace", trace}, {"ev", "retry"}, {"tool", "web_search"}}},
		{At: at, Source: "system", Level: LevelCritical, Msg: "store lost",
			Fields: []Field{{"severity", "critical"}, {"event", "system/3"}, {"err", "disk"}}},
		{At: at, Source: "sandbox", Level: slog.LevelDebug, Msg: "ran", Fields: []Field{{"surface", "discord"}, {"event", "not/an/id"}}},
	} {
		e := EventFromEntry(v1)
		back := EntryFromEvent(e)
		if !back.At.Equal(at) || back.Source != v1.Source || back.Level != v1.Level || back.Msg != v1.Msg || !reflect.DeepEqual(back.Fields, v1.Fields) {
			t.Errorf("round trip of %s\ngave     %s", v1.Line(), back.Line())
		}
	}
	e := EventFromEntry(Entry{At: at, Source: "turns", Level: slog.LevelWarn, Msg: "m", Fields: []Field{{"ev", "retry"}, {"trace", trace}}})
	if e.Log != "turns" || e.Provider != "turns" || e.ID != 0 || e.Level != event.Warning || e.Corr.TraceID != trace ||
		!reflect.DeepEqual(e.Data, []event.Field{{Key: "ev", Value: "retry"}}) {
		t.Errorf("EventFromEntry = %+v", e)
	}

	ev := event.Event{
		Log: "security", Provider: "tenant.approval", ID: 4103, Time: at.UTC(), Level: event.Critical,
		Task: "Approval", Opcode: event.OpFail, Msg: "Approval ap-3 denied: os_exec",
		Data: []event.Field{{Key: "action", Value: "os_exec"}, {Key: "args", Value: "rm -rf ~", Sensitive: true}},
		Corr: event.Correlation{TraceID: trace, ActivityID: runtrace.NewID(), RelatedID: trace,
			SessionID: "s1", ApprovalID: "ap-3", ToolCallID: "call_1"},
		Actor: "operator", Surface: "discord",
	}
	en := EntryFromEvent(ev)
	if en.Source != "security" || en.Level != LevelCritical || en.Field("event") != "tenant.approval/4103" || en.Field("args") != event.Masked {
		t.Errorf("EntryFromEvent: %s", en.Line())
	}
	want := ev
	want.Data = []event.Field{ev.Data[0], {Key: "args", Value: event.Masked, Sensitive: true}}
	if got := EventFromEntry(en); !reflect.DeepEqual(got, want) {
		t.Errorf("event round trip\n got %+v\nwant %+v", got, want)
	}
	// Its line on disk says ERROR; severity=critical keeps it critical.
	if parsed, ok := ParseLine(en.Line()); !ok || parsed.Level != slog.LevelError {
		t.Fatalf("ParseLine(%q) = %+v, %v", en.Line(), parsed, ok)
	} else if got := EventFromEntry(parsed); got.Level != event.Critical || got.ID != 4103 || got.Corr != ev.Corr {
		t.Errorf("the mirror line read back: %+v", got)
	}
	if event.Critical.Slog() != LevelCritical {
		t.Error("event.Critical and LevelCritical disagree")
	}
}
