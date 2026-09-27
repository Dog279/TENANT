package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"tenant/internal/logs/event"
	"tenant/internal/logs/query"
)

// Query returns up to limit events that match f, and the cursor to pass next
// time: the events after from, oldest first, or with reverse the events
// before it, newest first (the zero Cursor then starts at the newest).
// limit <= 0 means 1000. A cursor from another store restarts, and restarted
// says so.
func (s *Store) Query(ctx context.Context, f query.Filter, from Cursor, limit int, reverse bool) (evs []event.Event, next Cursor, restarted bool, err error) {
	if from.StoreID != s.id {
		restarted = from != Cursor{}
		from = Cursor{StoreID: s.id}
	}
	if limit <= 0 {
		limit = 1000
	}
	where, args, err := s.where(f, false)
	if err != nil {
		return nil, from, restarted, err
	}
	seqs, err := s.page(ctx, where, args, from.Seq, limit, reverse)
	if err != nil || len(seqs) == 0 {
		return nil, from, restarted, err
	}
	order := ""
	if reverse {
		order = " DESC"
	}
	list, _ := json.Marshal(seqs)
	evs, err = queryEvents(ctx, s.r, selectEvents+` WHERE e.seq IN (SELECT value FROM json_each(?)) ORDER BY e.seq`+order, string(list))
	if err != nil {
		return nil, from, restarted, fmt.Errorf("store: query: %w", err)
	}
	if next = from; len(evs) > 0 {
		next.Seq = evs[len(evs)-1].Seq
	}
	return evs, next, restarted, nil
}

// pageWindow is how many seqs a page scans at a time, in order from the
// cursor, before it turns to the indexes; a variable for the tests.
var pageWindow int64 = 20_000

// pageWindows caps that scan.
const pageWindows = 4

