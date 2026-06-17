package main

// federation.go (TEN-243): federated knowledge search. When paired peers are
// connected, the agent's OWN search tools (wiki_search; memory_search later)
// transparently extend to those peers — local results first, peer results
// appended and flagged "trust but verify". The per-peer peer_*_search tools are
// NOT exposed to the agent (folded in): the toolMux holds the peer dispatchers
// and fans out on a local search call. No peers ⇒ plain local search.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"tenant/internal/model"
)

// federatableTools maps a LOCAL search tool to the PEER tool it fans out to.
// Only these tools federate; everything else dispatches normally.
var federatableTools = map[string]string{
	"wiki_search": "peer_wiki_search",
	// "memory_search": "peer_memory_search",  // Phase B (needs a local memory_search host tool)
}

// namedDisp pairs a peer's local label with its dispatcher (for fan-out).
type namedDisp struct {
	name string
	disp plugin
}

// isFederatedCounterpart reports whether a PEER tool name is folded into a local
// tool (and so hidden from the agent — the local tool fans out to it).
func isFederatedCounterpart(name string) bool {
	for _, v := range federatableTools {
		if v == name {
			return true
		}
	}
	return false
}

// adoptPeer registers a connected peer's dispatcher for federated search. The
// peer's FEDERATED tools (peer_wiki_search) are hidden — the matching local tool
// (wiki_search) fans out to them. The peer's NON-federated tools (e.g.
// peer_memory_search, until Phase B folds memory in) stay directly callable, so
// adopting a peer never *removes* reachable capability. Idempotent: a duplicate
// adopt drops the new connection.
func (m *toolMux) adoptPeer(name string, disp plugin, cleanup func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.peerDisp == nil {
		m.peerDisp = map[string]plugin{}
	}
	if _, exists := m.peerDisp[name]; exists {
		if cleanup != nil {
			go cleanup() // already adopted; drop this connection
		}
		return
	}
	m.peerDisp[name] = disp
	label := "peer:" + name
	for _, spec := range disp.Tools() {
		if isFederatedCounterpart(spec.Name) {
			continue // folded into a local search tool — not exposed
		}
		if _, exists := m.byName[spec.Name]; exists {
			continue // first-registrant-wins (can't shadow a local tool)
		}
		m.order = append(m.order, spec.Name)
		m.byName[spec.Name] = &toolEntry{spec: spec, owner: disp, plugin: label, enabled: true}
	}
	if cleanup != nil {
		m.cleanups = append(m.cleanups, cleanup)
	}
	m.refreshFederationDescLocked()
}

// hasPeer reports whether a peer is already adopted (so reconnect skips it).
func (m *toolMux) hasPeer(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.peerDisp[name]
	return ok
}

// refreshFederationDescLocked rewrites each federated tool's description to name
// the connected peers (the LLM-facing awareness signal). Caller holds m.mu.
func (m *toolMux) refreshFederationDescLocked() {
	names := make([]string, 0, len(m.peerDisp))
	for n := range m.peerDisp {
		names = append(names, n)
	}
	sort.Strings(names)
	for local := range federatableTools {
		e, ok := m.byName[local]
		if !ok {
			continue
		}
		if m.baseDesc[local] == "" {
			m.baseDesc[local] = e.spec.Description
		}
		base := m.baseDesc[local]
		if len(names) == 0 {
			e.spec.Description = base
			continue
		}
		e.spec.Description = base + " This ALSO searches connected peers (" +
			strings.Join(names, ", ") + "); peer results are EXTERNAL — weigh them but VERIFY before relying."
	}
}

// federate appends each connected peer's results for the counterpart tool to the
// local result, flagged trust-but-verify. Denials / offline / empty peers are
// skipped silently so the agent only sees real cross-instance knowledge.
func (m *toolMux) federate(ctx context.Context, localOut, counterpart string, args json.RawMessage, peers []namedDisp) string {
	var b strings.Builder
	b.WriteString(localOut)
	for _, p := range peers {
		res, isErr, err := p.disp.Dispatch(ctx, model.ToolCall{Name: counterpart, Arguments: args})
		if err != nil || isErr {
			continue // peer offline, tool missing, or sharing denied → skip
		}
		res = strings.TrimSpace(res)
		if res == "" || strings.Contains(res, "(0)") || strings.Contains(res, "(no results)") {
			continue // peer has nothing for this query
		}
		fmt.Fprintf(&b, "\n\n### From peer %q — trust but verify:\n%s", p.name, res)
	}
	return b.String()
}

// peersForFederation snapshots the peer dispatchers (name-sorted) for a federated
// tool call. Returns nil for non-federatable tools or when no peers are connected.
func (m *toolMux) peersForFederation(toolName string) (counterpart string, peers []namedDisp) {
	counterpart = federatableTools[toolName]
	if counterpart == "" || len(m.peerDisp) == 0 {
		return "", nil
	}
	for name, d := range m.peerDisp {
		peers = append(peers, namedDisp{name: name, disp: d})
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].name < peers[j].name })
	return counterpart, peers
}
