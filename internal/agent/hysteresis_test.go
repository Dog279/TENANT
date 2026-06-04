package agent

import "testing"

// The Schmitt trigger: arm at high, stay armed through the (low,high) band,
// disarm only at/below low, then re-arm on the next high crossing.
func TestHysteresis_SchmittSemantics(t *testing.T) {
	h := newCompactionHysteresis() // high 0.70, low 0.40
	steps := []struct {
		frac float64
		want bool
		why  string
	}{
		{0.50, false, "below high, never armed → no compaction"},
		{0.69, false, "just under high → still no"},
		{0.70, true, "reaches high → arm + recommend"},
		{0.60, true, "in the (low,high) band while armed → stay armed (suppress re-eval thrash)"},
		{0.41, true, "just above low → still armed"},
		{0.40, false, "reaches low → disarm"},
		{0.55, false, "in-band but disarmed → no (needs a fresh high crossing)"},
		{0.70, true, "crosses high again → re-arm"},
		{0.10, false, "well below low → disarm"},
	}
	for i, s := range steps {
		if got := h.shouldCompact(s.frac); got != s.want {
			t.Errorf("step %d frac=%.2f: shouldCompact=%v want %v (%s)", i, s.frac, got, s.want, s.why)
		}
	}
}

// Hysteresis fires far fewer times than a single-line trigger when usage hovers
// near the threshold — the whole point (DoD: "fewer re-triggers"). The trace
// bakes in the compaction effect: each fire is followed by a drop to ~0.26 of
// the working slot (a successful compaction), then a slow climb back up.
func TestHysteresis_FewerReTriggersThanSingleLine(t *testing.T) {
	// Three grow→compact cycles. The drops to ~0.26 are what a real compaction
	// produces; the climbs model turns adding to the working set.
	trace := []float64{
		0.55, 0.62, 0.68, 0.72, // ramp 1 → crosses high at 0.72
		0.26, 0.34, 0.45, 0.58, 0.66, 0.71, // compacted, climbs back, crosses high at 0.71
		0.26, 0.38, 0.52, 0.63, 0.69, 0.74, // again
		0.26, 0.40, 0.61, // tail, never re-crosses high
	}

	// Old behavior: a single 0.60 line fires every turn usage is above it.
	singleLine := 0
	for _, f := range trace {
		if f > 0.60 {
			singleLine++
		}
	}

	// New behavior: the hysteresis fires only on each high crossing.
	h := newCompactionHysteresis()
	hyst := 0
	for _, f := range trace {
		if h.shouldCompact(f) {
			hyst++
		}
	}

	if hyst >= singleLine {
		t.Errorf("hysteresis fired %d times, single-line %d — hysteresis must fire FEWER", hyst, singleLine)
	}
	if hyst != 3 {
		t.Errorf("expected exactly 3 fires (one per high crossing), got %d", hyst)
	}
	t.Logf("re-triggers: single-line=%d, hysteresis=%d", singleLine, hyst)
}

// A successful compaction always disarms in one shot: after the trigger fires,
// the working tier drops well below the low watermark (the WHOLE design point of
// measuring the working tier — facts/episodes can't keep it armed).
func TestHysteresis_SuccessfulCompactionDisarms(t *testing.T) {
	h := newCompactionHysteresis()
	if !h.shouldCompact(1.00) { // working tier full → arm + fire
		t.Fatal("a full working tier must fire")
	}
	// Post-compaction the working tier is ~0.26 of its slot regardless of how
	// full facts/episodes are (they're a different tier the compactor can't see).
	if h.shouldCompact(0.26) {
		t.Error("after compaction drops the working tier to 0.26 of slot, the trigger must DISARM (not stick)")
	}
	// And it stays quiet until the working tier climbs back past high.
	if h.shouldCompact(0.65) {
		t.Error("a disarmed trigger must not re-fire in the band — needs a fresh high crossing")
	}
}

// A compaction that fails / under-shoots (working stays high) keeps the trigger
// armed so it RETRIES next turn — level-triggered, not edge-triggered (so a
// transient summarizer outage can't latch compaction off forever).
func TestHysteresis_RetriesWhileHigh(t *testing.T) {
	h := newCompactionHysteresis()
	if !h.shouldCompact(0.95) {
		t.Fatal("must arm + fire at 0.95")
	}
	for i := 0; i < 3; i++ {
		if !h.shouldCompact(0.92) { // compaction didn't reduce it (e.g. summarizer down)
			t.Errorf("retry %d: must STAY armed and keep recommending while working is still high", i)
		}
	}
}
