package logs

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"sync"
	"time"

	"tenant/internal/logs/event"
	"tenant/internal/logs/store"
)

// The write path once a store is attached (docs/design/logging-v2.md §12.1):
// every event this process writes joins one bounded queue, and one writer
// goroutine takes a batch every batchEvery or batchMax events and runs it
// through the pipeline: scrub, truncate, mirror to the v1 text files, commit
// to logbook.db, then feed the v1 ring and wake its readers.
const (
	queueCap   = 64 << 10
	batchMax   = 512
	batchEvery = 100 * time.Millisecond
	closeWait  = 2 * time.Second // the final flush's bound
)

var errClosed = errors.New("logs: the log book is closed")

type pipeline struct {
	h      *Hub
	st     *store.Store
	q      chan *event.Event
	urgent chan auditReq

	mu     sync.RWMutex // senders hold it shared; close takes it to stop them
	closed bool

	stop, done chan struct{}
	ctx        context.Context // the writer's; cancelled when the final flush runs out of time
	cancel     context.CancelFunc

	dmu     sync.Mutex
	dropped map[string]uint64 // per log: events the full queue had no room for

	failing bool // the last commit failed (writer goroutine only)
}

// auditReq is a Security record waiting for its commit.
type auditReq struct {
	e    *event.Event
	done chan error
}

// AttachStore makes st the log book's system of record. From then on, Log
// and the event emitter queue events for the pipeline instead of writing the
// text files themselves; the files, the ring and the Activity feed carry on,
// fed by the pipeline. The hub owns st: Close flushes the queue and closes it.
func (h *Hub) AttachStore(st *store.Store) {
	p := newPipeline(h, st)
	go p.run()
	if old := h.pipe.Swap(p); old != nil {
		old.close()
	}
}

func newPipeline(h *Hub, st *store.Store) *pipeline {
	ctx, cancel := context.WithCancel(context.Background())
	return &pipeline{h: h, st: st, q: make(chan *event.Event, queueCap), urgent: make(chan auditReq),
		stop: make(chan struct{}), done: make(chan struct{}), ctx: ctx, cancel: cancel, dropped: map[string]uint64{}}
}

// Dropped counts, per log, the events the pipeline had no room for: a full
// queue, or low disk (housekeeper.go). Nil when no store is attached.
func (h *Hub) Dropped() map[string]uint64 {
	p := h.pipe.Load()
	if p == nil {
		return nil
	}
	p.dmu.Lock()
	defer p.dmu.Unlock()
	return maps.Clone(p.dropped)
}

// logEntry is Log for a hub with a store attached: the entry, cleaned as Log
// cleans it, joins the queue as an event. False once the pipeline is closed.
func (p *pipeline) logEntry(e Entry) bool {
	e.Source = cleanSource(e.Source)
	if e.At.IsZero() {
		e.At = p.h.now()
	}
	ev := EventFromEntry(e)
	return p.enqueue(context.Background(), &ev)
}

func cleanSource(s string) string {
	if ValidSource(s) {
		return s
	}
	if n := NormalizeSource(s); n != "" {
		return n
	}
	return DefaultSource
}

// enqueue hands e to the writer, unless its log's policy or low disk keeps
// it out (admit). When the queue is full, an operational event waits up to a
// millisecond and a diagnostic one not at all, then it is dropped and
// counted. A Security record is never queued or kept out: it forces a
// commit. False only once the pipeline is closed.
func (p *pipeline) enqueue(ctx context.Context, e *event.Event) bool {
	if e.Log == store.SecurityLog {
		return !errors.Is(p.audit(ctx, e), errClosed)
	}
	if !p.admit(e) {
		return true
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return false
	}
	select {
	case p.q <- e:
		return true
	default:
	}
	if e.Channel == event.Operational {
		t := time.NewTimer(time.Millisecond)
		defer t.Stop()
		select {
		case p.q <- e:
			return true
		case <-t.C:
		}
	}
	p.dmu.Lock()
	p.dropped[e.Log]++
	p.dmu.Unlock()
	return true
}

