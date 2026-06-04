package mcpserver_test

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"tenant/internal/mcp"
	"tenant/internal/mcp/transport"
	"tenant/internal/mcpserver"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/model/testllm"
)

// --- wire shapes (mirror the unexported server-side structs) ---

type toolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}
type toolsListResult struct {
	Tools []toolDef `json:"tools"`
}
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
type toolCallResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError"`
}
type resourceDef struct {
	URI  string `json:"uri"`
	Name string `json:"name"`
}
type resourcesListResult struct {
	Resources []resourceDef `json:"resources"`
}
type resourceContents struct {
	URI  string `json:"uri"`
	Text string `json:"text"`
}
type resourceReadResult struct {
	Contents []resourceContents `json:"contents"`
}
type toolCallArgs struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// --- scaffolding ---

func pipePair(t *testing.T) (clientT, serverT transport.Transport) {
	t.Helper()
	aR, bW := io.Pipe()
	bR, aW := io.Pipe()
	clientT = transport.NewStdioStreams(aR, aW)
	serverT = transport.NewStdioStreams(bR, bW)
	t.Cleanup(func() {
		_ = clientT.Close()
		_ = serverT.Close()
	})
	return
}

type fixture struct {
	t      *testing.T
	estore *episodic.Store
	sstore *semantic.Store
	client *mcp.Client
}

func newFixture(t *testing.T, allowWrites bool, withEmbedder bool) *fixture {
	t.Helper()
	estore, err := episodic.Open(":memory:")
	if err != nil {
		t.Fatalf("episodic.Open: %v", err)
	}
	t.Cleanup(func() { _ = estore.Close() })
	sstore, err := semantic.Open(":memory:")
	if err != nil {
		t.Fatalf("semantic.Open: %v", err)
	}
	t.Cleanup(func() { _ = sstore.Close() })

	cfg := mcpserver.Config{
		AgentID:     "main",
		Episodic:    estore,
		Semantic:    sstore,
		AllowWrites: allowWrites,
	}
	if withEmbedder {
		cfg.Embedder = testllm.New() // default Embed: 8-dim by text length
	}
	srv, err := mcpserver.New(cfg)
	if err != nil {
		t.Fatalf("mcpserver.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	clientT, serverT := pipePair(t)
	serverSess := mcp.NewSession(serverT, srv.Handler())
	serverSess.Start(ctx)
	t.Cleanup(func() { _ = serverSess.Close() })

	client := mcp.NewClient(clientT)
	client.Start(ctx)
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.Initialize(ctx); err != nil {
		t.Fatalf("client.Initialize: %v", err)
	}
	return &fixture{t: t, estore: estore, sstore: sstore, client: client}
}

func (f *fixture) call(method string, params any, out any) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := f.client.Session().Call(ctx, method, mustJSON(f.t, params))
	if err != nil {
		f.t.Fatalf("%s: %v", method, err)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			f.t.Fatalf("%s: unmarshal result: %v (raw=%s)", method, err, raw)
		}
	}
}

func (f *fixture) callExpectErr(method string, params any) error {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := f.client.Session().Call(ctx, method, mustJSON(f.t, params))
	return err
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	return b
}

func vec() []float32 { return []float32{1, 0, 0, 0} }

// --- tests ---

func TestNew_Validation(t *testing.T) {
	if _, err := mcpserver.New(mcpserver.Config{}); err == nil {
		t.Fatal("expected error: missing AgentID")
	}
	if _, err := mcpserver.New(mcpserver.Config{AgentID: "a"}); err == nil {
		t.Fatal("expected error: missing Episodic")
	}
	es, _ := episodic.Open(":memory:")
	defer es.Close()
	if _, err := mcpserver.New(mcpserver.Config{AgentID: "a", Episodic: es}); err == nil {
		t.Fatal("expected error: missing Semantic")
	}
}

func TestInitialize_ReportsCapabilities(t *testing.T) {
	f := newFixture(t, false, false)
	info := f.client.ServerInfo()
	if info.Name != "tenant-memory" {
		t.Errorf("ServerInfo.Name = %q, want tenant-memory", info.Name)
	}
	caps := f.client.ServerCapabilities()
	if caps.Tools == nil || caps.Resources == nil {
		t.Errorf("expected Tools+Resources capabilities, got %+v", caps)
	}
}

