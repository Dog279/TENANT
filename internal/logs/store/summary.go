package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"tenant/internal/logs/event"
)

// The Summary page's reach: a sparkline of SummaryHours hours, and at most
// summaryTop event IDs and summaryLatest events in each list.
const (
	SummaryHours  = 7 * 24
	summaryTop    = 10
	summaryLatest = 10
)

// Levels counts events by level.
type Levels struct {
	Critical int64 `json:"critical"`
	Error    int64 `json:"error"`
	Warning  int64 `json:"warning"`
	Info     int64 `json:"info"`
	Verbose  int64 `json:"verbose"`
}

func (l *Levels) add(lvl event.Level, n int64) {
	switch lvl {
	case event.Critical:
		l.Critical += n
	case event.Error:
		l.Error += n
	case event.Warning:
		l.Warning += n
	case event.Info:
		l.Info += n
	default:
		l.Verbose += n
	}
}

// LogSummary is one log's row of the Summary page.
type LogSummary struct {
	Log        string        `json:"log"`
	Hour       Levels        `json:"hour"`          // the last hour
	Day        Levels        `json:"day"`           // the last 24 hours
	Week       Levels        `json:"week"`          // the last 7 days
	Hourly     []int64       `json:"hourly"`        // events per hour, SummaryHours of them, oldest first; the last is this hour
	Bytes      int64         `json:"bytes"`         // its events' summed payload: its share of the budget
	MaxBytes   int64         `json:"max_bytes"`     // its size limit (0: none)
	MaxAge     time.Duration `json:"max_age"`       // how long its events are kept, in nanoseconds (0: forever)
	OnFull     string        `json:"on_full"`       // what happens past MaxBytes
	Dropped    int64         `json:"dropped"`       // events lost before they reached the store
	Suppressed int64         `json:"suppressed"`    // events a rate limit held back
	Last       time.Time     `json:"last,omitzero"` // its newest event
}

// Summary is the Summary page (docs/design/logging-v2.md §9.2): what has
// gone wrong recently, at a glance.
type Summary struct {
	Now      time.Time     `json:"now"`
	Logs     []LogSummary  `json:"logs"`     // every log the store holds, by name
	Top      []IDCount     `json:"top"`      // the IDs most frequent among warnings and worse in the last 24 hours, in whole hours
	Critical []event.Event `json:"critical"` // the latest critical events of the last 7 days, newest first
	Errors   []event.Event `json:"errors"`   // the latest errors of the last 7 days, newest first
	Exits    []event.Event `json:"exits"`    // the latest unexpected exits of the last 7 days: the system log's critical events
}

// hourCount is one row of rollup_hour.
type hourCount struct {
	hour, log int64
	level     event.Level
	n         int64
}

// Summary reads the Summary page as of now under b's policies. The counts
// come from the per-hour rollups, plus the per-minute ones for the partial
// hour where a window starts, so they are exact to the minute; the top IDs
// from the per-hour ID rollup. The rollups also say which hours hold the
// events it lists, so each list reads only those hours. So its time depends
// on how many logs, IDs and hours there are, not on how many events.
func (s *Store) Summary(ctx context.Context, b Budget, now time.Time) (Summary, error) {
	sum := Summary{Now: now.UTC()}
	pols, err := s.logPolicies(ctx, b)
	if err != nil {
		return sum, err
	}
	sort.Slice(pols, func(i, j int) bool { return pols[i].name < pols[j].name })
	sum.Logs = make([]LogSummary, len(pols))
	byID := make(map[int64]*LogSummary, len(pols))
	var system int64
	for i, p := range pols {
		sum.Logs[i] = LogSummary{Log: p.name, Bytes: p.bytes, MaxBytes: p.MaxBytes, MaxAge: p.MaxAge, OnFull: p.OnFull,
			Hourly: make([]int64, SummaryHours)}
		byID[p.id] = &sum.Logs[i]
		if p.name == "system" {
			system = p.id
		}
	}
	if err := s.summaryLogs(ctx, byID); err != nil {
		return sum, err
	}
	hours, err := s.summaryCounts(ctx, byID, now)
	if err != nil {
		return sum, err
	}

	if sum.Top, err = s.topIDs(ctx, now); err != nil {
		return sum, err
	}
	if sum.Critical, err = s.latest(ctx, hours, event.Critical, 0); err != nil {
		return sum, err
	}
	if sum.Errors, err = s.latest(ctx, hours, event.Error, 0); err != nil {
		return sum, err
	}
	if system != 0 {
		sum.Exits, err = s.latest(ctx, hours, event.Critical, system)
	}
	return sum, err
}

