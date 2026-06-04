package eval

// passK is the τ-Bench reliability metric: of k independent rollouts of
// a single task, what fraction passed? A model that passes 4 of 5
// rollouts (pass^5 = 0.8) is meaningfully different from one that
// passes 5 of 5 — important for local models where flake is the
// dominant noise source. (Sierra τ-Bench, arXiv 2406.12045)
//
// v1 reports pass^k as a TaskResult field alongside the binary Passed.
// Downstream consumers can choose the strictness threshold (e.g.
// SoulNudgeJob may want pass^5 ≥ 0.8 before accepting an edit).
func passK(rollouts []TaskResult) (passed int, total int) {
	for _, r := range rollouts {
		if r.Passed {
			passed++
		}
	}
	return passed, len(rollouts)
}

// PassFraction returns passed/total as a fraction. Returns 0 for
// total=0 (no rollouts → vacuously not-yet-passed).
func PassFraction(passed, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(passed) / float64(total)
}
