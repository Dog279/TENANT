package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"tenant/internal/memory/working"
	"tenant/internal/model"
)

// lazyFakeReg is a ToolRegistry over a fixed spec list (all "enabled").
type lazyFakeReg struct{ specs []model.ToolSpec }

func (r *lazyFakeReg) Get(name string) (model.ToolSpec, bool) {
	for _, s := range r.specs {
		if s.Name == name {
			return s, true
		}
	}
	return model.ToolSpec{}, false
}
func (r *lazyFakeReg) All() []model.ToolSpec { return append([]model.ToolSpec(nil), r.specs...) }
func (r *lazyFakeReg) Search(_ context.Context, _ []float32, k int) ([]model.ToolSpec, error) {
	if k <= 0 || k > len(r.specs) {
		k = len(r.specs)
	}
	return append([]model.ToolSpec(nil), r.specs[:k]...), nil
}

// a representative MINIFIED (post-TEN-227) MCP-style schema: compact, no
// examples/$schema, but real nested structure — ~the wire cost of one kept tool.
const lazyRealSchema = `{"type":"object","additionalProperties":false,"properties":{"cloudId":{"type":"string","description":"Cloud ID (UUID or site URL)"},"projectKey":{"type":"string","description":"Project key"},"summary":{"type":"string","description":"Issue summary"},"issueTypeName":{"type":"string","description":"Type (Task, Bug, Story)"},"description":{"type":"string","description":"Issue description body"},"priority":{"type":"object","properties":{"name":{"type":"string","enum":["High","Medium","Low"]}}},"labels":{"type":"array","items":{"type":"string"}}},"required":["cloudId","projectKey","summary"]}`

func mkLazyTools(n int) []model.ToolSpec {
	out := make([]model.ToolSpec, n)
	for i := 0; i < n; i++ {
		out[i] = model.ToolSpec{
			Name:        fmt.Sprintf("tool_%02d", i),
			Description: fmt.Sprintf("Tool number %d that does a specific useful thing for the agent.", i),
			Parameters:  json.RawMessage(lazyRealSchema),
		}
	}
	return out
}

func wireBytes(tools []model.ToolSpec) int {
	n := 0
	for _, t := range tools {
		n += len(t.Name) + 1 + len(t.Description) + 1 + len(t.Parameters) + 1
	}
	return n
}

func TestRenderToolCatalog(t *testing.T) {
	all := mkLazyTools(5)
	active := map[string]bool{"tool_00": true, "tool_01": true}
	cat := renderToolCatalog(all, active)
	if strings.Contains(cat, "tool_00") || strings.Contains(cat, "tool_01") {
		t.Errorf("active tools should NOT appear in the catalog:\n%s", cat)
	}
	for _, n := range []string{"tool_02", "tool_03", "tool_04"} {
		if !strings.Contains(cat, n) {
			t.Errorf("catalog should list non-active tool %s:\n%s", n, cat)
		}
	}
	if strings.Contains(cat, loadToolName+":") {
		t.Error("load_tool should never be listed in its own catalog")
	}
	// Nothing extra to offer → empty catalog.
	allActive := map[string]bool{}
	for _, s := range all {
		allActive[s.Name] = true
	}
	if renderToolCatalog(all, allActive) != "" {
		t.Error("catalog should be empty when every tool is already active")
	}
}

func TestBuildLazyTools(t *testing.T) {
	reg := &lazyFakeReg{specs: mkLazyTools(10)}
	a := &Agent{cfg: Config{Tools: reg}}
	work := reg.specs[:3] // ranked working set
	loaded := map[string]bool{"tool_07": true}

	got := a.buildLazyTools(work, loaded)
	if got[0].Name != loadToolName {
		t.Errorf("load_tool must lead the array; got %q", got[0].Name)
	}
	names := map[string]bool{}
	for _, s := range got {
		names[s.Name] = true
	}
	for _, s := range work {
		if !names[s.Name] {
			t.Errorf("working-set tool %s missing", s.Name)
		}
	}
	if !names["tool_07"] {
		t.Error("loaded tool_07 should be present")
	}
	// loaded schema is minified (no whitespace bloat — it's already compact here,
	// but assert it parses + has the structure).
	for _, s := range got {
		if s.Name == "tool_07" && !strings.Contains(string(s.Parameters), `"required"`) {
			t.Errorf("loaded tool schema lost structure: %s", s.Parameters)
		}
	}
	// Dedup: loading a tool already in the working set doesn't duplicate it.
	got2 := a.buildLazyTools(work, map[string]bool{"tool_00": true})
	count := 0
	for _, s := range got2 {
		if s.Name == "tool_00" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("tool_00 should appear once (it's in the working set), got %d", count)
	}
}

