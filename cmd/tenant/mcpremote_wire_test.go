package main

import (
	"context"
	"testing"

	"tenant/internal/model"
)

// fakeActivatedPlugin stands in for a connected remote MCP dispatcher: it
// advertises tools only after activation (the stub had none).
type fakeActivatedPlugin struct{ specs []model.ToolSpec }

func (f fakeActivatedPlugin) Tools() []model.ToolSpec { return f.specs }
func (f fakeActivatedPlugin) Dispatch(_ context.Context, call model.ToolCall) (string, bool, error) {
	return "called:" + call.Name, false, nil
}

// TestMaybeActivate_MergesZeroSpecStubTools is the end-to-end regression for the
// HIGH the security review caught: a zero-spec stub's discovered tools must
// become reachable + enabled on /enable, while never shadowing a local tool.
func TestMaybeActivate_ZeroSpecStubBecomesReachable(t *testing.T) {
	m := newToolMux()

	// A pre-registered local tool that the remote must NOT be able to shadow.
	m.add("local", fakeActivatedPlugin{specs: []model.ToolSpec{{Name: "shared_tool"}}})

	// A zero-spec remote stub + an activator that "connects" and reveals tools,
	// including one whose name collides with the local tool.
	m.add("mcp:test", stubPlugin{hint: "enable to connect"})
	m.SetEnabled("mcp:test", false)
	m.registerActivator("mcp:test", func() (plugin, func(), error) {
		return fakeActivatedPlugin{specs: []model.ToolSpec{
			{Name: "remote_read"},
			{Name: "remote_write", Gated: true},
			{Name: "shared_tool"}, // must be ignored (first-registrant-wins)
		}}, nil, nil
	})

	n, scope, err := m.SetEnabled("mcp:test", true)
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if scope != "plugin" || n < 2 {
		t.Fatalf("expected to enable the remote plugin's tools, got n=%d scope=%q", n, scope)
	}

	// Both remote tools are now reachable + enabled.
	for _, name := range []string{"remote_read", "remote_write"} {
		e, ok := m.byName[name]
		if !ok {
			t.Fatalf("%q not merged into the mux after activation", name)
		}
		if !e.enabled {
			t.Errorf("%q should be enabled after /enable", name)
		}
		if e.plugin != "mcp:test" {
			t.Errorf("%q owned by %q, want mcp:test", name, e.plugin)
		}
	}

	// Shadow protection: shared_tool still belongs to the local plugin.
	if e := m.byName["shared_tool"]; e.plugin != "local" {
		t.Errorf("remote shadowed a local tool: shared_tool owned by %q, want local", e.plugin)
	}

	// Dispatch routes to the activated remote dispatcher.
	out, _, err := m.Dispatch(context.Background(), model.ToolCall{Name: "remote_read"})
	if err != nil || out != "called:remote_read" {
		t.Errorf("Dispatch(remote_read) = %q, %v", out, err)
	}
}
