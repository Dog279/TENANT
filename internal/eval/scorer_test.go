package eval

import (
	"strings"
	"testing"
)

// TEN-517: result_contains asserts what the tool answered, so a component
// that validated ("shown: table") can be told from one the validator refused.
func TestEvalGate_ResultContains(t *testing.T) {
	exp := Expected{MustCall: []ExpectedToolCall{{Tool: "ui_render", ArgsContain: []string{`"table"`}, ResultContains: []string{"shown: table"}}}}
	refused := []callRecord{{tool: "ui_render", args: `{"kind":"table"}`, result: "table: 14 columns, the limit is 12"}}
	if fail := evalGate(exp, refused, "x"); len(fail) != 1 || !strings.Contains(fail[0], "result containing") {
		t.Fatalf("a refused call should fail the gate: %v", fail)
	}
	fixed := append(refused, callRecord{tool: "ui_render", args: `{"kind":"table"}`, result: "shown: table c2, 1 rows"})
	if fail := evalGate(exp, fixed, "x"); len(fail) != 0 {
		t.Fatalf("a corrected call should pass: %v", fail)
	}
	if fail := evalGate(Expected{MustNotCall: []string{"ui_render"}}, fixed, "x"); len(fail) == 0 {
		t.Fatalf("must_not_call still works: %v", fail)
	}
}
