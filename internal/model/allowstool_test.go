package model_test

import (
	"testing"

	"tenant/internal/model"
)

// AllowsTool gates capability tools (TEN-103): an explicit Capabilities flag
// always wins; otherwise the "recall" heuristic keys on the usable WritableBudget
// (NOT raw ContextLength), and unknown gates fail safe (deny).
func TestProfile_AllowsTool(t *testing.T) {
	// Explicit capability wins over the heuristic, both directions.
	on := model.Profile{Capabilities: map[string]any{"recall": true}, OperationalContextBudget: 1000}
	if !on.AllowsTool("recall") {
		t.Error("explicit recall=true must allow even with a tiny budget")
	}
	off := model.Profile{Capabilities: map[string]any{"recall": false}, OperationalContextBudget: 1 << 20}
	if off.AllowsTool("recall") {
		t.Error("explicit recall=false must deny even with a huge budget")
	}

	// Heuristic: a large usable budget enables recall; a small one disables it.
	big := model.Profile{OperationalContextBudget: 100000} // WritableBudget 100000 >= 32768
	if !big.AllowsTool("recall") {
		t.Errorf("a large budget should enable recall (writable=%d)", big.WritableBudget())
	}
	small := model.Profile{OperationalContextBudget: 8000} // WritableBudget 8000 < 32768
	if small.AllowsTool("recall") {
		t.Errorf("a small budget should disable recall (writable=%d)", small.WritableBudget())
	}

	// Unknown gate → deny (fail safe).
	if (model.Profile{OperationalContextBudget: 1 << 20}).AllowsTool("not-a-real-gate") {
		t.Error("an unknown gate must default to deny")
	}
}