func TestPing(t *testing.T) {
	f := newFixture(t, false, false)
	if err := f.client.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestToolsList_ReadOnlyOmitsFactAdd(t *testing.T) {
	f := newFixture(t, false, false)
	var res toolsListResult
	f.call("tools/list", nil, &res)
	names := toolNames(res)
	if !names["memory_search"] {
		t.Errorf("memory_search missing: %v", names)
	}
	if names["memory_fact_add"] {
		t.Errorf("memory_fact_add should be absent in read-only mode: %v", names)
	}
}

func TestToolsList_WritableIncludesFactAdd(t *testing.T) {
	f := newFixture(t, true, true)
	var res toolsListResult
	f.call("tools/list", nil, &res)
	if !toolNames(res)["memory_fact_add"] {
		t.Error("memory_fact_add should be present when AllowWrites=true")
	}
}

func TestToolsCall_SearchReturnsFactsAndEpisodes(t *testing.T) {
	f := newFixture(t, false, true)
	ctx := context.Background()
	_, _ = f.sstore.Insert(ctx, &semantic.Fact{
		AgentID: "main", Fact: "User prefers Go", Confidence: 0.9,
		EmbedderID: "t", Embedding: vec(),
	})
	_, _ = f.estore.Insert(ctx, &episodic.Episode{
		AgentID: "main", Prompt: "how do I build", Response: "go build ./...",
		EmbedderID: "t", Embedding: vec(),
	})

	var res toolCallResult
	f.call("tools/call", toolCallArgs{
		Name:      "memory_search",
		Arguments: mustJSON(t, map[string]any{"query": "build go"}),
	}, &res)
	if res.IsError {
		t.Fatalf("search reported error: %+v", res)
	}
	text := res.Content[0].Text
	if !strings.Contains(text, "## Facts") || !strings.Contains(text, "User prefers Go") {
		t.Errorf("facts missing from search output:\n%s", text)
	}
	if !strings.Contains(text, "## Episodes") || !strings.Contains(text, "how do I build") {
		t.Errorf("episodes missing from search output:\n%s", text)
	}
}

func TestToolsCall_SearchKindFactsOnly(t *testing.T) {
	f := newFixture(t, false, true)
	ctx := context.Background()
	_, _ = f.sstore.Insert(ctx, &semantic.Fact{
		AgentID: "main", Fact: "fact one", Confidence: 0.8,
		EmbedderID: "t", Embedding: vec(),
	})
	_, _ = f.estore.Insert(ctx, &episodic.Episode{
		AgentID: "main", Prompt: "episode prompt", Response: "r",
		EmbedderID: "t", Embedding: vec(),
	})
	var res toolCallResult
	f.call("tools/call", toolCallArgs{
		Name:      "memory_search",
		Arguments: mustJSON(t, map[string]any{"query": "fact", "kind": "facts"}),
	}, &res)
	text := res.Content[0].Text
	if !strings.Contains(text, "## Facts") {
		t.Errorf("facts section missing: %s", text)
	}
	if strings.Contains(text, "## Episodes") {
		t.Errorf("episodes should be excluded for kind=facts: %s", text)
	}
}

func TestToolsCall_FactAddRejectedReadOnly(t *testing.T) {
	f := newFixture(t, false, true)
	var res toolCallResult
	f.call("tools/call", toolCallArgs{
		Name:      "memory_fact_add",
		Arguments: mustJSON(t, map[string]any{"fact": "x"}),
	}, &res)
	if !res.IsError {
		t.Fatal("fact_add should be an error in read-only mode")
	}
}

func TestToolsCall_FactAddSucceedsWritable(t *testing.T) {
	f := newFixture(t, true, true)
	var res toolCallResult
	f.call("tools/call", toolCallArgs{
		Name:      "memory_fact_add",
		Arguments: mustJSON(t, map[string]any{"fact": "User runs Tenant on Windows", "confidence": 0.95}),
	}, &res)
	if res.IsError {
		t.Fatalf("fact_add failed: %+v", res)
	}
	if !strings.Contains(res.Content[0].Text, "added fact #") {
		t.Errorf("unexpected fact_add result: %q", res.Content[0].Text)
	}
	// Verify it actually landed.
	n, _ := f.sstore.Count(context.Background(), false, false)
	if n != 1 {
		t.Errorf("semantic store has %d facts, want 1", n)
	}
}

func TestToolsCall_UnknownToolIsToolError(t *testing.T) {
	f := newFixture(t, false, false)
	var res toolCallResult
	// Must NOT be a protocol error — MCP convention: unknown tool is
	// isError content so the client can show it.
	f.call("tools/call", toolCallArgs{Name: "nope", Arguments: mustJSON(t, map[string]any{})}, &res)
	if !res.IsError {
		t.Fatal("unknown tool should be a tool-level error")
	}
}

func TestResourcesList(t *testing.T) {
	f := newFixture(t, false, false)
	var res resourcesListResult
	f.call("resources/list", nil, &res)
	uris := map[string]bool{}
	for _, r := range res.Resources {
		uris[r.URI] = true
	}
	if !uris["memory://soul/main"] || !uris["memory://facts/main"] {
		t.Errorf("expected soul+facts resources, got %+v", res.Resources)
	}
}

func TestResourcesRead_Soul(t *testing.T) {
	f := newFixture(t, false, false)
	var res resourceReadResult
	f.call("resources/read", map[string]any{"uri": "memory://soul/main"}, &res)
	if len(res.Contents) != 1 {
		t.Fatalf("contents len = %d, want 1", len(res.Contents))
	}
	// Default scaffold soul renders "About You" with the default name.
	if !strings.Contains(res.Contents[0].Text, "# About You") {
		t.Errorf("soul render missing expected header:\n%s", res.Contents[0].Text)
	}
}

func TestResourcesRead_Facts(t *testing.T) {
	f := newFixture(t, false, false)
	ctx := context.Background()
	_, _ = f.sstore.Insert(ctx, &semantic.Fact{
		AgentID: "main", Fact: "User is on macOS", Confidence: 0.9,
		EmbedderID: "t", Embedding: vec(),
	})
	var res resourceReadResult
	f.call("resources/read", map[string]any{"uri": "memory://facts/main"}, &res)
	text := res.Contents[0].Text
	if !strings.Contains(text, "User is on macOS") {
		t.Errorf("fact missing from facts resource:\n%s", text)
	}
}

func TestResourcesRead_UnknownURIIsProtocolError(t *testing.T) {
	f := newFixture(t, false, false)
	err := f.callExpectErr("resources/read", map[string]any{"uri": "memory://bogus/x"})
	if err == nil {
		t.Fatal("expected protocol error for unknown resource URI")
	}
	var rpcErr *mcp.Error
	if !asMCPError(err, &rpcErr) {
		t.Fatalf("expected *mcp.Error, got %T: %v", err, err)
	}
}

func TestSearch_ScopedByAgentID(t *testing.T) {
	f := newFixture(t, false, true)
	ctx := context.Background()
	// "main" is the server's agent. Insert a fact for a different agent.
	_, _ = f.sstore.Insert(ctx, &semantic.Fact{
		AgentID: "other", Fact: "other-agent secret", Confidence: 0.9,
		EmbedderID: "t", Embedding: vec(),
	})
	_, _ = f.sstore.Insert(ctx, &semantic.Fact{
		AgentID: "main", Fact: "main-agent fact", Confidence: 0.9,
		EmbedderID: "t", Embedding: vec(),
	})
	var res toolCallResult
	f.call("tools/call", toolCallArgs{
		Name:      "memory_search",
		Arguments: mustJSON(t, map[string]any{"query": "agent", "kind": "facts"}),
	}, &res)
	text := res.Content[0].Text
	if strings.Contains(text, "other-agent secret") {
		t.Errorf("cross-agent leak: %s", text)
	}
	if !strings.Contains(text, "main-agent fact") {
		t.Errorf("own agent fact missing: %s", text)
	}
}

// --- helpers ---

func toolNames(r toolsListResult) map[string]bool {
	m := map[string]bool{}
	for _, t := range r.Tools {
		m[t.Name] = true
	}
	return m
}

func asMCPError(err error, target **mcp.Error) bool {
	for err != nil {
		if e, ok := err.(*mcp.Error); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		if u, ok := err.(unwrapper); ok {
			err = u.Unwrap()
			continue
		}
		return false
	}
	return false
}
