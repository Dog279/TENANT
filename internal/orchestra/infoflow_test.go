package orchestra

import (
	"testing"

	"tenant/internal/model"
)

func TestCommsTools_InfoFlowMetadata(t *testing.T) {
	byName := map[string]model.ToolSpec{}
	for _, spec := range (CommsTool{}).Tools() {
		byName[spec.Name] = spec
	}
	for _, name := range []string{"team_send", "team_broadcast"} {
		if !byName[name].Outbound {
			t.Errorf("%s must be outbound-capable", name)
		}
	}
	for _, name := range []string{"team_check", "team_history"} {
		labels := byName[name].ResultLabels
		if len(labels) != 1 || labels[0].Provenance != model.ProvenancePeer {
			t.Errorf("%s labels = %+v", name, labels)
		}
	}
}
