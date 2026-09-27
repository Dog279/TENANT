package logs

import (
	"context"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"tenant/internal/datalock"
	"tenant/internal/logs/catalog"
	"tenant/internal/logs/event"
	"tenant/internal/logs/store"
	"tenant/internal/runtrace"
)

// The log book watching itself (docs/design/logging-v2.md §12.1, §12.4): it
// opens logbook.db, moving a corrupt one aside; rides out a store that
// fails by running on the text mirror alone and importing the outage's
// lines once commits succeed again; reports what it dropped and suppressed,
// a clock jump and the store's integrity; and tells the operator, through
// OnAlert, while logbook.db isn't being written.

// Seams for the fault-injection tests.
var (
	commitBatch   = (*store.Store).Commit
	recoverMirror = (*Hub).importMirror
	// clockNow reads the wall clock (without its monotonic reading) and the
	// monotonic time elapsed since the process started.
	clockNow = func() (time.Time, time.Duration) {
		now := time.Now()
		return now.Round(0), now.Sub(procStart)
	}
	procStart = time.Now()
)

const (
	failLimit  = 3               // commits failed in a row before the mirror carries on alone
	clockSlack = 2 * time.Second // a larger gap between the wall and monotonic clocks is a jump
)

var (
	retryFirst  = 100 * time.Millisecond // the wait before a failed commit's second attempt; it doubles
	retryMax    = 30 * time.Second       // on the mirror alone: the longest wait between attempts
	reportEvery = time.Minute            // the dropped and suppressed report
)

var errMirrorOnly = errors.New("logs: logbook.db is unavailable; the log book runs on its text files alone")

// logbook is a hub's watch on itself.
type logbook struct {
	mu       sync.Mutex
	degraded string       // why logbook.db isn't being written; "" while it is
	alert    func(string) // the operator's status feed (OnAlert)

	lim  limiter                 // ratelimit.go
	boot atomic.Pointer[bootRec] // this process's boot record, while it keeps one (boots.go)

	// The housekeeper loop's (watch).
	wall       time.Time
	mono       time.Duration
	reportedAt time.Duration
}

// outage is the writer goroutine's view of a failing store.
type outage struct {
	fails     int           // commits failed in a row
	down      bool          // running on the mirror alone
	since     time.Time     // the first event the store missed
	wait      time.Duration // before the next attempt, while down
	retryAt   time.Time
	mirrorErr bool // the last mirror write failed
}

// lossKey names what dropped events are counted by.
type lossKey struct{ log, provider string }

// emit files one of the log book's own events through this hub, whichever
// hub is installed.
func (h *Hub) emit(d *event.Def, kv ...any) {
	ctx := context.Background()
	hubEmitter{h}.Emit(ctx, d.Build(ctx, kv...))
}

// OpenStore opens <root>/logbook.db, the system of record, with the Security
// log's chain key, attaches it and returns it. A database SQLite finds
// corrupt is moved aside with its -wal and -shm (store.Quarantine) and an
// empty one started, unless another process has it open: then this process
// runs on its text files alone, as it does when the store won't open at all,
// and says so (Degraded). Call it before Start.
//
// ponytail: an open that failed is not retried; the process runs on the text
// files until it restarts. Retry from the housekeeper if a transient failure
// at start ever shows up.
func (h *Hub) OpenStore(chainKey []byte) (*store.Store, error) {
	if h.root == "" {
		return nil, errors.New("logs: a memory-only hub has no store")
	}
	path := filepath.Join(h.root, "logbook.db")
	opts := store.Options{BootID: runtrace.BootID(), ChainKey: chainKey}
	st, err := store.Open(path, opts)
	var moved string
	var damage error
	if errors.Is(err, store.ErrCorrupt) {
		damage = err
		var qerr error
		moved, qerr = store.Quarantine(path)
		switch {
		case qerr == nil || errors.Is(qerr, datalock.ErrBusy):
			// A fresh store, or the one another process moved in meanwhile.
			st, err = store.Open(path, opts)
		default:
			err = errors.Join(err, qerr)
		}
		if errors.Is(err, store.ErrCorrupt) && errors.Is(qerr, datalock.ErrBusy) {
			h.emit(catalog.CorruptInUse, "err", err)
			h.degrade("logbook.db is corrupt and other Tenant processes have it open; stop them all, then start one")
			return nil, err
		}
	}
	if err != nil {
		h.emit(catalog.MirrorOnly, "err", err, "since", time.Now())
		h.degrade("logbook.db did not open (" + err.Error() + "); the log book runs on its text files alone")
		return nil, err
	}
	h.AttachStore(st)
	id := st.StoreID()
	if moved != "" {
		h.emit(catalog.Quarantined, "file", moved, "store", hex.EncodeToString(id[:]), "err", damage)
	}
	db, wal, _ := st.Size(context.Background())
	h.emit(catalog.LogbookOpened, "bytes", db+wal, "schema", store.SchemaVersion(), "store", hex.EncodeToString(id[:]))
	return st, nil
}

