package toolmux

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"tenant/internal/model"
	"tenant/internal/plugins/mcpremote"
)

// fakeAuthPlugin is a remote connector whose auth health is scripted.
type fakeAuthPlugin struct{ h mcpremote.Health }

func (f *fakeAuthPlugin) Tools() []model.ToolSpec {
	return []model.ToolSpec{{Name: "searchJiraIssuesUsingJql", Parameters: json.RawMessage(`{"type":"object"}`)}}
}
func (f *fakeAuthPlugin) Dispatch(context.Context, model.ToolCall) (string, bool, error) {
	return "", false, nil
}
func (f *fakeAuthPlugin) AuthHealth() mcpremote.Health { return f.h }

func TestRemoteMCPList_ReportsDeadAuth(t *testing.T) {
	m := New()
	const label, url = "mcp:mcp.atlassian.com", "https://mcp.atlassian.com/v1/mcp"
	m.mcpServers[label] = url
	since := time.Unix(1_700_000_000, 0).UTC()
	m.Add(label, &fakeAuthPlugin{h: mcpremote.Health{NeedsReauth: true, Err: "invalid_grant", Since: since}})

	list := m.RemoteMCPList()
	if len(list) != 1 {
		t.Fatalf("want 1 server, got %+v", list)
	}
	s := list[0]
	if !s.NeedsReauth || s.AuthErr != "invalid_grant" || !s.AuthSince.Equal(since) {
		t.Fatalf("status must carry the dead-auth state: %+v", s)
	}
	if s.ToolCount != 1 {
		t.Errorf("tool count still reported (frozen from connect time): %+v", s)
	}
	if probs := m.RemoteMCPAuthProblems(); len(probs) != 1 || probs[0].Label != label {
		t.Fatalf("RemoteMCPAuthProblems = %+v", probs)
	}

	// A healthy connector reports nothing.
	m.mcpServers["mcp:ok"] = "https://ok/mcp"
	m.Add("mcp:ok", &fakeAuthPlugin{})
	if probs := m.RemoteMCPAuthProblems(); len(probs) != 1 {
		t.Errorf("a healthy connector must not be listed as a problem: %+v", probs)
	}
}

// The notifier installed AFTER connectors are registered is still what a
// connector's OnAuthChange reaches (read at fire time, not captured at
// registration).
func TestMCPAuthNotifier_InstalledLate(t *testing.T) {
	m := New()
	var got []string
	m.SetMCPAuthNotifier(func(label, url string, needs bool, err string) {
		got = append(got, label+"|"+url+"|"+map[bool]string{true: "dead", false: "ok"}[needs]+"|"+err)
	})
	m.fireMCPAuthChange("mcp:x", "https://x/mcp", true, "invalid_grant")
	m.fireMCPAuthChange("mcp:x", "https://x/mcp", false, "")
	if len(got) != 2 || got[0] != "mcp:x|https://x/mcp|dead|invalid_grant" || got[1] != "mcp:x|https://x/mcp|ok|" {
		t.Fatalf("notifier calls = %v", got)
	}
}
