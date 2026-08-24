package model

import "testing"

// GateKnown separates "denied on purpose" from "denied by ignorance". The
// distinction is what lets the agent warn on the second without nagging about
// the first: an unknown gate hides its tool from EVERY profile with no trace
// otherwise.
func TestGateKnown(t *testing.T) {
	// Explicitly configured gates are known - including an explicit DENY,
	// which is a deliberate decision and must not be warned about.
	p := Profile{Capabilities: map[string]any{"newthing": false}}
	if !p.GateKnown("newthing") {
		t.Error("explicitly configured gate (even deny) must be known")
	}
	// Heuristic gates are known without configuration.
	if !(Profile{}).GateKnown("recall") {
		t.Error("heuristic gate recall must be known")
	}
	// Structural pin: AllowsTool and GateKnown now read the SAME map, so
	// for every heuristic gate the decision must be callable and the gate
	// known - and a gate absent from the map must be both unknown AND denied.
	for g, decide := range gateHeuristics {
		if !(Profile{}).GateKnown(g) {
			t.Errorf("heuristic gate %q not known", g)
		}
		if decide == nil {
			t.Errorf("heuristic gate %q has a nil decision func", g)
		}
	}
	// The recall heuristic itself still behaves: a roomy profile allows,
	// a cramped one denies - through the map, not a parallel switch.
	roomy := Profile{ContextLength: 200000}
	if !roomy.AllowsTool("recall") {
		t.Error("roomy profile should allow recall via the heuristic")
	}
	if (Profile{ContextLength: 4096}).AllowsTool("recall") {
		t.Error("cramped profile should deny recall via the heuristic")
	}
	// Unrecognized gates are NOT known (and AllowsTool still denies them).
	if (Profile{}).GateKnown("bogus") {
		t.Error("unrecognized gate must not be known")
	}
	if (Profile{}).AllowsTool("bogus") {
		t.Error("unrecognized gate must still be denied - fail-safe unchanged")
	}
}
