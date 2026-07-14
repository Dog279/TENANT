package route

import (
	"tenant/internal/model"
)

// Candidate is one (role, profile) routing option annotated with learned
// evidence. Score is the RESERVED TEN-17 policy input: RouteScoringJob will
// blend per-(role,profile) outcome history ("this profile historically
// succeeds on prompts like this") with the classifier's features ("this
// prompt looks hard"). TEN-292 ships only this join surface; no score is
// computed inside internal/route.
type Candidate struct {
	Role      model.Role
	ProfileID string
	Score     float64
}

// ScoreProvider supplies learned candidate scores for one classified prompt.
// Implementations live outside internal/route (TEN-17's RouteScoringJob);
// this package only defines the seam and a zero-value stub.
type ScoreProvider interface {
	// Scores returns candidate evidence for the signal. A nil or empty result
	// means "no learned evidence"; policy behavior is then unchanged.
	Scores(sig Signal, candidates []Candidate) []Candidate
}

// ZeroScores is the stub provider wired until TEN-17 lands: it returns the
// candidates unchanged with zero scores, making the join surface exercisable
// end-to-end with no behavioral influence.
type ZeroScores struct{}

// Scores implements ScoreProvider with no learned evidence.
func (ZeroScores) Scores(_ Signal, candidates []Candidate) []Candidate {
	out := make([]Candidate, len(candidates))
	copy(out, candidates)
	for i := range out {
		out[i].Score = 0
	}
	return out
}
