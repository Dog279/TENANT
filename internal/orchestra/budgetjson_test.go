package orchestra

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// budgets.max_wall_time reads as a duration or as the integer nanoseconds of
// older files, writes as a duration, and names itself when it can't be read.
func TestBudgetsWallTimeJSON(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{`{"max_wall_time":"45m","max_agents_per_run":8}`, 45 * time.Minute},
		{`{"max_wall_time":1800000000000}`, 30 * time.Minute},
		{`{"max_agents_per_run":8}`, 0},
	} {
		var b Budgets
		if err := json.Unmarshal([]byte(tc.in), &b); err != nil || b.MaxWallTime != tc.want {
			t.Errorf("%s → %v, %v; want %v", tc.in, b.MaxWallTime, err, tc.want)
		}
		if strings.Contains(tc.in, "max_agents") && b.MaxAgentsPerRun != 8 {
			t.Errorf("%s: other fields lost: %+v", tc.in, b)
		}
	}
	out, err := json.Marshal(Budgets{MaxWallTime: 2 * time.Hour, MaxAgentsPerRun: 3})
	if err != nil || string(out) != `{"max_agents_per_run":3,"max_wall_time":"2h0m0s"}` {
		t.Fatalf("marshal = %s, %v", out, err)
	}
	var back Budgets
	if err := json.Unmarshal(out, &back); err != nil || back.MaxWallTime != 2*time.Hour || back.MaxAgentsPerRun != 3 {
		t.Fatalf("round trip = %+v, %v", back, err)
	}
	var bad Budgets
	if err := json.Unmarshal([]byte(`{"max_wall_time":"half an hour"}`), &bad); err == nil || !strings.Contains(err.Error(), "budgets.max_wall_time") {
		t.Fatalf("bad duration: %v", err)
	}
	// The team-run status that embeds the limits round-trips too.
	st, _ := json.Marshal(BudgetStatus{Limits: Budgets{MaxWallTime: time.Minute}})
	var st2 BudgetStatus
	if err := json.Unmarshal(st, &st2); err != nil || st2.Limits.MaxWallTime != time.Minute {
		t.Fatalf("status round trip: %s → %+v, %v", st, st2.Limits, err)
	}
}
