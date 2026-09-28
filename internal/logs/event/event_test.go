package event_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"tenant/internal/logs/event"
)

var (
	testP = event.NewProvider("eventtest", "eventtest")
	sent  = testP.Event(1001, event.Info, "Send", "Sent {n} messages to {to}", "n", "to", "body").
		Sensitive("body").Keywords("network").Doc("m", "c", "a")
	run     = testP.Event(1002, event.Info, "Job run", "Job {job}", "job").Diagnostic()
	dropped = testP.Event(2001, event.Warning, "Send", "Dropped {n} messages", "n", "body").Sensitive("body")

	badP  = event.NewProvider("eventtest.bad", "eventtest")
	_     = badP.Event(1, event.Info, "T", "ok {a}", "a")
	other = badP.Event(2, event.Info, "T", "other")
)

// capture is an emitter that keeps what it is given.
type capture struct {
	events   []*event.Event
	auditErr error
}

func (c *capture) Emit(_ context.Context, e *event.Event) { c.events = append(c.events, e) }
func (c *capture) Audit(_ context.Context, e *event.Event) error {
	c.events = append(c.events, e)
	return c.auditErr
}

func install(t *testing.T) *capture {
	t.Helper()
	c := &capture{}
	event.Install(c)
	t.Cleanup(func() { event.Install(nil) })
	return c
}

func TestLogBuildsTheEventItsDefinitionDescribes(t *testing.T) {
	c := install(t)
	at := time.Date(2026, 9, 27, 12, 0, 0, 0, time.FixedZone("PDT", -7*3600))
	type exit int
	sent.Log(context.Background(), "n", 3, "to", "general", "body", "hi there",
		"i32", int32(-2), "u64", uint64(math.MaxUint64), "f32", float32(0.5), "nan", math.NaN(), "code", exit(7),
		"d", 2*time.Second, "at", at, "err", errors.New("boom"), "lvl", event.Warning,
		"list", []string{"a"}, "raw", json.RawMessage(`{"k":1}`), "odd")
	e := c.events[0]
	if e.Log != "eventtest" || e.Provider != "eventtest" || e.ID != 1001 || e.Level != event.Info ||
		e.Task != "Send" || e.Opcode != "" || e.Channel != event.Operational || !reflect.DeepEqual(e.Keywords, []string{"network"}) {
		t.Fatalf("identity: %+v", e)
	}
	if e.Msg != "Sent 3 messages to general" {
		t.Errorf("Msg = %q", e.Msg)
	}
	if e.Time.Location() != time.UTC || time.Since(e.Time) > time.Minute {
		t.Errorf("Time = %v, want now in UTC", e.Time)
	}
	want := []event.Field{
		{Key: "n", Value: int64(3)}, {Key: "to", Value: "general"}, {Key: "body", Value: "hi there", Sensitive: true},
		{Key: "i32", Value: int64(-2)}, {Key: "u64", Value: "18446744073709551615"}, {Key: "f32", Value: 0.5}, {Key: "nan", Value: "NaN"},
		{Key: "code", Value: int64(7)}, {Key: "d", Value: 2 * time.Second}, {Key: "at", Value: at.UTC()},
		{Key: "err", Value: "boom"}, {Key: "lvl", Value: "warning"}, {Key: "list", Value: json.RawMessage(`["a"]`)},
		{Key: "raw", Value: json.RawMessage(`{"k":1}`)}, {Key: "!BADKEY", Value: "odd"},
	}
	if !reflect.DeepEqual(e.Data, want) {
		t.Errorf("Data =\n%#v\nwant\n%#v", e.Data, want)
	}

	// A missing field stays visible in the message; Audit reports the emitter's error.
	c.auditErr = errors.New("disk full")
	if err := sent.Audit(context.Background(), "n", 1); err != c.auditErr {
		t.Errorf("Audit = %v, want the emitter's error", err)
	}
	if got := c.events[1].Msg; got != "Sent 1 messages to {to}" {
		t.Errorf("Msg with a field missing = %q", got)
	}
}

