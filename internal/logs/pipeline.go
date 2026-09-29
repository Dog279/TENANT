package logs

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
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
	queueCap = 64 << 10
	batchMax = 512
)

// Vars so a test can push them past what a slow CI runner takes.
var (
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

	dmu    sync.Mutex
	recent map[lossKey]uint64 // per log and provider: events the full queue dropped since the last report (logbook.go)
	shed   map[string]uint64  // per log: events low disk kept out since the last report
	hwm    atomic.Int64       // the queue's deepest since the last report

	out outage         // a failing store (logbook.go; writer goroutine only)
	bg  sync.WaitGroup // an outage's import
}

// auditReq is a Security record waiting for its commit (nil: a flush).
type auditReq struct {
	e    *event.Event
	done chan error
}

// AttachStore makes st the log book's system of record. From then on, Log
// and the event emitter queue events for the pipeline instead of writing the
// text files themselves; the files and the ring carry on, fed by the
// pipeline. The hub owns st: Close flushes the queue and closes it.
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
		stop: make(chan struct{}), done: make(chan struct{}), ctx: ctx, cancel: cancel,
		recent: map[lossKey]uint64{}, shed: map[string]uint64{}}
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
// it out (admit), or its rate limit does. When the queue is full, an
// operational event waits up to a millisecond and a diagnostic one not at
// all, then it is dropped and counted. A Security record is never queued,
// limited or kept out: it forces a commit. False only once the pipeline is
// closed.
func (p *pipeline) enqueue(ctx context.Context, e *event.Event) bool {
	if e.Log == store.SecurityLog {
		return !errors.Is(p.audit(ctx, e), errClosed)
	}
	if !p.admit(e) || !p.h.lb.lim.allow(e) {
		return true
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return false
	}
	select {
	case p.q <- e:
		p.high()
		return true
	default:
	}
	if e.Channel == event.Operational {
		t := time.NewTimer(time.Millisecond)
		defer t.Stop()
		select {
		case p.q <- e:
			p.high()
			return true
		case <-t.C:
		}
	}
	p.drop(e)
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
			if batch = p.take(batch); r.e != nil { // what was queued before it goes first
				batch = append(batch, *r.e)
			}
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
// still in the text files; commit retries it, and runs on the mirror alone
// while the store keeps failing (logbook.go).
func (p *pipeline) flush(batch []event.Event) error {
	if len(batch) == 0 {
		return nil
	}
	entries := make([]Entry, 0, len(batch))
	for i := range batch {
		e := &batch[i]
		p.h.scrubEvent(e, p.h.opts.MaxFieldBytes) // 1, 2: scrub, then cap
		e.Log = cleanSource(e.Log)                // it names a folder
		if e.Proc.PID == 0 {
			e.Proc = process()
		}
		if e.Channel == event.Operational {
			entries = append(entries, EntryFromEvent(*e))
		}
	}
	p.mirrored(p.h.mirror(entries))
	err := p.commit(batch)
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
	p.bg.Wait()
	_ = p.st.Close()
}

// mirror appends entries to their sources' day files, under one lock, and
// returns the first write's error ("" when every line was written).
func (h *Hub) mirror(entries []Entry) (err string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ""
	}
	for _, en := range entries {
		s := h.sourceLocked(en.Source, true)
		if h.root != "" {
			if h.writeLocked(s, en.At, en.Line()); err == "" {
				err = s.err
			}
		}
	}
	return err
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
