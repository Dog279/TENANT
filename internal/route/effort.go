package route

import "strings"

// EffortHintHigh is the single per-request reasoning override TEN-292 emits.
// Complexity 5 marks the hardest prompt class Supra can express; anything
// subtler belongs to the operator's persisted /reasoning setting, not routing.
const EffortHintHigh = "high"

// EffortHintFor returns the per-request ReasoningEffort override for one
// classified decision, or "" when no override applies. The caller supplies
// profile facts (locality of capability knowledge stays in cmd/tenant):
// supportsReasoning is the provider kind's capability, currentEffort the
// profile's already-persisted effort.
//
// The hint never DOWNGRADES: a profile already at high/xhigh keeps its
// operator-chosen setting.
func EffortHintFor(sig Signal, supportsReasoning bool, currentEffort string) string {
	if !supportsReasoning || sig.Complexity != 5 {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(currentEffort)) {
	case "high", "xhigh", "max": // max = Fugu's alias for xhigh
		return ""
	}
	return EffortHintHigh
}
