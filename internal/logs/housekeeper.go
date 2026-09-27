package logs

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"tenant/internal/logs/event"
	"tenant/internal/logs/store"
)

// The housekeeper (docs/design/logging-v2.md §5): work that must run in
// exactly one Tenant process — retention, checkpoints, and the tasks later
// tickets add with Housekeep — runs in whichever long-running process holds
// the store's housekeeper lease. Every process whose hub is Started with a
// store runs the loop. Each tick it first refreshes what its own writes obey
// (the retention settings, each log's policy, the free disk space), then
// takes or renews the lease and, while it holds it, runs the tasks that are
// due, in a goroutine of their own so a long task never delays the renewal.
var (
	leaseEvery = 30 * time.Second // the tick; tests shorten both
	leaseTTL   = 60 * time.Second // a dead holder is replaced within this
)

const (
	leaseName   = "housekeeper"
	retainEvery = 5 * time.Minute
	walLimit    = 64 << 20 // a WAL past this is checkpointed at the next tick

	// Low disk (§6.2), by the free space on the data volume.
	lowDisk      = 1 << 30   // or lowDiskShare of the volume: diagnostic events are shed
	lowDiskShare = 0.05      //
	veryLowDisk  = 256 << 20 // operational events below warning are shed too
)

// housekeeper is a hub's lease loop and what it keeps.
type housekeeper struct {
	mu     sync.Mutex
	chores []*chore

	gate atomic.Pointer[gate] // what this process writes; nil until the first tick

	// diskSpace reports the data volume's free and total bytes (tests
	// replace it).
	diskSpace func(path string) (free, total uint64, err error)

	// Alerts already raised, so each is raised once per crossing (the
	// running tasks' goroutine only).
	full map[string]bool
	over bool
}

type chore struct {
	name  string
	every time.Duration
	fn    func(context.Context) error
	next  time.Time
}

// gate is what this process's writes obey: each log's policy, and the low
// disk floor.
type gate struct {
	budget store.Budget
	logs   map[string]store.Policy // the store's logs; any other follows budget
	shed   int                     // 0: none; 1: diagnostic events; 2: those and operational ones below warning
	debug  map[string]time.Time    // logs whose timed debug switch is on, until when
}

func (g *gate) policy(log string) store.Policy {
	if p, ok := g.logs[log]; ok {
		return p
	}
	return g.budget.For(log)
}

// Housekeep registers fn to run about every interval (<= 0: every tick) in
// whichever process holds the housekeeper lease, first at the first tick it
// holds it. Tasks run one after another; the context each gets is cancelled
// when the lease is lost or the hub closes. A task's error is logged to the
// logbook log and never stops the loop. Register at start-up.
func (h *Hub) Housekeep(name string, every time.Duration, fn func(ctx context.Context) error) {
	h.hk.mu.Lock()
	defer h.hk.mu.Unlock()
	h.hk.chores = append(h.hk.chores, &chore{name: name, every: every, fn: fn})
}

// store is the attached store, nil when there is none.
func (h *Hub) store() *store.Store {
	if p := h.pipe.Load(); p != nil {
		return p.st
	}
	return nil
}