// Flush commits what is queued now, and waits for it at most 2 seconds.
// Serve and the TUI flush as the last step of their shutdown; the store
// stays open for the shutdown's own last events until Close.
func (h *Hub) Flush() error {
	p := h.pipe.Load()
	if p == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), closeWait)
	defer cancel()
	if err := p.audit(ctx, nil); !errors.Is(err, errClosed) {
		return err
	}
	return nil
}

// Degraded says why logbook.db isn't being written, "" while it is.
func (h *Hub) Degraded() string {
	h.lb.mu.Lock()
	defer h.lb.mu.Unlock()
	return h.lb.degraded
}

// OnAlert gives fn a line for the operator's status feed each time the log
// book stops writing logbook.db and when it writes it again, and one at once
// if it isn't writing it now. Serve and the TUI pass their feed, which the
// terminal, the TUI and the system log show.
func (h *Hub) OnAlert(fn func(string)) {
	h.lb.mu.Lock()
	h.lb.alert = fn
	why := h.lb.degraded
	h.lb.mu.Unlock()
	if why != "" && fn != nil {
		fn("⚠ log book: " + why)
	}
}

// degrade records why logbook.db isn't being written ("" once it is again)
// and tells the operator when that changes.
func (h *Hub) degrade(why string) {
	h.lb.mu.Lock()
	was, fn := h.lb.degraded, h.lb.alert
	h.lb.degraded = why
	h.lb.mu.Unlock()
	switch {
	case fn == nil || (was == "") == (why == ""):
	case why != "":
		fn("⚠ log book: " + why)
	default:
		fn("✅ log book: logbook.db is written again")
	}
}

// commit files batch in the store, retrying with backoff (§12.1). After
// failLimit failures in a row the process runs on the mirror alone, which
// already has every batch: a commit is tried again at most every retryMax,
// and the first that succeeds imports the mirror's lines from the outage.
//
// ponytail: an outage still open when the process exits is never imported;
// its lines stay in the text files. Keep the window in a file beside the
// store for the next start if that ever matters.
func (p *pipeline) commit(batch []event.Event) error {
	o := &p.out
	if o.down && time.Now().Before(o.retryAt) {
		return errMirrorOnly
	}
	for {
		err := commitBatch(p.st, p.ctx, batch)
		// ErrNoChainKey: only the batch's security records were refused, and
		// their callers hear it; the store itself is working.
		if err == nil || errors.Is(err, store.ErrNoChainKey) {
			if o.down {
				p.recovered(batch)
			}
			o.fails = 0
			return err
		}
		if o.fails == 0 {
			o.since = first(batch)
		}
		o.fails++
		if o.down {
			o.wait = min(2*o.wait, retryMax)
			o.retryAt = time.Now().Add(o.wait)
			return err
		}
		p.h.emit(catalog.CommitFailed, "count", len(batch), "attempt", o.fails, "err", err)
		if o.fails >= failLimit {
			o.down, o.wait = true, retryFirst<<failLimit
			o.retryAt = time.Now().Add(o.wait)
			p.h.emit(catalog.MirrorOnly, "err", err, "since", o.since)
			p.h.degrade("logbook.db can't be written (" + err.Error() + "); the log book runs on its text files alone")
			return err
		}
		select {
		case <-time.After(retryFirst << (o.fails - 1)):
		case <-p.stop: // closing: the final flush tries again
			return err
		}
	}
}

// recovered ends an outage: batch, the first committed since, marks its end,
// and the mirror's lines from before it are imported in the background
// (importMirror skips the lines the store already holds, such as another
// process's).
func (p *pipeline) recovered(batch []event.Event) {
	from, to := p.out.since, first(batch)
	p.out.down = false
	p.h.degrade("")
	p.h.emit(catalog.MirrorOnlyEnded, "outage", to.Sub(from).Round(time.Second), "from", from, "to", to)
	p.bg.Add(1)
	go func() {
		defer p.bg.Done()
		n, err := recoverMirror(p.h, p.ctx, from, to, "recovered")
		if err != nil {
			p.h.emit(catalog.RecoveryFailed, "err", err, "from", from, "to", to)
			return
		}
		p.h.emit(catalog.RecoveryImport, "count", n, "from", from, "to", to)
	}()
}

