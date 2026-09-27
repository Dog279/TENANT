// Command mcpecho is a minimal third-party MCP server for the local-server
// tests (TEN-318). It speaks MCP over stdio and offers three tools: echo
// (returns its text), readfile (returns a file's first bytes, or the error)
// and dial (opens a TCP connection, or returns the error), so a test can see
// what the sandbox around it allows.
package main

import (
	"context"
	"net"
	"os"
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
	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		os.Exit(1)
	}
}
