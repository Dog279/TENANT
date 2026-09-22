package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"tenant/internal/model"
	"tenant/internal/tui"
)

// fakeAdopter stands in for mcpTUIControl: records what the tools asked of it.
type fakeAdopter struct {
	servers []tui.MCPServerInfo
	tools   map[string][]string
	addErr  error
	adds    []string
	removes []string
	trusts  []string
}

func (f *fakeAdopter) List() []tui.MCPServerInfo { return f.servers }
func (f *fakeAdopter) Add(url string) (tui.MCPServerInfo, error) {
	f.adds = append(f.adds, url)
	if f.addErr != nil {
		return tui.MCPServerInfo{}, f.addErr
	}
	info := tui.MCPServerInfo{Label: "mcp:127.0.0.1:9000", URL: url, Enabled: true, ToolCount: 2, Trust: "ask"}
	f.servers = append(f.servers, info)
	return info, nil
}
func (f *fakeAdopter) Remove(target string) (bool, error) {
	f.removes = append(f.removes, target)
	return target != "nothing", nil
}
func (f *fakeAdopter) SetTrust(target, mode string) (tui.MCPServerInfo, error) {
	f.trusts = append(f.trusts, target+"="+mode)
	for i := range f.servers {
		if f.servers[i].Label == target || f.servers[i].URL == target {
			f.servers[i].Trust = mode
			return f.servers[i], nil
		}
	}
	return tui.MCPServerInfo{}, errors.New("no such server")
}
func (f *fakeAdopter) ToolNames(label string) []string { return f.tools[label] }

// recordingConfirm answers per action and records every prompt the tools raised.
type recordingConfirm struct {
	answers map[string]bool
	asked   []string
}

func (r *recordingConfirm) confirm(_ context.Context, action, detail string) bool {
	r.asked = append(r.asked, action+": "+detail)
	return r.answers[action]
}

func mcpArgs(t *testing.T, v map[string]any) json.RawMessage {
	t.Helper()
	b, _ := json.Marshal(v)
	return b
}

func newTestMCPTools(answers map[string]bool) (*mcpToolPlugin, *fakeAdopter, *recordingConfirm) {
	fa := &fakeAdopter{tools: map[string][]string{"mcp:127.0.0.1:9000": {"echo", "write_note"}}}
	rc := &recordingConfirm{answers: answers}
	return newMCPToolPlugin(fa, rc.confirm), fa, rc
}

// The adoption flow: the operator approves the connect, then (because the
// agent asked for allow) decides whether the server's tools run unprompted.
func TestMCPTools_AddFlow_ApprovedAndTrusted(t *testing.T) {
	p, fa, rc := newTestMCPTools(map[string]bool{"mcp_add": true, "mcp_trust": true})
	out, isErr, err := p.Dispatch(context.Background(), model.ToolCall{Name: "mcp_add",
		Arguments: mcpArgs(t, map[string]any{"url": "http://127.0.0.1:9000/mcp", "trust": "allow", "reason": "disk watcher I just built"})})
	if err != nil || isErr {
		t.Fatalf("add: %q isErr=%v err=%v", out, isErr, err)
	}
	if len(fa.adds) != 1 || fa.adds[0] != "http://127.0.0.1:9000/mcp" {
		t.Errorf("Add calls = %v", fa.adds)
	}
	if len(rc.asked) != 2 || !strings.HasPrefix(rc.asked[0], "mcp_add: http://127.0.0.1:9000/mcp — disk watcher") || !strings.HasPrefix(rc.asked[1], "mcp_trust: mcp:127.0.0.1:9000 — allow its 2 tool(s) (echo, write_note)") {
		t.Errorf("prompts = %q", rc.asked)
	}
	if len(fa.trusts) != 1 || fa.trusts[0] != "mcp:127.0.0.1:9000=allow" {
		t.Errorf("trust calls = %v", fa.trusts)
	}
	if !strings.Contains(out, "2 tool(s) now available: echo, write_note") || !strings.Contains(out, "trust: allow") {
		t.Errorf("summary = %q", out)
	}
}

