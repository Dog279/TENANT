package logs

import (
	"context"
	"time"

	"tenant/internal/logs/event"
	"tenant/internal/logs/store"
)

// What the logging API (internal/logs/logapi) and tenant logs reach through
// a hub: its store, its retention settings, and the doors for events from
// outside the binary.

// Store is the attached store, nil when there is none.
func (h *Hub) Store() *store.Store { return h.store() }

// Budget is the retention configuration in force: Options.Budget's, else
// store.DefaultBudget.
func (h *Hub) Budget() store.Budget {
	if h.opts.Budget != nil {
		return h.opts.Budget()
	}
	return store.DefaultBudget()
}

// Reload rereads what this process's writes obey (each log's policy and
// debug switch) now, instead of at the next housekeeper tick, so a change
// made here applies at once.
func (h *Hub) Reload(ctx context.Context) {
	if st := h.store(); st != nil {
		h.refresh(ctx, st)
	}
}

// Emit files e as the event emitter does: into the pipeline once a store is
// attached, else as a v1 line.
func (h *Hub) Emit(ctx context.Context, e *event.Event) { hubEmitter{h}.Emit(ctx, e) }

// External keeps only what a writer outside the binary may set
// (docs/design/logging-v2.md §10.4): the log, message, level, time and data,
// and the trace and activity it runs under, which it has from its
// environment. It becomes an uncatalogued event of its log's own provider,
// with keyword external. Whatever else it claimed (an event ID, a task, the
// session, approval or tool call it belongs to, an actor, a surface, a
// channel, a process) is dropped: nothing can vouch for it.
func External(e *event.Event) {
	*e = event.Event{Log: e.Log, Provider: e.Log, Time: e.Time, Level: e.Level, Msg: e.Msg, Data: e.Data,
		Keywords: []string{KeywordExternal}, Corr: event.Correlation{TraceID: e.Corr.TraceID, ActivityID: e.Corr.ActivityID}}
}

// LogExternal is Log for a line from outside the binary (the v1 logging API,
// tenant log): with a store attached, it is filed as External.
func (h *Hub) LogExternal(e Entry) {
	p := h.pipe.Load()
	if p == nil || e.Level < h.opts.MinLevel && !h.debugging(e.Source) {
		h.Log(e)
		return
	}
	e.Source = cleanSource(e.Source)
	if e.At.IsZero() {
		e.At = h.now()
	}
	ev := EventFromEntry(e)
	External(&ev)
	if !p.enqueue(context.Background(), &ev) {
		h.Log(e)
	}
}

// debugging reports whether log's timed debug switch is on here.
func (h *Hub) debugging(log string) bool {
	g := h.hk.gate.Load()
	return g != nil && g.debugging(log)
}

// debugAny reports whether any log's debug switch may be on, so a verbose
// record is worth building.
func (h *Hub) debugAny() bool {
	g := h.hk.gate.Load()
	return g != nil && len(g.debug) > 0
}

func (g *gate) debugging(log string) bool {
	until, ok := g.debug[log]
	return ok && time.Now().Before(until)
}