// summaryLogs fills in each log's dropped and suppressed counts and its
// newest event, one index lookup per log.
func (s *Store) summaryLogs(ctx context.Context, byID map[int64]*LogSummary) error {
	rows, err := s.r.QueryContext(ctx, `SELECT l.log_id, coalesce(l.dropped, 0), coalesce(l.suppressed, 0),
		(SELECT max(time_ns) FROM events WHERE log_id = l.log_id) FROM logs l`)
	if err != nil {
		return fmt.Errorf("store: summary: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, dropped, suppressed int64
		var last *int64
		if err := rows.Scan(&id, &dropped, &suppressed, &last); err != nil {
			return fmt.Errorf("store: summary: %w", err)
		}
		if l := byID[id]; l != nil {
			l.Dropped, l.Suppressed = dropped, suppressed
			if last != nil {
				l.Last = time.Unix(0, *last).UTC()
			}
		}
	}
	return rows.Err()
}

// summaryCounts fills in each log's counts and sparkline, and returns the
// week's per-hour rows, newest first. A window counts rollup_hour's hours
// after the one it starts in, and rollup's minutes for the rest of that one.
func (s *Store) summaryCounts(ctx context.Context, byID map[int64]*LogSummary, now time.Time) ([]hourCount, error) {
	nowMin := now.UnixNano() / int64(time.Minute)
	nowHour := nowMin / 60
	first := nowHour - SummaryHours + 1
	windows := []struct {
		start int64 // minute
		pick  func(*LogSummary) *Levels
	}{
		{nowMin - 60, func(l *LogSummary) *Levels { return &l.Hour }},
		{nowMin - 24*60, func(l *LogSummary) *Levels { return &l.Day }},
		{nowMin - SummaryHours*60, func(l *LogSummary) *Levels { return &l.Week }},
	}

	rows, err := s.r.QueryContext(ctx, `SELECT hour, log_id, level, n FROM rollup_hour WHERE hour BETWEEN ? AND ? ORDER BY hour DESC`, first, nowHour)
	if err != nil {
		return nil, fmt.Errorf("store: summary: %w", err)
	}
	defer rows.Close()
	var hours []hourCount
	for rows.Next() {
		var h hourCount
		if err := rows.Scan(&h.hour, &h.log, &h.level, &h.n); err != nil {
			return nil, fmt.Errorf("store: summary: %w", err)
		}
		hours = append(hours, h)
		l := byID[h.log]
		if l == nil {
			continue
		}
		l.Hourly[h.hour-first] += h.n
		for _, w := range windows {
			if h.hour > w.start/60 {
				w.pick(l).add(h.level, h.n)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: summary: %w", err)
	}
	for _, w := range windows {
		if err := s.addMinutes(ctx, byID, w.start, (w.start/60+1)*60, w.pick); err != nil {
			return nil, err
		}
	}
	return hours, nil
}

// addMinutes adds the rollup's counts for minutes [from, to) to each log's
// levels that pick chooses.
func (s *Store) addMinutes(ctx context.Context, byID map[int64]*LogSummary, from, to int64, pick func(*LogSummary) *Levels) error {
	rows, err := s.r.QueryContext(ctx, `SELECT log_id, level, sum(n) FROM rollup WHERE minute >= ? AND minute < ? GROUP BY 1, 2`, from, to)
	if err != nil {
		return fmt.Errorf("store: summary: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, n int64
		var lvl event.Level
		if err := rows.Scan(&id, &lvl, &n); err != nil {
			return fmt.Errorf("store: summary: %w", err)
		}
		if l := byID[id]; l != nil {
			pick(l).add(lvl, n)
		}
	}
	return rows.Err()
}

// latest returns the newest summaryLatest events at level (of log, when it
// is not 0) in the week, newest first. hours, newest first, say which hours
// hold them, and only those hours are read, through events_admin.
func (s *Store) latest(ctx context.Context, hours []hourCount, level event.Level, log int64) ([]event.Event, error) {
	var starts []int64
	var found int64
	for _, h := range hours {
		if found >= summaryLatest {
			break
		}
		if h.level != level || log != 0 && h.log != log {
			continue
		}
		if start := h.hour * int64(time.Hour); len(starts) == 0 || starts[len(starts)-1] != start {
			starts = append(starts, start)
		}
		found += h.n
	}
	if len(starts) == 0 {
		return nil, nil
	}
	list, _ := json.Marshal(starts)
	// CROSS JOIN keeps the hours as the outer loop: one range of events_admin
	// each. level <= 3 is written literally, so the planner can use it.
	q := `SELECT e.seq FROM json_each(?) h CROSS JOIN events e
		WHERE e.time_ns >= h.value AND e.time_ns < h.value + ? AND e.level <= 3 AND e.level = ?`
	args := []any{string(list), int64(time.Hour), int64(level)}
	if log != 0 {
		q, args = q+` AND e.log_id = ?`, append(args, log)
	}
	evs, err := queryEvents(ctx, s.r, selectEvents+` WHERE e.seq IN (`+q+` ORDER BY e.time_ns DESC LIMIT ?) ORDER BY e.time_ns DESC, e.seq DESC`,
		append(args, summaryLatest)...)
	if err != nil {
		return nil, fmt.Errorf("store: summary: %w", err)
	}
	return evs, nil
}

// addRollups adds the events seqs from to to, which the transaction just
// inserted, to the rollups: per minute and per hour by log and level, and per
// hour by event ID for warnings and worse; and to their logs' bytes. Commit
// calls it.
func addRollups(ctx context.Context, tx *sql.Tx, from, to uint64) error {
	for _, q := range []string{`INSERT INTO rollup (minute, log_id, level, n)
		SELECT time_ns / 60000000000, log_id, level, count(*) FROM events WHERE seq BETWEEN ? AND ? GROUP BY 1, 2, 3
		ON CONFLICT (minute, log_id, level) DO UPDATE SET n = n + excluded.n`,
		`INSERT INTO rollup_hour (hour, log_id, level, n)
		SELECT time_ns / 3600000000000, log_id, level, count(*) FROM events WHERE seq BETWEEN ? AND ? GROUP BY 1, 2, 3
		ON CONFLICT (hour, log_id, level) DO UPDATE SET n = n + excluded.n`,
		`INSERT INTO rollup_id (hour, provider_id, event_id, n)
		SELECT time_ns / 3600000000000, provider_id, event_id, count(*) FROM events WHERE seq BETWEEN ? AND ? AND level <= 3 GROUP BY 1, 2, 3
		ON CONFLICT (hour, provider_id, event_id) DO UPDATE SET n = n + excluded.n`,
		`INSERT INTO logs (log_id, bytes)
		SELECT log_id, sum(bytes) FROM events WHERE seq BETWEEN ? AND ? GROUP BY 1
		ON CONFLICT (log_id) DO UPDATE SET bytes = coalesce(bytes, 0) + excluded.bytes`} {
		if _, err := tx.ExecContext(ctx, q, from, to); err != nil {
			return err
		}
	}
	return nil
}

// topIDs is the summaryTop event IDs most frequent among warnings and worse
// in the hours from the one 24 hours ago to now, the most frequent first.
func (s *Store) topIDs(ctx context.Context, now time.Time) ([]IDCount, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT p.name, r.event_id, sum(r.n) AS c FROM rollup_id r JOIN names p ON p.id = r.provider_id
		WHERE r.hour >= ? GROUP BY r.provider_id, r.event_id HAVING c > 0 ORDER BY c DESC, p.name, r.event_id LIMIT ?`,
		now.Add(-24*time.Hour).UnixNano()/int64(time.Hour), summaryTop)
	if err != nil {
		return nil, fmt.Errorf("store: summary: %w", err)
	}
	defer rows.Close()
	var out []IDCount
	for rows.Next() {
		var c IDCount
		if err := rows.Scan(&c.Provider, &c.ID, &c.N); err != nil {
			return nil, fmt.Errorf("store: summary: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
