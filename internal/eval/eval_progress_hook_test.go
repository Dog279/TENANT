package eval

import (
	"context"
	"sync"
	"testing"
)

// The Progress hook must fire once per completed task with a correct running
// count, in both the sequential and the concurrent pool paths - it is the
// Quality page's only window into a multi-hour run.
func TestHarness_ProgressHook(t *testing.T) {
	for _, conc := range []int{1, 3} {
		h, err := LoadHarness(EmbeddedTasks, nil)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		h.Concurrency = conc
		var mu sync.Mutex
		var counts []int
		total := 0
		h.Progress = func(done, tot int, taskID string) {
			mu.Lock()
			counts = append(counts, done)
			total = tot
			if taskID == "" {
				t.Error("progress fired with empty task id")
			}
			mu.Unlock()
		}
		rep, err := h.Run(context.Background(), SubsetSmoke)
		if err != nil {
			t.Fatalf("conc=%d run: %v", conc, err)
		}
		mu.Lock()
		if len(counts) != len(rep.Results) {
			t.Errorf("conc=%d: %d progress calls for %d results", conc, len(counts), len(rep.Results))
		}
		if total != len(rep.Results) {
			t.Errorf("conc=%d: reported total %d, want %d", conc, total, len(rep.Results))
		}
		// The final count must reach the total regardless of completion order.
		max := 0
		for _, c := range counts {
			if c > max {
				max = c
			}
		}
		if max != len(rep.Results) {
			t.Errorf("conc=%d: max done %d never reached total %d", conc, max, len(rep.Results))
		}
		mu.Unlock()
	}
}
