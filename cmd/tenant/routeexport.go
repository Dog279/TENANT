package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tenant/internal/memory/episodic"
	"tenant/internal/route"
)

// Dataset export (TEN-292): `tenant route export-dataset` joins the durable
// decision JSONL against episodic outcomes and writes Supra-format training
// rows. This file owns the I/O and the join; label derivation and row
// formatting live in internal/route (pure, tested there).

const exportDatasetName = "route-dataset.jsonl"

type exportOptions struct {
	Out          string
	Since        time.Duration
	IncludeRelay bool
}

type exportCounts struct {
	Exported      int
	Gold          int
	SkippedUnjoin int // decision has no episode carrying its id
	SkippedNoRow  int // joined but yields no training row (no signal / big+fail)
	ExcludedRelay int
	ExcludedOld   int
	MalformedRows int
}

// exportRouteDataset performs the full export. It reads (never rewrites) the
// decision log, so it is safe to run against a live tenant.
func exportRouteDataset(ctx context.Context, dataDir string, opts exportOptions) (string, error) {
	if dataDir == "" {
		return "", errors.New("route: export: data dir unavailable")
	}
	decisions, malformed, err := readDecisionLog(filepath.Join(dataDir, route.DecisionLogName))
	if err != nil {
		return "", err
	}
	if len(decisions) == 0 {
		return "", errors.New("route: export: no decisions recorded yet (run some turns with routing enabled first)")
	}

	episodes, err := exportEpisodes(ctx, dataDir)
	if err != nil {
		return "", err
	}
	// Join key: the route:decision=<id> episode tag (the TEN-289 telemetry
	// contract). Prompt text comes from the episode; the decision row only
	// ever has the hash.
	byDecision := make(map[string]*episodic.Episode, len(episodes))
	for _, ep := range episodes {
		if ep == nil {
			continue
		}
		for _, tag := range ep.Tags {
			if id := strings.TrimPrefix(tag, route.TagDecisionPrefix); id != tag && id != "" {
				byDecision[id] = ep
			}
		}
	}

	var cutoff time.Time
	if opts.Since > 0 {
		cutoff = time.Now().Add(-opts.Since)
	}

	counts := exportCounts{MalformedRows: malformed}
	rows := make([]route.TrainingRow, 0, len(decisions))
	for _, d := range decisions {
		if !cutoff.IsZero() && d.Timestamp.Before(cutoff) {
			counts.ExcludedOld++
			continue
		}
		// TEN-256 posture: relay-ingested content stays out of training data
		// unless the operator explicitly opts it in. Federated knowledge never
		// becomes an episode with route tags, so the relay surface is the one
		// ingestion channel to fence here.
		if !opts.IncludeRelay && d.Surface == "relay" {
			counts.ExcludedRelay++
			continue
		}
		ep, ok := byDecision[d.ID]
		if !ok {
			counts.SkippedUnjoin++
			continue
		}
		outcome, _ := route.TurnOutcomeFromTags(ep.Tags, ep.Outcome)
		row, err := route.DeriveRow(route.ExportJoin{
			Decision: d, Prompt: ep.Prompt, Outcome: ep.Outcome, Applied: outcome.Applied,
		})
		if err != nil {
			counts.SkippedNoRow++
			continue
		}
		if row.Source == route.ExportSourceGold {
			counts.Gold++
		}
		rows = append(rows, row)
		counts.Exported++
	}

	out := strings.TrimSpace(opts.Out)
	if out == "" {
		out = filepath.Join(dataDir, exportDatasetName)
	}
	if err := writeTrainingRows(out, rows); err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "route dataset exported: %d row(s) (%d gold) -> %s\n", counts.Exported, counts.Gold, out)
	fmt.Fprintf(&b, "  decisions read: %d\n", len(decisions))
	fmt.Fprintf(&b, "  skipped, no joinable episode: %d\n", counts.SkippedUnjoin)
	fmt.Fprintf(&b, "  skipped, no derivable row: %d\n", counts.SkippedNoRow)
	fmt.Fprintf(&b, "  excluded, relay surface (use --include-relay to opt in): %d\n", counts.ExcludedRelay)
	if counts.ExcludedOld > 0 {
		fmt.Fprintf(&b, "  excluded, older than --since: %d\n", counts.ExcludedOld)
	}
	if counts.MalformedRows > 0 {
		fmt.Fprintf(&b, "  malformed decision rows ignored: %d\n", counts.MalformedRows)
	}
	b.WriteString("  next: fine-tune the checkpoint on this JSONL, `ollama pull` the result, and point route.provider.model at it")
	return b.String(), nil
}

func readDecisionLog(path string) ([]route.Decision, int, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("route: export: read decision log: %w", err)
	}
	var decisions []route.Decision
	malformed := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var d route.Decision
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			malformed++
			continue
		}
		decisions = append(decisions, d)
	}
	return decisions, malformed, nil
}

func exportEpisodes(ctx context.Context, dataDir string) ([]*episodic.Episode, error) {
	path := filepath.Join(dataDir, "episodes.db")
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("route: export: episodes store unavailable at %s (the join needs episode prompts): %w", path, err)
	}
	store, err := episodic.Open(path)
	if err != nil {
		return nil, fmt.Errorf("route: export: open episodes store: %w", err)
	}
	defer store.Close()
	listCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	episodes, err := store.List(listCtx, episodic.ListFilter{})
	if err != nil {
		return nil, fmt.Errorf("route: export: list episodes: %w", err)
	}
	return episodes, nil
}

func writeTrainingRows(path string, rows []route.TrainingRow) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("route: export: create output dir: %w", err)
		}
	}
	var b strings.Builder
	for _, row := range rows {
		line, err := json.Marshal(row)
		if err != nil {
			return fmt.Errorf("route: export: encode row: %w", err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("route: export: write dataset: %w", err)
	}
	return nil
}
