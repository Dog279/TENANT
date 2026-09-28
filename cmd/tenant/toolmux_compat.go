package main

import "tenant/internal/toolmux"

// The tool mux lives in internal/toolmux (TEN-314). These names keep package
// main's call sites as they were; new code may use toolmux directly.
type (
	toolMux         = toolmux.Mux
	plugin          = toolmux.Plugin
	stubPlugin      = toolmux.Stub
	mcpRuntimeDeps  = toolmux.MCPDeps
	mcpServerStatus = toolmux.ServerStatus
	toolStats       = toolmux.Stats
	PeerFedStat     = toolmux.PeerFedStat
)

const rankActivateThreshold = toolmux.RankActivateThreshold

var (
	errNotConfigured    = toolmux.ErrNotConfigured
	peerFederationTools = toolmux.PeerFederationTools
)

// newToolMux builds a mux that logs the operator's plugin toggles.
func newToolMux() *toolMux {
	m := toolmux.New()
	m.SetOnToggle(logPluginToggle)
	return m
}

func newToolStats(dataDir, role string) *toolStats { return toolmux.NewStats(dataDir, role) }

func mcpLabel(rawURL string) string { return toolmux.MCPLabel(rawURL) }
