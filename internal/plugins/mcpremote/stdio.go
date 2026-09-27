package mcpremote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"tenant/internal/logs/event"
)

// StdioConfig is a local MCP server Tenant talks to over its stdin and stdout
// (TEN-318). Connect starts it: the caller decides where it runs and under
// what boundary (the execution sandbox), and returns its stdout and its stdin.
// Closing the stdin ends the server; the caller's Close for it should wait for
// the exit and kill a server that does not.
type StdioConfig struct {
	Label   string
	Connect func(ctx context.Context) (stdout io.ReadCloser, stdin io.WriteCloser, err error)
}

// StdioHandshake bounds a local server's initialize handshake and tool
// listing.
var StdioHandshake = 60 * time.Second

// OpenStdio starts the server and returns a Dispatcher over its tools, gated
// exactly like a remote connector's: deny-by-default unless the operator
// trusts the server's read-only annotations, then its trust mode (ask, allow,
// deny), tool rules and the approval gate. The cleanup ends the session, and
// with it the server. Connect runs under ctx as given, so the process belongs
// to the caller's activity (its Server); the session is a Connection activity
// under it.
func OpenStdio(ctx context.Context, cfg StdioConfig, trustAnnotations bool, policy Policy) (*Dispatcher, func(), error) {
	if cfg.Connect == nil {
		return nil, nil, errors.New("mcpremote: OpenStdio needs Connect")
	}
	// ctx is the server's lifetime; only the handshake and the tool listing
	// are bounded, so a server that never answers cannot park its caller.
	stdout, stdin, err := cfg.Connect(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("mcpremote: start %s: %w", cfg.Label, err)
	}
	ctx, act := event.Start(WithServerLog(ctx, cfg.Label), Connection, "server", cfg.Label)
	hctx, cancel := context.WithTimeout(ctx, StdioHandshake)
	defer cancel()
	session, err := newClient(ctx, cfg.Label).Connect(hctx, &mcp.IOTransport{Reader: stdout, Writer: stdin}, nil)
	if err != nil {
		_ = stdin.Close()
		err = fmt.Errorf("mcpremote: connect %s: %w", cfg.Label, err)
		act.End(&err)
		return nil, nil, err
	}
	cleanup := func() { _ = session.Close() }
	d, err := newDispatcher(hctx, cfg.Label, session, trustAnnotations, nil, policy)
	if err != nil {
		cleanup()
		err = fmt.Errorf("mcpremote: list tools of %s: %w", cfg.Label, err)
		act.End(&err)
		return nil, nil, err
	}
	return d, d.live(ctx, act, cleanup), nil
}
