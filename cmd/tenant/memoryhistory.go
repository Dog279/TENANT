package main

// memoryhistory.go (TEN-255 Phase 2): the "as of date X" recall tool. Where
// memory_search retrieves the CURRENT best-matching facts, memory_history
// reconstructs the historical state — the facts that were valid at a past
// moment, INCLUDING ones later superseded — over the bi-temporal validity
// recorded by supersession-as-transition. Pure local read; registered enabled
// alongside memory_search (strictly additive — when no facts have temporal
// bounds it just returns the current set as-of "now").

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"tenant/internal/memory/semantic"
	"tenant/internal/model"
)

type memoryHistoryArgs struct {
	AsOf  string `json:"as_of"`
	Query string `json:"query"`
	K     int    `json:"k"`
}

type memoryHistoryDispatcher struct {
	selfName string
	semantic *semantic.Store
}

func newMemoryHistoryDispatcher(name string, sem *semantic.Store) *memoryHistoryDispatcher {
	if name == "" {
		name = "this tenant"
	}
	return &memoryHistoryDispatcher{selfName: name, semantic: sem}
}

func (d *memoryHistoryDispatcher) Tools() []model.ToolSpec {
	return []model.ToolSpec{{
		Name:        "memory_history",
		Description: "Recall what you knew AS OF a past date — the historical state of your long-term facts, including ones later superseded/changed. Use for 'what did we believe/decide back in <month>', 'what was the plan before X changed', or to see how knowledge evolved. Provide as_of as a date.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"as_of":{"type":"string","description":"the point in time, e.g. \"2026-01-15\" or an RFC3339 timestamp"},"query":{"type":"string","description":"optional keyword to filter the recalled facts"},"k":{"type":"integer","description":"max facts (default 20)"}},"required":["as_of"]}`),
	}}
}

func (d *memoryHistoryDispatcher) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	if call.Name != "memory_history" {
		return "unknown tool: " + call.Name, true, nil
	}
	var a memoryHistoryArgs
	if len(call.Arguments) > 0 {
		_ = json.Unmarshal(call.Arguments, &a)
	}
	asOf, ok := parseAsOf(strings.TrimSpace(a.AsOf))
	if !ok {
		return "as_of is required — give a date like 2026-01-15 or an RFC3339 timestamp", true, nil
	}
	if d.semantic == nil {
		return "(memory unavailable)", false, nil
	}
	k := a.K
	if k <= 0 {
		k = 20
	}
	// Overfetch then keyword-filter in Go, so an optional query still returns
	// up to k matches rather than k pre-filter rows.
	facts, err := d.semantic.FactsAsOf(ctx, "", asOf, k*4)
	if err != nil {
		return "memory_history failed: " + err.Error(), true, nil
	}
	filter := strings.ToLower(strings.TrimSpace(a.Query))
	var b strings.Builder
	fmt.Fprintf(&b, "## What %s knew as of %s\n", d.selfName, asOf.Format("2006-01-02"))
	n := 0
	for _, f := range facts {
		if filter != "" && !strings.Contains(strings.ToLower(f.Fact), filter) {
			continue
		}
		fmt.Fprintf(&b, "- %s (%s, confidence %.2f)\n", capSnippet(f.Fact), f.Visibility, f.Confidence)
		if n++; n >= k {
			break
		}
	}
	if n == 0 {
		return "(no facts recorded as of that date)", false, nil
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

// parseAsOf accepts a few common date/time layouts (interpreted UTC).
// time.RFC3339 already parses sub-second/nanosecond timestamps on input
// (Go's parser accepts a fractional-second field even when the layout omits
// it), so RFC3339Nano is not needed as a separate entry.
func parseAsOf(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
		"2006/01/02",
		"01/02/2006",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
