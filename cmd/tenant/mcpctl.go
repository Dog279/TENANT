package main

// mcpctl.go is `tenant mcp connect <url>` (TEN-162): the end-to-end remote-MCP
// client probe — connects to a remote MCP server with OAuth 2.1 + Dynamic Client
// Registration (no pre-created app), opens the browser to authorize, and lists
// the server's tools. This is the "Claude Code connector" flow, in Go.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"tenant/internal/plugins/mcpremote"
)

func cmdMCP(ctx context.Context, args []string) error {
	if len(args) < 2 || args[0] != "connect" {
		return fmt.Errorf("usage: tenant mcp connect <url> [--trust-annotations] [--callback host:port]\n  e.g. tenant mcp connect https://mcp.atlassian.com/v1/mcp")
	}
	url := args[1]
	fs := flag.NewFlagSet("mcp connect", flag.ContinueOnError)
	c := bindCommon(fs)
	trust := fs.Bool("trust-annotations", false, "trust the server's read-only annotations (relax the deny-by-default gate)")
	callback := fs.String("callback", "", "localhost callback host:port (default 127.0.0.1:8765)")
	noBrowser := fs.Bool("no-browser", false, "reconnect from the cached token only; fail (no browser) if none — tests the launch/restart silent-reconnect path")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if err := c.resolve(); err != nil {
		return err
	}

	if *noBrowser {
		fmt.Fprintf(os.Stderr, "Reconnecting to %s from the cached token (no browser)…\n", url)
	} else {
		fmt.Fprintf(os.Stderr, "Connecting to %s — a browser will open to authorize.\n(If the server shows two screens — approve, then pick a site — click through once each; don't re-click.)\n", url)
	}
	d, cleanup, err := mcpremote.Open(ctx, mcpremote.Config{
		ServerURL:    url,
		Label:        "mcp",
		CallbackAddr: *callback,
		OpenBrowser:  openBrowser,
		CacheDir:     filepath.Join(c.cfgDir, "mcp"),
		Interactive:  !*noBrowser,
	}, *trust, mcpremote.Policy{})
	if err != nil {
		return err
	}
	defer cleanup()

	tools := d.Tools()
	fmt.Fprintf(os.Stderr, "\n✓ connected — %d tool(s) available:\n", len(tools))
	for _, t := range tools {
		kind := "read"
		if t.Gated {
			kind = "WRITE (gated)"
		}
		fmt.Fprintf(os.Stderr, "  - %-44s [%s]\n", t.Name, kind)
	}
	fmt.Fprintf(os.Stderr, "\nTo use these in the agent: launch with --mcp-remote %s then `/enable %s`.\n", url, mcpLabel(url))
	return nil
}
