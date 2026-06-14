// Package agent implements Tenant's bounded ReAct loop: take a user
// query, assemble a budgeted prompt, call the planner LLM, dispatch
// any tool calls, feed results back, repeat until the model emits a
// final response or PlanLoopCeiling iterations have passed.
//
// "Bounded" is the key word. Small local models loop forever given
// the chance. The loop has hard iteration caps from Profile.
// PlanLoopCeiling, per-iteration parallel-tool caps from Profile.
// MaxParallelTools, and a validation-failure escape (after 2 consecutive
// invalid tool calls, the runtime surfaces to the user instead of
// retrying further). These are deliberate guardrails, not safety nets.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"tenant/internal/model"
)

// ToolRegistry holds the set of tools available to the agent. v1 ships
// an in-memory StaticRegistry; later versions may swap in an MCP-based
// dynamic registry that queries connected plugins on each Search call.
type ToolRegistry interface {
	// Get returns the tool spec by name. False if not registered.
	Get(name string) (model.ToolSpec, bool)
	// Search returns the top-K most relevant tools given a query
	// embedding. v1 StaticRegistry ignores the embedding and returns
	// the first K tools; real semantic tool retrieval is a v1.1 lift
	// once we have meaningful tool descriptions to embed.
	Search(ctx context.Context, embedding []float32, k int) ([]model.ToolSpec, error)
	// All returns every registered tool. Used for diagnostics.
	All() []model.ToolSpec
}

// RankingReporter is an OPTIONAL capability of a ToolRegistry: it reports what
// the most recent Search did — whether cosine ranking trimmed the catalog or it
// fell back to the full enabled set, and why (TEN-225). The agent loop type-
// asserts to this so it can surface a per-turn diagnostic; registries that
// don't implement it fall back to a count heuristic. ok is false before the
// first Search of the session.
type RankingReporter interface {
	RankingStatus() (ranked bool, surfaced, catalog int, reason string, ok bool)
}

// StaticRegistry is the simple map-backed implementation. Thread-safe
// for concurrent reads; Register is for setup time, not hot path.
type StaticRegistry struct {
	mu    sync.RWMutex
	tools map[string]model.ToolSpec
	order []string // registration order, for deterministic Search results
}

// NewStaticRegistry returns an empty registry.
func NewStaticRegistry() *StaticRegistry {
	return &StaticRegistry{tools: make(map[string]model.ToolSpec)}
}

// Register adds a tool. Replaces any existing tool with the same name.
func (r *StaticRegistry) Register(t model.ToolSpec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[t.Name]; !exists {
		r.order = append(r.order, t.Name)
	}
	r.tools[t.Name] = t
}

// Get implements ToolRegistry.
func (r *StaticRegistry) Get(name string) (model.ToolSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Search returns the first K registered tools. This is a placeholder
// for real semantic retrieval — flagged as v1.1 work in TODOS.md.
func (r *StaticRegistry) Search(_ context.Context, _ []float32, k int) ([]model.ToolSpec, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if k <= 0 || k > len(r.order) {
		k = len(r.order)
	}
	out := make([]model.ToolSpec, 0, k)
	for i := 0; i < k; i++ {
		out = append(out, r.tools[r.order[i]])
	}
	return out, nil
}

// All returns every registered tool in registration order.
func (r *StaticRegistry) All() []model.ToolSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]model.ToolSpec, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.tools[name])
	}
	return out
}

// ToolDispatcher actually runs a tool. v1 implementations either
// shell out to in-process functions (the AsyncDispatcher pattern) or
// forward to MCP plugin servers. The interface is intentionally narrow:
// agent doesn't care HOW a tool runs, just that it does and returns
// a result.
type ToolDispatcher interface {
	// Dispatch invokes the tool. Returns:
	//   result   — string content to feed back to the LLM
	//   isError  — true if the tool reported a logical error
	//              (e.g. "file not found") — distinct from err
	//   err      — transport / unexpected failures
	Dispatch(ctx context.Context, call model.ToolCall) (result string, isError bool, err error)
}

// DispatcherFunc lets a plain function satisfy ToolDispatcher.
type DispatcherFunc func(ctx context.Context, call model.ToolCall) (string, bool, error)

