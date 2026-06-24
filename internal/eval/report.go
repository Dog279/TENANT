package eval

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// WriteTerminal renders a Report as a human-readable table to w. Used
// by `tenant eval` for the operator-visible summary; the JSON view
// (WriteJSON) is the machine-readable counterpart.
func WriteTerminal(w io.Writer, rep *Report) {
	fmt.Fprintf(w, "Eval run · subset=%s · %d tasks · %s\n\n", rep.Subset, len(rep.Results), rep.RanAt.Format("2006-01-02 15:04:05"))

	// Per-task table.
	fmt.Fprintln(w, "  ID                                       CATEGORY                   SCORE   ELAPSED")
	fmt.Fprintln(w, "  ---------------------------------------- -------------------------- ------- -------")
	for _, r := range rep.Results {
		mark := "✓"
		if !r.Passed {
			mark = "✗"
		}
		fmt.Fprintf(w, "%s %-40s %-26s %5.1f  %4dms\n",
			mark, truncate(r.TaskID, 40), truncate(r.Category, 26), r.Score, r.ElapsedMS)
		if r.Rollouts > 1 { // pass^k reliability (TEN-286)
			fmt.Fprintf(w, "    └─ pass^%d = %.2f (%d/%d rollouts passed)\n",
				r.Rollouts, PassFraction(r.RolloutsPassed, r.Rollouts), r.RolloutsPassed, r.Rollouts)
		}
		for _, f := range r.Failures {
			fmt.Fprintf(w, "    └─ %s\n", f)
		}
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Per-category:")
	cats := make([]string, 0, len(rep.Aggregates.PerCategory))
	for k := range rep.Aggregates.PerCategory {
		cats = append(cats, k)
	}
	sort.Strings(cats)
	for _, c := range cats {
		fmt.Fprintf(w, "  %-30s %5.1f\n", c, rep.Aggregates.PerCategory[c])
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "Overall: %.1f   Passed: %d/%d   Wall: %dms",
		rep.Aggregates.Overall,
		rep.Aggregates.PassCount,
		rep.Aggregates.PassCount+rep.Aggregates.FailCount,
		rep.Aggregates.TotalElapsed)
	if n := rep.Aggregates.UngradedCount; n > 0 {
		fmt.Fprintf(w, "   Ungraded: %d (judge unusable — excluded)", n)
	}
	if n := rep.Aggregates.SkippedCount; n > 0 {
		fmt.Fprintf(w, "   Skipped: %d (tools unavailable — excluded)", n)
	}
	fmt.Fprintln(w)
}

// WriteJSON emits the report as schema-versioned JSON. External
// consumers (CI dashboards, future web UI, GEPA fitness adapter) read
// schema_version first to detect breaking shape changes.
func WriteJSON(w io.Writer, rep *Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < 4 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

// AllPassed is a convenience for callers that just want a single bool.
func AllPassed(rep *Report) bool {
	return rep.Aggregates.FailCount == 0
}

// FailedTaskIDs returns the IDs of failed tasks, useful for CI summary
// lines. Ungraded (TEN-197) and skipped (TEN-198) tasks are not
// failures — they're excluded.
func FailedTaskIDs(rep *Report) []string {
	var out []string
	for _, r := range rep.Results {
		if !r.Passed && !r.Ungraded && !r.Skipped {
			out = append(out, r.TaskID)
		}
	}
	return out
}
