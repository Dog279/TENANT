// A complete MCP tool server the agent (or you) can copy, edit and run — then
// adopt into Tenant with one tool call, no rebuild, no restart:
//
//	go run ./reference/mcp/toolserver -addr 127.0.0.1:9000
//	# in a Tenant turn:  mcp_add url=http://127.0.0.1:9000/mcp trust=allow reason="disk watcher"
//	# or as the operator: /mcp add http://127.0.0.1:9000/mcp
//
// It speaks Streamable HTTP with NO sign-in, so Tenant connects instantly (no
// browser, no callback port), and it logs into Tenant's log book through the
// copy-paste client in reference/logging — its own folder and dashboard tab.
//
// To copy it out of the repo: take this file plus reference/logging/tenantlog.go,
// `go mod init yourtool && go get github.com/modelcontextprotocol/go-sdk`, and
// change the tenantlog import to your module path.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	tenantlog "tenant/reference/logging"
)

// --- your tools ------------------------------------------------------------

type echoIn struct {
	Text string `json:"text" jsonschema:"the text to echo back"`
}

type noteIn struct {
	Note string `json:"note" jsonschema:"one line to append to the notes file"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:9000", "listen address (loopback: only this box's Tenant reaches it)")
	name := flag.String("name", "example-tools", "log source (folder + dashboard tab) and server name")
	notes := flag.String("notes", "notes.txt", "file write_note appends to")
	flag.Parse()

	logbook := tenantlog.New(*name)
	logbook.Info("tool server starting", "addr", *addr, "pid", os.Getpid())

	srv := mcp.NewServer(&mcp.Implementation{Name: *name, Version: "0.1"}, nil)

	// A read-only tool: annotate it so an operator who trusts annotations
	// (--mcp-trust-annotations) gets it ungated even in trust "ask".
	mcp.AddTool(srv, &mcp.Tool{
		Name: "echo", Description: "Echo the text back — a connectivity check.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(_ context.Context, _ *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, any, error) {
		logbook.Info("echo", "text", in.Text)
		return text("echo: " + in.Text), nil, nil
	})

	// A write tool: unannotated ⇒ Tenant gates it under trust "ask" (the
	// operator approves each call) and runs it freely under trust "allow".
	mcp.AddTool(srv, &mcp.Tool{
		Name: "write_note", Description: "Append one line to the notes file.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in noteIn) (*mcp.CallToolResult, any, error) {
		line := strings.TrimSpace(in.Note)
		if line == "" {
			return text("note is empty"), nil, nil
		}
		f, err := os.OpenFile(*notes, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			logbook.Error("write_note failed", "err", err)
			return text("could not open notes file: " + err.Error()), nil, nil
		}
		defer f.Close()
		fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), line)
		logbook.Info("write_note", "chars", len(line), "file", *notes)
		return text("appended to " + *notes), nil, nil
	})

	// --- serve -----------------------------------------------------------------
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	log.Printf("%s: serving MCP at http://%s/mcp  (adopt with: mcp_add url=http://%s/mcp)", *name, *addr, *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		logbook.Error("tool server stopped", "err", err)
		log.Fatal(err)
	}
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}
