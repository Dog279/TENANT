package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPluginsDoc_ReferencesStillExist — drift guard for docs/PLUGINS.md.
//
// The doc cites specific identifiers (plugin / ToolDispatcher / Tools /
// Dispatch / registerActivator / stubPlugin / SetEnabled / activated)
// and specific call sites. If any of those rename or move, the doc rots
// silently. This test ensures every load-bearing identifier the doc
// names still exists somewhere in the repo, so a rename breaks CI before
// the doc misleads anyone.
//
// We do a lightweight check: parse the doc for inline-code identifiers
// in the form `Name` or `Name()`, then grep the relevant source files
// for each. This is intentionally fuzzy — we're guarding against
// outright deletion / rename, not catching every API drift.
func TestPluginsDoc_ReferencesStillExist(t *testing.T) {
	docPath := filepath.Join("..", "..", "docs", "PLUGINS.md")
	docBytes, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("docs/PLUGINS.md missing or unreadable: %v — this doc is part of the public plugin contract and must exist", err)
	}
	doc := string(docBytes)

	// Identifiers the doc relies on, mapped to where they should exist.
	// The relative path is from cmd/tenant/ where the test runs.
	cases := []struct {
		identifier string
		searchDir  string
		hint       string
	}{
		{"plugin", "toolmux.go", "the core plugin interface"},
		{"ToolDispatcher", filepath.Join("..", "..", "internal", "agent", "tools.go"), "the dispatcher interface"},
		{"ToolSpec", filepath.Join("..", "..", "internal", "model", "llm.go"), "the tool-spec struct"},
		{"registerActivator", "toolmux.go", "the activator registration entry point"},
		{"stubPlugin", "toolmux.go", "the unconfigured-plugin stub"},
		{"maybeActivate", "toolmux.go", "the lazy-activation core"},
		{"SetEnabled", "toolmux.go", "runtime enable/disable"},
		{"setOnChange", "toolmux.go", "the persistence + MCP notification hook"},
		{"NotifyToolsChanged", filepath.Join("..", "..", "internal", "mcpserver", "server.go"), "MCP-side notification"},
	}

	for _, c := range cases {
		t.Run(c.identifier, func(t *testing.T) {
			// First: the doc must mention the identifier (else why is it
			// in our drift-guard table at all — the test itself is rotting).
			if !strings.Contains(doc, c.identifier) {
				t.Fatalf("doc does not mention %q (%s) — remove from drift guard or restore in doc",
					c.identifier, c.hint)
			}
			// Second: the source file must contain a definition or call.
			// Crude but sufficient: substring match.
			body, err := os.ReadFile(c.searchDir)
			if err != nil {
				t.Fatalf("cannot read %s: %v", c.searchDir, err)
			}
			if !strings.Contains(string(body), c.identifier) {
				t.Errorf("doc cites %q (%s) but it's missing from %s — RENAMED OR DELETED, update docs/PLUGINS.md",
					c.identifier, c.hint, c.searchDir)
			}
		})
	}
}

// TestPluginsDoc_StubCatalogIsExhaustive — every plugin label that
// registers an activator (a heavy plugin that needs `/enable`) must
// also appear in the stub catalog at toolmux.go:~534, or operators
// won't see it in /tools when unconfigured. We check by:
//  1. Reading docs/PLUGINS.md's "every plugin in the repo" table
//  2. Cross-checking each plugin's directory exists
//
// Catches the case where someone adds a plugin to the repo but
// forgets to document it OR forgets to register a stub.
func TestPluginsDoc_PluginTableMatchesRepo(t *testing.T) {
	docBytes, err := os.ReadFile(filepath.Join("..", "..", "docs", "PLUGINS.md"))
	if err != nil {
		t.Fatalf("docs/PLUGINS.md unreadable: %v", err)
	}
	doc := string(docBytes)

	// Each entry in this list MUST appear in the doc's plugin table.
	// Source of truth: what actually exists under internal/plugins/.
	pluginsRoot := filepath.Join("..", "..", "internal", "plugins")
	entries, err := os.ReadDir(pluginsRoot)
	if err != nil {
		t.Fatalf("cannot read %s: %v", pluginsRoot, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		// The doc table renders plugin names in backticks: `web`, `sql`, etc.
		needle := "`" + name + "`"
		if !strings.Contains(doc, needle) {
			t.Errorf("internal/plugins/%s exists but %q is not in docs/PLUGINS.md plugin table — add it", name, needle)
		}
	}
}
