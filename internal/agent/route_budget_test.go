package agent_test

import (
	"testing"
	"time"

	"tenant/internal/agent"
)

// The active-routing join used to be a hardcoded 250ms while route.timeout_ms
// only sized the classifier's HTTP client — so raising timeout_ms appeared to
// grant more time and granted none. The budget must now track the configured
// value, and fall back to the documented default when unset.
func TestActiveRouteBudget_TracksConfigAndDefaults(t *testing.T) {
	t.Cleanup(func() { agent.SetActiveRouteBudget(0) })

	if got := agent.ActiveRouteBudget(); got != agent.DefaultActiveRouteBudget {
		t.Fatalf("unset budget = %v, want default %v", got, agent.DefaultActiveRouteBudget)
	}

	agent.SetActiveRouteBudget(2 * time.Second)
	if got := agent.ActiveRouteBudget(); got != 2*time.Second {
		t.Fatalf("configured budget = %v, want 2s", got)
	}

	// Non-positive means "operator said nothing" → documented default, never 0
	// (a zero deadline would fail every routed-role resolution instantly).
	for _, d := range []time.Duration{0, -1 * time.Second} {
		agent.SetActiveRouteBudget(d)
		if got := agent.ActiveRouteBudget(); got != agent.DefaultActiveRouteBudget {
			t.Fatalf("SetActiveRouteBudget(%v) = %v, want default %v", d, got, agent.DefaultActiveRouteBudget)
		}
	}
}
