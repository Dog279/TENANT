package agent

// expand.go is TEN-104 (Compaction P4.6): reversibility + audit. A compaction
// summary stamps its Meta with the archive source-range it replaced;
// ExpandLatestCompaction rehydrates that span from the append-only archive so a
// `/expand` TUI command and a dashboard affordance can SHOW the original turns
// on demand — compaction becomes "paged out, not lost." Read-only: it does NOT
// mutate the working set (re-inserting the span would re-arm compaction and
// double-count provenance), so restore-into-context is a deliberate non-goal.

import (
	"context"
	"time"

	"tenant/internal/memory/archive"
	"tenant/internal/memory/compress"
	"tenant/internal/memory/working"
)

// expandMaxEvents bounds how many archived turns a single expansion collects, so
// a huge compacted span can't produce an unbounded result.
const expandMaxEvents = 500

// CompactionExpansion is the rehydrated provenance of the latest compaction
// summary: its text, the source range it covered, and the original archived
// turns within that range.
type CompactionExpansion struct {
	Summary string
	Source  CompactionSource
	Events  []ExpandedEvent
}

// CompactionSource is the summary's stamped provenance (compress §4.3.3).
type CompactionSource struct {
	SessionID string
	After     time.Time // inclusive start of the summarized span
	Before    time.Time // exclusive end (the tail that was kept)
	MsgCount  int       // how many messages the summary covered
	Origin    string    // "working" | "archive"
}

// ExpandedEvent is one rehydrated archived turn.
type ExpandedEvent struct {
	When    time.Time
	Role    string
	Content string
}

// ExpandLatestCompaction rehydrates the most recent compaction summary's source
// span from the archive. Returns (nil, nil) when nothing has been compacted yet
// (no summary in the working set). Read-only.
//
// Boundary reconstruction mirrors compress.collectSpan: archive.Filter bounds
// are EXCLUSIVE and source_after is the inclusive first event truncated to
// seconds, so we (a) don't set Filter.After (apply the inclusive lower bound in
// code instead) and (b) pad the exclusive upper bound by 1s so a boundary event
// isn't clipped.
func (a *Agent) ExpandLatestCompaction(_ context.Context) (*CompactionExpansion, error) {
	if a.cfg.Working == nil {
		return nil, nil
	}
	msgs := a.cfg.Working.Messages()
	var summary *working.Message
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Kind == compress.KindCompactionSummary {
			m := msgs[i]
			summary = &m
			break
		}
	}
	if summary == nil {
		return nil, nil // nothing compacted yet
	}

	src := CompactionSource{
		SessionID: metaString(summary.Meta, "source_session"),
		MsgCount:  int(metaInt64(summary.Meta, "source_msg_count")),
		Origin:    metaString(summary.Meta, "source_origin"),
	}
	if sa := metaInt64(summary.Meta, "source_after"); sa > 0 {
		src.After = time.Unix(sa, 0)
	}
	if sb := metaInt64(summary.Meta, "source_before"); sb > 0 {
		src.Before = time.Unix(sb, 0)
	}

	exp := &CompactionExpansion{Summary: summary.Content, Source: src}
	if a.cfg.Archive == nil {
		return exp, nil // provenance only; no archive to rehydrate from
	}

	filter := archive.Filter{SessionID: src.SessionID}
	if !src.Before.IsZero() {
		filter.Before = src.Before.Add(time.Second) // pad the exclusive, seconds-truncated bound
	}
	for ev, err := range a.cfg.Archive.Reader().Stream(filter) {
		if err != nil {
			break
		}
		if !src.After.IsZero() && ev.Timestamp.Before(src.After) {
			continue // inclusive lower bound (Filter.After is exclusive)
		}
		exp.Events = append(exp.Events, ExpandedEvent{When: ev.Timestamp, Role: ev.Role, Content: ev.Content})
		if len(exp.Events) >= expandMaxEvents {
			break
		}
	}
	return exp, nil
}

// metaString reads a string Meta value ("" if absent / wrong type).
func metaString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// metaInt64 reads an integer Meta value, tolerating int64 / int / float64 — the
// live working set holds int64, but a JSON-rehydrated set (or a test fake) may
// carry float64.
func metaInt64(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	default:
		return 0
	}
}