// page finds the seqs of one page of Query. With no statistics, SQLite reads
// a filter through its likeliest index and sorts every candidate by seq,
// which is slow when they are many (log:turns can be 40% of the book). So a
// page first scans the seqs next to the cursor, in order, where a common
// match turns up at once. When matches are rare there, so that finishing by
// scanning would take more than pageWindows windows, the indexes find the
// rest.
func (s *Store) page(ctx context.Context, where string, args []any, from uint64, limit int, reverse bool) ([]int64, error) {
	var head int64
	if err := s.r.QueryRowContext(ctx, `SELECT coalesce(max(seq), 0) FROM events`).Scan(&head); err != nil {
		return nil, fmt.Errorf("store: query: %w", err)
	}
	edge := int64(from) // the page starts after it, or before it with reverse
	scan := `SELECT e.seq FROM events e NOT INDEXED WHERE (` + where + `) AND e.seq > ? AND e.seq <= ? ORDER BY e.seq LIMIT ?`
	rest := `SELECT e.seq FROM events e WHERE (` + where + `) AND e.seq > ? ORDER BY e.seq LIMIT ?`
	if reverse {
		if edge == 0 || edge > head {
			edge = head + 1
		}
		scan = `SELECT e.seq FROM events e NOT INDEXED WHERE (` + where + `) AND e.seq >= ? AND e.seq < ? ORDER BY e.seq DESC LIMIT ?`
		rest = `SELECT e.seq FROM events e WHERE (` + where + `) AND e.seq < ? ORDER BY e.seq DESC LIMIT ?`
	}
	var out []int64
	collect := func(q string, a ...any) error {
		rows, err := s.r.QueryContext(ctx, q, append(args[:len(args):len(args)], a...)...)
		if err != nil {
			return fmt.Errorf("store: query: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var seq int64
			if err := rows.Scan(&seq); err != nil {
				return fmt.Errorf("store: query: %w", err)
			}
			out = append(out, seq)
		}
		return rows.Err()
	}
	for i := int64(1); len(out) < limit; i++ {
		lo, hi := edge, edge+pageWindow
		if reverse {
			lo, hi = edge-pageWindow, edge
		}
		if err := collect(scan, lo, hi, limit-len(out)); err != nil {
			return nil, err
		}
		if edge = hi; reverse {
			edge = lo
		}
		if reverse && edge <= 1 || !reverse && edge >= head {
			return out, nil // scanned to the end
		}
		// The windows left can't finish at the rate matches turned up.
		if len(out) == 0 || int64(limit-len(out))*i/int64(len(out)) > pageWindows-i {
			break
		}
	}
	if len(out) < limit {
		if err := collect(rest, edge, limit-len(out)); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Count is how many events match f.
func (s *Store) Count(ctx context.Context, f query.Filter) (int64, error) {
	where, args, err := s.where(f, false)
	if err != nil {
		return 0, err
	}
	var n int64
	if err := s.r.QueryRowContext(ctx, `SELECT count(*) FROM events e WHERE `+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count: %w", err)
	}
	return n, nil
}

// Bucket counts the events of one log at one level in one span of time.
type Bucket struct {
	Start time.Time // the span's start, UTC; zero for Stats with no bucket
	Log   string
	Level event.Level
	N     int64
}

// Stats counts the events that match f by span, log and level, in time
// order. Spans are bucket long, in whole minutes from the Unix epoch; bucket
// <= 0 counts each log and level once over all time. A filter on log, level,
// since and until only is answered from the rollups, whose times are whole
// minutes: a since or until inside a minute counts that whole minute. Any
// other filter counts the events themselves.
func (s *Store) Stats(ctx context.Context, f query.Filter, bucket time.Duration) ([]Bucket, error) {
	return s.stats(ctx, f, bucket, rollupable(f))
}

func (s *Store) stats(ctx context.Context, f query.Filter, bucket time.Duration, rollup bool) ([]Bucket, error) {
	where, args, err := s.where(f, rollup)
	if err != nil {
		return nil, err
	}
	// The matching rows are gathered first, so SQLite plans the filter on its
	// own instead of walking an index in the grouping's order.
	unit, src := int64(1), `MATERIALIZED (SELECT e.time_ns AS t, e.log_id, e.level, 1 AS n FROM events e WHERE `+where+`)`
	if rollup {
		unit, src = int64(time.Minute), `NOT MATERIALIZED (SELECT r.minute AS t, r.log_id, r.level, r.n FROM rollup r WHERE `+where+`)`
	}
	span := "0"
	if bucket > 0 {
		b := max(bucket.Truncate(time.Minute), time.Minute).Nanoseconds() / unit
		span, args = "(m.t / ?) * ?", append(args, b, b)
	}
	rows, err := s.r.QueryContext(ctx, `WITH m AS `+src+` SELECT `+span+`, n.name, m.level, sum(m.n)
		FROM m JOIN names n ON n.id = m.log_id GROUP BY 1, m.log_id, m.level ORDER BY 1, n.name, m.level`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: stats: %w", err)
	}
	defer rows.Close()
	var out []Bucket
	for rows.Next() {
		var b Bucket
		var start int64
		if err := rows.Scan(&start, &b.Log, &b.Level, &b.N); err != nil {
			return nil, fmt.Errorf("store: stats: %w", err)
		}
		if bucket > 0 {
			b.Start = time.Unix(0, start*unit).UTC()
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// IDCount is how many times one event ID occurred.
type IDCount struct {
	Provider string
	ID       uint32
	N        int64
}

// TopIDs is the n event IDs (10 when n <= 0) that occur most among the
// events matching f, the most frequent first.
func (s *Store) TopIDs(ctx context.Context, f query.Filter, n int) ([]IDCount, error) {
	if n <= 0 {
		n = 10
	}
	where, args, err := s.where(f, false)
	if err != nil {
		return nil, err
	}
	// As in stats, the matching rows first: grouping in events_id's order
	// would walk the whole index.
	rows, err := s.r.QueryContext(ctx, `WITH m AS MATERIALIZED (SELECT e.provider_id, e.event_id FROM events e WHERE `+where+`)
		SELECT p.name, m.event_id, count(*) AS c FROM m JOIN names p ON p.id = m.provider_id
		GROUP BY m.provider_id, m.event_id ORDER BY c DESC, p.name, m.event_id LIMIT ?`, append(args, n)...)
	if err != nil {
		return nil, fmt.Errorf("store: top IDs: %w", err)
	}
	defer rows.Close()
	var out []IDCount
	for rows.Next() {
		var c IDCount
		if err := rows.Scan(&c.Provider, &c.ID, &c.N); err != nil {
			return nil, fmt.Errorf("store: top IDs: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Timeline bounds: how far down related_id it follows, and how many events it
// returns.
const (
	MaxTimelineDepth = 16
	MaxTimeline      = 10_000
)

// Timeline is the pivot (§7): every event of the turn or activity id, across
// all logs, oldest first. It follows related_id down depth levels (at most
// MaxTimelineDepth): an activity started under id belongs to it, and so on,
// so a turn reaches its tool calls, their sandbox runs and the runs' child
// output. It returns at most MaxTimeline events, the oldest.
func (s *Store) Timeline(ctx context.Context, id string, depth int) ([]event.Event, error) {
	if id == "" {
		return nil, nil
	}
	depth = min(max(depth, 0), MaxTimelineDepth)
	evs, err := queryEvents(ctx, s.r, `WITH RECURSIVE ids(id, depth) AS (
		SELECT ?, 0
		UNION
		SELECT e.activity_id, ids.depth + 1 FROM ids JOIN events e ON e.related_id = ids.id
		WHERE ids.depth < ? AND e.activity_id IS NOT NULL)
		`+selectEvents+` WHERE e.seq IN (
		SELECT seq FROM events WHERE trace_id IN (SELECT id FROM ids)
		UNION SELECT seq FROM events WHERE activity_id IN (SELECT id FROM ids)
		UNION SELECT seq FROM events WHERE related_id IN (SELECT id FROM ids))
		ORDER BY e.seq LIMIT ?`, idValue(id), depth, MaxTimeline)
	if err != nil {
		return nil, fmt.Errorf("store: timeline: %w", err)
	}
	return evs, nil
}

// IDStats is how often one event ID has occurred: "has this happened
// before?" (§9.1, question 7).
type IDStats struct {
	Count       int64
	First, Last time.Time // UTC; zero when it never occurred
	Trend       [7]int64  // per day over the last 7, oldest first: Trend[6] is the last 24 hours
}

// EventStats counts provider's event id in the store, with its first and
// last sightings and its trend over the last 7 days.
func (s *Store) EventStats(ctx context.Context, provider string, id uint32) (IDStats, error) {
	var st IDStats
	var first, last sql.NullInt64
	const match = `provider_id = (SELECT id FROM names WHERE kind = 'provider' AND name = ?) AND event_id = ?`
	if err := s.r.QueryRowContext(ctx, `SELECT count(*), min(time_ns), max(time_ns) FROM events WHERE `+match,
		provider, id).Scan(&st.Count, &first, &last); err != nil {
		return st, fmt.Errorf("store: event stats: %w", err)
	}
	if st.Count == 0 {
		return st, nil
	}
	st.First, st.Last = time.Unix(0, first.Int64).UTC(), time.Unix(0, last.Int64).UTC()
	day := int64(24 * time.Hour)
	start := time.Now().UnixNano() - 7*day
	rows, err := s.r.QueryContext(ctx, `SELECT min((time_ns - ?) / ?, 6), count(*) FROM events WHERE `+match+` AND time_ns >= ? GROUP BY 1`,
		start, day, provider, id, start)
	if err != nil {
		return st, fmt.Errorf("store: event stats: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var i, n int64
		if err := rows.Scan(&i, &n); err != nil {
			return st, fmt.Errorf("store: event stats: %w", err)
		}
		st.Trend[i] += n
	}
	return st, rows.Err()
}
