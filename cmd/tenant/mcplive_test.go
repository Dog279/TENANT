package main

// A real Streamable-HTTP MCP server (the go-sdk's, like one the agent would
// write), adopted into a live mux with no sign-in, no browser and no callback
// port: the end-to-end path an LLM-built tool takes into Tenant.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"tenant/internal/model"
)

type echoIn struct {
	Text string `json:"text"`
}

// startToolServer serves two tools, "<prefix>echo" (read-only annotated) and
// "<prefix>write_note". Tool names are GLOBAL in the mux (first registrant
// wins), so a second server in one test needs its own prefix — as two real
// servers would need distinct tool names to both be usable.
func startToolServer(t *testing.T, name, prefix string) *httptest.Server {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: name, Version: "0.1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: prefix + "echo", Description: "echo the text back", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(_ context.Context, _ *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "echo:" + in.Text}}}, nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: prefix + "write_note", Description: "append a note"},
		func(_ context.Context, _ *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "wrote:" + in.Text}}}, nil, nil
		})
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(ts.Close)
	return ts
}

func liveMux(t *testing.T) (*toolMux, *[]string) {
	t.Helper()
	asked := &[]string{}
	mux := newToolMux()
	mux.mcpDeps = mcpRuntimeDeps{
		ctx:      context.Background(),
		callback: "127.0.0.1:0", // never bound on this path — no server here answers 401
		cacheDir: t.TempDir(),
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		confirm: func(_ context.Context, action, detail string) bool {
			*asked = append(*asked, action)
			return true
		},
	}
	t.Cleanup(mux.Close)
	return mux, asked
}

func TestToolMux_AdoptsLocalServerLive(t *testing.T) {
	ts := startToolServer(t, "watcher", "")
	mux, asked := liveMux(t)
	ctx := context.Background()

	label, n, err := mux.AddRemoteMCP(ts.URL)
	if err != nil {
		t.Fatalf("AddRemoteMCP: %v", err)
	}
	if !strings.HasPrefix(label, "mcp:127.0.0.1:") || n != 2 {
		t.Fatalf("label=%q tools=%d", label, n)
	}
	enabled := map[string]bool{}
	for _, ti := range mux.ToolList() {
		if ti.Plugin == label {
			enabled[ti.Name] = ti.Enabled
		}
	}
	if !enabled["echo"] || !enabled["write_note"] {
		t.Fatalf("tools should be live and enabled: %v", enabled)
	}
	if list := mux.RemoteMCPList(); len(list) != 1 || list[0].Trust != "ask" || !list[0].Enabled {
		t.Fatalf("RemoteMCPList = %+v", list)
	}

	// ask (default): every tool is gated (annotations untrusted) → the operator is prompted.
	args, _ := json.Marshal(echoIn{Text: "hi"})
	out, isErr, _ := mux.Dispatch(ctx, model.ToolCall{Name: "echo", Arguments: args})
	if isErr || out != "echo:hi" || len(*asked) != 1 {
		t.Fatalf("ask: out=%q isErr=%v asked=%v", out, isErr, *asked)
	}

	// allow: no prompt at all.
	if err := mux.SetMCPTrust(label, "allow"); err != nil {
		t.Fatal(err)
	}
	out, isErr, _ = mux.Dispatch(ctx, model.ToolCall{Name: "write_note", Arguments: args})
	if isErr || out != "wrote:hi" || len(*asked) != 1 {
		t.Fatalf("allow: out=%q isErr=%v asked=%v", out, isErr, *asked)
	}

	// deny: tools hidden (disabled) and refused.
	if err := mux.SetMCPTrust(label, "deny"); err != nil {
		t.Fatal(err)
	}
	for _, ti := range mux.ToolList() {
		if ti.Plugin == label && ti.Enabled {
			t.Errorf("deny should hide %s", ti.Name)
		}
	}
	if list := mux.RemoteMCPList(); list[0].Trust != "deny" || list[0].Enabled {
		t.Errorf("deny state = %+v", list[0])
	}

	// back to ask: tools visible again, gate active again.
	if err := mux.SetMCPTrust(label, "ask"); err != nil {
		t.Fatal(err)
	}
	out, isErr, _ = mux.Dispatch(ctx, model.ToolCall{Name: "echo", Arguments: args})
	if isErr || out != "echo:hi" || len(*asked) != 2 {
		t.Fatalf("ask again: out=%q asked=%v", out, *asked)
	}
	if err := mux.SetMCPTrust("mcp:nope", "allow"); err == nil {
		t.Error("unknown label must error")
	}
	if err := mux.SetMCPTrust(label, "maybe"); err == nil {
		t.Error("bad mode must error")
	}

	// A SECOND local server adopts into the same hub without a restart and gets
	// its own label (the port is part of it).
	ts2 := startToolServer(t, "second", "s2_")
	label2, n2, err := mux.AddRemoteMCP(ts2.URL)
	if err != nil || n2 != 2 {
		t.Fatalf("second add: label=%q n=%d err=%v", label2, n2, err)
	}
	if label2 == label {
		t.Fatalf("two servers on 127.0.0.1 must not share a label: %q", label2)
	}
	if len(mux.RemoteMCPList()) != 2 {
		t.Errorf("both connectors should be listed: %+v", mux.RemoteMCPList())
	}
}