func TestActivitiesNestAndEnd(t *testing.T) {
	c := install(t)
	ctx, outer := event.Start(context.Background(), run, "job", "outer")
	func() (err error) {
		ctx, act := event.Start(ctx, run, "job", "inner")
		defer act.End(&err)
		sent.Log(ctx, "n", 1, "to", "x")
		return errors.New("boom")
	}()
	outer.End(nil)

	if len(c.events) != 5 {
		t.Fatalf("got %d events, want 5", len(c.events))
	}
	oStart, iStart, logged, iFail, oStop := c.events[0], c.events[1], c.events[2], c.events[3], c.events[4]
	hex := regexp.MustCompile(`^[0-9a-f]{32}$`)
	a, b := oStart.Corr.ActivityID, iStart.Corr.ActivityID
	if !hex.MatchString(a) || !hex.MatchString(b) || a == b || event.ActivityFromContext(ctx) != a {
		t.Fatalf("activity IDs %q, %q (ctx has %q)", a, b, event.ActivityFromContext(ctx))
	}
	if oStart.Opcode != event.OpStart || oStart.Corr.RelatedID != "" || oStart.Msg != "Job outer" || oStart.Channel != event.Diagnostic {
		t.Errorf("outer start: %+v", oStart)
	}
	if iStart.Opcode != event.OpStart || iStart.Corr.RelatedID != a {
		t.Errorf("the inner start should name the outer activity as related: %+v", iStart.Corr)
	}
	if logged.Corr.ActivityID != b || logged.Opcode != "" {
		t.Errorf("an event under the inner activity: %+v", logged.Corr)
	}
	if iFail.Opcode != event.OpFail || iFail.Level != event.Error || iFail.Corr.ActivityID != b || iFail.Msg != "Job inner" ||
		field(iFail, "error") != "boom" || field(iFail, "duration") == nil {
		t.Errorf("inner fail: %+v", iFail)
	}
	if oStop.Opcode != event.OpStop || oStop.Level != event.Info || oStop.Corr.ActivityID != a || oStop.Corr.RelatedID != "" {
		t.Errorf("outer stop: %+v", oStop)
	}
}

// WithLog files one provider's events in another log; the other providers'
// events, and a bad log name, leave theirs alone. Lookup finds a definition.
func TestWithLogAndLookup(t *testing.T) {
	c := install(t)
	ctx := event.WithLog(context.Background(), testP, "elsewhere")
	sent.Log(ctx, "n", 1)
	other.Log(ctx)
	event.Start(ctx, run, "job", "j")
	sent.Log(event.WithLog(context.Background(), testP, "Bad Name"), "n", 1)
	var logs []string
	for _, e := range c.events {
		logs = append(logs, e.Log)
	}
	if want := []string{"elsewhere", "eventtest", "elsewhere", "eventtest"}; !reflect.DeepEqual(logs, want) {
		t.Errorf("logs = %q, want %q", logs, want)
	}
	if d, ok := event.Lookup("eventtest", 1001); !ok || d != sent {
		t.Errorf("Lookup(eventtest, 1001) = %v, %v", d, ok)
	}
	for _, miss := range []struct {
		p  string
		id uint32
	}{{"eventtest", 9}, {"nope", 1001}, {"eventtest", 0}} {
		if _, ok := event.Lookup(miss.p, miss.id); ok {
			t.Errorf("Lookup(%s, %d) found a definition", miss.p, miss.id)
		}
	}
}

func TestEndWithoutAnErrorStops(t *testing.T) {
	c := install(t)
	_, act := event.Start(context.Background(), run, "job", "j")
	var err error
	act.End(&err)
	act.End(nil)
	for _, e := range c.events[1:] {
		d, ok := field(e, "duration").(time.Duration)
		if e.Opcode != event.OpStop || e.Level != event.Info || !ok || d < 0 || field(e, "error") != nil {
			t.Errorf("stop: %+v", e)
		}
	}
}

