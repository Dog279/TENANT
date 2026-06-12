package main

// eval_cadence_test.go covers the TEN-196 scheduling semantics: the anchor
// math, the restart-surviving due predicates, schedule resolution precedence,
// and the trend-seeded clock. The acceptance scenario being killed here: 30
// relaunches on a dev day must not fire 30 evals.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"tenant/internal/eval"
)

func TestParseEvalAt(t *testing.T) {
	cases := []struct {
		in     string
		hh, mm int
		ok     bool
	}{
		{"03:15", 3, 15, true},
		{"3:15", 3, 15, true}, // single-digit hour is unambiguous — accept
		{"00:00", 0, 0, true},
		{"23:59", 23, 59, true},
		{" 03:15 ", 3, 15, true},
		{"24:00", 0, 0, false},
		{"12:60", 0, 0, false},
		{"-1:30", 0, 0, false},
		{"0315", 0, 0, false},
		{"3:15:00", 0, 0, false},
		{"three:15", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, c := range cases {
		hh, mm, ok := parseEvalAt(c.in)
		if ok != c.ok || hh != c.hh || mm != c.mm {
			t.Errorf("parseEvalAt(%q) = (%d,%d,%v), want (%d,%d,%v)", c.in, hh, mm, ok, c.hh, c.mm, c.ok)
		}
	}
}

func TestMostRecentAnchor(t *testing.T) {
	loc := time.FixedZone("test", -8*3600)
	// After today's anchor → today's anchor.
	now := time.Date(2026, 6, 11, 10, 0, 0, 0, loc)
	if a := mostRecentAnchor(now, 3, 15); !a.Equal(time.Date(2026, 6, 11, 3, 15, 0, 0, loc)) {
		t.Errorf("10:00 vs 03:15 anchor = %v, want today 03:15", a)
	}
	// Before today's anchor → yesterday's anchor.
	now = time.Date(2026, 6, 11, 2, 0, 0, 0, loc)
	if a := mostRecentAnchor(now, 3, 15); !a.Equal(time.Date(2026, 6, 10, 3, 15, 0, 0, loc)) {
		t.Errorf("02:00 vs 03:15 anchor = %v, want yesterday 03:15", a)
	}
	// Exactly at the anchor → today's (not after now).
	now = time.Date(2026, 6, 11, 3, 15, 0, 0, loc)
	if a := mostRecentAnchor(now, 3, 15); !a.Equal(now) {
		t.Errorf("at-anchor = %v, want now itself", a)
	}
	// Month boundary: 00:30 on the 1st, anchor 03:15 → last day of prior month.
	now = time.Date(2026, 7, 1, 0, 30, 0, 0, loc)
	if a := mostRecentAnchor(now, 3, 15); !a.Equal(time.Date(2026, 6, 30, 3, 15, 0, 0, loc)) {
		t.Errorf("month boundary = %v, want Jun 30 03:15", a)
	}
}

func TestEvalAtDue(t *testing.T) {
	loc := time.FixedZone("test", 0)
	due := evalAtDue(3, 15)
	now := time.Date(2026, 6, 11, 9, 0, 0, 0, loc)

	// Missed-anchor catchup: box was asleep at 03:15, last run yesterday
	// 03:20 → due on the first tick after wake.
	if !due(time.Date(2026, 6, 10, 3, 20, 0, 0, loc), now) {
		t.Error("last run before today's anchor must be due (missed-anchor catchup)")
	}
	// Already ran after today's anchor → relaunches stay silent until tomorrow.
	if due(time.Date(2026, 6, 11, 3, 16, 0, 0, loc), now) {
		t.Error("run after today's anchor must NOT re-fire")
	}
	if due(now.Add(-time.Minute), now) {
		t.Error("just-ran must NOT re-fire")
	}
	// Never ran → due (bootstrap).
	if !due(time.Time{}, now) {
		t.Error("zero lastRun must be due")
	}
}

func TestEvalEveryDue(t *testing.T) {
	due := evalEveryDue(24 * time.Hour)
	now := time.Now()
	if due(now.Add(-time.Hour), now) {
		t.Error("1h-old run with 24h interval must NOT be due (the relaunch case)")
	}
	if !due(now.Add(-25*time.Hour), now) {
		t.Error("25h-old run with 24h interval must be due")
	}
	if !due(time.Time{}, now) {
		t.Error("zero lastRun must be due (bootstrap)")
	}
}

func TestResolveEvalDue_Precedence(t *testing.T) {
	// Explicit flag wins over both config values.
	due, tick, desc := resolveEvalDue(true, 6*time.Hour, "24h", "03:15", nil)
	if due == nil || tick != 6*time.Hour {
		t.Fatalf("flag-set: due=%v tick=%v (%s), want every-6h", due == nil, tick, desc)
	}
	// Flag explicitly 0 ⇒ off, even with config present.
	if due, _, _ := resolveEvalDue(true, 0, "24h", "03:15", nil); due != nil {
		t.Fatal("flag-set 0 must disarm regardless of config")
	}
	// Valid eval_at wins over eval_every; anchor mode contributes no tick hint.
	due, tick, desc = resolveEvalDue(false, 0, "24h", "03:15", nil)
	if due == nil || tick != 0 || desc != "daily at 03:15" {
		t.Fatalf("eval_at: due=%v tick=%v desc=%q, want anchor mode", due == nil, tick, desc)
	}
	// Malformed eval_at falls back to eval_every (fail-closed to the weaker schedule).
	due, tick, _ = resolveEvalDue(false, 0, "24h", "25:99", nil)
	if due == nil || tick != 24*time.Hour {
		t.Fatalf("malformed eval_at: due=%v tick=%v, want every-24h fallback", due == nil, tick)
	}
	// Nothing set ⇒ off.
	if due, _, _ := resolveEvalDue(false, 0, "", "", nil); due != nil {
		t.Fatal("no schedule configured must be off")
	}
	// Malformed everything ⇒ off (resolveEvalCadence fail-closed preserved).
	if due, _, _ := resolveEvalDue(false, 0, "soon", "later", nil); due != nil {
		t.Fatal("malformed eval_at AND eval_every must be off")
	}
}

func TestLatestTrendTime(t *testing.T) {
	tmp := t.TempDir()
	// Missing log → zero time (bootstrap: fires at first tick).
	if got := latestTrendTime(tmp); !got.IsZero() {
		t.Fatalf("missing trend: %v, want zero", got)
	}
	// Entries out of order + one malformed line: max valid TS wins.
	older := time.Date(2026, 6, 10, 3, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 6, 11, 3, 0, 0, 0, time.UTC)
	appendEvalTrend(tmp, evalTrendEntry{TS: newer.Format(time.RFC3339), Subset: "full"}, nil)
	appendEvalTrend(tmp, evalTrendEntry{TS: older.Format(time.RFC3339), Subset: "full"}, nil)
	appendEvalTrend(tmp, evalTrendEntry{TS: "not-a-timestamp", Subset: "full"}, nil)
	if got := latestTrendTime(tmp); !got.Equal(newer) {
		t.Fatalf("latestTrendTime = %v, want %v (max across entries)", got, newer)
	}
}

// trendEntryFor is shared by the nightly job and --append-trend; the contract
// is shape-identical lines. No baseline file ⇒ no verdict, never an error;
// empty artifact path ⇒ empty Artifact field (not filepath.Base's ".").
func TestTrendEntryFor(t *testing.T) {
	rep := &eval.Report{
		Subset:     eval.SubsetSmoke,
		Aggregates: eval.Aggregates{Overall: 87.5, PassCount: 7, FailCount: 1},
	}
	e := trendEntryFor(rep, filepath.Join(t.TempDir(), "missing.json"), "", nil)
	if e.HasBaseline || e.Regressed {
		t.Error("missing baseline must yield HasBaseline=false, Regressed=false")
	}
	if e.Artifact != "" {
		t.Errorf("empty artifact path must keep Artifact empty, got %q", e.Artifact)
	}
	if e.Subset != "smoke" || e.Overall != 87.5 || e.Passed != 7 || e.Total != 8 {
		t.Errorf("entry fields wrong: %+v", e)
	}
	if _, err := time.Parse(time.RFC3339, e.TS); err != nil {
		t.Errorf("TS not RFC3339: %q", e.TS)
	}
	// With an artifact path, only the base name is recorded.
	e = trendEntryFor(rep, "", filepath.Join("some", "dir", "eval-x.json"), nil)
	if e.Artifact != "eval-x.json" {
		t.Errorf("Artifact = %q, want base name", e.Artifact)
	}
}

// The manual --append-trend path and the trend-seeded clock compose: after a
// manual append, latestTrendTime is fresh, so evalEveryDue stands down — a
// workday operator's morning run replaces (not duplicates) the auto-fire.
func TestManualAppendAdvancesClock(t *testing.T) {
	tmp := t.TempDir()
	rep := &eval.Report{Subset: eval.SubsetSmoke, Aggregates: eval.Aggregates{Overall: 100, PassCount: 8}}
	artifact := writeEvalArtifact(tmp, rep, nil)
	if artifact == "" {
		t.Fatal("writeEvalArtifact failed")
	}
	if _, err := os.Stat(artifact); err != nil {
		t.Fatalf("artifact not on disk: %v", err)
	}
	appendEvalTrend(tmp, trendEntryFor(rep, "", artifact, nil), nil)

	seed := latestTrendTime(tmp)
	if seed.IsZero() {
		t.Fatal("trend seed should be set after manual append")
	}
	if due := evalEveryDue(24 * time.Hour); due(seed, time.Now()) {
		t.Error("fresh manual run must stand the 24h auto-fire down")
	}
}
