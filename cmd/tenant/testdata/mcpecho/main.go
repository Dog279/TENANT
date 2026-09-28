// Command mcpecho is a minimal third-party MCP server for the local-server
// tests (TEN-318). It speaks MCP over stdio and offers three tools: echo
// (returns its text), readfile (returns a file's first bytes, or the error)
// and dial (opens a TCP connection, or returns the error), so a test can see
// what the sandbox around it allows. Three more show what its parent files
// of it (TEN-364): stderr (writes its text to stderr), getenv (returns an
// environment variable) and stubborn (from then on it ignores its stdin
// closing, so it has to be killed).
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type textArgs struct {
	Text string `json:"text"`
}

type pathArgs struct {
	Path string `json:"path"`
}

type addrArgs struct {
	Addr string `json:"addr"`
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func main() {
	srv := mcp.NewServer(&mcp.Implementation{Name: "mcpecho", Version: "1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "echo", Description: "returns its text"},
		func(_ context.Context, _ *mcp.CallToolRequest, a textArgs) (*mcp.CallToolResult, any, error) {
			return text("echo:" + a.Text), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "readfile", Description: "reads a file"},
		func(_ context.Context, _ *mcp.CallToolRequest, a pathArgs) (*mcp.CallToolResult, any, error) {
			b, err := os.ReadFile(a.Path)
			if err != nil {
				return text("error: " + err.Error()), nil, nil
			}
			if len(b) > 200 {
				b = b[:200]
			}
			return text("read:" + string(b)), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "dial", Description: "opens a TCP connection"},
		func(_ context.Context, _ *mcp.CallToolRequest, a addrArgs) (*mcp.CallToolResult, any, error) {
			c, err := net.DialTimeout("tcp", a.Addr, 3*time.Second)
			if err != nil {
				return text("error: " + err.Error()), nil, nil
			}
			c.Close()
			return text("connected"), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "stderr", Description: "writes its text to stderr"},
		func(_ context.Context, _ *mcp.CallToolRequest, a textArgs) (*mcp.CallToolResult, any, error) {
			fmt.Fprintln(os.Stderr, a.Text)
			return text("written"), nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "getenv", Description: "returns an environment variable"},
		func(_ context.Context, _ *mcp.CallToolRequest, a textArgs) (*mcp.CallToolResult, any, error) {
			return text(os.Getenv(a.Text)), nil, nil
		})
	var stubborn atomic.Bool
	mcp.AddTool(srv, &mcp.Tool{Name: "stubborn", Description: "ignores its stdin closing from now on"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ textArgs) (*mcp.CallToolResult, any, error) {
			stubborn.Store(true)
			return text("stubborn"), nil, nil
		})
	err := srv.Run(context.Background(), &mcp.StdioTransport{})
	if stubborn.Load() {
		time.Sleep(time.Hour)
	}
	if err != nil {
		os.Exit(1)
	}
}