func TestHandleLoadTool(t *testing.T) {
	reg := &lazyFakeReg{specs: mkLazyTools(5)}
	a := &Agent{cfg: Config{Tools: reg, Working: working.New(), AgentID: "t"}}
	ctx := context.Background()
	call := func(name string) model.ToolCall {
		args, _ := json.Marshal(map[string]string{"name": name})
		return model.ToolCall{Name: loadToolName, ID: "c1", Arguments: args}
	}

	loaded := map[string]bool{}
	a.handleLoadTool(ctx, call("tool_03"), loaded)
	if !loaded["tool_03"] {
		t.Error("a valid load_tool should add the tool to the loaded set")
	}
	if got := lastToolMsg(a); !strings.Contains(got, "loaded") {
		t.Errorf("should feed a confirmation result; got %q", got)
	}

	// Unknown tool → error, not loaded.
	a.handleLoadTool(ctx, call("ghost_tool"), loaded)
	if loaded["ghost_tool"] {
		t.Error("unknown tool must NOT be added to the loaded set")
	}
	if got := lastToolMsg(a); !strings.Contains(got, "no enabled tool") {
		t.Errorf("unknown tool should report a clear error; got %q", got)
	}

	// Empty name → error.
	a.handleLoadTool(ctx, call("  "), loaded)
	if got := lastToolMsg(a); !strings.Contains(got, "non-empty") {
		t.Errorf("empty name should error; got %q", got)
	}

	// Already loaded → friendly, idempotent.
	a.handleLoadTool(ctx, call("tool_03"), loaded)
	if got := lastToolMsg(a); !strings.Contains(got, "already loaded") {
		t.Errorf("re-loading should be a friendly no-op; got %q", got)
	}
}

func TestSplitLoadToolCalls_NoAliasing(t *testing.T) {
	// The durable working-set assistant message holds resp.ToolCalls by
	// reference; splitting must NOT write through its backing array.
	in := []model.ToolCall{{Name: loadToolName, ID: "0"}, {Name: "real_tool", ID: "1"}}
	durable := in // simulates the already-appended assistant message's slice
	loads, rest := splitLoadToolCalls(in)
	if len(loads) != 1 || loads[0].ID != "0" {
		t.Fatalf("expected 1 load_tool call, got %+v", loads)
	}
	if len(rest) != 1 || rest[0].Name != "real_tool" {
		t.Fatalf("expected 1 real call in rest, got %+v", rest)
	}
	// Mutate rest — the durable/input slice MUST be untouched.
	rest[0] = model.ToolCall{Name: "MUTATED"}
	if durable[0].Name != loadToolName || durable[1].Name != "real_tool" {
		t.Errorf("rest aliases the input backing array — durable history corrupted: %+v", durable)
	}

	// No load_tool → (nil, nil): caller leaves resp.ToolCalls as-is, no alloc.
	if l, r := splitLoadToolCalls([]model.ToolCall{{Name: "a"}, {Name: "b"}}); l != nil || r != nil {
		t.Errorf("no load_tool should return (nil,nil); got loads=%v rest=%v", l, r)
	}
}

func lastToolMsg(a *Agent) string {
	msgs := a.cfg.Working.Messages()
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "tool" {
			return msgs[i].Content
		}
	}
	return ""
}

// TestLazyTools_TokenCost measures the on-wire tool-definition cost with lazy
// loading OFF vs ON for a representative catalog — the answer to "what's the
// token difference". Bytes are the real on-wire cost; ~4 bytes/token is the
// usual rough conversion.
func TestLazyTools_TokenCost(t *testing.T) {
	const N, K = 45, 12 // catalog size, ranked working set (post-TEN-226)
	reg := &lazyFakeReg{specs: mkLazyTools(N)}
	a := &Agent{cfg: Config{Tools: reg}}
	work := reg.specs[:K]

	offFull := wireBytes(reg.specs) // OFF: ship all N schemas to keep access to every tool
	offRanked := wireBytes(work)    // OFF typical: 226 ranks to K full schemas (no access beyond)

	active := map[string]bool{}
	for _, s := range work {
		active[s.Name] = true
	}
	onArray := wireBytes(a.buildLazyTools(work, map[string]bool{})) // load_tool + K schemas
	catalog := len(renderToolCatalog(reg.specs, active))            // N-K cheap lines in the prompt
	on := onArray + catalog

	// A leaner working set is now safe (access preserved via load_tool): K=4.
	leanWork := reg.specs[:4]
	leanActive := map[string]bool{}
	for _, s := range leanWork {
		leanActive[s.Name] = true
	}
	onLean := wireBytes(a.buildLazyTools(leanWork, map[string]bool{})) + len(renderToolCatalog(reg.specs, leanActive))

	tok := func(b int) int { return b / 4 }
	t.Logf("catalog=%d tools, working set=%d", N, K)
	t.Logf("OFF, full access (all %d schemas): %6d B  (~%5d tok)", N, offFull, tok(offFull))
	t.Logf("OFF, 226-ranked (%d schemas):       %6d B  (~%5d tok)  [no access beyond the %d]", K, offRanked, tok(offRanked), K)
	t.Logf("ON,  228 (%d schemas + load_tool + %d-line catalog): %6d B  (~%5d tok)  [full access]", K, N-K, on, tok(on))
	t.Logf("ON,  228 lean (4 schemas + load_tool + catalog):     %6d B  (~%5d tok)  [full access]", onLean, tok(onLean))
	t.Logf("228 vs full-access:        %d%% smaller", 100*(offFull-on)/offFull)
	t.Logf("228 lean vs full-access:   %d%% smaller", 100*(offFull-onLean)/offFull)
	t.Logf("228 lean vs 226-ranked:    %d%% smaller (AND keeps access to all %d)", 100*(offRanked-onLean)/offRanked, N)

	if on >= offFull {
		t.Errorf("228 should beat full-access cost: on=%d offFull=%d", on, offFull)
	}
	if onLean >= offRanked {
		t.Errorf("228-lean should beat even the 226-ranked array while preserving access: onLean=%d offRanked=%d", onLean, offRanked)
	}
}
