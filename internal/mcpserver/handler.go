package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"tenant/internal/mcp"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/ftsutil"
	"tenant/internal/memory/semantic"
	"tenant/internal/memory/soul"
	"tenant/internal/model"
)

// handler implements mcp.Handler for the MCP server protocol subset we
// support: initialize, ping, tools/list, tools/call, resources/list,
// resources/read.
type handler struct {
	cfg Config
	log *slog.Logger
}

// --- MCP wire shapes (response bodies the spec defines) ---

type toolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type toolsListResult struct {
	Tools []toolDef `json:"tools"`
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type toolCallResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

type resourceDef struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

type resourcesListResult struct {
	Resources []resourceDef `json:"resources"`
}

type resourceReadParams struct {
	URI string `json:"uri"`
}

type resourceContents struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text"`
}

type resourceReadResult struct {
	Contents []resourceContents `json:"contents"`
}

// --- dispatch ---

func (h *handler) HandleRequest(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	switch method {
	case mcp.MethodInitialize:
		return h.initialize()
	case mcp.MethodPing:
		return json.Marshal(struct{}{})
	case "tools/list":
		return h.toolsList()
	case "tools/call":
		return h.toolsCall(ctx, params)
	case "resources/list":
		return h.resourcesList()
	case "resources/read":
		return h.resourcesRead(ctx, params)
	default:
		return nil, mcp.NewError(mcp.ErrMethodNotFound, "mcpserver: unknown method: "+method)
	}
}

func (h *handler) HandleNotification(_ context.Context, method string, _ json.RawMessage) {
	// notifications/initialized is the only one we expect; it needs no
	// reply. Anything else is a peer concern we don't act on.
	if method != mcp.MethodInitialized {
		h.log.Debug("mcpserver: ignoring notification", "method", method)
	}
}

// --- initialize ---

func (h *handler) initialize() (json.RawMessage, error) {
	res := mcp.InitializeResult{
		ProtocolVersion: mcp.ProtocolVersion,
		Capabilities: mcp.ServerCapabilities{
			// listChanged advertises that the tool list can change at
			// runtime (the dynamic toolMux) — clients re-fetch on the
			// notifications/tools/list_changed we send.
			Tools:     &mcp.ToolsCapability{ListChanged: h.cfg.Tools != nil},
			Resources: &mcp.ResourcesCapability{},
		},
		ServerInfo: mcp.Implementation{
			Name:    h.cfg.ServerName,
			Version: h.cfg.ServerVersion,
		},
		Instructions: "Tenant memory server. Read memory://soul/" + h.cfg.AgentID +
			" for the agent's identity, memory://facts/" + h.cfg.AgentID +
			" for what it has learned, or call memory_search to query episodes+facts.",
	}
	return json.Marshal(res)
}

// --- tools ---

const searchSchema = `{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "natural-language query"},
    "k": {"type": "integer", "description": "max results per kind (default 8)"},
    "kind": {"type": "string", "enum": ["all","facts","episodes"], "description": "what to search (default all)"}
  },
  "required": ["query"]
}`

const factAddSchema = `{
  "type": "object",
  "properties": {
    "fact": {"type": "string", "description": "one-sentence atomic claim"},
    "confidence": {"type": "number", "description": "0..1 (default 0.7)"}
  },
  "required": ["fact"]
}`

func (h *handler) toolsList() (json.RawMessage, error) {
	tools := []toolDef{
		{
			Name:        "memory_search",
			Description: "Search the agent's episodic memory (past conversations) and semantic memory (distilled facts). Hybrid keyword + vector when an embedder is configured.",
			InputSchema: json.RawMessage(searchSchema),
		},
	}
	if h.cfg.AllowWrites {
		tools = append(tools, toolDef{
			Name:        "memory_fact_add",
			Description: "Add a durable fact to the agent's semantic memory. Requires an embedder.",
			InputSchema: json.RawMessage(factAddSchema),
		})
	}
	// Expose the live tool multiplexer (wiki/sql/web/os/…) so external MCP
	// clients see the full, dynamic toolset — not just the memory tools.
	if h.cfg.Tools != nil {
		seen := map[string]bool{}
		for _, t := range tools {
			seen[t.Name] = true
		}
		for _, s := range h.cfg.Tools.All() {
			if seen[s.Name] {
				continue // a memory tool already owns this name
			}
			schema := s.Parameters
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			tools = append(tools, toolDef{Name: s.Name, Description: s.Description, InputSchema: schema})
		}
	}
	return json.Marshal(toolsListResult{Tools: tools})
}