// Persisted trust is applied when a connector reconnects at launch.
func TestToolMux_TrustAppliedOnSilentReconnect(t *testing.T) {
	ts := startToolServer(t, "watcher", "")
	mux, asked := liveMux(t)
	mux.mcpDeps.trust = map[string]string{ts.URL: "allow"}
	label := mux.registerMCPRemote(ts.URL)
	mux.reconnectMCPSilently(ts.URL) // no cached token, no 401 → connects, adopts live
	args, _ := json.Marshal(echoIn{Text: "x"})
	out, isErr, _ := mux.Dispatch(context.Background(), model.ToolCall{Name: "write_note", Arguments: args})
	if isErr || out != "wrote:x" || len(*asked) != 0 {
		t.Fatalf("persisted allow should apply on reconnect: out=%q asked=%v", out, *asked)
	}
	if mux.MCPTrust(label) != "allow" {
		t.Errorf("MCPTrust = %q", mux.MCPTrust(label))
	}
}

func TestMCPLabel_KeepsPortAndDisambiguates(t *testing.T) {
	if got := mcpLabel("https://mcp.atlassian.com/v1/mcp"); got != "mcp:mcp.atlassian.com" {
		t.Errorf("hosted label changed: %q", got)
	}
	if got := mcpLabel("http://127.0.0.1:9000/mcp"); got != "mcp:127.0.0.1:9000" {
		t.Errorf("local label should keep the port: %q", got)
	}
	m := newToolMux()
	a := m.registerMCPRemote("http://127.0.0.1:9000/a")
	b := m.registerMCPRemote("http://127.0.0.1:9000/b")
	if a != "mcp:127.0.0.1:9000" || b != "mcp:127.0.0.1:9000/b" {
		t.Errorf("collision labels = %q, %q", a, b)
	}
	if again := m.labelFor("http://127.0.0.1:9000/b"); again != b {
		t.Errorf("a registered URL must keep its label: %q", again)
	}
}

func TestMCPControl_PersistTrust(t *testing.T) {
	lc := &launchConfig{}
	a := newMCPControl(newToolMux(), t.TempDir(), lc)
	a.persistTrust("http://x/mcp", "allow")
	if lc.MCPTrust["http://x/mcp"] != "allow" {
		t.Fatalf("persist allow: %v", lc.MCPTrust)
	}
	a.persistTrust("http://x/mcp", "ask") // ask is the default → not stored
	if _, ok := lc.MCPTrust["http://x/mcp"]; ok {
		t.Errorf("ask should clear the entry: %v", lc.MCPTrust)
	}
	a.persistTrust("http://x/mcp", "deny")
	a.persistTrust("http://x/mcp", "")
	if len(lc.MCPTrust) != 0 {
		t.Errorf("remove should clear: %v", lc.MCPTrust)
	}
}
