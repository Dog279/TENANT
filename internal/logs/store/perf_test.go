package store

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"tenant/internal/logs/event"
)

// sample is a typical operational event: a catalogued tool call with a
// template message, four typed fields (one sensitive), a turn and a session.
func sample(i int) event.Event {
	return event.Event{
		Log: "turns", Provider: "tenant.agent", ID: 1001, Time: time.Now().UTC(), Level: event.Info, Task: "Tool call",
		Msg: fmt.Sprintf("tool call web_search returned %d results in %dms", i%20, 100+i%400),
		Data: []event.Field{
			{Key: "tool", Value: "web_search"},
			{Key: "iter", Value: int64(i % 7)},
			{Key: "args", Value: `{"q":"hn top stories today","n":10}`, Sensitive: true},
			{Key: "took", Value: time.Duration(100+i%400) * time.Millisecond},
		},
		Corr: event.Correlation{TraceID: fmt.Sprintf("%032x", i/20), SessionID: "discord:general"},
		Proc: event.Process{PID: 4242, Role: "serve", BootID: "00112233445566778899aabbccddeeff", Host: "mac", Version: "v1.0.0"},
	}
}

// commitN commits n sample events in batches of 512, the pipeline's size.
func commitN(t testing.TB, s *Store, n int) {
	batch := make([]event.Event, 0, 512)
	for i := range n {
		batch = append(batch, sample(i))
		if len(batch) == cap(batch) || i == n-1 {
			if err := s.Commit(ctx, batch); err != nil {
				t.Fatal(err)
			}
			batch = batch[:0]
		}
	}
}

// BenchmarkCommit is the writer's throughput: events/s over 512-event
// batches with synchronous=FULL.
func BenchmarkCommit(b *testing.B) {
	s := open(b, tempDB(b), "")
	b.ResetTimer()
	start := time.Now()
	commitN(b, s, b.N)
	b.ReportMetric(float64(b.N)/time.Since(start).Seconds(), "events/s")
}

// baseline is one OS's recorded writer throughput (baselines/logbook-<os>.json).
type baseline struct {
	EventsPerSec  float64 `json:"events_per_sec"`
	BytesPerEvent float64 `json:"bytes_per_event"`
	Measured      string  `json:"measured"`
}

// TestWriterThroughput checks §12.3's budgets: at least 3,000 events/s and at
// most 1 KB per event, indexes and full text included, and no regression of
// more than 20% against this OS's baseline. Opt in with
// TENANT_LOGBOOK_PERF=1; TENANT_LOGBOOK_PERF=update records the baseline.
func TestWriterThroughput(t *testing.T) {
	mode := os.Getenv("TENANT_LOGBOOK_PERF")
	if mode == "" {
		t.Skip("set TENANT_LOGBOOK_PERF=1 to measure the writer")
	}
	const n = 50_000
	path := tempDB(t)
	s := open(t, path, "")
	start := time.Now()
	commitN(t, s, n)
	rate := n / time.Since(start).Seconds()
	if _, err := s.w.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	var size int64
	for _, f := range []string{path, path + "-wal"} {
		if fi, err := os.Stat(f); err == nil {
			size += fi.Size()
		}
	}
	got := baseline{EventsPerSec: math.Round(rate), BytesPerEvent: math.Round(float64(size) / n), Measured: time.Now().UTC().Format(time.DateOnly)}
	t.Logf("%s/%s: %.0f events/s, %.0f bytes/event", runtime.GOOS, runtime.GOARCH, got.EventsPerSec, got.BytesPerEvent)
	if rate < 3000 {
		t.Errorf("writer throughput %.0f events/s, below the 3,000 budget", rate)
	}
	if got.BytesPerEvent > 1024 {
		t.Errorf("%.0f bytes per event, above the 1 KB budget", got.BytesPerEvent)
	}
	file := filepath.Join("..", "..", "..", "baselines", "logbook-"+runtime.GOOS+".json")
	if mode == "update" {
		b, _ := json.MarshalIndent(got, "", "  ")
		if err := os.WriteFile(file, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		t.Skipf("no baseline for %s yet (%s): record one with TENANT_LOGBOOK_PERF=update", runtime.GOOS, file)
	}
	var want baseline
	if err != nil || json.Unmarshal(raw, &want) != nil {
		t.Fatalf("baseline %s: %v", file, err)
	}
	if rate < 0.8*want.EventsPerSec {
		t.Errorf("writer throughput %.0f events/s regressed more than 20%% from the baseline's %.0f", rate, want.EventsPerSec)
	}
	if got.BytesPerEvent > 1.2*want.BytesPerEvent {
		t.Errorf("%.0f bytes per event grew more than 20%% from the baseline's %.0f", got.BytesPerEvent, want.BytesPerEvent)
	}
}
