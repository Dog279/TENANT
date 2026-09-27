package logs

import (
	"context"
	"errors"
	"time"

	"tenant/internal/logs/store"
)

// Summary is the log book's Summary page (store.Summary) as of now, under the
// retention settings in force, with the events this process's full queue
// dropped added to their logs' counts: they never reached the store.
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
	dropped := h.Dropped()
	for i := range sum.Logs {
		sum.Logs[i].Dropped += int64(dropped[sum.Logs[i].Log])
	}
	return sum, err
}
