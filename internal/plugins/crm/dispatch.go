package crm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"tenant/internal/model"
)

// Policy gates crm-tool subcommands by blast radius — same shape as
// imessage/osys/gsuite. The READ subcommands (search/lookup/history/show) are
// always allowed once the plugin is enabled. The heavier/potentially-mutating
// subcommands (ask/align/commitments-list) are OFF unless AllowMutate, or a
// per-action Confirm explicitly approves them (nil Confirm + !AllowMutate ⇒
// deny). The model cannot change the policy.
type Policy struct {
	AllowMutate bool
	Confirm     func(ctx context.Context, action, detail string) bool
}

// mutatingSubcommands classifies which subcommands route through the gate.
// Rationale: search/lookup/history/show are plainly read-only lookups; ask,
// align, and commitments-list are the heavier set that can write/derive state
// (ask runs an LLM/agent path; align mutates alignment; commitments-list is
// grouped with them as the cautious default). Treating this set as gated is
// the deny-by-default posture — a read misclassified as gated only ever costs
// an approval prompt, never a silent mutation.
var mutatingSubcommands = map[string]bool{
	"ask":              true,
	"align":            true,
	"commitments-list": true,
}

// gate enforces the Policy for a (potentially) mutating subcommand. Read
// subcommands never reach here. With neither AllowMutate nor an approving
// Confirm, the op is BLOCKED — a blast-radius boundary, not a bug.
func (p Policy) gate(ctx context.Context, action, detail string) error {
	if p.AllowMutate {
		return nil
	}
	if p.Confirm != nil && p.Confirm(ctx, action, detail) {
		return nil
	}
	return fmt.Errorf("blocked: crm %s is a gated (potentially state-changing) operation and was "+
		"not approved — enable it (--crm-allow-mutate) or confirm. This is a blast-radius boundary, "+
		"not a bug", action)
}

// Dispatcher implements agent.ToolDispatcher for the crm plugin.
type Dispatcher struct {
	svc    *Service
	policy Policy
}

// NewDispatcher builds a dispatcher. Passing a nil svc is valid for Tools()-
// only use (the stub catalog entry); Dispatch on a nil svc reports an error.
func NewDispatcher(svc *Service, policy Policy) *Dispatcher {
	return &Dispatcher{svc: svc, policy: policy}
}

func (d *Dispatcher) Tools() []model.ToolSpec {
	obj := func(props string, req ...string) json.RawMessage {
		r := ""
		for i, x := range req {
			if i > 0 {
				r += ","
			}
			r += `"` + x + `"`
		}
		return json.RawMessage(`{"type":"object","properties":{` + props + `},"required":[` + r + `]}`)
	}
	queryProp := `"query":{"type":"string","description":"the search/lookup text passed positionally to crm-tool"}`
	return []model.ToolSpec{
		// Read subcommands — always allowed.
		{Name: "crm_search", Description: "Search the CRM (people/notes/meetings) via crm-tool's `search` subcommand. Read-only.",
			Parameters: obj(queryProp, "query")},
		{Name: "crm_lookup", Description: "Look up a specific CRM record via crm-tool's `lookup` subcommand. Read-only.",
			Parameters: obj(queryProp, "query")},
		{Name: "crm_history", Description: "Show interaction history for a person/entity via crm-tool's `history` subcommand. Read-only.",
			Parameters: obj(queryProp, "query")},
		{Name: "crm_show", Description: "Show full details of a CRM record via crm-tool's `show` subcommand. Read-only.",
			Parameters: obj(queryProp, "query")},
		// Gated subcommands — potentially state-changing / heavier.
		{Name: "crm_ask", Description: "Ask the CRM assistant a natural-language question via crm-tool's `ask` subcommand. GATED: requires operator approval (heavier/agentic path).",
			Parameters: obj(queryProp, "query"), Gated: true},
		{Name: "crm_align", Description: "Run crm-tool's `align` subcommand. GATED: requires operator approval (may change alignment state).",
			Parameters: obj(queryProp, "query"), Gated: true},
		{Name: "crm_commitments_list", Description: "List commitments via crm-tool's `commitments-list` subcommand. GATED: requires operator approval.",
			Parameters: obj(queryProp), Gated: true},
	}
}

// toolToSubcommand maps each tool name to the crm-tool subcommand it runs and
// whether that subcommand is gated. Single source of truth so the routing and
// the gate stay consistent. Unknown tool ⇒ ok=false.
func toolToSubcommand(tool string) (sub string, gated bool, ok bool) {
	switch tool {
	case "crm_search":
		return "search", false, true
	case "crm_lookup":
		return "lookup", false, true
	case "crm_history":
		return "history", false, true
	case "crm_show":
		return "show", false, true
	case "crm_ask":
		return "ask", true, true
	case "crm_align":
		return "align", true, true
	case "crm_commitments_list":
		return "commitments-list", true, true
	default:
		return "", false, false
	}
}

func (d *Dispatcher) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	sub, gated, ok := toolToSubcommand(call.Name)
	if !ok {
		return "unknown crm tool: " + call.Name, true, nil
	}
	if d.svc == nil {
		return "crm plugin is not configured — set --crm-tool-path (or $CRM_TOOL_PATH)", true, nil
	}

	var a struct {
		Query string `json:"query"`
	}
	// commitments-list takes no query; everything else accepts an optional one.
	if len(call.Arguments) > 0 {
		if err := json.Unmarshal(call.Arguments, &a); err != nil {
			return "invalid arguments: " + err.Error(), true, nil
		}
	}
	a.Query = strings.TrimSpace(a.Query)

	// Build positional argv (never a shell string). Empty query ⇒ no extra arg.
	var args []string
	if a.Query != "" {
		args = []string{a.Query}
	}

	if gated {
		detail := fmt.Sprintf("crm-tool %s %q", sub, a.Query)
		if err := d.policy.gate(ctx, call.Name, detail); err != nil {
			return err.Error(), true, nil
		}
	}

	out, err := d.svc.Exec(ctx, sub, args...)
	if err != nil {
		// Surface the (capped) output alongside the error — a non-zero exit
		// or rejected subcommand is signal, not a transport failure.
		if strings.TrimSpace(out) != "" {
			return fmt.Sprintf("%s\n[%v]", out, err), true, nil
		}
		return err.Error(), true, nil
	}
	return out, false, nil
}
