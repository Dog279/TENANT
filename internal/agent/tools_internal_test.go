package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"tenant/internal/model"
)

// oneSpecRegistry is a ToolRegistry holding a single spec.
type oneSpecRegistry struct{ spec model.ToolSpec }

func (r oneSpecRegistry) Get(name string) (model.ToolSpec, bool) {
	if name == r.spec.Name {
		return r.spec, true
	}
	return model.ToolSpec{}, false
}
func (r oneSpecRegistry) Search(context.Context, []float32, int) ([]model.ToolSpec, error) {
	return []model.ToolSpec{r.spec}, nil
}
func (r oneSpecRegistry) All() []model.ToolSpec { return []model.ToolSpec{r.spec} }

func TestValidateToolCall_TypeChecking(t *testing.T) {
	reg := oneSpecRegistry{spec: model.ToolSpec{
		Name: "f",
		Parameters: json.RawMessage(`{"type":"object","properties":{
			"path":{"type":"string"},"k":{"type":"integer"},"flag":{"type":"boolean"}},
			"required":["path"]}`),
	}}
	call := func(args string) model.ToolCall {
		return model.ToolCall{Name: "f", Arguments: json.RawMessage(args)}
	}

	// Correct types pass.
	if err := validateToolCall(call(`{"path":"/x","k":5,"flag":true}`), reg); err != nil {
		t.Fatalf("valid call rejected: %v", err)
	}
	// Integer accepts a whole float; unknown args tolerated.
	if err := validateToolCall(call(`{"path":"/x","k":5.0,"extra":"ok"}`), reg); err != nil {
		t.Fatalf("whole-float integer / extra arg rejected: %v", err)
	}
	// Wrong type → clear error naming the arg + expected type.
	if err := validateToolCall(call(`{"path":42}`), reg); err == nil || !strings.Contains(err.Error(), "path") || !strings.Contains(err.Error(), "string") {
		t.Fatalf("string-arg-got-number should be a clear type error, got: %v", err)
	}
	// Non-integer number for an integer field is rejected.
	if err := validateToolCall(call(`{"path":"/x","k":2.5}`), reg); err == nil || !strings.Contains(err.Error(), "k") {
		t.Fatalf("fractional integer should be rejected: %v", err)
	}
	// Missing required still caught.
	if err := validateToolCall(call(`{"k":5}`), reg); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("missing required not caught: %v", err)
	}
	// Unknown tool.
	if err := validateToolCall(model.ToolCall{Name: "ghost"}, reg); err == nil {
		t.Fatal("unknown tool should fail")
	}
}

// A spec with no properties block must not break validation.
func TestValidateToolCall_LenientOnSparseSchema(t *testing.T) {
	reg := oneSpecRegistry{spec: model.ToolSpec{Name: "g", Parameters: json.RawMessage(`{}`)}}
	if err := validateToolCall(model.ToolCall{Name: "g", Arguments: json.RawMessage(`{"anything":123}`)}, reg); err != nil {
		t.Fatalf("sparse schema should accept anything: %v", err)
	}
}