// housekeep is the lease loop. Start runs it when a store is attached.
func (h *Hub) housekeep(ctx context.Context) {
	defer h.wg.Done()
	ctx, stop := context.WithCancel(ctx)
	var running sync.WaitGroup
	defer func() {
		stop()
		running.Wait()
	}()
	t := time.NewTicker(leaseEvery)
	defer t.Stop()
	busy := make(chan struct{}, 1) // a run of the tasks is in flight
	held := false
	var term struct { // one term of holding the lease: its tasks' context ends with it
		ctx context.Context
		end context.CancelFunc
	}
	term.ctx, term.end = context.WithCancel(ctx)
	defer func() { term.end() }()
	for {
		if st := h.store(); st != nil {
			h.refresh(ctx, st)
			ok, err := st.TryLease(ctx, leaseName, leaseTTL)
			if ok = ok && err == nil; ok != held {
				held = ok
				if held {
					h.Logf("logbook", slog.LevelInfo, "logbook: this process now does the log book's housekeeping")
				} else {
					term.end()
					term.ctx, term.end = context.WithCancel(ctx)
					var kv []any
					if err != nil {
						kv = append(kv, "err", err)
					}
					h.Logf("logbook", slog.LevelInfo, "logbook: this process no longer holds the housekeeping lease", kv...)
				}
			}
			if held {
				select {
				case busy <- struct{}{}:
					running.Add(1)
					go func(ctx context.Context) {
						defer running.Done()
						defer func() { <-busy }()
						h.runChores(ctx)
					}(term.ctx)
				default: // the last run is still going
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-h.stop:
			return
		case <-t.C:
		}
	}
}

// runChores runs the tasks that are due. A task is due half a tick early, so
// one meant for every minute runs every other tick rather than every third.
func (h *Hub) runChores(ctx context.Context) {
	h.hk.mu.Lock()
	chores := slices.Clone(h.hk.chores)
	h.hk.mu.Unlock()
	for _, c := range chores {
		now := time.Now()
		if ctx.Err() != nil {
			return
		}
		if c.next.After(now.Add(leaseEvery / 2)) {
			continue
		}
		c.next = now.Add(c.every)
		if err := c.fn(ctx); err != nil && ctx.Err() == nil {
			h.Logf("logbook", slog.LevelError, "logbook: housekeeping task "+c.name+" failed", "err", err)
		}
	}
}

// refresh reloads what this process's writes obey: the retention settings,
// each log's policy and the free disk space. A crossing into or out of low
// disk is logged.
func (h *Hub) refresh(ctx context.Context, st *store.Store) {
	g := &gate{budget: store.DefaultBudget()}
	if h.opts.Budget != nil {
		g.budget = h.opts.Budget()
	}
	old := h.hk.gate.Load()
	var err error
	if g.logs, err = st.Policies(ctx, g.budget); err != nil && old != nil {
		g.logs = old.logs // keep the last ones read
	}
	if g.debug, err = st.Debugging(ctx); err != nil && old != nil {
		g.debug = old.debug
	}
	free, total, err := h.hk.space(h.root)
	switch {
	case err != nil:
	case free < veryLowDisk:
		g.shed = 2
	case free < lowDisk || float64(free) < lowDiskShare*float64(total):
		g.shed = 1
	}
	h.hk.gate.Store(g)
	if old == nil && g.shed == 0 || old != nil && old.shed == g.shed {
		return
	}
	mb := free >> 20
	switch g.shed {
	case 0:
		h.Logf("logbook", slog.LevelInfo, "logbook: disk space recovered; every event is written again", "free_mb", mb)
	case 1:
		h.Logf("logbook", slog.LevelWarn, "logbook: low disk space; diagnostic events are not written until space is freed", "free_mb", mb)
	case 2:
		h.Logf("logbook", slog.LevelWarn, "logbook: very low disk space; only warnings, errors and the security log are written until space is freed", "free_mb", mb)
	}
}

func (hk *housekeeper) space(path string) (free, total uint64, err error) {
	if hk.diskSpace != nil {
		return hk.diskSpace(path)
	}
	return diskSpace(path)
}

// admit reports whether this process writes e: its log is enabled, e is as
// severe as the log's minimum level, and low disk isn't shedding it. A shed
// event is counted with the dropped ones. The Security log never comes here.
func (p *pipeline) admit(e *event.Event) bool {
	g := p.h.hk.gate.Load()
	if g == nil {
		return true
	}
	if pol := g.policy(e.Log); pol.Disabled || pol.MinLevel != 0 && e.Level > pol.MinLevel && !g.debugging(e.Log) {
		return false
	}
	if g.shed > 0 && (e.Channel == event.Diagnostic || g.shed > 1 && e.Level > event.Warning) {
		p.dmu.Lock()
		p.dropped[e.Log]++
		p.dmu.Unlock()
		return false
	}
	return true
}

// retain is the retention task: one pass under the settings in force, then
// the alerts the operator needs, each once per crossing.
func (h *Hub) retain(ctx context.Context) error {
	st, g := h.store(), h.hk.gate.Load()
	if st == nil || g == nil {
		return nil
	}
	rep, err := st.Retain(ctx, g.budget, time.Now())
	for _, f := range rep.Archived {
		h.Logf("logbook", slog.LevelInfo, "logbook: archived the oldest records", "file", f)
	}
	full := map[string]bool{}
	for _, name := range rep.Kept {
		full[name] = true
		if !h.hk.full[name] {
			h.Logf("logbook", slog.LevelWarn, "logbook: log "+name+" is past its size limit; its policy keeps every record, so nothing is deleted", "log", name)
		}
	}
	h.hk.full = full
	if rep.Over && !h.hk.over {
		h.Logf("logbook", LevelCritical, "logbook: the log book is over its disk budget, and only records retention may not delete are left (security, archived and kept logs)",
			"size_mb", rep.Size>>20, "budget_mb", g.budget.TotalBytes>>20)
	}
	h.hk.over = rep.Over
	return err
}

// checkpointWAL checkpoints a WAL that has passed walLimit since the last
// retention pass did.
func (h *Hub) checkpointWAL(ctx context.Context) error {
	st := h.store()
	if st == nil {
		return nil
	}
	_, wal, err := st.Size(ctx)
	if err != nil || wal <= walLimit {
		return err
	}
	h.Logf("logbook", slog.LevelInfo, "logbook: the WAL passed its checkpoint threshold; checkpointing", "wal_mb", wal>>20)
	return st.Checkpoint(ctx)
}
