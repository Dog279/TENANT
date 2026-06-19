package agent

// lazytools.go (TEN-228): on-demand tool loading. When Config.LazyToolLoad is
// set, the per-iteration native tool array carries only the ranked working set
// plus a `load_tool` meta-tool; every OTHER enabled tool appears as one cheap
// name+description line in a CATALOG injected into the system prompt. The model
// calls load_tool(name) to pull a catalog tool's full (minified) schema into the
// NEXT loop iteration. This keeps tool-schema tokens flat as the catalog scales
// and preserves access to tools ranking didn't surface (the cardinal rule:
// never leave the model unable to reach a tool it needs).
//
// All major tool APIs require the full schema up front for every OFFERED tool
// (vLLM compiles each into a guided-decoding FSM), so the expansion happens
// across loop iterations at Tenant's orchestration layer, not within one wire
// request.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"tenant/internal/model"
)

const loadToolName = "load_tool"

// splitLoadToolCalls separates load_tool calls from the rest. CRITICAL: the
// returned `rest` is a FRESH slice (never the input's backing array) — the input
// slice is shared by reference with the durable assistant message already
// appended to the working set, so an in-place filter (`calls[:0]`) would corrupt
// recorded history (orphaned/duplicate tool_use pairs → the model:400 class).
// Returns (nil, nil) when there are no load_tool calls, so the caller leaves
// resp.ToolCalls untouched (no allocation on the common path).
func splitLoadToolCalls(calls []model.ToolCall) (loads, rest []model.ToolCall) {
	for _, c := range calls {
		if c.Name == loadToolName {
			loads = append(loads, c)
		}
	}
	if len(loads) == 0 {
		return nil, nil
	}
	rest = make([]model.ToolCall, 0, len(calls)-len(loads))
	for _, c := range calls {
		if c.Name != loadToolName {
			rest = append(rest, c)
		}
	}
	return loads, rest
}

// loadToolSpec is the cheap meta-tool the model uses to activate a catalog tool.
func loadToolSpec() model.ToolSpec {
	return model.ToolSpec{
		Name:        loadToolName,
		Description: "Activate a tool from the Tool catalog so you can call it. Pass the tool's EXACT name. Use this when a tool you need is in the catalog but not in your current tool set; it becomes callable on your next step.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"exact tool name from the Tool catalog"}},"required":["name"]}`),
	}
}

// catalogLineMax bounds each catalog entry's description so the catalog stays
// cheap (one short line per tool, vs the full schema).
const catalogLineMax = 120

// renderToolCatalog builds the system-prompt catalog: one "name: short desc"
// line for every enabled tool NOT already active this turn (active tools carry
// their full schema in the native array, so listing them as loadable is noise).
// Returns "" when there's nothing extra to offer.
func renderToolCatalog(all []model.ToolSpec, active map[string]bool) string {
	rows := make([]model.ToolSpec, 0, len(all))
	for _, t := range all {
		if t.Name == loadToolName || active[t.Name] {
			continue
		}
		rows = append(rows, t)
	}
	if len(rows) == 0 {
		return ""
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	var b strings.Builder
	b.WriteString("\n\n## Tool catalog (load on demand)\n")
	b.WriteString("These tools are available but NOT in your active tool set right now. To use one, call ")
	b.WriteString(loadToolName)
	b.WriteString(" with its exact name; it becomes callable on your next step. Don't guess its arguments until it's loaded.\n")
	for _, t := range rows {
		fmt.Fprintf(&b, "- %s: %s\n", t.Name, catalogDesc(t.Description))
	}
	return strings.TrimRight(b.String(), "\n")
}

// catalogDesc reduces a tool description to a single short catalog line.
func catalogDesc(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if r := []rune(s); len(r) > catalogLineMax {
		return string(r[:catalogLineMax]) + "…"
	}
	return s
}

// buildLazyTools assembles the per-iteration native tool array: the load_tool
// meta-tool, the ranked working set (already minified), then each on-demand
// loaded tool's full schema (minified). Deduped by name (a loaded tool that was
// also ranked in appears once); name-sorted loaded set for reproducibility.
func (a *Agent) buildLazyTools(base []model.ToolSpec, loaded map[string]bool) []model.ToolSpec {
	out := make([]model.ToolSpec, 0, len(base)+len(loaded)+1)
	seen := make(map[string]bool, len(base)+len(loaded)+1)
	add := func(s model.ToolSpec) {
		if !seen[s.Name] {
			seen[s.Name] = true
			out = append(out, s)
		}
	}
	add(loadToolSpec())
	for _, s := range base {
		add(s)
	}
	names := make([]string, 0, len(loaded))
	for n := range loaded {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if seen[n] {
			continue
		}
		if spec, ok := a.cfg.Tools.Get(n); ok {
			spec.Parameters = model.MinifySchema(spec.Parameters) // TEN-227: lean schema
			add(spec)
		}
	}
	return out
}

// handleLoadTool services a load_tool call in-agent (it isn't a mux tool): it
// validates the name against the enabled catalog, marks it loaded for the turn,
// and feeds a tool result. The next loop iteration's buildLazyTools then carries
// that tool's schema. Unknown/disabled names are refused (the catalog only lists
// enabled tools; Get returns enabled tools only).
func (a *Agent) handleLoadTool(ctx context.Context, call model.ToolCall, loaded map[string]bool) {
	var args struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(call.Arguments, &args)
	name := strings.TrimSpace(args.Name)
	res := ToolCallResult{Call: call}
	switch {
	case name == "":
		res.Result, res.IsError = "load_tool needs a non-empty \"name\" (the exact tool name from the catalog).", true
	case name == loadToolName:
		res.Result, res.IsError = "load_tool is always available; name a catalog tool instead.", true
	case loaded[name]:
		res.Result = fmt.Sprintf("%q is already loaded — call it directly.", name)
	default:
		if spec, ok := a.cfg.Tools.Get(name); ok {
			loaded[name] = true
			res.Result = fmt.Sprintf("✓ loaded %q (%s) — it's now in your tools; call it on your next step.", name, catalogDesc(spec.Description))
		} else {
			res.Result, res.IsError = fmt.Sprintf("no enabled tool named %q in the catalog — use a name exactly as listed in the Tool catalog.", name), true
		}
	}
	a.emit(Event{Kind: EventToolResult, Tool: loadToolName, Result: res.Result, IsErr: res.IsError})
	a.feedToolResult(ctx, res, 0) // short fixed control message — never needs a context cap
}
