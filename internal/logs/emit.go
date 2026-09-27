package logs

import (
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"

	"tenant/internal/logs/event"
	"tenant/internal/runtrace"
	"tenant/internal/version"
)

// hubEmitter files catalogued events (internal/logs/event) through a hub:
// into its pipeline once a store is attached (pipeline.go), else as v1 lines.
type hubEmitter struct{ h *Hub }

// installEmitter makes h the event emitter; a nil hub uninstalls it.
func installEmitter(h *Hub) {
	if h == nil {
		event.Install(nil)
		return
	}
	event.Install(hubEmitter{h})
}

func (em hubEmitter) Emit(ctx context.Context, e *event.Event) {
	if e.Level.Slog() < em.h.opts.MinLevel && !em.h.debugging(e.Log) {
		return
	}
	enrich(ctx, e)
	if p := em.h.pipe.Load(); p != nil && p.enqueue(ctx, e) {
		return
	}
	em.h.Log(EntryFromEvent(*e))
}

// Audit returns once the record is committed to the store, with the commit's
// error. ponytail: with no store attached it returns once the hub has the
// line, as Emit does, and can't report a failed write.
func (em hubEmitter) Audit(ctx context.Context, e *event.Event) error {
	enrich(ctx, e)
	if p := em.h.pipe.Load(); p != nil {
		if err := p.audit(ctx, e); !errors.Is(err, errClosed) {
			return err
		}
	}
	em.h.Log(EntryFromEvent(*e))
	return nil
}

// process is this process, as the events it writes name it.
var process = sync.OnceValue(func() event.Process {
	host, _ := os.Hostname()
	return event.Process{PID: os.Getpid(), BootID: runtrace.BootID(), Host: host, Version: version.Version}
})

// enrich adds what only the log book knows: the process, the turn in ctx,
// and that turn as the parent of an activity started outside any other.
func enrich(ctx context.Context, e *event.Event) {
	if e.Proc.PID == 0 {
		e.Proc = process()
	}
	if e.Corr.TraceID == "" {
		e.Corr.TraceID = runtrace.IDFromContext(ctx)
	}
	if e.Opcode == event.OpStart && e.Corr.RelatedID == "" {
		e.Corr.RelatedID = e.Corr.TraceID
	}
}

// attr is an event attribute a v1 entry carries as a field.
type attr struct {
	key string
	val *string
}

// attrs are e's attributes as v1 fields, in the order EntryFromEvent writes
// them.
func attrs(e *event.Event) []attr {
	return []attr{
		{"task", &e.Task}, {"opcode", &e.Opcode},
		{"trace", &e.Corr.TraceID}, {"activity", &e.Corr.ActivityID}, {"related", &e.Corr.RelatedID},
		{"session", &e.Corr.SessionID}, {"approval", &e.Corr.ApprovalID}, {"tool_call", &e.Corr.ToolCallID},
		{"actor", &e.Actor}, {"surface", &e.Surface},
	}
}

// EntryFromEvent renders e as a v1 entry, the text mirror's line: its log is
// the source, a critical event is ERROR with severity=critical, and its ID
// (event=<provider>/<id>), task, opcode, correlation IDs, actor and surface
// are fields ahead of its data. A sensitive field is written as «sensitive»,
// never its value.
func EntryFromEvent(e event.Event) Entry {
	en := Entry{At: e.Time.Local(), Source: e.Log, Level: e.Level.Slog(), Msg: e.Msg, Fields: make([]Field, 0, len(e.Data)+4)}
	if e.Level == event.Critical {
		en.Fields = append(en.Fields, Field{Key: "severity", Value: "critical"})
	}
	if e.ID != 0 {
		en.Fields = append(en.Fields, Field{Key: "event", Value: e.Provider + "/" + strconv.FormatUint(uint64(e.ID), 10)})
	}
	for _, a := range attrs(&e) {
		if *a.val != "" {
			en.Fields = append(en.Fields, Field{Key: a.key, Value: *a.val})
		}
	}
	for _, f := range e.Data {
		v := f.Text()
		if f.Sensitive {
			v = event.Masked
		}
		en.Fields = append(en.Fields, Field{Key: f.Key, Value: v})
	}
	return en
}

// EventFromEntry reads a v1 entry (from Hub.Log, the slog handler,
// agent.EntryFor, or a parsed line) as an event. Its source is its log and
// provider, and it is uncatalogued (ID 0) unless it carries event=. The
// fields EntryFromEvent writes ahead of the data become the event's own
// attributes again; the rest are its data, as strings.
func EventFromEntry(en Entry) event.Event {
	src := en.Source
	if src == "" {
		src = DefaultSource
	}
	e := event.Event{Log: src, Provider: src, Time: en.At.UTC(), Level: event.LevelFromSlog(en.Level), Msg: en.Msg}
	as := attrs(&e)
	for _, f := range en.Fields {
		switch {
		case f.Key == "severity" && f.Value == "critical":
			e.Level = event.Critical
			continue
		case f.Key == "event":
			if p, id, ok := parseEventID(f.Value); ok {
				e.Provider, e.ID = p, id
				continue
			}
		default:
			if i := slices.IndexFunc(as, func(a attr) bool { return a.key == f.Key }); i >= 0 {
				*as[i].val = f.Value
				continue
			}
		}
		e.Data = append(e.Data, event.Field{Key: f.Key, Value: f.Value, Sensitive: f.Value == event.Masked})
	}
	// A catalogued entry (agent.EntryFor's) takes what its v1 line doesn't
	// carry from its definition: its channel, its keywords and which fields
	// are sensitive.
	if e.ID != 0 {
		if d, ok := event.Lookup(e.Provider, e.ID); ok {
			s := d.Spec()
			e.Channel, e.Keywords = s.Channel, s.Keywords
			if e.Task == "" {
				e.Task = s.Task
			}
			for i := range e.Data {
				e.Data[i].Sensitive = e.Data[i].Sensitive || slices.Contains(s.Sensitive, e.Data[i].Key)
			}
		}
	}
	return e
}

// parseEventID splits event=<provider>/<id>.
func parseEventID(s string) (string, uint32, bool) {
	i := strings.LastIndexByte(s, '/')
	if i <= 0 {
		return "", 0, false
	}
	id, err := strconv.ParseUint(s[i+1:], 10, 32)
	if err != nil || id == 0 {
		return "", 0, false
	}
	return s[:i], uint32(id), true
}
