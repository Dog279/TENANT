package logs

import (
	"context"
	"errors"
	"time"

	"tenant/internal/logs/store"
)

// Summary is the log book's Summary page (store.Summary) as of now, under the
// retention settings in force, with the events this process's full queue
// dropped since its last report added to their logs' counts: the report
// (once a minute) moves them into the store's counters.
func (h *Hub) Summary(ctx context.Context, now time.Time) (store.Summary, error) {
	st := h.store()
	if st == nil {
		return store.Summary{}, errors.New("logs: the log book has no store")
	}
	b := store.DefaultBudget()
	if g := h.hk.gate.Load(); g != nil {
		b = g.budget
	} else if h.opts.Budget != nil {
		b = h.opts.Budget()
	}
	sum, err := st.Summary(ctx, b, now)
	pending := h.unreportedDrops()
	for i := range sum.Logs {
		sum.Logs[i].Dropped += int64(pending[sum.Logs[i].Log])
	}
	return sum, err
}

// unreportedDrops is, per log, what the queue dropped since the last report
// put the counts into the store.
func (h *Hub) unreportedDrops() map[string]uint64 {
	p := h.pipe.Load()
	if p == nil {
		return nil
	}
	p.dmu.Lock()
	defer p.dmu.Unlock()
	out := map[string]uint64{}
	for k, n := range p.recent {
		out[k.log] += n
	}
	for log, n := range p.shed {
		out[log] += n
	}
	return out
}
