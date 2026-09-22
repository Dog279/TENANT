package imessage

import (
	"testing"

	"tenant/internal/model"
)

func TestTools_InfoFlowMetadata(t *testing.T) {
	for _, spec := range NewDispatcher(nil, Policy{}).Tools() {
		if !spec.Outbound {
			t.Errorf("%s must be outbound-capable", spec.Name)
		}
		if len(spec.ResultLabels) != 1 || spec.ResultLabels[0].Provenance != model.ProvenanceLocalService {
			t.Errorf("%s labels = %+v", spec.Name, spec.ResultLabels)
		}
	}
}
