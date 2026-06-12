package eval

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"sort"
)

// BaselineSchemaVersion is the on-disk shape of baseline.json. Bump on
// breaking changes — old baselines fail to load loudly, not silently.
const BaselineSchemaVersion = 1

// DefaultBootstrapN is the resample count for paired bootstrap CI on
// the nightly subset. v1 plan §1 trade-off table:
//   - 1000: nightly CI, statistical norm (Efron's recommendation)
//   - 200:  in-Job fitness calls (10× cheaper, well-converged for 10-
//     task fitness subsets)
//
// Per TEN-11 decision: 1000 default, callers pass Options.BootstrapN
// to override.
const DefaultBootstrapN = 1000

// Baseline is the persisted point-of-comparison for regression
// detection. Per-task scores are kept (not just category means) so the
// paired bootstrap has the raw data it needs.
type Baseline struct {
	SchemaVersion int                `json:"schema_version"`
	Subset        Subset             `json:"subset"`
	CapturedAt    string             `json:"captured_at"`
	JudgeProfile  string             `json:"judge_profile,omitempty"`
	TenantVersion string             `json:"tenant_version,omitempty"`
	TaskScores    map[string]float64 `json:"task_scores"` // id → score
	Overall       float64            `json:"overall"`
}

// NewBaseline snapshots a Report into a Baseline. Idempotent — taking
// the same Report twice produces equal Baselines. Ungraded tasks
// (judge unusable, TEN-197) are excluded: their scores are artifacts of
// grader failure and would seed meaningless reference points.
func NewBaseline(rep *Report, capturedAt, judgeProfile, tenantVersion string) *Baseline {
	scores := make(map[string]float64, len(rep.Results))
	for _, r := range rep.Results {
		if r.Ungraded {
			continue
		}
		scores[r.TaskID] = r.Score
	}
	return &Baseline{
		SchemaVersion: BaselineSchemaVersion,
		Subset:        rep.Subset,
		CapturedAt:    capturedAt,
		JudgeProfile:  judgeProfile,
		TenantVersion: tenantVersion,
		TaskScores:    scores,
		Overall:       rep.Aggregates.Overall,
	}
}

// WriteJSON serializes the baseline to JSON with stable indent so
// commits are diffable. Named WriteJSON (not WriteTo) to avoid
// shadowing the io.WriterTo interface — that interface's contract is
// (int64, error), not just error.
func (b *Baseline) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(b)
}

// ReadBaseline parses a baseline JSON blob and verifies the schema
// version matches what this binary supports.
func ReadBaseline(data []byte) (*Baseline, error) {
	var b Baseline
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("baseline: parse: %w", err)
	}
	if b.SchemaVersion != BaselineSchemaVersion {
		return nil, fmt.Errorf("baseline: schema version %d not supported (want %d)", b.SchemaVersion, BaselineSchemaVersion)
	}
	return &b, nil
}

// RegressionReport summarizes a baseline-vs-current comparison.
type RegressionReport struct {
	Regressed   bool     // overall CI suggests a real regression
	Delta       float64  // mean Δ across paired tasks (current - baseline)
	CILow       float64  // 95% CI low bound on Δ
	CIHigh      float64  // 95% CI high bound on Δ
	BootstrapN  int      // resample count used
	MissingIDs  []string // tasks in baseline absent from current
	NewIDs      []string // tasks in current absent from baseline (not regression-relevant)
	PairedCount int      // tasks present in both (the basis for Δ)
}

// CompareToBaseline runs a paired bootstrap CI on the per-task delta
// between current and baseline. Returns Regressed=true when the upper
// bound of the 95% CI on Δ is below 0 — i.e., we have ≥97.5% confidence
// the score actually dropped.
//
// Paired bootstrap shrinks variance vs unpaired: the same task IDs are
// the unit of resampling, so per-task noise cancels. With ~50 tasks we
// realistically detect ~12% category regressions (v1 plan §Risks).
//
// Reproducibility: pass a deterministic rand.Source via opts.RandSeed
// (non-zero seeds it; zero uses crypto-style entropy). Tests pass a
// fixed seed; production passes 0.
func CompareToBaseline(base *Baseline, current *Report, opts CompareOptions) *RegressionReport {
	n := opts.BootstrapN
	if n <= 0 {
		n = DefaultBootstrapN
	}
	rng := rand.New(rand.NewSource(opts.RandSeed))

	currentByID := make(map[string]float64, len(current.Results))
	for _, r := range current.Results {
		if r.Ungraded {
			// A judge-unusable task must not pair (TEN-197): its score is a
			// grader artifact, and pairing it against a real baseline score
			// would manufacture a fake regression.
			continue
		}
		currentByID[r.TaskID] = r.Score
	}

	// Find paired tasks (present in both).
	var pairedIDs []string
	var deltas []float64
	for id, baseScore := range base.TaskScores {
		if curScore, ok := currentByID[id]; ok {
			pairedIDs = append(pairedIDs, id)
			deltas = append(deltas, curScore-baseScore)
		}
	}
	sort.Strings(pairedIDs)

	// Missing/new IDs for diagnostics.
	var missing, newIDs []string
	for id := range base.TaskScores {
		if _, ok := currentByID[id]; !ok {
			missing = append(missing, id)
		}
	}
	for id := range currentByID {
		if _, ok := base.TaskScores[id]; !ok {
			newIDs = append(newIDs, id)
		}
	}
	sort.Strings(missing)
	sort.Strings(newIDs)

	rep := &RegressionReport{
		BootstrapN:  n,
		MissingIDs:  missing,
		NewIDs:      newIDs,
		PairedCount: len(deltas),
	}
	if len(deltas) == 0 {
		return rep // can't compare without paired tasks
	}

	rep.Delta = meanFloat(deltas)
	rep.CILow, rep.CIHigh = pairedBootstrap95(deltas, n, rng)
	// Regressed if the 97.5th percentile of Δ is still negative — i.e.
	// even the optimistic estimate of the change shows a drop.
	rep.Regressed = rep.CIHigh < 0
	return rep
}

// CompareOptions controls bootstrap behavior. Zero values give the
// sensible defaults (BootstrapN=1000, RandSeed=42 for reproducibility).
type CompareOptions struct {
	BootstrapN int
	RandSeed   int64
}

// pairedBootstrap95 computes the 95% CI on the mean of deltas by
// resampling task indices with replacement n times. The 2.5th and
// 97.5th percentile of resampled means are the bounds.
func pairedBootstrap95(deltas []float64, n int, rng *rand.Rand) (low, high float64) {
	if len(deltas) == 0 || n <= 0 {
		return 0, 0
	}
	means := make([]float64, n)
	k := len(deltas)
	for i := 0; i < n; i++ {
		var sum float64
		for j := 0; j < k; j++ {
			sum += deltas[rng.Intn(k)]
		}
		means[i] = sum / float64(k)
	}
	sort.Float64s(means)
	lowIdx := int(float64(n) * 0.025)
	highIdx := int(float64(n) * 0.975)
	if highIdx >= n {
		highIdx = n - 1
	}
	return means[lowIdx], means[highIdx]
}

func meanFloat(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}
