// Package peering implements Tenant-to-Tenant federation identity and pairing
// (epic TEN-182, Phase 1). A peer Tenant is "just another remote MCP server":
// the serving side stands up a go-sdk streamable-HTTP listener (TEN-184), the
// dialing side reuses the hardened internal/plugins/mcpremote client spine
// (TEN-186). Pairing is mutual-consent via one-time invite codes (TEN-183);
// peers.json (0600) is the single authoritative per-peer store.
//
// This package currently holds the go-sdk server+auth+client loop spike that
// de-risks TEN-184 (see spike_test.go); the identity/pairing types land here
// in TEN-183.
package peering
