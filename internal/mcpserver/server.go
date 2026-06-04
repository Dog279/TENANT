// Package mcpserver exposes Tenant's memory layer as an MCP server.
// Any MCP client (Claude Desktop, Cursor, Zed, ...) can connect over
// stdio and:
//
//   - read  memory://soul/{agent}   (the agent's rendered identity)
//   - read  memory://facts/{agent}  (current distilled facts)
//   - call  memory_search           (hybrid search over episodes+facts)
//   - call  memory_fact_add         (contribute a fact — gated by AllowWrites)
//
// This is the network-effect move from MEMORY-DESIGN.md: one memory
// layer for the user's whole agent ecosystem, and the cleanest way to
// inspect what the agent has learned from outside Tenant itself.
//
// The server is a mcp.Handler over a stdio transport — exactly how MCP
// clients spawn server processes (`tenant mcp-memory`, stdin/stdout
// piped). Handler() is also exposed for in-process embedding + tests.
package mcpserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"

	"tenant/internal/mcp"
	"tenant/internal/mcp/transport"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/model"
)

// Config wires the memory server. Episodic + Semantic are required;
// Embedder is optional (nil → keyword-only search, no fact_add).
type Config struct {
	// AgentID scopes every read/search/write to one agent's memory.
	// External clients see exactly this agent's private+shared records.
	AgentID string

	// SoulDir is the config dir containing soul/{agent}.toml
	// (typically soul.DefaultDir()). May be empty → soul resource
	// returns the default scaffold instead of a saved soul.
	SoulDir string

	Episodic *episodic.Store
	Semantic *semantic.Store

	// Embedder enables semantic (vector) search and fact_add. When
	// nil, search degrades to FTS5 keyword-only and fact_add is
	// rejected (it needs an embedding to store).
	Embedder model.Embedder

	// AllowWrites gates memory_fact_add. Default false: a read-only
	// window into the agent's memory. Set true to let external
	// clients contribute facts.
	AllowWrites bool

	// Tools, if set, exposes Tenant's full runtime tool set over MCP — so
	// external clients see and can call the live, dynamic toolset (the tool
	// multiplexer), not just the memory tools. Optional. When set, the
	// server advertises tools/list_changed; call Server.NotifyToolsChanged
	// when the set changes.
	Tools ToolSource

	// ServerName/Version appear in the MCP initialize handshake.
	ServerName    string // default "tenant-memory"
	ServerVersion string // default mcp.LibraryVersion

	Logger *slog.Logger
}

// ToolSource exposes a dynamic tool set over MCP. Implemented by the tool
// multiplexer (cmd/tenant). Kept as an interface so mcpserver doesn't
// depend on the multiplexer.
type ToolSource interface {
	All() []model.ToolSpec
	Dispatch(ctx context.Context, call model.ToolCall) (result string, isError bool, err error)
}

// Server is the MCP memory server.
type Server struct {
	h    *handler
	sess atomic.Pointer[mcp.Session] // active session (set during Serve)
}

// New validates config and constructs the server.
func New(cfg Config) (*Server, error) {
	if cfg.AgentID == "" {
		return nil, errors.New("mcpserver: AgentID required")
	}
	if cfg.Episodic == nil {
		return nil, errors.New("mcpserver: Episodic store required")
	}
	if cfg.Semantic == nil {
		return nil, errors.New("mcpserver: Semantic store required")
	}
	if cfg.ServerName == "" {
		cfg.ServerName = "tenant-memory"
	}
	if cfg.ServerVersion == "" {
		cfg.ServerVersion = mcp.LibraryVersion
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Server{h: &handler{cfg: cfg, log: cfg.Logger}}, nil
}

// Handler returns the mcp.Handler. Use this to embed the memory server
// in a custom transport, or in tests via an in-memory pipe.
func (s *Server) Handler() mcp.Handler { return s.h }

// NotifyToolsChanged sends notifications/tools/list_changed to the
// connected client so it re-fetches tools/list. No-op if no session is
// active (e.g. before Serve, or in-process embedding). Wire this to the
// tool multiplexer's change hook so runtime /enable·/disable propagates.
func (s *Server) NotifyToolsChanged(ctx context.Context) {
	if sess := s.sess.Load(); sess != nil {
		_ = sess.Notify(ctx, "notifications/tools/list_changed", nil)
	}
}

// Serve runs the memory server over the process's stdin/stdout — the
// way an MCP client launches a server subprocess. Blocks until the
// transport closes (client disconnect / stdin EOF) or ctx is done.
func (s *Server) Serve(ctx context.Context) error {
	return s.ServeTransport(ctx, transport.NewStdioSelf())
}

// ServeTransport runs the server over an arbitrary transport (stdio, SSE,
// …). Blocks until the transport closes or ctx is done.
func (s *Server) ServeTransport(ctx context.Context, t transport.Transport) error {
	sess := mcp.NewSession(t, s.h)
	s.sess.Store(sess)
	defer s.sess.Store(nil)
	sess.Start(ctx)
	select {
	case <-ctx.Done():
		// Operator stop (SIGINT) or parent cancel. A server told to
		// stop did its job — that is not an error.
		_ = sess.Close()
		return nil
	case <-sess.Done():
		// An MCP client disconnecting (stdin EOF / session closed) is
		// the NORMAL end of a stdio MCP server's life, not a failure.
		// Only surface genuine transport/protocol faults.
		err := sess.Err()
		if err == nil ||
			errors.Is(err, io.EOF) ||
			errors.Is(err, mcp.ErrSessionClosed) ||
			errors.Is(err, transport.ErrClosed) {
			return nil
		}
		return err
	}
}
