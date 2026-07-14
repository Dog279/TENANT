package route

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Decision-log retention (TEN-292). The JSONL log grows one row per turn
// forever; retention pruning bounds it without a database. Pruning is
// best-effort telemetry maintenance: it must never fail or delay a turn.

// DefaultLogRetentionDays bounds the durable decision log. 90 days holds the
// full graduation observation window several times over.
const DefaultLogRetentionDays = 90

const pruneInterval = 24 * time.Hour

// PruneDecisionLog drops rows older than cutoff, rewriting the file
// atomically. Malformed rows are RETAINED: a corrupt tail is an audit
// artifact, and retention must not silently destroy evidence. A missing file
// prunes nothing.
func PruneDecisionLog(path string, cutoff time.Time) (kept, dropped int, err error) {
	if path == "" {
		return 0, 0, nil
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("route: prune: read decision log: %w", err)
	}
	var out bytes.Buffer
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var d Decision
		if unmarshalErr := json.Unmarshal(line, &d); unmarshalErr == nil &&
			!d.Timestamp.IsZero() && d.Timestamp.Before(cutoff) {
			dropped++
			continue
		}
		kept++
		out.Write(line)
		out.WriteByte('\n')
	}
	if dropped == 0 {
		return kept, 0, nil
	}
	if err := replaceDecisionLog(path, out.Bytes()); err != nil {
		return kept, dropped, fmt.Errorf("route: prune: rewrite decision log: %w", err)
	}
	return kept, dropped, nil
}

// SetRetention arms daily time-gated pruning on the append path. days <= 0
// disables engine-driven pruning (the operator can still prune explicitly).
func (e *Engine) SetRetention(days int) {
	if e == nil {
		return
	}
	e.logMu.Lock()
	e.retentionDays = days
	e.logMu.Unlock()
}

// Prune immediately applies the configured retention. Used at startup wiring
// and by tests; the append path uses the same logic behind the daily gate.
func (e *Engine) Prune() (kept, dropped int, err error) {
	if e == nil {
		return 0, 0, nil
	}
	e.logMu.Lock()
	defer e.logMu.Unlock()
	return e.pruneLocked()
}

func (e *Engine) pruneLocked() (kept, dropped int, err error) {
	if e.retentionDays <= 0 || e.path == "" {
		return 0, 0, nil
	}
	now := time.Now()
	if e.now != nil {
		now = e.now()
	}
	e.lastPrune = now
	// AddDate, not a Duration multiply: a huge (misconfigured) day count must
	// saturate into the distant past, never overflow into "delete everything".
	cutoff := now.AddDate(0, 0, -e.retentionDays)
	if cutoff.After(now) { // AddDate overflowed a pathological day count
		return 0, 0, nil
	}
	return PruneDecisionLog(e.path, cutoff)
}

// pruneDueLocked reports whether the daily gate has elapsed and, when it has,
// stamps lastPrune so concurrent appends cannot schedule a second prune.
// Callers hold logMu.
func (e *Engine) pruneDueLocked() bool {
	if e.retentionDays <= 0 || e.path == "" {
		return false
	}
	now := time.Now()
	if e.now != nil {
		now = e.now()
	}
	if !e.lastPrune.IsZero() && now.Sub(e.lastPrune) < pruneInterval {
		return false
	}
	e.lastPrune = now
	return true
}
