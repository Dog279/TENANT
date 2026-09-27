package logs

import (
	"context"
	"sync"

	"tenant/internal/logs/event"
	"tenant/internal/logs/store"
)

// AuditEmitter is the event emitter of a one-shot command (tenant backup,
// tenant privacy, tenant route, …). Its Log keeps the path events take with
// no emitter: warnings and worse to slog.Default, which the command prints
// and files. Its Audit attaches the store to h on first use (open says how),
// so the command's Security records reach logbook.db and the chain as
// serve's do, and returns once the record is committed.
func AuditEmitter(h *Hub, open func() (*store.Store, error)) event.Emitter {
	return &auditEmitter{h: h, open: open}
}

type auditEmitter struct {
	h    *Hub
	open func() (*store.Store, error)
	once sync.Once
	err  error
}

func (a *auditEmitter) Emit(ctx context.Context, e *event.Event) { event.Fallback(ctx, e) }

func (a *auditEmitter) Audit(ctx context.Context, e *event.Event) error {
	a.once.Do(func() {
		st, err := a.open()
		if err != nil {
			a.err = err
			return
		}
		a.h.AttachStore(st)
	})
	if a.err != nil {
		return a.err
	}
	return hubEmitter{a.h}.Audit(ctx, e)
}
