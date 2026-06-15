package main

import (
	"context"
	"testing"

	"tenant/internal/agent"
	"tenant/internal/model"
)

type recordDispatcher struct{ called []string }

func (r *recordDispatcher) Dispatch(_ context.Context, call model.ToolCall) (string, bool, error) {
	r.called = append(r.called, call.Name)
	return "ok", false, nil
}

func TestRestrictForDiscord_AllToolsExposed(t *testing.T) {
	full := []model.ToolSpec{
		{Name: "web_search"},
		{Name: "os_exec", Gated: true},
		{Name: "os_write", Gated: true},
		{Name: "discord_send_message", Gated: true},
		{Name: "team_spawn"},
		{Name: "orchestr_run"},
		{Name: "sql_exec", Gated: true},
		{Name: "createJiraIssue", Gated: true},
	}
	live := agent.NewStaticRegistry()
	for _, sp := range full {
		live.Register(sp)
	}
	inner := &recordDispatcher{}
	reg, disp, _ := restrictForDiscord(live, inner)

	// Every tool must be in the registry — no filtering.
	regNames := map[string]bool{}
	for _, sp := range reg.All() {
		regNames[sp.Name] = true
	}
	for _, sp := range full {
		if !regNames[sp.Name] {
			t.Errorf("%s must be in the Discord registry (no tool wall)", sp.Name)
		}
	}

	// Every tool must dispatch through to inner — nothing refused.
	for _, sp := range full {
		if _, isErr, _ := disp.Dispatch(context.Background(), model.ToolCall{Name: sp.Name}); isErr {
			t.Errorf("%s must dispatch through (no tool wall)", sp.Name)
		}
	}
	if len(inner.called) != len(full) {
		t.Fatalf("expected %d calls to inner, got %d (%v)", len(full), len(inner.called), inner.called)
	}
}

// TEN-229: the registry is LIVE — a tool registered AFTER the relay agent was
// built (e.g. the Atlassian/Jira MCP adopting its tools asynchronously, or a
// mid-session /enable) must be visible to the Discord agent immediately. This
// is the whole fix: a frozen startup snapshot was why remote turns couldn't see
// the Jira tools.
func TestRestrictForDiscord_RegistryIsLive(t *testing.T) {
	live := agent.NewStaticRegistry()
	live.Register(model.ToolSpec{Name: "web_search"})
	reg, _, _ := restrictForDiscord(live, &recordDispatcher{})

	if _, ok := reg.Get("jira_transition"); ok {
		t.Fatal("jira_transition should not exist yet")
	}
	// A tool comes online after the relay was already constructed.
	live.Register(model.ToolSpec{Name: "jira_transition", Gated: true})

	if _, ok := reg.Get("jira_transition"); !ok {
		t.Error("a tool registered after the relay was built must appear in the live Discord registry")
	}
	names := map[string]bool{}
	for _, sp := range reg.All() {
		names[sp.Name] = true
	}
	if !names["jira_transition"] {
		t.Error("All() must reflect tools added after construction")
	}
}
