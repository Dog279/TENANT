package logs

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"tenant/internal/logs/store"
)

// The hub's Summary reads its store under the retention settings in force,
// and counts the events this process's queue dropped, which never reached
// the store.
func TestHubSummary(t *testing.T) {
	budget := func() store.Budget {
		b := store.DefaultBudget()
		b.Operational.MaxBytes = 1 << 20
		return b
	}
	h, _, _ := openHub(t, Options{Budget: budget})
	if _, err := NewMemory(0).Summary(context.Background(), base); err == nil {
		t.Error("a hub with no store has a summary")
	}
	r := attach(t, h)
	h.Logf("x", slog.LevelWarn, "careful")
	waitFor(t, 5*time.Second, func() bool { return len(all(t, r)) == 1 })
	p := h.pipe.Load()
	p.dmu.Lock()
	p.recent[lossKey{log: "x", provider: "x"}] = 3 // dropped since the last report
	p.dmu.Unlock()

	sum, err := h.Summary(context.Background(), base.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Logs) != 1 {
		t.Fatalf("logs %+v", sum.Logs)
	}
	if x := sum.Logs[0]; x.Log != "x" || x.Hour.Warning != 1 || x.Dropped != 3 || x.MaxBytes != 1<<20 {
		t.Errorf("x: %+v", x)
	}
}
