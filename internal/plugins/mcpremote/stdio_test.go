package mcpremote

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"tenant/internal/model"
)

// pipeServer runs an in-process MCP server over a pair of pipes, standing in
// for a local server process: its stdout and stdin are the client's ends.
func pipeServer(t *testing.T, calls *atomic.Int32) func(context.Context) (io.ReadCloser, io.WriteCloser, error) {
	t.Helper()
	return func(ctx context.Context) (io.ReadCloser, io.WriteCloser, error) {
		srv := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"}, nil)
		type args struct {
			Text string `json:"text"`
		}
		mcp.AddTool(srv, &mcp.Tool{Name: "shout", Description: "upper-cases text"},
			func(ctx context.Context, req *mcp.CallToolRequest, a args) (*mcp.CallToolResult, any, error) {
				calls.Add(1)
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.ToUpper(a.Text)}}}, nil, nil
			})
		mcp.AddTool(srv, &mcp.Tool{Name: "peek", Description: "reads", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
			func(ctx context.Context, req *mcp.CallToolRequest, a args) (*mcp.CallToolResult, any, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "seen"}}}, nil, nil
			})
		clientR, serverW := io.Pipe()
		serverR, clientW := io.Pipe()
		go func() {
			_ = srv.Run(ctx, &mcp.IOTransport{Reader: serverR, Writer: serverW})
		}()
		return clientR, clientW, nil
	}
}

// A stdio server's tools come through gated like a remote connector's: every
// tool asks unless its annotations are trusted, and a denied call never
// reaches the server.
func TestOpenStdioGatesLikeARemoteConnector(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	asked := 0
	approve := false
	d, cleanup, err := OpenStdio(ctx, StdioConfig{Label: "mcp:fake", Connect: pipeServer(t, &calls)}, false, Policy{
		Confirm: func(context.Context, string, string) bool { asked++; return approve },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	specs := map[string]model.ToolSpec{}
	for _, s := range d.Tools() {
		specs[s.Name] = s
	}
	if len(specs) != 2 || !specs["shout"].Gated || !specs["peek"].Gated {
		t.Fatalf("specs = %+v; with untrusted annotations every tool is gated", specs)
	}
	call := func(name string) (string, bool) {
		args, _ := json.Marshal(map[string]string{"text": "hi"})
		out, isErr, err := d.Dispatch(ctx, model.ToolCall{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return out, isErr
	}
	if _, isErr := call("shout"); !isErr || calls.Load() != 0 || asked != 1 {
		t.Errorf("a denied call reached the server: calls=%d asked=%d", calls.Load(), asked)
	}
	approve = true
	if out, isErr := call("shout"); isErr || out != "HI" || calls.Load() != 1 {
		t.Errorf("approved call: %q isErr=%v calls=%d", out, isErr, calls.Load())
	}
	d.SetTrust(TrustDeny)
	if out, isErr := call("peek"); !isErr || !strings.Contains(out, "deny") {
		t.Errorf("deny trust: %q", out)
	}
}

func TestOpenStdioReportsAFailedStart(t *testing.T) {
	_, _, err := OpenStdio(context.Background(), StdioConfig{Label: "mcp:x", Connect: func(context.Context) (io.ReadCloser, io.WriteCloser, error) {
		return nil, nil, errors.New("no such program")
	}}, false, Policy{})
	if err == nil || !strings.Contains(err.Error(), "mcp:x") || !strings.Contains(err.Error(), "no such program") {
		t.Errorf("err = %v", err)
	}
	if _, _, err := OpenStdio(context.Background(), StdioConfig{Label: "mcp:x"}, false, Policy{}); err == nil {
		t.Error("a config without Connect opened")
	}
}
