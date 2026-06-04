package model_test

import (
	"errors"
	"testing"

	"tenant/internal/model"
)

func TestLoadProfileYAML_Valid(t *testing.T) {
	y := []byte(`
id: test-profile
role: planner
backend: vllm
endpoint: http://localhost:8000
model: Qwen/Qwen3.6-72B-Instruct
context_length: 128000
operational_context_budget: 102400
reserve_system_prompt: 2048
reserve_tool_defs: 4096
reserve_response: 84000
tool_format: qwen
supports_grammar: true
max_tools_per_call: 5
max_parallel_tools: 3
plan_loop_ceiling: 10
capabilities:
  reasoning_depth: high
`)
	p, err := model.LoadProfileYAML(y)
	if err != nil {
		t.Fatalf("LoadProfileYAML: %v", err)
	}
	if p.ID != "test-profile" {
		t.Fatalf("ID = %q, want %q", p.ID, "test-profile")
	}
	if p.Role != model.RolePlanner {
		t.Fatalf("Role = %q, want %q", p.Role, model.RolePlanner)
	}
	if p.WritableBudget() != 102400-2048-4096-84000 {
		t.Fatalf("WritableBudget = %d, want %d", p.WritableBudget(), 102400-2048-4096-84000)
	}
	if got, _ := p.Capabilities["reasoning_depth"].(string); got != "high" {
		t.Fatalf("Capabilities[reasoning_depth] = %q, want %q", got, "high")
	}
}

func TestLoadProfileYAML_MissingRequired(t *testing.T) {
	cases := map[string][]byte{
		"missing id": []byte(`
role: planner
backend: vllm
endpoint: http://localhost:8000
context_length: 128000
`),
		"missing role": []byte(`
id: p
backend: vllm
endpoint: http://localhost:8000
context_length: 128000
`),
		"missing endpoint": []byte(`
id: p
role: planner
backend: vllm
context_length: 128000
`),
		"missing context_length": []byte(`
id: p
role: planner
backend: vllm
endpoint: http://localhost:8000
`),
	}
	for name, y := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := model.LoadProfileYAML(y)
			if !errors.Is(err, model.ErrInvalidProfile) {
				t.Fatalf("err = %v, want ErrInvalidProfile", err)
			}
		})
	}
}

func TestLoadProfileYAML_MalformedYAML(t *testing.T) {
	_, err := model.LoadProfileYAML([]byte("not: valid: yaml: here:\n  - oops"))
	if !errors.Is(err, model.ErrInvalidProfile) {
		t.Fatalf("err = %v, want ErrInvalidProfile", err)
	}
}

func TestLoadProfileYAML_UnknownFieldsIgnored(t *testing.T) {
	// Forward compatibility: future Profile fields should not break older configs.
	y := []byte(`
id: p
role: planner
backend: vllm
endpoint: http://localhost:8000
model: m
context_length: 128000
future_field_we_havent_invented: 42
`)
	if _, err := model.LoadProfileYAML(y); err != nil {
		t.Fatalf("LoadProfileYAML rejected unknown field: %v", err)
	}
}

func TestProfile_WritableBudgetFallback(t *testing.T) {
	// When OperationalContextBudget is unset, fall back to 80% of ContextLength.
	p := model.Profile{ContextLength: 128000}
	got := p.WritableBudget()
	want := (128000 * 8) / 10
	if got != want {
		t.Fatalf("WritableBudget = %d, want %d", got, want)
	}
}

func TestProfile_WritableBudgetSubtractsSoul(t *testing.T) {
	// Adding ReserveSoul must reduce WritableBudget by the same amount.
	p1 := model.Profile{
		OperationalContextBudget: 100000,
		ReserveSystemPrompt:      2000,
		ReserveToolDefs:          4000,
		ReserveResponse:          80000,
	}
	p2 := p1
	p2.ReserveSoul = 2000

	if p1.WritableBudget()-p2.WritableBudget() != 2000 {
		t.Fatalf("ReserveSoul not subtracted: p1=%d p2=%d (expected delta of 2000)",
			p1.WritableBudget(), p2.WritableBudget())
	}
}

func TestProfile_WritableBudgetMissingSoulFieldIsZero(t *testing.T) {
	// Forward compat: user YAMLs without reserve_soul should load with 0,
	// not regress writable budget unexpectedly.
	y := []byte(`
id: legacy
role: planner
backend: vllm
endpoint: http://localhost:8000
model: m
context_length: 128000
operational_context_budget: 100000
reserve_system_prompt: 2000
reserve_tool_defs: 4000
reserve_response: 80000
`)
	p, err := model.LoadProfileYAML(y)
	if err != nil {
		t.Fatalf("LoadProfileYAML: %v", err)
	}
	if p.ReserveSoul != 0 {
		t.Fatalf("ReserveSoul = %d, want 0 when omitted from YAML", p.ReserveSoul)
	}
	if got := p.WritableBudget(); got != 100000-2000-4000-80000 {
		t.Fatalf("WritableBudget = %d, want %d", got, 100000-2000-4000-80000)
	}
}

func TestProfile_WritableBudgetNegative(t *testing.T) {
	// Reserves exceeding budget → 0, not negative.
	p := model.Profile{
		OperationalContextBudget: 1000,
		ReserveSystemPrompt:      500,
		ReserveToolDefs:          500,
		ReserveResponse:          500,
	}
	if got := p.WritableBudget(); got != 0 {
		t.Fatalf("WritableBudget = %d, want 0", got)
	}
}