// Trust declined ⇒ connected with ask (no trust call); trust not requested ⇒ no
// second prompt at all.
func TestMCPTools_AddFlow_TrustDeclinedOrNotRequested(t *testing.T) {
	p, fa, rc := newTestMCPTools(map[string]bool{"mcp_add": true, "mcp_trust": false})
	out, isErr, _ := p.Dispatch(context.Background(), model.ToolCall{Name: "mcp_add",
		Arguments: mcpArgs(t, map[string]any{"url": "http://127.0.0.1:9000/mcp", "trust": "allow"})})
	if isErr || len(rc.asked) != 2 || len(fa.trusts) != 0 || !strings.Contains(out, "trust: ask") {
		t.Errorf("declined trust: out=%q asked=%q trusts=%v", out, rc.asked, fa.trusts)
	}

	p2, fa2, rc2 := newTestMCPTools(map[string]bool{"mcp_add": true, "mcp_trust": true})
	out, isErr, _ = p2.Dispatch(context.Background(), model.ToolCall{Name: "mcp_add",
		Arguments: mcpArgs(t, map[string]any{"url": "http://127.0.0.1:9000/mcp"})})
	if isErr || len(rc2.asked) != 1 || len(fa2.trusts) != 0 || !strings.Contains(out, "trust: ask") {
		t.Errorf("default trust: out=%q asked=%q trusts=%v", out, rc2.asked, fa2.trusts)
	}
}

// A declined adoption never connects; a missing confirm hook fails closed; bad
// input never reaches the control; a connect error is reported, not hidden.
func TestMCPTools_AddFlow_DeclinedAndInvalid(t *testing.T) {
	p, fa, _ := newTestMCPTools(map[string]bool{})
	out, isErr, _ := p.Dispatch(context.Background(), model.ToolCall{Name: "mcp_add", Arguments: mcpArgs(t, map[string]any{"url": "http://127.0.0.1:9000/mcp"})})
	if !isErr || !strings.Contains(out, "not approved") || len(fa.adds) != 0 {
		t.Errorf("declined: out=%q adds=%v", out, fa.adds)
	}
	closed := newMCPToolPlugin(fa, nil)
	if out, isErr, _ := closed.Dispatch(context.Background(), model.ToolCall{Name: "mcp_add", Arguments: mcpArgs(t, map[string]any{"url": "http://x/mcp"})}); !isErr || len(fa.adds) != 0 {
		t.Errorf("nil confirm must fail closed: %q", out)
	}
	for _, bad := range []map[string]any{{"url": "127.0.0.1:9000/mcp"}, {"url": "http://x/mcp", "trust": "deny"}, {"url": ""}} {
		if out, isErr, _ := p.Dispatch(context.Background(), model.ToolCall{Name: "mcp_add", Arguments: mcpArgs(t, bad)}); !isErr || len(fa.adds) != 0 {
			t.Errorf("bad args %v must be rejected before any prompt: %q", bad, out)
		}
	}
	pe, fae, _ := newTestMCPTools(map[string]bool{"mcp_add": true})
	fae.addErr = errors.New("dial tcp: connection refused")
	if out, isErr, _ := pe.Dispatch(context.Background(), model.ToolCall{Name: "mcp_add", Arguments: mcpArgs(t, map[string]any{"url": "http://127.0.0.1:1/mcp"})}); !isErr || !strings.Contains(out, "connection refused") {
		t.Errorf("connect error should surface: %q", out)
	}
}

