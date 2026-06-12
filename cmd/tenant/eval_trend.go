package main

// eval_trend.go is the nightly-eval trend log (TEN-158): a compact, append-only
// JSON-lines series of each run's score + regression verdict, and the
// `tenant eval --trend` reader over it. The full per-run artifacts stay the
// heavy source of truth; this series exists to persist the as-of-that-run
// delta/CI the artifacts discard, so the operator can see whether quality is
// trending and decide where the ceiling is (prompt vs routing vs memory).

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// evalTrendEntry is one line in trend.jsonl.
type evalTrendEntry struct {
	TS          string  `json:"ts"`
	Subset      string  `json:"subset"`
	Overall     float64 `json:"overall"`
	Passed      int     `json:"passed"`
	Total       int     `json:"total"`
	HasBaseline bool    `json:"has_baseline"`
	Regressed   bool    `json:"regressed"`
	Delta       float64 `json:"delta"`
	CIHigh      float64 `json:"ci_high"`
	Artifact    string  `json:"artifact,omitempty"`
}

func evalTrendPath(artifactDir string) string {
	return filepath.Join(artifactDir, "trend.jsonl")
}

// appendEvalTrend appends one entry as a single line. A <512B line written with
// one O_APPEND syscall is atomic in practice on macOS/Linux/Windows, so the
// rare nightly-vs-manual interleave can't tear a line. All failures are logged,
// never fatal — the run's value is the score, not this index.
func appendEvalTrend(artifactDir string, e evalTrendEntry, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		log.Warn("eval-trend: mkdir", "err", err)
		return
	}
	line, err := json.Marshal(e)
	if err != nil {
		log.Warn("eval-trend: marshal", "err", err)
		return
	}
	f, err := os.OpenFile(evalTrendPath(artifactDir), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Warn("eval-trend: open", "err", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil { // one syscall, one line
		log.Warn("eval-trend: write", "err", err)
	}
}

// latestTrendTime returns the newest entry timestamp in trend.jsonl, or the
// zero time when the log is missing/empty/unparseable. This is the durable
// "when did an eval last run" record (TEN-196): the scheduler seeds the eval
// job's clock from it so a relaunch doesn't re-fire a run that already
// happened — no separate state file needed, because every run (nightly or
// --append-trend) already lands here. Takes the max over all entries rather
// than the last line, since manual and nightly appends can interleave.
func latestTrendTime(artifactDir string) time.Time {
	entries, err := readEvalTrend(artifactDir)
	if err != nil {
		return time.Time{}
	}
	var latest time.Time
	for _, e := range entries {
		if ts, perr := time.Parse(time.RFC3339, e.TS); perr == nil && ts.After(latest) {
			latest = ts
		}
	}
	return latest
}

// readEvalTrend returns the trend entries oldest-first (skipping unparseable
// lines). A missing file is not an error — returns nil.
func readEvalTrend(artifactDir string) ([]evalTrendEntry, error) {
	f, err := os.Open(evalTrendPath(artifactDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []evalTrendEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		b := strings.TrimSpace(sc.Text())
		if b == "" {
			continue
		}
		var e evalTrendEntry
		if json.Unmarshal([]byte(b), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// renderEvalTrend formats the last n entries newest-first as a table.
func renderEvalTrend(entries []evalTrendEntry, n int) string {
	if len(entries) == 0 {
		return "no eval trend yet — arm nightly eval (--eval-every or improve.eval_every) to start the series"
	}
	if n <= 0 {
		n = 20
	}
	if len(entries) > n {
		entries = entries[len(entries)-n:]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "eval trend (last %d):\n", len(entries))
	fmt.Fprintf(&b, "  %-20s %-8s %-7s %-7s %-9s %s\n", "WHEN (UTC)", "SUBSET", "OVERALL", "PASS", "Δ", "STATUS")
	for i := len(entries) - 1; i >= 0; i-- { // newest first
		e := entries[i]
		status := "—"
		if e.HasBaseline {
			if e.Regressed {
				status = "REGRESSION"
			} else {
				status = "ok"
			}
		}
		delta := "—"
		if e.HasBaseline {
			delta = fmt.Sprintf("%+.1f", e.Delta)
		}
		fmt.Fprintf(&b, "  %-20s %-8s %-7.1f %d/%-5d %-9s %s\n",
			clip(e.TS, 20), clip(e.Subset, 8), e.Overall, e.Passed, e.Total, delta, status)
	}
	return strings.TrimRight(b.String(), "\n")
}

// runEvalTrend implements `tenant eval --trend [-n N]` (offline; no model).
func runEvalTrend(c *commonFlags, n int) error {
	if err := c.resolveDirs(); err != nil {
		return err
	}
	entries, err := readEvalTrend(filepath.Join(c.dataDir, "eval-artifacts"))
	if err != nil {
		return fmt.Errorf("read eval trend: %w", err)
	}
	fmt.Fprintln(os.Stdout, renderEvalTrend(entries, n))
	return nil
}
