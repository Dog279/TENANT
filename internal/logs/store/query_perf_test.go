package store

import (
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"tenant/internal/logs/event"
	"tenant/internal/logs/query"
)

// perfLogs are the fixture's logs, with each one's share of every 100 events.
var perfLogs = []struct {
	log, provider string
	share         int
}{
	{"turns", "tenant.agent", 40}, {"discord", "discord", 15}, {"cron", "cron", 10}, {"mcp.github", "mcp.github", 10},
	{"mcp.jira", "mcp.jira", 5}, {"system", "system", 10}, {"sandbox", "sandbox", 5}, {"eval", "eval", 3}, {"security", "security", 2},
}

var perfTools = []string{"web_search", "os_exec", "read_file", "write_file", "memory_search", "fetch", "jira", "github", "calendar", "mail", "shell", "python"}

// perfEvent is fixture event i of n in a log book that spans 7 days up to end:
// the logs above; 0.1% critical, 2% error, 5% warning, 70% info, the rest
// verbose; a 1,000-word vocabulary, with "permission denied" in 0.5%; turns
// of 20 events and activities of 50; a new boot every 100,000 events.
func perfEvent(i, n int, end time.Time) event.Event {
	h := uint64(i+1) * 0x9E3779B97F4A7C15
	k := int(h>>40) % 100
	pl := perfLogs[0]
	for _, l := range perfLogs {
		if k < l.share {
			pl = l
			break
		}
		k -= l.share
	}
	e := event.Event{Log: pl.log, Provider: pl.provider, Time: end.Add(-time.Duration(n-i) * (7 * 24 * time.Hour / time.Duration(n))).UTC(),
		Task: "Work", Proc: event.Process{PID: 4000 + i/100_000, BootID: fmt.Sprintf("%032x", 0xb00+i/100_000)}}
	tool := perfTools[h>>8%uint64(len(perfTools))]
	switch r := h >> 20 % 1000; {
	case r < 1:
		e.Level, e.ID, e.Msg = event.Critical, 3, "the previous run ended unexpectedly"
	case r < 21:
		e.Level, e.ID, e.Msg = event.Error, 3000+uint32(h%50), fmt.Sprintf("%s failed: timeout after %ds", tool, h%30)
	case r < 71:
		e.Level, e.ID, e.Msg = event.Warning, 2000+uint32(h%50), fmt.Sprintf("%s slow, retrying in %ds", tool, h%10)
	case r < 771:
		e.Level, e.ID, e.Msg = event.Info, 1000+uint32(h%50), fmt.Sprintf("tool call %s returned %d results in %dms", tool, h%20, 100+h%400)
	default:
		e.Level, e.ID, e.Msg, e.Channel = event.Verbose, 9000+uint32(h%10), fmt.Sprintf("payload for %s: %d bytes", tool, h%5000), event.Diagnostic
	}
	e.Msg += fmt.Sprintf(" w%d", h>>30%1000)
	if h%200 == 0 {
		e.Msg += ": permission denied"
	}
	if pl.log == "security" && e.Level <= event.Warning {
		e.Keywords = []string{"audit.failure"}
	}
	e.Data = []event.Field{{Key: "tool", Value: tool}, {Key: "iter", Value: int64(h % 7)},
		{Key: "args", Value: `{"q":"hn top stories today","n":10}`, Sensitive: true}, {Key: "took", Value: time.Duration(100+h%400) * time.Millisecond}}
	if pl.log == "turns" {
		e.Corr = event.Correlation{TraceID: fmt.Sprintf("%032x", i/20), SessionID: "discord:general"}
	} else {
		e.Corr.ActivityID = fmt.Sprintf("a%031x", i/50)
	}
	return e
}