// audit puts e into the batch being built, has that batch committed now, and
// returns the commit's error.
func (p *pipeline) audit(ctx context.Context, e *event.Event) error {
	r := auditReq{e: e, done: make(chan error, 1)}
	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		return errClosed
	}
	select {
	case p.urgent <- r:
	case <-ctx.Done():
		p.mu.RUnlock()
		return ctx.Err()
	}
	p.mu.RUnlock()
	select {
	case err := <-r.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run is the writer goroutine.
func (p *pipeline) run() {
	defer close(p.done)
	t := time.NewTicker(batchEvery)
	defer t.Stop()
	batch := make([]event.Event, 0, batchMax)
	for {
		select {
		case e := <-p.q:
			if batch = append(batch, *e); len(batch) < batchMax {
				continue
			}
		case r := <-p.urgent:
			batch = append(p.take(batch), *r.e) // what was queued before it goes first
			r.done <- p.flush(batch)
			batch = batch[:0]
			continue
		case <-t.C:
		case <-p.stop:
			p.flush(p.take(batch))
			return
		}
		p.flush(batch)
		batch = batch[:0]
	}
}

// take moves what is queued now into batch.
func (p *pipeline) take(batch []event.Event) []event.Event {
	for n := len(p.q); n > 0; n-- {
		batch = append(batch, *<-p.q)
	}
	return batch
}

// flush runs one batch through the pipeline and returns the commit's error.
// The mirror is written before the commit, so a batch the store loses is
// still in the text files.
func (p *pipeline) flush(batch []event.Event) error {
	if len(batch) == 0 {
		return nil
	}
	scrub, max := &p.h.scrub, p.h.opts.MaxFieldBytes
	entries := make([]Entry, 0, len(batch))
	for i := range batch {
		e := &batch[i]
		e.Msg = scrub.ScrubCap(e.Msg, max) // 1, 2: scrub, then cap
		for _, a := range attrs(e) {
			*a.val = scrub.Field(a.key, *a.val, max)
		}
		for j, f := range e.Data {
			switch v := f.Value.(type) {
			case string:
				e.Data[j].Value = scrub.Field(f.Key, v, max)
			case json.RawMessage: // the store keeps it as text if this breaks its JSON
				if s := scrub.Field(f.Key, string(v), max); s != string(v) {
					e.Data[j].Value = json.RawMessage(s)
				}
			}
		}
		e.Log = cleanSource(e.Log) // it names a folder
		if e.Proc.PID == 0 {
			e.Proc = process()
		}
		if e.Channel == event.Operational {
			entries = append(entries, EntryFromEvent(*e))
		}
	}
	p.h.mirror(entries)
	err := p.st.Commit(p.ctx, batch)
	if err != nil && !p.failing {
		warn := Entry{At: p.h.now(), Source: "logbook", Level: slog.LevelWarn,
			Msg: "logbook: the store could not commit; the text files still have these events", Fields: []Field{{Key: "err", Value: err.Error()}}}
		p.h.mirror([]Entry{warn})
		entries = append(entries, warn)
	}
	p.failing = err != nil
	p.h.feed(entries)
	return err
}

// close stops the queue, flushes what it holds (for at most closeWait, after
// which a commit still waiting on the lock is abandoned: the mirror has it),
// stops the writer and closes the store.
func (p *pipeline) close() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	close(p.stop)
	select {
	case <-p.done:
	case <-time.After(closeWait):
		p.cancel()
		<-p.done
	}
	p.cancel()
	_ = p.st.Close()
}

// mirror appends entries to their sources' day files, under one lock.
func (h *Hub) mirror(entries []Entry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	for _, en := range entries {
		s := h.sourceLocked(en.Source, true)
		if h.root != "" {
			h.writeLocked(s, en.At, en.Line())
		}
	}
}

// feed appends committed entries to the ring and wakes its readers, as Log
// does for a hub with no store.
func (h *Hub) feed(entries []Entry) {
	if len(entries) == 0 {
		return
	}
	h.mu.Lock()
	if !h.closed {
		for _, en := range entries {
			h.appendLocked(en)
		}
	}
	h.mu.Unlock()
	h.wake()
}
