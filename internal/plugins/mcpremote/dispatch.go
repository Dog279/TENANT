package mcpremote

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tenant/internal/model"
)

// Policy gates remote writes by blast radius (same shape as the local plugins).
type Policy struct {
	AllowWrite bool
	Confirm    func(ctx context.Context, action, detail string) bool
}

// Dispatcher adapts a remote MCP session to Tenant's agent tool interface:
// Tools() from the server's tools/list (cached), Dispatch() → tools/call.
type Dispatcher struct {
	label   string
	session *mcp.ClientSession
	specs   []model.ToolSpec
	gated   map[string]bool
	policy  Policy
}

// newDispatcher lists the remote tools and caches their specs + gate status.
// Deny-by-default: a tool is gated (needs approval) UNLESS the operator trusts
// this server's annotations AND the server explicitly marks it read-only. The
// MCP spec says unannotated tools default to write+destructive, and that clients
// must not trust a server's annotations unless the server is trusted — both of
// which this honors.
//
// ungate is an explicit allowlist of tool NAMES that bypass gating regardless of
// the annotation rule (TEN-252): a tool is ungated iff it's in `ungate` OR the
// annotation rule ungates it. Peers pass trustAnnotations=false + the fixed
// federation toolset, so a compromised peer that advertises a NOVEL read-only
// tool can't get it auto-called — only the known federation tools ungate. nil
// ungate ⇒ pure annotation rule (the remote-MCP / Atlassian path, unchanged).
func newDispatcher(ctx context.Context, label string, session *mcp.ClientSession, trustAnnotations bool, ungate map[string]bool, policy Policy) (*Dispatcher, error) {
	res, err := session.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	d := &Dispatcher{label: label, session: session, policy: policy, gated: map[string]bool{}}
	for _, t := range res.Tools {
		readOnly := t.Annotations != nil && t.Annotations.ReadOnlyHint
		gated := gatedTool(readOnly, trustAnnotations) && !ungate[t.Name]
		d.gated[t.Name] = gated
		schema, _ := json.Marshal(t.InputSchema)
		if len(schema) == 0 || string(schema) == "null" {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		d.specs = append(d.specs, model.ToolSpec{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  schema,
			Gated:       gated,
		})
	}
	return d, nil
}

// gatedTool decides whether a remote tool requires approval. Deny-by-default:
// a tool is ungated ONLY when the operator trusts this server's annotations AND
// the server explicitly marked it read-only. Everything else is gated.
func gatedTool(readOnly, trustAnnotations bool) bool {
	return !(trustAnnotations && readOnly)
}

func (d *Dispatcher) Tools() []model.ToolSpec { return d.specs }

func (d *Dispatcher) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	if d.gated[call.Name] {
		if err := d.gate(ctx, call.Name); err != nil {
			return err.Error(), true, nil
		}
	}
	var args any
	if len(call.Arguments) > 0 {
		args = json.RawMessage(call.Arguments)
	}
	res, err := d.session.CallTool(ctx, &mcp.CallToolParams{Name: call.Name, Arguments: args})
	if err != nil {
		return "remote tool call failed: " + err.Error(), true, nil
	}
	text := renderContent(res.Content)
	if text == "" && res.IsError {
		text = "remote tool reported an error"
	}
	return text, res.IsError, nil
}

// CallRawJSON calls a remote tool and returns its result as JSON bytes — the
// StructuredContent (typed result) when present, else the flattened text
// content. Unlike Dispatch (which renders human text for the agent), this is for
// callers that need the machine-readable result, e.g. the peer_hello capability
// stamp whose fields live in StructuredContent, not a text block. It applies the
// SAME deny-by-default gate as Dispatch — a structured call must not be a way to
// invoke a gated/write tool without approval.
func (d *Dispatcher) CallRawJSON(ctx context.Context, call model.ToolCall) ([]byte, error) {
	// Same deny-by-default gate as Dispatch — exporting a structured call must not
	// become a way to invoke a gated/write tool without approval. (peer_hello is
	// marked read-only by a trusting server, so it's ungated and this is a no-op.)
	if d.gated[call.Name] {
		if err := d.gate(ctx, call.Name); err != nil {
			return nil, err
		}
	}
	var args any
	if len(call.Arguments) > 0 {
		args = json.RawMessage(call.Arguments)
	}
	res, err := d.session.CallTool(ctx, &mcp.CallToolParams{Name: call.Name, Arguments: args})
	if err != nil {
		return nil, err
	}
	if res.IsError {
		return nil, fmt.Errorf("remote tool reported an error")
	}
	if res.StructuredContent != nil {
		return json.Marshal(res.StructuredContent)
	}
	return []byte(renderContent(res.Content)), nil
}

func (d *Dispatcher) gate(ctx context.Context, name string) error {
	if d.policy.AllowWrite {
		return nil
	}
	detail := fmt.Sprintf("remote MCP tool %q (%s) — may modify external state", name, d.label)
	if d.policy.Confirm != nil && d.policy.Confirm(ctx, d.label+":"+name, detail) {
		return nil
	}
	return fmt.Errorf("blocked: %s and was not approved (not marked read-only by a trusted server). "+
		"Approve it, or trust this server's annotations. Blast-radius boundary, not a bug", detail)
}

// renderContent flattens an MCP tool result's content blocks to text (the agent
// consumes strings). Non-text blocks (images, etc.) are skipped.
func renderContent(content []mcp.Content) string {
	var b strings.Builder
	for _, c := range content {
		if tc, ok := c.(*mcp.TextContent); ok {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(tc.Text)
		}
	}
	return strings.TrimSpace(b.String())
}
