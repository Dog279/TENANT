package eval

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// SchemaVersion is the current report schema. Bumped on breaking changes
// to Report's JSON shape so external consumers (web UI, GEPA fitness
// adapter, dashboards) can detect mismatches at parse time.
const SchemaVersion = 1

// Harness holds the loaded task catalog plus the wiring the runner
// needs. Fixture-mode tasks need only Tasks + Log; live-mode tasks
// additionally require AgentFactory + Judge (set on the Harness as
// defaults, or passed per-call via RunWith).
//
// Concurrency: Harness is safe for concurrent Run / RunWith calls —
// the runMu serializes mutation-prone state and the Tasks slice is
// treated as read-only after LoadHarness.
type Harness struct {
	Tasks []*Task
	Log   *slog.Logger

	// AgentFactory is the default consulted for live-mode tasks. nil →
	// live tasks fail with a clear "factory not configured" message;
	// smoke tasks are unaffected. RunWith overrides this per-call.
	AgentFactory AgentFactory

	// Judge is the default that grades responses against anchored
	// rubrics for live-mode tasks. nil → live tasks pass the
	// deterministic gate only; judge score is recorded as 0 but the
	// task still passes if the gate is satisfied (gate-only mode is
	// useful for operators who want a quick live check without burning
	// judge tokens). RunWith overrides this per-call.
	Judge Judge

	// AvailableTools, when non-nil, is the set of tool names live in this
	// run's agent toolset (TEN-198). A LIVE task whose must_call names a
	// tool absent from the set is SKIPPED — recorded with a reason but
	// excluded from pass/fail aggregates and baselines, because a task
	// the environment cannot run measures the environment, not the
	// agent (~20 of 57 catalog tasks need plugins that eval never
	// auto-enables: interactive auth would hang a non-interactive run).
	// nil ⇒ no skipping: fixture-only runs, tests, and older callers
	// behave exactly as before.
	AvailableTools map[string]struct{}

	// runMu serializes Run / RunWith. The per-task work is sequential
	// anyway (rollouts share the agent + judge state); the mutex
	// prevents two concurrent callers from mutating Tasks-affecting
	// state mid-run. v1 plan §Architecture finding 1E.
	runMu sync.Mutex
}

// Report is the full output of one eval run. Schema-versioned for
// stable external consumption.
type Report struct {
	SchemaVersion int          `json:"schema_version"`
	RanAt         time.Time    `json:"ran_at"`
	Subset        Subset       `json:"subset"`
	Results       []TaskResult `json:"results"`
	Aggregates    Aggregates   `json:"aggregates"`
}

// Aggregates summarizes Results across categories. Per-task scores are
// kept in Results so Phase 4's paired bootstrap has the raw data.
type Aggregates struct {
	Overall      float64            `json:"overall"`
	PerCategory  map[string]float64 `json:"per_category"`
	PerSubset    map[string]float64 `json:"per_subset"`
	PassCount    int                `json:"pass_count"`
	FailCount    int                `json:"fail_count"`
	TotalElapsed int64              `json:"total_elapsed_ms"`
	// UngradedCount is how many tasks were excluded from the counts and
	// scores above because the judge was unusable after its retry
	// (TEN-197) — surfaced so a run with grader trouble is visibly
	// different from a clean one.
	UngradedCount int `json:"ungraded_count,omitempty"`
	// SkippedCount is how many tasks were excluded because the run's
	// environment lacks a tool they require (TEN-198) — the denominator
	// only counts tasks this machine can actually attempt.
	SkippedCount int `json:"skipped_count,omitempty"`
}

// LoadHarness loads every embedded task. Caller provides the FS so
// tests can supply a synthetic catalog.
func LoadHarness(fsys fs.FS, log *slog.Logger) (*Harness, error) {
	if log == nil {
		log = slog.Default()
	}
	tasks, err := LoadTasksFromFS(fsys, "tasks")
	if err != nil {
		return nil, fmt.Errorf("eval: load tasks: %w", err)
	}
	// Deterministic order for reproducible reports.
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	return &Harness{Tasks: tasks, Log: log}, nil
}

// FilterSubset returns the tasks belonging to sub, in load order. full (and
// the empty subset) means the ENTIRE catalog — it's the comprehensive nightly
// suite, so it includes the smoke + fitness tasks rather than a separate
// "full"-tagged slice (which would otherwise be empty). smoke and fitness are
// curated exact-match slices.
func (h *Harness) FilterSubset(sub Subset) []*Task {
	if sub == "" || sub == SubsetFull {
		return h.Tasks
	}
	out := make([]*Task, 0, len(h.Tasks))
	for _, t := range h.Tasks {
		if t.Subset == sub {
			out = append(out, t)
		}
	}
	return out
}

// Run executes the named subset against the Harness's configured
// AgentFactory + Judge defaults. Equivalent to RunWith with those
// defaults plugged in.
func (h *Harness) Run(ctx context.Context, sub Subset) (*Report, error) {
	return h.RunWith(ctx, sub, h.AgentFactory, h.Judge)
}