// Dispatch implements ToolDispatcher.
func (f DispatcherFunc) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	return f(ctx, call)
}

// ToolCallResult is the outcome of one dispatched tool call.
type ToolCallResult struct {
	Call    model.ToolCall
	Result  string
	IsError bool
	Err     error
}

// dispatchBatch runs up to maxParallel tool calls concurrently. Order
// of the returned results matches the input order. Errors are kept
// per-call (one bad tool doesn't kill the batch).
func dispatchBatch(ctx context.Context, calls []model.ToolCall, maxParallel int, d ToolDispatcher) []ToolCallResult {
	if maxParallel < 1 {
		maxParallel = 1
	}
	results := make([]ToolCallResult, len(calls))
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	for i, call := range calls {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, call model.ToolCall) {
			defer wg.Done()
			defer func() { <-sem }()
			out, isErr, err := d.Dispatch(ctx, call)
			results[i] = ToolCallResult{Call: call, Result: out, IsError: isErr, Err: err}
		}(i, call)
	}
	wg.Wait()
	return results
}

// validateToolCall does the minimal checks a v1 agent needs:
//
//  1. Tool with that name is registered.
//  2. Arguments are valid JSON.
//  3. Required fields (per the JSON schema's "required" array) are present.
//  4. Provided arguments match their declared JSON-schema type.
//
// Catching shape errors HERE — before dispatch — turns a model's malformed
// call into a precise, self-correcting error ("argument k should be a
// number") fed back to the planner, instead of a confusing plugin-level
// failure. Type checking is best-effort (top-level properties only); a
// malformed schema never fails validation.
func validateToolCall(call model.ToolCall, reg ToolRegistry) error {
	if call.Name == "" {
		return errors.New("tool call missing name")
	}
	spec, ok := reg.Get(call.Name)
	if !ok {
		return fmt.Errorf("unknown tool %q", call.Name)
	}
	// Arguments must be valid JSON (or empty).
	var argMap map[string]any
	if len(call.Arguments) > 0 {
		if err := json.Unmarshal(call.Arguments, &argMap); err != nil {
			return fmt.Errorf("tool %q: arguments are not valid JSON: %w", call.Name, err)
		}
	}
	if len(spec.Parameters) == 0 {
		return nil
	}
	var schema struct {
		Required   []string `json:"required"`
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	// Don't fail validation if the schema itself is malformed — the spec
	// might be hand-written with no required/properties block.
	_ = json.Unmarshal(spec.Parameters, &schema)

	sort.Strings(schema.Required)
	for _, r := range schema.Required {
		if _, ok := argMap[r]; !ok {
			return fmt.Errorf("tool %q: missing required argument %q", call.Name, r)
		}
	}
	// Type-check provided args against their declared type. Unknown args
	// are tolerated (additionalProperties); null is allowed (absent-ish).
	for name, val := range argMap {
		p, declared := schema.Properties[name]
		if !declared || p.Type == "" || val == nil {
			continue
		}
		if want, got, ok := jsonTypeMismatch(p.Type, val); !ok {
			return fmt.Errorf("tool %q: argument %q should be a %s, got %s", call.Name, name, want, got)
		}
	}
	return nil
}

// jsonTypeMismatch reports whether val matches the JSON-schema type. Returns
// (wantType, gotType, ok). ok=true means it matches (or the type is one we
// don't strictly check). json.Unmarshal yields: string, float64, bool,
// map[string]any, []any, nil.
func jsonTypeMismatch(want string, val any) (string, string, bool) {
	got := jsonKind(val)
	switch want {
	case "string":
		return want, got, got == "string"
	case "number":
		return want, got, got == "number"
	case "integer":
		f, isNum := val.(float64)
		return want, got, isNum && f == float64(int64(f))
	case "boolean":
		return want, got, got == "boolean"
	case "object":
		return want, got, got == "object"
	case "array":
		return want, got, got == "array"
	default:
		return want, got, true // unknown/union type — don't enforce
	}
}

func jsonKind(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case nil:
		return "null"
	default:
		return "unknown"
	}
}
