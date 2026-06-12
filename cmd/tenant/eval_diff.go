package main

// eval_diff.go renders the per-task movers analysis between a baseline and
// the newest eval artifact (TEN-198): which tasks improved, which declined
// and WHY (failure autopsy inline), and how much of the catalog is inert
// (stuck at zero in both runs, skipped, ungraded). This automates the hand
// analysis that decomposed the first real run's "+17.9, ok" into ~+15
// harness fix, ~+3 real improvement, and an outage misread as regressions.
// Surfaces: `tenant eval --baseline-diff` and `/eval diff` in the TUI.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"tenant/internal/eval"
)

// newestArtifact returns the path of the most recent eval-*.json report in
// dir. Timestamped names (eval-20060102-150405.json) sort lexically.
func newestArtifact(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("eval-diff: read artifact dir: %w", err)
	}
	newest := ""
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, "eval-") && strings.HasSuffix(n, ".json") && n > newest {
			newest = n
		}
	}
	if newest == "" {
		return "", fmt.Errorf("eval-diff: no eval-*.json artifacts in %s — run an eval first", dir)
	}
	return filepath.Join(dir, newest), nil
}

// renderBaselineDiff loads the newest artifact in artifactDir plus the
// baseline (explicit path, or baselines/<artifact-subset>.json) and renders
// the movers tables. Pure read — no model, no run.
func renderBaselineDiff(baselinePath, artifactDir string) (string, error) {
	artPath, err := newestArtifact(artifactDir)
	if err != nil {
		return "", err
	}
	artData, err := os.ReadFile(artPath)
	if err != nil {
		return "", fmt.Errorf("eval-diff: read artifact: %w", err)
	}
	var rep eval.Report
	if err := json.Unmarshal(artData, &rep); err != nil {
		return "", fmt.Errorf("eval-diff: parse artifact %s: %w", filepath.Base(artPath), err)
	}

	if baselinePath == "" {
		baselinePath = filepath.Join("baselines", string(rep.Subset)+".json")
	}
	baseData, err := os.ReadFile(baselinePath)
	if err != nil {
		return "", fmt.Errorf("eval-diff: read baseline %s: %w (capture one with --baseline-write)", baselinePath, err)
	}
	base, err := eval.ReadBaseline(baseData)
	if err != nil {
		return "", fmt.Errorf("eval-diff: parse baseline: %w", err)
	}

	type mover struct {
		delta, from, to float64
		res             eval.TaskResult
	}
	var movers []mover
	paired, unchanged, stuckZero, ungraded, skipped := 0, 0, 0, 0, 0
	for _, r := range rep.Results {
		if r.Skipped {
			skipped++
			continue
		}
		if r.Ungraded {
			ungraded++
			continue
		}
		baseScore, ok := base.TaskScores[r.TaskID]
		if !ok {
			continue // new since the baseline; not paired
		}
		paired++
		if r.Score == baseScore {
			unchanged++
			if r.Score == 0 {
				stuckZero++
			}
			continue
		}
		movers = append(movers, mover{delta: r.Score - baseScore, from: baseScore, to: r.Score, res: r})
	}
	sort.Slice(movers, func(i, j int) bool { return movers[i].delta > movers[j].delta })

	var b strings.Builder
	fmt.Fprintf(&b, "baseline: %s (overall %.1f, captured %s)\n", baselinePath, base.Overall, base.CapturedAt)
	fmt.Fprintf(&b, "artifact: %s (%s, overall %.1f)\n", filepath.Base(artPath), rep.Subset, rep.Aggregates.Overall)
	fmt.Fprintf(&b, "paired: %d · moved: %d · unchanged: %d (%d stuck at 0 in both)", paired, len(movers), unchanged, stuckZero)
	if ungraded > 0 {
		fmt.Fprintf(&b, " · ungraded now: %d (not paired)", ungraded)
	}
	if skipped > 0 {
		fmt.Fprintf(&b, " · skipped now: %d (not paired)", skipped)
	}
	b.WriteString("\n\nIMPROVED:\n")
	up := 0
	for _, m := range movers {
		if m.delta <= 0 {
			continue
		}
		up++
		fmt.Fprintf(&b, "  %+7.1f  %5.1f → %5.1f  %s  [%s]\n", m.delta, m.from, m.to, m.res.TaskID, m.res.Category)
	}
	if up == 0 {
		b.WriteString("  (none)\n")
	}
	b.WriteString("DECLINED:\n")
	down := 0
	for i := len(movers) - 1; i >= 0; i-- { // worst first
		m := movers[i]
		if m.delta >= 0 {
			continue
		}
		down++
		fmt.Fprintf(&b, "  %+7.1f  %5.1f → %5.1f  %s  [%s]\n", m.delta, m.from, m.to, m.res.TaskID, m.res.Category)
		// Autopsy inline: a decline caused by a network flake or grader
		// hiccup must be distinguishable from a real regression without
		// opening the JSON.
		if len(m.res.Failures) > 0 {
			fmt.Fprintf(&b, "          ↳ %s\n", clip(m.res.Failures[0], 110))
		}
		if jr := strings.TrimSpace(m.res.JudgeReasoning); jr != "" {
			fmt.Fprintf(&b, "          ↳ judge: %s\n", clip(jr, 110))
		}
	}
	if down == 0 {
		b.WriteString("  (none)\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}
