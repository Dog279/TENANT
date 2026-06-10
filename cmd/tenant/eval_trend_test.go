package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveEvalCadence(t *testing.T) {
	cases := []struct {
		name    string
		flagSet bool
		flagVal time.Duration
		cfg     string
		want    time.Duration
	}{
		{"flag set wins over config", true, 12 * time.Hour, "24h", 12 * time.Hour},
		{"flag set to 0 force-disables", true, 0, "24h", 0}, // explicit --eval-every 0
		{"flag absent uses config", false, 0, "24h", 24 * time.Hour},
		{"flag absent, empty config = off", false, 0, "", 0},
		{"flag absent, malformed config fails closed", false, 0, "nightly", 0},
		{"flag absent, bare number fails closed", false, 0, "24", 0},
		{"flag absent, zero config = off", false, 0, "0s", 0},
		{"flag absent, negative config = off", false, 0, "-5m", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveEvalCadence(tc.flagSet, tc.flagVal, tc.cfg, nil); got != tc.want {
				t.Errorf("resolveEvalCadence(%v,%v,%q) = %v, want %v", tc.flagSet, tc.flagVal, tc.cfg, got, tc.want)
			}
		})
	}
}

func TestEvalTrend_AppendReadRender(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "eval-artifacts")

	// Missing file → nil + a friendly render, no error.
	got, err := readEvalTrend(dir)
	if err != nil || got != nil {
		t.Fatalf("missing trend should be (nil,nil), got (%v,%v)", got, err)
	}
	if !strings.Contains(renderEvalTrend(nil, 20), "no eval trend yet") {
		t.Error("empty render should explain how to start the series")
	}

	appendEvalTrend(dir, evalTrendEntry{
		TS: "2026-06-09T10:00:00Z", Subset: "full", Overall: 88.0, Passed: 44, Total: 50,
		HasBaseline: true, Regressed: false, Delta: 1.2, CIHigh: 2.0, Artifact: "eval-1.json",
	}, nil)
	appendEvalTrend(dir, evalTrendEntry{
		TS: "2026-06-10T10:00:00Z", Subset: "full", Overall: 80.0, Passed: 40, Total: 50,
		HasBaseline: true, Regressed: true, Delta: -8.0, CIHigh: -3.0, Artifact: "eval-2.json",
	}, nil)
	appendEvalTrend(dir, evalTrendEntry{
		TS: "2026-06-11T10:00:00Z", Subset: "smoke", Overall: 100.0, Passed: 5, Total: 5,
		HasBaseline: false,
	}, nil)

	entries, err := readEvalTrend(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("want 3 entries, got %d", len(entries))
	}
	if entries[0].TS != "2026-06-09T10:00:00Z" || entries[2].Subset != "smoke" {
		t.Errorf("entries not oldest-first: %+v", entries)
	}

	out := renderEvalTrend(entries, 20)
	// Newest first.
	iSmoke := strings.Index(out, "smoke")
	iRegress := strings.Index(out, "REGRESSION")
	if iSmoke == -1 || iRegress == -1 || iSmoke > iRegress {
		t.Errorf("expected newest-first with REGRESSION + a no-baseline row:\n%s", out)
	}
	if !strings.Contains(out, "-8.0") {
		t.Errorf("expected the regression delta in the table:\n%s", out)
	}
	// No-baseline row shows — for its status (last column), not a false "ok".
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "smoke") {
			continue
		}
		fields := strings.Fields(line)
		if last := fields[len(fields)-1]; last != "—" {
			t.Errorf("no-baseline row status should be —, got %q in %q", last, line)
		}
	}

	// -n caps to the most recent.
	if n := strings.Count(renderEvalTrend(entries, 1), "20060102"); n != 0 {
		_ = n // (format sanity placeholder)
	}
	capped := renderEvalTrend(entries, 1)
	if strings.Contains(capped, "2026-06-09") {
		t.Errorf("-n 1 should drop the oldest entry:\n%s", capped)
	}
}
