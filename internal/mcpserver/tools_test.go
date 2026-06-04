package mcpserver_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"tenant/internal/mcpserver"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/model"
)

type fakeToolSource struct {
	gotName string
	gotArgs json.RawMessage
	isErr   bool
}

func (f *fakeToolSource) All() []model.ToolSpec {
	return []model.ToolSpec{{
		Name:        "wiki_search",
		Description: "search the wiki",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
	}}
}

func (f *fakeToolSource) Dispatch(_ context.Context, c model.ToolCall) (string, bool, error) {
	f.gotName, f.gotArgs = c.Name, c.Arguments
	return "ran " + c.Name, f.isErr, nil
}

func newToolServer(t *testing.T, src mcpserver.ToolSource) *mcpserver.Server {
	t.Helper()
	es, err := episodic.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = es.Close() })
	ss, err := semantic.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	srv, err := mcpserver.New(mcpserver.Config{AgentID: "main", Episodic: es, Semantic: ss, Tools: src})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func TestToolSource_ListedAndRouted(t *testing.T) {
	src := &fakeToolSource{}
	h := newToolServer(t, src).Handler()
	ctx := context.Background()

	// tools/list merges the source's tools with the memory tools.
	listRaw, err := h.HandleRequest(ctx, "tools/list", nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if !strings.Contains(string(listRaw), "wiki_search") || !strings.Contains(string(listRaw), "memory_search") {
		t.Fatalf("tools/list should include both source + memory tools: %s", listRaw)
	}

	// initialize advertises listChanged when a ToolSource is present.
	initRaw, err := h.HandleRequest(ctx, "initialize", nil)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if !strings.Contains(string(initRaw), `"listChanged":true`) {
		t.Fatalf("initialize should advertise tools.listChanged: %s", initRaw)
	}

	// tools/call for a non-memory tool routes to the source's Dispatch.
	callRaw, err := h.HandleRequest(ctx, "tools/call",
		json.RawMessage(`{"name":"wiki_search","arguments":{"q":"go"}}`))
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	if src.gotName != "wiki_search" || !strings.Contains(string(src.gotArgs), `"q":"go"`) {
		t.Fatalf("dispatch not routed correctly: name=%q args=%s", src.gotName, src.gotArgs)
	}
	if !strings.Contains(string(callRaw), "ran wiki_search") {
		t.Fatalf("result not returned: %s", callRaw)
	}
}

func TestToolSource_ErrorBecomesToolError(t *testing.T) {
	src := &fakeToolSource{isErr: true}
	h := newToolServer(t, src).Handler()
	raw, err := h.HandleRequest(context.Background(), "tools/call",
		json.RawMessage(`{"name":"wiki_search","arguments":{}}`))
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	if !strings.Contains(string(raw), `"isError":true`) {
		t.Fatalf("source isError should surface as a tool error: %s", raw)
	}
}

// Without a ToolSource, listChanged is not advertised (back-compat).
func TestNoToolSource_NoListChanged(t *testing.T) {
	es, _ := episodic.Open(":memory:")
	defer es.Close()
	ss, _ := semantic.Open(":memory:")
	defer ss.Close()
	srv, err := mcpserver.New(mcpserver.Config{AgentID: "main", Episodic: es, Semantic: ss})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := srv.Handler().HandleRequest(context.Background(), "initialize", nil)
	if strings.Contains(string(raw), `"listChanged":true`) {
		t.Fatalf("no ToolSource should not advertise listChanged: %s", raw)
	}
}