func (h *handler) toolsCall(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	var p toolCallParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, mcp.NewError(mcp.ErrInvalidParams, "mcpserver: bad tools/call params: "+err.Error())
	}
	switch p.Name {
	case "memory_search":
		return h.callSearch(ctx, p.Arguments)
	case "memory_fact_add":
		return h.callFactAdd(ctx, p.Arguments)
	default:
		// Route anything else to the live tool multiplexer, if exposed.
		if h.cfg.Tools != nil {
			res, isErr, err := h.cfg.Tools.Dispatch(ctx, model.ToolCall{Name: p.Name, Arguments: p.Arguments})
			if err != nil {
				return nil, mcp.NewError(mcp.ErrInternalError, "mcpserver: tool dispatch: "+err.Error())
			}
			if isErr {
				return toolErr(res)
			}
			return toolText(res)
		}
		// Per MCP convention, an unknown tool is a tool-level error
		// (isError content), not a protocol error — clients show it.
		return toolErr("unknown tool: " + p.Name)
	}
}

func (h *handler) callSearch(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	var a struct {
		Query string `json:"query"`
		K     int    `json:"k"`
		Kind  string `json:"kind"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return toolErr("bad arguments: " + err.Error())
	}
	if strings.TrimSpace(a.Query) == "" {
		return toolErr("query is required")
	}
	if a.K <= 0 {
		a.K = 8
	}
	if a.Kind == "" {
		a.Kind = "all"
	}

	// Embed the query if we have an embedder; always pass keywords too
	// (hybrid). Embedding failure degrades to keyword-only, not fatal.
	var embedding []float32
	if h.cfg.Embedder != nil {
		if vecs, err := h.cfg.Embedder.Embed(ctx, []string{a.Query}); err == nil && len(vecs) == 1 {
			embedding = vecs[0]
		} else if err != nil {
			h.log.Warn("mcpserver: embed query failed, keyword-only", "err", err)
		}
	}

	var b strings.Builder

	if a.Kind == "all" || a.Kind == "facts" {
		hits, err := h.cfg.Semantic.Search(ctx, semantic.Query{
			AgentIDs:  []string{h.cfg.AgentID},
			Embedding: embedding,
			Keywords:  ftsQuery(a.Query),
			K:         a.K,
		})
		if err != nil {
			return toolErr("fact search: " + err.Error())
		}
		fmt.Fprintf(&b, "## Facts (%d)\n", len(hits))
		for _, hit := range hits {
			fmt.Fprintf(&b, "- %s (confidence %.2f)\n", hit.Fact.Fact, hit.Fact.Confidence)
		}
		b.WriteString("\n")
	}

	if a.Kind == "all" || a.Kind == "episodes" {
		hits, err := h.cfg.Episodic.Search(ctx, episodic.Query{
			AgentIDs:  []string{h.cfg.AgentID},
			Embedding: embedding,
			Keywords:  ftsQuery(a.Query),
			K:         a.K,
		})
		if err != nil {
			return toolErr("episode search: " + err.Error())
		}
		fmt.Fprintf(&b, "## Episodes (%d)\n", len(hits))
		for _, hit := range hits {
			e := hit.Episode
			fmt.Fprintf(&b, "[%s] %s -> %s\n",
				e.Timestamp.Format("2006-01-02"),
				snippet(e.Prompt, 120), snippet(e.Response, 200))
		}
	}

	out := strings.TrimRight(b.String(), "\n")
	if out == "" {
		out = "(no results)"
	}
	return toolText(out)
}

func (h *handler) callFactAdd(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	if !h.cfg.AllowWrites {
		return toolErr("fact_add is disabled (server is read-only)")
	}
	if h.cfg.Embedder == nil {
		return toolErr("fact_add requires an embedder (none configured)")
	}
	var a struct {
		Fact       string  `json:"fact"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return toolErr("bad arguments: " + err.Error())
	}
	if strings.TrimSpace(a.Fact) == "" {
		return toolErr("fact is required")
	}
	if a.Confidence <= 0 {
		a.Confidence = 0.7
	}
	if a.Confidence > 1 {
		a.Confidence = 1
	}
	vecs, err := h.cfg.Embedder.Embed(ctx, []string{a.Fact})
	if err != nil || len(vecs) != 1 {
		return toolErr("embed fact failed")
	}
	id, err := h.cfg.Semantic.Insert(ctx, &semantic.Fact{
		AgentID:    h.cfg.AgentID,
		Visibility: semantic.VisibilityShared, // externally-contributed → shared
		Fact:       a.Fact,
		Confidence: a.Confidence,
		EmbedderID: "mcpserver",
		Embedding:  vecs[0],
	})
	if err != nil {
		return toolErr("insert fact: " + err.Error())
	}
	return toolText(fmt.Sprintf("added fact #%d", id))
}