func field(e *event.Event, key string) any {
	for _, f := range e.Data {
		if f.Key == key {
			return f.Value
		}
	}
	return nil
}

func TestBeforeInstallWarningsGoToSlogAndAuditFails(t *testing.T) {
	event.Install(nil)
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	sent.Log(context.Background(), "n", 1, "to", "x")
	dropped.Log(context.Background(), "n", 2, "body", "secret text")
	out := buf.String()
	if strings.Contains(out, "Sent 1") || !strings.Contains(out, "Dropped 2 messages") ||
		!strings.Contains(out, "event=eventtest/2001") || strings.Contains(out, "secret text") || !strings.Contains(out, event.Masked) {
		t.Errorf("fallback output:\n%s", out)
	}
	if err := sent.Audit(context.Background()); !errors.Is(err, event.ErrNoEmitter) {
		t.Errorf("Audit before Install = %v, want ErrNoEmitter", err)
	}
}

func TestLevels(t *testing.T) {
	for l := event.Critical; l <= event.Verbose; l++ {
		if got, ok := event.ParseLevel(strings.ToUpper(l.String())); !ok || got != l {
			t.Errorf("ParseLevel(%q) = %v, %v", l, got, ok)
		}
		if got := event.LevelFromSlog(l.Slog()); got != l {
			t.Errorf("LevelFromSlog(%v.Slog()) = %v", l, got)
		}
	}
	if event.Critical.Slog() != slog.LevelError+4 || event.Verbose.Slog() != slog.LevelDebug {
		t.Error("Critical is slog.LevelError+4 and Verbose is slog.LevelDebug")
	}
	if event.LevelFromSlog(slog.LevelWarn+2) != event.Warning || event.LevelFromSlog(slog.LevelDebug-4) != event.Verbose {
		t.Error("in-between slog levels bucket down")
	}
	if l, ok := event.ParseLevel("warn"); !ok || l != event.Warning {
		t.Error("warn is warning")
	}
	if _, ok := event.ParseLevel("loud"); ok {
		t.Error("an unknown level parses")
	}
}

func TestBadDefinitionsPanicWhenTheyRegister(t *testing.T) {
	p := badP
	for name, f := range map[string]func(){
		"a repeated ID":             func() { p.Event(1, event.Info, "T", "x") },
		"ID 0":                      func() { p.Event(0, event.Info, "T", "x") },
		"no level":                  func() { p.Event(2, 0, "T", "x") },
		"an undeclared placeholder": func() { p.Event(3, event.Info, "T", "{a} {b}", "a") },
		"a sensitive placeholder":   func() { p.Event(4, event.Info, "T", "{a}", "a").Sensitive("a") },
		"an undeclared sensitive":   func() { p.Event(5, event.Info, "T", "x", "a").Sensitive("b") },
		"a field declared twice":    func() { p.Event(6, event.Info, "T", "x", "a", "a") },
		"a doc missing its cause":   func() { p.Event(7, event.Info, "T", "x").Doc("m", "", "a") },
		"a provider named twice":    func() { event.NewProvider("eventtest.bad", "x") },
		"a bad log name":            func() { event.NewProvider("eventtest.log", "Bad Log") },
		"a Windows device log name": func() { event.NewProvider("eventtest.dev", "nul.txt") },
		"a device provider name":    func() { event.NewProvider("com1", "eventtest.dev") },
		"a log name ending in dot":  func() { event.NewProvider("eventtest.dot", "x.") },
	} {
		if !panics(f) {
			t.Errorf("%s did not panic", name)
		}
	}
	// A definition that failed its checks is never left registered wrong.
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
}

func panics(f func()) (did bool) {
	defer func() { did = recover() != nil }()
	f()
	return false
}
