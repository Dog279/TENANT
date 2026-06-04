package main

import (
	"fmt"

	"tenant/internal/improve"
)

// formatSelfImproveFeedLine decides whether a self-improvement job run
// deserves a feed line, and if so renders it.
//
// Returns (line, true) when the line should be shown:
//   - the job ERRORED (always surface — operator needs to know)
//   - the job CHANGED durable state (the actual "improvement happened"
//     signal: distilled new facts, induced a new skill, refreshed the
//     user profile with new facts, etc.)
//
// Returns ("", false) — and the caller drops the line entirely — when
// the run was a silent no-op ("processed 0 episodes", "user profile: no
// new facts"). These ran every 10 minutes by default and spammed the
// feed with nothing for operators to act on.
//
// Pure function so it's trivially unit-testable + the filter rule stays
// one place (not duplicated across cmdTUI / cmdOrchestrate / cmdServe).
func formatSelfImproveFeedLine(rec improve.JobRunRecord) (string, bool) {
	if rec.Err != nil {
		return fmt.Sprintf("self-improve: %s FAILED — %v", rec.JobName, rec.Err), true
	}
	if !rec.Result.Changed {
		return "", false // silent no-op; suppress
	}
	return fmt.Sprintf("self-improve: %s — %s", rec.JobName, rec.Result.Summary), true
}