// RunWith is the Job-callable fitness API. Downstream consumers
// (SoulNudgeJob, SkillInductionJob, distill.Supersede, future GEPA)
// supply per-call factory + judge so they can inject candidate state
// (e.g. a proposed Soul edit) without mutating shared Harness fields
// and racing other consumers.
//
// Concurrency: RunWith serializes via runMu. Concurrent callers queue
// rather than racing; each gets its own *Report. Per-task work inside
// one RunWith stays sequential — rollouts share judge/factory state.
//
// Phase 5 deliverable: this signature is the stable Job-callable
// surface. Future GEPA-style optimizers use exactly this entrypoint.
func (h *Harness) RunWith(ctx context.Context, sub Subset, factory AgentFactory, judge Judge) (*Report, error) {
	h.runMu.Lock()
	defer h.runMu.Unlock()

	// Temporarily swap in the per-call factory/judge. Restored at end
	// so the Harness's own defaults stay intact for the next caller.
	prevFactory, prevJudge := h.AgentFactory, h.Judge
	h.AgentFactory = factory
	h.Judge = judge
	defer func() {
		h.AgentFactory = prevFactory
		h.Judge = prevJudge
	}()

	return h.runLocked(ctx, sub)
}

func (h *Harness) runLocked(ctx context.Context, sub Subset) (*Report, error) {
	start := time.Now()
	tasks := h.FilterSubset(sub)
	rep := &Report{
		SchemaVersion: SchemaVersion,
		RanAt:         start,
		Subset:        sub,
		Results:       make([]TaskResult, 0, len(tasks)),
		Aggregates: Aggregates{
			PerCategory: make(map[string]float64),
			PerSubset:   make(map[string]float64),
		},
	}

	for _, t := range tasks {
		if ctx.Err() != nil {
			break
		}
		res := h.runOne(ctx, t)
		rep.Results = append(rep.Results, res)
	}

	rep.Aggregates = aggregate(rep.Results, time.Since(start).Milliseconds())
	return rep, nil
}

func (h *Harness) runOne(ctx context.Context, t *Task) TaskResult {
	start := time.Now()

	var res TaskResult
	switch t.Mode {
	case ModeFixture:
		res = ScoreFixture(t)
	case ModeLive:
		if missing := h.missingTool(t); missing != "" {
			// SKIPPED (TEN-198): the environment lacks a tool this task
			// requires, so the agent cannot pass by construction. Recorded
			// (never silently dropped) but excluded from aggregates and
			// baselines — same discipline as Ungraded.
			res = TaskResult{
				TaskID:     t.ID,
				Category:   t.Category,
				Skipped:    true,
				SkipReason: "tool unavailable: " + missing,
			}
			break
		}
		res = h.runLive(ctx, t)
	default:
		res = TaskResult{
			TaskID:   t.ID,
			Category: t.Category,
			Passed:   false,
			Failures: []string{fmt.Sprintf("unknown mode: %q", t.Mode)},
		}
	}
	res.ElapsedMS = time.Since(start).Milliseconds()
	return res
}

// missingTool returns the first must_call tool name absent from the
// run's AvailableTools set, or "" when the task is runnable. A nil set
// disables skipping entirely (TEN-198).
func (h *Harness) missingTool(t *Task) string {
	if h.AvailableTools == nil {
		return ""
	}
	for _, want := range t.Expected.MustCall {
		if _, ok := h.AvailableTools[want.Tool]; !ok {
			return want.Tool
		}
	}
	return ""
}

// aggregate reduces a Results slice into Aggregates. Weights are
// task-level; defaults to 1.0 (Phase 4 lifts adversarial to 2.0×).
// totalElapsedMS folds in the wall-clock measurement from the caller so
// every Aggregates field is populated by this function — no fragile
// post-assignment.
func aggregate(results []TaskResult, totalElapsedMS int64) Aggregates {
	ag := Aggregates{
		PerCategory:  make(map[string]float64),
		PerSubset:    make(map[string]float64),
		TotalElapsed: totalElapsedMS,
	}
	if len(results) == 0 {
		return ag
	}

	type bucket struct{ sum, n float64 }
	cat := make(map[string]*bucket)

	var totalSum, totalN float64
	for _, r := range results {
		if r.Skipped {
			// Environment gap, not agent failure (TEN-198): counted apart,
			// never in pass/fail or any score average.
			ag.SkippedCount++
			continue
		}
		if r.Ungraded {
			// Grader failure, not agent failure (TEN-197): counted apart,
			// never in pass/fail or any score average.
			ag.UngradedCount++
			continue
		}
		if r.Passed {
			ag.PassCount++
		} else {
			ag.FailCount++
		}
		totalSum += r.Score
		totalN++
		b, ok := cat[r.Category]
		if !ok {
			b = &bucket{}
			cat[r.Category] = b
		}
		b.sum += r.Score
		b.n++
	}
	if totalN > 0 {
		ag.Overall = totalSum / totalN
	}
	for name, b := range cat {
		ag.PerCategory[name] = b.sum / b.n
	}
	return ag
}
