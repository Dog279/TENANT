package agent

// hysteresis.go is TEN-102 (Compaction P3, part b): a Schmitt-trigger replacing
// the single-line compaction trigger so compaction doesn't re-fire every turn
// near the threshold.
//
// Why measure the WORKING tier (not total budget usage): compaction only shrinks
// the working set; retrieved facts + episodes survive it. If the trigger watched
// total usage, a full retrieval floor (~0.35 of budget) plus a compacted working
// set (~0.17) could sit above any sane low watermark, so the trigger would arm
// and NEVER disarm — silently latching compaction off mid-session. Watching
// working/workingSlot makes disarm correct by construction: a successful
// compaction drops the working tier to a small fraction of its slot, always
// below the low watermark.
//
// Why LEVEL-triggered (return armed each call, not just on the rising edge): if
// a compaction fails or is insufficient (summarizer briefly down → the compactor
// returns the set unchanged), staying armed retries next turn instead of
// latching off forever. There's no thrash, because a successful compaction
// disarms in one shot and the compactor cheaply no-ops (no LLM call) when the
// head is too small to compact.

import "sync"

// compaction watermarks, as a fraction of the working-tier slot.
const (
	compactionHighFrac = 0.70 // arm: recommend compaction at/above this working-tier fill
	compactionLowFrac  = 0.40 // disarm: stop recommending once compacted to/below this
)

// hysteresis is the compaction trigger's Schmitt state. armed is mutated ONLY
// from the agent's post-turn defer (turns are serialized per agent); the mutex
// is defensive insurance for any future concurrent caller.
type hysteresis struct {
	mu        sync.Mutex
	high, low float64
	armed     bool
}

func newCompactionHysteresis() *hysteresis {
	return &hysteresis{high: compactionHighFrac, low: compactionLowFrac}
}

// shouldCompact updates the armed state from the current working-tier usage
// fraction and reports whether compaction is recommended this turn. It arms when
// usage crosses high and disarms only when usage falls to/below low; the gap
// between the two prevents re-firing every turn near the threshold.
func (h *hysteresis) shouldCompact(workingFrac float64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.armed {
		if workingFrac <= h.low {
			h.armed = false
		}
	} else if workingFrac >= h.high {
		h.armed = true
	}
	return h.armed
}
