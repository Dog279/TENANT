package mcp

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"tenant/internal/mcp/transport"
)

// DefaultInitializeTimeout bounds the handshake when the caller's ctx
// carries no deadline. A peer that accepts the connection but never
// replies would otherwise hang Initialize (and its underlying
// request round-trip) forever, since the read pump never sees a frame
// to close the session. Tunable for tests; respected only when the
// incoming ctx has no deadline of its own.
var DefaultInitializeTimeout = 30 * time.Second

// ClientState tracks the lifecycle of a Client. Transitions:
//
//	created → initializing → ready → closed
//
// State is consulted by Initialize and Call to reject out-of-order
// requests. Stored as int32 for atomic access.
type ClientState int32

const (
	StateCreated ClientState = iota
	StateInitializing
	StateReady
	StateClosed
)

// ErrNotReady is returned by Call/CallTool/etc. before Initialize completes.
var ErrNotReady = errors.New("mcp: client not initialized")

// Client is the high-level MCP client. It wraps a Session, enforces
// lifecycle ordering, and exposes typed methods for each primitive
// (tools, resources, prompts) as they get added.
type Client struct {
	session *Session
	info    Implementation
	caps    ClientCapabilities

	state atomic.Int32 // ClientState

	// Populated after Initialize.
	serverInfo         Implementation
	serverCaps         ServerCapabilities
	serverInstructions string
}

// ClientOption configures a Client at construction.
type ClientOption func(*Client)

// WithClientInfo overrides the default implementation identity.
func WithClientInfo(info Implementation) ClientOption {
	return func(c *Client) { c.info = info }
}

// WithCapabilities overrides the default advertised capabilities.
func WithCapabilities(caps ClientCapabilities) ClientOption {
	return func(c *Client) { c.caps = caps }
}

// NewClient constructs a Client over t. The client does NOT start
// the underlying session's read pump — call Start.
func NewClient(t transport.Transport, opts ...ClientOption) *Client {
	c := &Client{
		info: Implementation{Name: "tenant", Version: LibraryVersion},
		caps: ClientCapabilities{}, // empty until we wire roots/sampling
	}
	for _, opt := range opts {
		opt(c)
	}
	// The client never receives requests in v1 — pure outbound caller.
	// When we wire sampling, replace nopHandler with a router.
	c.session = NewSession(t, nil)
	c.state.Store(int32(StateCreated))
	return c
}

// Start launches the session read pump. Must be called before
// Initialize. ctx cancellation triggers a clean session shutdown.
func (c *Client) Start(ctx context.Context) {
	c.session.Start(ctx)
}

// Initialize performs the MCP handshake. Idempotent: subsequent
// calls return the cached result.
func (c *Client) Initialize(ctx context.Context) (*InitializeResult, error) {
	if !c.state.CompareAndSwap(int32(StateCreated), int32(StateInitializing)) {
		// Already initializing or done — re-read state.
		switch ClientState(c.state.Load()) {
		case StateReady:
			return &InitializeResult{
				ProtocolVersion: ProtocolVersion,
				Capabilities:    c.serverCaps,
				ServerInfo:      c.serverInfo,
				Instructions:    c.serverInstructions,
			}, nil
		case StateClosed:
			return nil, ErrSessionClosed
		default:
			return nil, fmt.Errorf("mcp: initialize already in progress")
		}
	}

	// Guard against a peer that accepts the connection but never
	// responds: if the caller didn't bound the handshake with a
	// deadline, impose a default one so Initialize fails fast instead
	// of hanging. An existing deadline is respected — never shortened.
	timedOut := false
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultInitializeTimeout)
		defer cancel()
		timedOut = true
	}

	params := InitializeParams{
		ProtocolVersion: ProtocolVersion,
		Capabilities:    c.caps,
		ClientInfo:      c.info,
	}
	result, err := CallTyped[InitializeResult](ctx, c.session, MethodInitialize, params)
	if err != nil {
		c.state.Store(int32(StateCreated)) // allow retry
		// Surface our self-imposed timeout with a clear, actionable
		// message rather than a bare context.DeadlineExceeded.
		if timedOut && errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("mcp: initialize timed out after %s (peer unresponsive)", DefaultInitializeTimeout)
		}
		return nil, err
	}

	// Spec § Version Negotiation: if server speaks a different version,
	// we either accept it or hang up. For now, require exact match.
	if result.ProtocolVersion != ProtocolVersion {
		c.state.Store(int32(StateClosed))
		_ = c.session.Close()
		return nil, fmt.Errorf("mcp: protocol mismatch: client=%s server=%s",
			ProtocolVersion, result.ProtocolVersion)
	}

	c.serverInfo = result.ServerInfo
	c.serverCaps = result.Capabilities
	c.serverInstructions = result.Instructions

	// Per spec, client follows with the initialized notification before
	// any other request.
	if err := c.session.Notify(ctx, MethodInitialized, nil); err != nil {
		c.state.Store(int32(StateClosed))
		_ = c.session.Close()
		return nil, fmt.Errorf("mcp: send initialized: %w", err)
	}

	c.state.Store(int32(StateReady))
	return &result, nil
}

// Ping sends an MCP ping. Useful as a liveness check or to keep
// long-idle connections warm.
func (c *Client) Ping(ctx context.Context) error {
	if ClientState(c.state.Load()) != StateReady {
		return ErrNotReady
	}
	_, err := c.session.Call(ctx, MethodPing, nil)
	return err
}

// ServerInfo returns the server identity reported during initialize.
// Panics if called before Initialize completes successfully.
func (c *Client) ServerInfo() Implementation { return c.serverInfo }

// ServerCapabilities returns the server's advertised capabilities.
func (c *Client) ServerCapabilities() ServerCapabilities { return c.serverCaps }

// Close shuts down the session. Idempotent.
func (c *Client) Close() error {
	c.state.Store(int32(StateClosed))
	return c.session.Close()
}

// Session exposes the underlying session for primitive packages
// (tools, resources, prompts) that need to issue raw calls. Keeps
// the Client surface small while permitting extension.
func (c *Client) Session() *Session { return c.session }