func TestMCPTools_TrustRemoveList(t *testing.T) {
	p, fa, rc := newTestMCPTools(map[string]bool{"mcp_trust": true, "mcp_remove": true})
	fa.servers = []tui.MCPServerInfo{{Label: "mcp:127.0.0.1:9000", URL: "http://127.0.0.1:9000/mcp", Enabled: true, ToolCount: 2, Trust: "ask"}}

	out, isErr, _ := p.Dispatch(context.Background(), model.ToolCall{Name: "mcp_trust", Arguments: mcpArgs(t, map[string]any{"target": "mcp:127.0.0.1:9000", "mode": "deny"})})
	if isErr || fa.trusts[0] != "mcp:127.0.0.1:9000=deny" || !strings.Contains(out, "now deny") || rc.asked[0] != "mcp_trust: mcp:127.0.0.1:9000 → deny" {
		t.Errorf("trust: out=%q trusts=%v asked=%q", out, fa.trusts, rc.asked)
	}
	if out, isErr, _ := p.Dispatch(context.Background(), model.ToolCall{Name: "mcp_trust", Arguments: mcpArgs(t, map[string]any{"target": "x", "mode": "maybe"})}); !isErr {
		t.Errorf("bad mode must be rejected: %q", out)
	}

	out, isErr, _ = p.Dispatch(context.Background(), model.ToolCall{Name: "mcp_list"})
	if isErr || !strings.Contains(out, "mcp:127.0.0.1:9000") || !strings.Contains(out, "trust: deny") {
		t.Errorf("list = %q", out)
	}

	out, isErr, _ = p.Dispatch(context.Background(), model.ToolCall{Name: "mcp_remove", Arguments: mcpArgs(t, map[string]any{"target": "mcp:127.0.0.1:9000"})})
	if isErr || fa.removes[0] != "mcp:127.0.0.1:9000" || !strings.Contains(out, "removed") {
		t.Errorf("remove: out=%q removes=%v", out, fa.removes)
	}
	if out, isErr, _ := p.Dispatch(context.Background(), model.ToolCall{Name: "mcp_remove", Arguments: mcpArgs(t, map[string]any{"target": "nothing"})}); !isErr || !strings.Contains(out, "no connected server matched") {
		t.Errorf("unmatched remove: %q", out)
	}
	pd, fad, _ := newTestMCPTools(map[string]bool{})
	if out, isErr, _ := pd.Dispatch(context.Background(), model.ToolCall{Name: "mcp_remove", Arguments: mcpArgs(t, map[string]any{"target": "x"})}); !isErr || len(fad.removes) != 0 {
		t.Errorf("declined remove must not remove: %q", out)
	}
}

// The management tools sit in their own permission category, and are cut from
// unattended cron runners like the cron tools themselves.
func TestMCPTools_CategoryAndCronCut(t *testing.T) {
	for _, a := range []string{"mcp_add", "mcp_remove", "mcp_trust"} {
		if categorize(a) != catMCP {
			t.Errorf("categorize(%s) = %s, want %s", a, categorize(a), catMCP)
		}
		if !notCron(a) {
			t.Errorf("%s must be cut from cron runners", a)
		}
	}
	if categorize("mcp:127.0.0.1:9000:write_note") == catMCP {
		t.Error("a connector's own calls are not in the mcp category (they follow its trust mode)")
	}
	b := newApprovalBroker(nil)
	if ok, err := b.SetPermission("mcp", "allow"); !ok || err != nil {
		t.Errorf("the mcp category must be settable to allow (the automation switch): ok=%v err=%v", ok, err)
	}
	found := false
	for _, pi := range b.Permissions() {
		if pi.Category == "mcp" && pi.Mode == "allow" {
			found = true
		}
	}
	if !found {
		t.Error("/permissions should list mcp")
	}
	specs := newMCPToolPlugin(&fakeAdopter{}, nil).Tools()
	for _, s := range specs {
		if (s.Name != "mcp_list") != s.Gated {
			t.Errorf("%s gated=%v (only mcp_list is read-only)", s.Name, s.Gated)
		}
	}
}