// TestQueryBudgets checks §12.3's query budgets over 1M events: the grid's
// first page (the newest 100) at p95 under 200 ms for filters on indexed
// fields, and under 1 s for data.<key> and full text. It logs each filter's
// Count, and the aggregates, alongside. Opt in with TENANT_LOGBOOK_PERF=1;
// TENANT_LOGBOOK_PERF_N sets the size. The fixture takes about 400 MB.
func TestQueryBudgets(t *testing.T) {
	if os.Getenv("TENANT_LOGBOOK_PERF") == "" {
		t.Skip("set TENANT_LOGBOOK_PERF=1 to measure queries over 1M events")
	}
	n := 1_000_000
	if v := os.Getenv("TENANT_LOGBOOK_PERF_N"); v != "" {
		fmt.Sscan(v, &n)
	}
	s := open(t, tempDB(t), fmt.Sprintf("%032x", 0xb00+(n-1)/100_000))
	end := time.Now()
	start := time.Now()
	batch := make([]event.Event, 0, 10_000)
	for i := range n {
		batch = append(batch, perfEvent(i, n, end))
		if len(batch) == cap(batch) || i == n-1 {
			if err := s.Commit(ctx, batch); err != nil {
				t.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	t.Logf("fixture: %d events in %v", n, time.Since(start).Round(time.Second))

	aTrace := fmt.Sprintf("%032x", n/2/20)
	classes := []struct {
		name   string
		budget time.Duration
		fs     []string
	}{
		{"indexed", 200 * time.Millisecond, []string{
			"log:discord", "log:discord level>=warning since:24h", "level>=warning since:24h", "level>=error", "level:critical",
			"provider:mcp.github id:3000-3999", "provider:mcp.* level>=error", "trace:" + aTrace, "since:2h until:1h50m",
			"boot:current level>=warning", "log:cron -id:1005 since:7d",
		}},
		{"data, tags and text", time.Second, []string{"tag:audit.failure since:7d",
			"data.tool:os_exec", "data.tool:os_exec level>=warning", "data.iter>=5 log:turns", `"permission denied"`,
			"timeout", "w123", `data.tool:os_exec "permission denied"`, "retrying since:24h", "data.tool:nothing", "tool", "call results",
		}},
	}
	const runs = 10
	for _, c := range classes {
		var page, count []time.Duration
		for _, fs := range c.fs {
			f, err := query.ParseAt(fs, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			var pWorst, cWorst time.Duration
			var matched int64
			for range runs {
				t0 := time.Now()
				if _, _, _, err := s.Query(ctx, f, Cursor{}, 100, true); err != nil {
					t.Fatal(err)
				}
				t1 := time.Now()
				if matched, err = s.Count(ctx, f); err != nil {
					t.Fatal(err)
				}
				t2 := time.Now()
				page, count = append(page, t1.Sub(t0)), append(count, t2.Sub(t1))
				pWorst, cWorst = max(pWorst, t1.Sub(t0)), max(cWorst, t2.Sub(t1))
			}
			t.Logf("  %-45s %8d match; worst page %6.1f ms, count %6.1f ms", fs, matched, ms(pWorst), ms(cWorst))
		}
		pp, cp := p95(page), p95(count)
		t.Logf("%s: p95 page %.1f ms, count %.1f ms (budget %v)", c.name, ms(pp), ms(cp), c.budget)
		if pp > c.budget {
			t.Errorf("%s: p95 page %v, over the %v budget", c.name, pp, c.budget)
		}
	}

	timed := func(name string, fn func() error) {
		var ds []time.Duration
		for range runs {
			t0 := time.Now()
			if err := fn(); err != nil {
				t.Fatal(err)
			}
			ds = append(ds, time.Since(t0))
		}
		t.Logf("%-45s p95 %6.1f ms", name, ms(p95(ds)))
	}
	day, _ := query.ParseAt("since:24h", time.Now())
	week, _ := query.ParseAt("since:7d", time.Now())
	admin, _ := query.ParseAt("level>=warning since:24h", time.Now())
	timed("Stats(since:24h, 1h), rollups", func() error { _, err := s.Stats(ctx, day, time.Hour); return err })
	timed("Stats(since:7d, none), rollups", func() error { _, err := s.Stats(ctx, week, 0); return err })
	timed("TopIDs(level>=warning since:24h, 10)", func() error { _, err := s.TopIDs(ctx, admin, 10); return err })
	timed("Timeline(a trace, 3)", func() error { _, err := s.Timeline(ctx, aTrace, 3); return err })
	timed("EventStats(discord, 2001)", func() error { _, err := s.EventStats(ctx, "discord", 2001); return err })
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func p95(ds []time.Duration) time.Duration {
	ds = slices.Clone(ds)
	slices.Sort(ds)
	return ds[(len(ds)*95+99)/100-1]
}
