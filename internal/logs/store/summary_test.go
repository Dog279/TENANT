package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tenant/internal/logs/event"
)

// The Summary page's figures on a small fixture: counts per window, exact to
// the minute at each window's start, the sparkline, sizes against limits,
// the top IDs and the lists; a privacy deletion takes its event out of them.
func TestSummaryFigures(t *testing.T) {
	s := open(t, tempDB(t), "")
	now := time.Date(2026, 9, 27, 12, 30, 30, 0, time.UTC)
	ev := func(log string, id uint32, lvl event.Level, ago time.Duration) event.Event {
		return event.Event{Log: log, Provider: log, ID: id, Level: lvl, Time: now.Add(-ago), Msg: fmt.Sprintf("%s %d %v", log, id, ago)}
	}
	batch := []event.Event{
		ev("system", 3, event.Critical, 2*time.Hour),
		ev("system", 1, event.Info, 5*time.Minute),
		ev("system", 1, event.Info, 60*time.Minute),                 // the window's first minute: in the hour
		ev("system", 1, event.Info, 61*time.Minute),                 // the minute before it: out
		ev("discord", 3001, event.Error, 23*time.Hour),              // in the day
		ev("discord", 3001, event.Error, 25*time.Hour),              // in the week only
		ev("discord", 1001, event.Info, 8*24*time.Hour),             // older than the week
		ev("cron", 3, event.Critical, 167*time.Hour+10*time.Minute), // the week's first hour
	}
	for range 3 {
		batch = append(batch, ev("discord", 2001, event.Warning, 30*time.Minute))
	}
	forgotten := ev("discord", 2001, event.Warning, 29*time.Minute)
	forgotten.Corr.SessionID = "discord:forget-me"
	batch = append(batch, forgotten)
	if err := s.Commit(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Forget(ctx, Match{Sessions: map[string]bool{"discord:forget-me": true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.w.Exec(`UPDATE logs SET dropped = 4, suppressed = 2 WHERE log_id = (SELECT id FROM names WHERE kind = 'log' AND name = 'discord')`); err != nil {
		t.Fatal(err)
	}

	sum, err := s.Summary(ctx, DefaultBudget(), now)
	if err != nil {
		t.Fatal(err)
	}
	logs := map[string]LogSummary{}
	for _, l := range sum.Logs {
		logs[l.Log] = l
	}
	if len(sum.Logs) != 3 || sum.Logs[0].Log != "cron" || sum.Logs[2].Log != "system" {
		t.Fatalf("logs %v, want cron, discord, system", sum.Logs)
	}
	sys, dis := logs["system"], logs["discord"]
	for _, c := range []struct {
		name      string
		got, want Levels
	}{
		{"system hour", sys.Hour, Levels{Info: 2}},
		{"system day", sys.Day, Levels{Critical: 1, Info: 3}},
		{"discord hour", dis.Hour, Levels{Warning: 3}},
		{"discord day", dis.Day, Levels{Warning: 3, Error: 1}},
		{"discord week", dis.Week, Levels{Warning: 3, Error: 2}},
		{"cron week", logs["cron"].Week, Levels{Critical: 1}},
	} {
		if c.got != c.want {
			t.Errorf("%s = %+v, want %+v", c.name, c.got, c.want)
		}
	}
	if len(dis.Hourly) != SummaryHours || dis.Hourly[SummaryHours-1] != 3 || dis.Hourly[SummaryHours-1-23] != 1 || dis.Hourly[SummaryHours-1-25] != 1 {
		t.Errorf("discord's sparkline ends %v", dis.Hourly[SummaryHours-30:])
	}
	if logs["cron"].Hourly[0] != 1 {
		t.Errorf("cron's critical is not in the sparkline's first hour: %v", logs["cron"].Hourly[:3])
	}
	if dis.MaxBytes != 64<<20 || dis.MaxAge != 30*24*time.Hour || dis.OnFull != Overwrite || sys.MaxBytes != 128<<20 || dis.Bytes <= 0 {
		t.Errorf("limits: discord %d/%v/%s, system %d", dis.MaxBytes, dis.MaxAge, dis.OnFull, sys.MaxBytes)
	}
	if dis.Dropped != 4 || dis.Suppressed != 2 || !dis.Last.Equal(now.Add(-30*time.Minute)) {
		t.Errorf("discord: dropped %d suppressed %d last %v", dis.Dropped, dis.Suppressed, dis.Last)
	}
	if len(sum.Top) == 0 || sum.Top[0] != (IDCount{Provider: "discord", ID: 2001, N: 3}) {
		t.Errorf("top IDs %v", sum.Top)
	}
	ids := func(evs []event.Event) (out []string) {
		for _, e := range evs {
			out = append(out, fmt.Sprintf("%s/%d", e.Log, e.ID))
		}
		return out
	}
	if got := fmt.Sprint(ids(sum.Critical), ids(sum.Errors), ids(sum.Exits)); got != "[system/3 cron/3] [discord/3001 discord/3001] [system/3]" {
		t.Errorf("critical, errors, exits = %s", got)
	}
	if !sum.Errors[0].Time.After(sum.Errors[1].Time) {
		t.Error("errors are not newest first")
	}
}

// A database at schema version 2 gains rollup_hour, filled from the minutes,
// and rollup_id, filled from its warnings and worse.
func TestUpgradeFromSchema2(t *testing.T) {
	path := tempDB(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT);` + migrations[0] + migrations[1] +
		`INSERT INTO meta VALUES ('schema_version', '2'), ('store_id', '0123456789abcdef0123456789abcdef');
		INSERT INTO rollup VALUES (120, 1, 4, 2), (179, 1, 4, 3), (180, 1, 4, 5), (130, 1, 2, 1);
		INSERT INTO events (log_id, provider_id, event_id, time_ns, level, msg, bytes)
		  VALUES (1, 2, 2001, 7200000000000, 3, 'w', 1), (1, 2, 2001, 7300000000000, 3, 'w', 1), (1, 2, 1001, 7300000000000, 4, 'i', 1);`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	s := open(t, path, "")
	for _, c := range []struct{ hour, level, n int64 }{{2, 4, 5}, {3, 4, 5}, {2, 2, 1}} {
		if n := count(t, s, `SELECT n FROM rollup_hour WHERE hour = ? AND log_id = 1 AND level = ?`, c.hour, c.level); n != c.n {
			t.Errorf("hour %d level %d: %d, want %d", c.hour, c.level, n, c.n)
		}
	}
	if n := count(t, s, `SELECT sum(n) FROM rollup_id WHERE hour = 2 AND provider_id = 2 AND event_id = 2001`); n != 2 {
		t.Errorf("rollup_id holds %d of the 2 warnings", n)
	}
	if n := count(t, s, `SELECT count(*) FROM rollup_id WHERE event_id = 1001`); n != 0 {
		t.Error("rollup_id counts an info event")
	}
}

// TestSummaryBudget checks §12.3's Summary budget: under 50 ms over 1M events
// (TENANT_LOGBOOK_PERF_N sets the size) spread over the week, the fixture
// TestQueryBudgets uses. Opt in with TENANT_LOGBOOK_PERF=1.
func TestSummaryBudget(t *testing.T) {
	if os.Getenv("TENANT_LOGBOOK_PERF") == "" {
		t.Skip("set TENANT_LOGBOOK_PERF=1 to measure the Summary over 1M events")
	}
	n := 1_000_000
	if v := os.Getenv("TENANT_LOGBOOK_PERF_N"); v != "" {
		fmt.Sscan(v, &n)
	}
	s := open(t, tempDB(t), "")
	end := time.Now()
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
	var ds []time.Duration
	var sum Summary
	for range 20 {
		t0 := time.Now()
		var err error
		if sum, err = s.Summary(ctx, DefaultBudget(), time.Now()); err != nil {
			t.Fatal(err)
		}
		ds = append(ds, time.Since(t0))
	}
	var week int64
	for _, l := range sum.Logs {
		week += l.Week.Critical + l.Week.Error + l.Week.Warning + l.Week.Info + l.Week.Verbose
	}
	t.Logf("%d events, %d logs, %d in the week; %d critical, %d errors, %d exits listed; p95 %.1f ms, worst %.1f ms",
		n, len(sum.Logs), week, len(sum.Critical), len(sum.Errors), len(sum.Exits), ms(p95(ds)), ms(slicesMax(ds)))
	// The fixture's oldest minutes leave the week while it is built.
	if week < int64(n)*999/1000 || len(sum.Top) != summaryTop || len(sum.Critical) != summaryLatest || len(sum.Errors) != summaryLatest {
		t.Errorf("the summary is incomplete")
	}
	if p := p95(ds); p > 50*time.Millisecond {
		t.Errorf("Summary p95 %v, over the 50 ms budget", p)
	}
}

func slicesMax(ds []time.Duration) (m time.Duration) {
	for _, d := range ds {
		m = max(m, d)
	}
	return m
}
