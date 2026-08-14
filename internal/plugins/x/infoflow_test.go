package x

import "testing"

func TestTools_InfoFlowMetadata(t *testing.T) {
	for _, spec := range NewDispatcher(nil, Policy{}).Tools() {
		if !spec.Outbound {
			t.Errorf("%s must be outbound-capable", spec.Name)
		}
	}
}