// --- resources ---

func (h *handler) resourcesList() (json.RawMessage, error) {
	agent := h.cfg.AgentID
	return json.Marshal(resourcesListResult{
		Resources: []resourceDef{
			{
				URI:         "memory://soul/" + agent,
				Name:        "Agent soul (" + agent + ")",
				Description: "The agent's identity, values, user profile, and operating instructions.",
				MimeType:    "text/markdown",
			},
			{
				URI:         "memory://facts/" + agent,
				Name:        "Distilled facts (" + agent + ")",
				Description: "What the agent currently knows, newest-confirmed first.",
				MimeType:    "text/markdown",
			},
		},
	})
}

func (h *handler) resourcesRead(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	var p resourceReadParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, mcp.NewError(mcp.ErrInvalidParams, "mcpserver: bad resources/read params: "+err.Error())
	}
	switch p.URI {
	case "memory://soul/" + h.cfg.AgentID:
		return h.readSoul(p.URI)
	case "memory://facts/" + h.cfg.AgentID:
		return h.readFacts(ctx, p.URI)
	default:
		return nil, mcp.NewError(mcp.ErrInvalidParams, "mcpserver: unknown resource: "+p.URI)
	}
}

func (h *handler) readSoul(uri string) (json.RawMessage, error) {
	var s *soul.Soul
	if h.cfg.SoulDir != "" {
		loaded, err := soul.Load(h.cfg.SoulDir, h.cfg.AgentID)
		if err == nil {
			s = loaded
		}
	}
	if s == nil {
		s = soul.NewDefault(h.cfg.AgentID)
	}
	return json.Marshal(resourceReadResult{
		Contents: []resourceContents{{
			URI: uri, MimeType: "text/markdown", Text: s.Render(),
		}},
	})
}

func (h *handler) readFacts(ctx context.Context, uri string) (json.RawMessage, error) {
	facts, err := h.cfg.Semantic.List(ctx, semantic.ListFilter{
		AgentIDs: []string{h.cfg.AgentID},
		Limit:    500, // a resource read is a snapshot, not unbounded
	})
	if err != nil {
		return nil, mcp.NewError(mcp.ErrInternalError, "mcpserver: list facts: "+err.Error())
	}
	// Stable, readable: most-recently-confirmed first (List already
	// orders that way); tie-break by id for determinism in tests.
	sort.SliceStable(facts, func(i, j int) bool {
		if facts[i].LastConfirmed.Equal(facts[j].LastConfirmed) {
			return facts[i].ID < facts[j].ID
		}
		return facts[i].LastConfirmed.After(facts[j].LastConfirmed)
	})
	var b strings.Builder
	fmt.Fprintf(&b, "# Facts (%d)\n\n", len(facts))
	for _, f := range facts {
		fmt.Fprintf(&b, "- %s (confidence %.2f)\n", f.Fact, f.Confidence)
	}
	return json.Marshal(resourceReadResult{
		Contents: []resourceContents{{
			URI: uri, MimeType: "text/markdown", Text: strings.TrimRight(b.String(), "\n"),
		}},
	})
}

// --- helpers ---

func toolText(s string) (json.RawMessage, error) {
	return json.Marshal(toolCallResult{Content: []contentBlock{{Type: "text", Text: s}}})
}

func toolErr(s string) (json.RawMessage, error) {
	return json.Marshal(toolCallResult{
		Content: []contentBlock{{Type: "text", Text: s}},
		IsError: true,
	})
}

func snippet(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ftsQuery sanitizes a free-text query for SQLite FTS5 MATCH via the
// shared ftsutil (punctuation-safe + stop-word filtered so the keyword
// channel carries real signal, not corpus-wide noise).
func ftsQuery(q string) string { return ftsutil.Sanitize(q) }