// first is the time of batch's earliest event.
func first(batch []event.Event) time.Time {
	t := time.Now().UTC()
	for _, e := range batch {
		if !e.Time.IsZero() && e.Time.Before(t) {
			t = e.Time
		}
	}
	return t
}

// mirrored notes the mirror write's result: a failure is reported once, until
// a write succeeds again.
func (p *pipeline) mirrored(err string) {
	if failed := err != ""; failed != p.out.mirrorErr {
		p.out.mirrorErr = failed
		if failed {
			p.h.emit(catalog.MirrorFailed, "err", err)
		}
	}
}

// drop counts e as dropped: a full queue, or low disk.
func (p *pipeline) drop(e *event.Event) {
	log := cleanSource(e.Log)
	p.dmu.Lock()
	p.dropped[log]++
	p.recent[lossKey{log, e.Provider}]++
	p.dmu.Unlock()
}

// high notes the queue's depth, for the high-water mark.
func (p *pipeline) high() {
	if n := int64(len(p.q)); n > p.hwm.Load() {
		p.hwm.Store(n)
	}
}

// watch is the housekeeper loop's look at the log book itself, every tick in
// every process: the clock, this process's heartbeat, and once a minute the
// report of what was dropped and suppressed.
func (h *Hub) watch(ctx context.Context, st *store.Store) {
	wall, mono := clockNow()
	lb := &h.lb
	if !lb.wall.IsZero() {
		if d := wall.Sub(lb.wall) - (mono - lb.mono); d > clockSlack || d < -clockSlack {
			h.emit(catalog.ClockJump, "delta", d.Round(time.Millisecond))
		}
	} else {
		lb.reportedAt = mono
	}
	lb.wall, lb.mono = wall, mono
	if b := lb.boot.Load(); b != nil {
		_ = st.Heartbeat(ctx, b.id, wall)
	}
	if window := mono - lb.reportedAt; window >= reportEvery {
		lb.reportedAt = mono
		h.report(ctx, st, window.Round(time.Second))
	}
}

// report files what was dropped and suppressed in the last window, per
// provider, adds it to each log's counters in the store, and the queue's
// high-water mark when it reached half the queue.
func (h *Hub) report(ctx context.Context, st *store.Store, window time.Duration) {
	p := h.pipe.Load()
	if p == nil {
		return
	}
	p.dmu.Lock()
	drops := p.recent
	p.recent = map[lossKey]uint64{}
	p.dmu.Unlock()
	lost := map[string]store.Lost{}
	for k, n := range drops {
		l := lost[k.log]
		l.Dropped += int64(n)
		lost[k.log] = l
		h.emit(catalog.Dropped, "count", n, "provider", k.provider, "log", k.log, "window", window)
	}
	for k, s := range h.lb.lim.take() {
		log := cleanSource(s.log)
		l := lost[log]
		l.Suppressed += int64(s.n)
		lost[log] = l
		id := strconv.Quote(k.tmpl)
		if k.id != 0 {
			id = strconv.FormatUint(uint64(k.id), 10)
		}
		h.emit(catalog.Suppressed, "count", s.n, "provider", k.provider, "event", id, "log", log, "window", window)
	}
	if depth := p.hwm.Swap(0); depth >= queueCap/2 {
		h.emit(catalog.QueueHigh, "depth", depth, "cap", queueCap, "window", window)
	}
	_ = st.AddLost(ctx, lost) // the events above carry the counts should this fail
}

// integrityEvery is how often the housekeeper runs the store's quick_check.
const integrityEvery = 24 * time.Hour

// checkIntegrity is the integrity task: a pass or a failure is an event.
// ponytail: a damaged database found here is only reported; it is moved
// aside when a process opens it with none other running, or by tenant logs
// recover.
func (h *Hub) checkIntegrity(ctx context.Context) error {
	st := h.store()
	if st == nil {
		return nil
	}
	start := time.Now()
	switch err := st.Check(ctx); {
	case errors.Is(err, store.ErrCorrupt):
		h.emit(catalog.IntegrityFailed, "err", err)
	case err != nil:
		return err
	default:
		h.emit(catalog.IntegrityOK, "took", time.Since(start).Round(time.Millisecond))
	}
	return nil
}
