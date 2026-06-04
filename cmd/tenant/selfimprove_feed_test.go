package main

import (
	"errors"
	"strings"
	"testing"

	"tenant/internal/improve"
)

// formatSelfImproveFeedLine must:
//   - DROP runs that did nothing (no error, no change) — these used to
//     spam the activity feed every 10 minutes with "processed 0 episodes"
//     and "user profile: no new facts" for no operator value
//   - SHOW runs that errored — the operator needs to know
//   - SHOW runs that actually changed something — the actual improvement
func TestFormatSelfImproveFeedLine(t *testing.T) {
	cases := []struct {
		name       string
		rec        improve.JobRunRecord
		wantShow   bool
		mustContain string
	}{
		{
			name: "silent no-op distill (0 facts)",
			rec: improve.JobRunRecord{
				JobName: "distill",
				Result:  improve.JobResult{Summary: "processed 0 episodes, 0 facts", Changed: false},
			},
			wantShow: false,
		},
		{
			name: "silent no-op profile (no new facts)",
			rec: improve.JobRunRecord{
				JobName: "user-profile",
				Result:  improve.JobResult{Summary: "user profile: no new facts", Changed: false},
			},
			wantShow: false,
		},
		{
			name: "silent no-op skill induction",
			rec: improve.JobRunRecord{
				JobName: "skill-induction",
				Result:  improve.JobResult{Summary: "scanned 92 episode(s), proposed 0 new skill(s)", Changed: false},
			},
			wantShow: false,
		},
		{
			name: "real improvement (3 new facts)",
			rec: improve.JobRunRecord{
				JobName: "distill",
				Result: improve.JobResult{
					Summary: "processed 5 episodes, 7 facts (3 new, 4 reaffirmed)",
					Changed: true,
				},
			},
			wantShow:    true,
			mustContain: "self-improve: distill — processed 5 episodes",
		},
		{
			name: "profile refreshed",
			rec: improve.JobRunRecord{
				JobName: "user-profile",
				Result:  improve.JobResult{Summary: "user profile refreshed", Changed: true},
			},
			wantShow:    true,
			mustContain: "user profile refreshed",
		},
		{
			name: "errored job ALWAYS shows even when Changed=false",
			rec: improve.JobRunRecord{
				JobName: "distill",
				Result:  improve.JobResult{Summary: "distill failed at cursor 0: ...", Changed: false},
				Err:     errors.New("connection refused"),
			},
			wantShow:    true,
			mustContain: "FAILED",
		},
		{
			name: "errored job with no error message — defensive",
			rec: improve.JobRunRecord{
				JobName: "skill-induction",
				Result:  improve.JobResult{Summary: "", Changed: false},
				Err:     errors.New("ctx canceled"),
			},
			wantShow:    true,
			mustContain: "skill-induction FAILED",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			line, ok := formatSelfImproveFeedLine(c.rec)
			if ok != c.wantShow {
				t.Fatalf("show = %v, want %v (line=%q)", ok, c.wantShow, line)
			}
			if !c.wantShow && line != "" {
				t.Errorf("suppressed run should produce empty line, got %q", line)
			}
			if c.wantShow && c.mustContain != "" && !strings.Contains(line, c.mustContain) {
				t.Errorf("line %q should contain %q", line, c.mustContain)
			}
		})
	}
}

// Drift guard: the three known job names must ALL behave the same way
// (no special-casing). If we ever add a new job, the rule still applies
// uniformly via JobResult.Changed.
func TestFormatSelfImproveFeedLine_UniformAcrossJobs(t *testing.T) {
	for _, job := range []string{"distill", "user-profile", "skill-induction", "future-job"} {
		// Changed=false, no err → always suppressed.
		_, ok := formatSelfImproveFeedLine(improve.JobRunRecord{
			JobName: job, Result: improve.JobResult{Changed: false},
		})
		if ok {
			t.Errorf("job %q with Changed=false should be suppressed", job)
		}
		// Changed=true → always shown.
		_, ok = formatSelfImproveFeedLine(improve.JobRunRecord{
			JobName: job, Result: improve.JobResult{Changed: true, Summary: "did stuff"},
		})
		if !ok {
			t.Errorf("job %q with Changed=true should be shown", job)
		}
	}
}
