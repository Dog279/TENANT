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

// federatableTools maps a LOCAL search tool to the PEER tool it fans out to (ALL
// connected peers, results appended). Only these tools federate; everything else
// dispatches normally.
var federatableTools = map[string]string{
	"wiki_search":   "peer_wiki_search",
	"memory_search": "peer_memory_search", // Phase B (local memory_search host tool)
}

// peerReadByMiss maps a LOCAL "read one item" tool to the PEER tool it falls back
// to WHEN THE LOCAL LOOKUP MISSES. Unlike federatableTools (fan-out + append),
// this is a fallback that returns the FIRST connected peer that has the item — so
// the agent reads a note by the same filename whether it's local or on a peer,
// with no extra ceremony. (Closes the "found via search, can't read" gap.)
var peerReadByMiss = map[string]string{
	"wiki_read": "peer_wiki_read",
}

// namedDisp pairs a peer's local label with its dispatcher (for fan-out).
type namedDisp struct {
	name string
	disp plugin
}

// isFederatedCounterpart reports whether a PEER tool name is folded into a local
// tool (and so hidden from the agent — the local tool fans out / falls back to it).
func isFederatedCounterpart(name string) bool {
	for _, v := range federatableTools {
		if v == name {
			return true
		}
	}
	for _, v := range peerReadByMiss {
		if v == name {
			return true
		}
	}
	return false
}

// adoptPeer registers a connected peer's dispatcher for federated search. The
// peer's FEDERATED tools (peer_wiki_search / peer_memory_search) are hidden — the
// matching local tool (wiki_search / memory_search) fans out to them, and that
// local tool is brought live so its peer half is reachable. The peer's
// NON-federated tools stay directly callable, so adopting a peer never *removes*
// reachable capability. Idempotent: a duplicate adopt drops the new connection.
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
			// Folded into a local search tool — not exposed. Make sure that
			// local tool is live so its peer half is actually reachable
			// (memory_search ships DISABLED and comes live here). Peers always
			// advertise peer_memory_search — the share gate is call-time — so
			// ANY adopted peer enables memory_search; a peer not sharing memory
			// just yields call-time denials (skipped, counted in /peer stats).
			m.enableFederatedLocalLocked(spec.Name)
			continue
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

// enableFederatedLocalLocked enables the LOCAL tool that fans out to the given
// peer counterpart (e.g. peer_memory_search → memory_search), if that local tool
// is registered. No-op when it's already enabled (wiki_search) or absent (no
// local memory store). Caller holds m.mu. Invalidates the ranking cache so the
// newly-live tool is embedded for the next Search.
func (m *toolMux) enableFederatedLocalLocked(counterpart string) {
	for local, peerTool := range federatableTools {
		if peerTool != counterpart {
			continue
		}
		if e, ok := m.byName[local]; ok && !e.enabled {
			e.enabled = true
			m.toolEmbeddings = nil
		}
		return
	}
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
	// Read-fallback tools (wiki_read): tell the LLM a note that isn't local is
	// fetched from a connected peer automatically — so it reads a filename the
	// same way whether it's local or on a peer.
	for local := range peerReadByMiss {
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
		e.spec.Description = base + " If the note isn't local but a connected peer (" +
			strings.Join(names, ", ") + ") has it, it's fetched from there automatically and flagged EXTERNAL — VERIFY before relying."
	}
}

// federate appends each connected peer's results for the counterpart tool to the
// local result, flagged trust-but-verify. Denials / offline / empty peers are
// skipped silently so the agent only sees real cross-instance knowledge. Every
// fan-out outcome is recorded for drift tracking (see /peer stats).
func (m *toolMux) federate(ctx context.Context, localOut, counterpart string, args json.RawMessage, peers []namedDisp) string {
	var b strings.Builder
	b.WriteString(localOut)
	for _, p := range peers {
		res, isErr, err := p.disp.Dispatch(ctx, model.ToolCall{Name: counterpart, Arguments: args})
		switch {
		case err != nil:
			m.recordFed(p.name, fedError, 0) // peer offline / transport failure
		case isErr:
			m.recordFed(p.name, fedDenied, 0) // share gate said no (or unknown tool)
		case !peerResultHasContent(res):
			m.recordFed(p.name, fedEmpty, 0) // connected + allowed but nothing matched
		default:
			res = strings.TrimSpace(res)
			fmt.Fprintf(&b, "\n\n### From peer %q — trust but verify:\n%s", p.name, res)
			m.recordFed(p.name, fedHit, len(res))
		}
	}
	if m.mcpDeps.log != nil {
		m.mcpDeps.log.Debug("federation: fan-out complete", "tool", counterpart, "peers", len(peers))
	}
	return b.String()
}

// peerResultHasContent reports whether a peer's result carries real entries
// rather than just headers / a no-results marker. Format-agnostic (works for the
// single-count wiki format and the two-count memory format) so a peer that has
// e.g. 2 facts but 0 episodes is NOT mistaken for empty.
func peerResultHasContent(res string) bool {
	for _, ln := range strings.Split(res, "\n") {
		ln = strings.TrimSpace(ln)
		switch {
		case ln == "":
		case strings.HasPrefix(ln, "#"): // markdown headers incl. "### Facts (0)"
		case strings.HasPrefix(ln, "(") && strings.HasSuffix(ln, ")"):
			// Parenthetical diagnostic, not content: "(no results)",
			// "(this peer has no wiki configured)", etc.
		default:
			return true // a real content/entry line
		}
	}
	return false
}

// peerReadHasContent is the emptiness test for a peer READ result (a full note
// body), distinct from peerResultHasContent (which is for SEARCH listings). A
// note body is real content even if it's all markdown headings — only an empty
// string or a single opaque diagnostic line ("(note not available…)") counts as
// "the peer doesn't have it".
func peerReadHasContent(res string) bool {
	t := strings.TrimSpace(res)
	if t == "" || t == "(no results)" {
		return false
	}
	if !strings.Contains(t, "\n") && strings.HasPrefix(t, "(") && strings.HasSuffix(t, ")") {
		return false // a single wholly-parenthetical line is a diagnostic, not a note
	}
	return true
}

// --- drift tracking (TEN-243) ------------------------------------------------
//
// Federation is a live link to another machine: a peer can go offline, start
// denying a capability it used to share, or quietly return nothing. These
// per-peer counters make that behavioral drift visible (surfaced via `/peer
// stats`) without a heavyweight metrics stack — they accumulate over the
// session, which for a long-lived connector is the window that matters.

type fedOutcome int

const (
	fedHit    fedOutcome = iota // peer returned usable content
	fedDenied                   // peer's share gate refused (or unknown tool)
	fedError                    // peer offline / transport failure
	fedEmpty                    // peer connected + allowed but nothing matched
)

// peerFedStat is the running fan-out tally for one peer.
type peerFedStat struct {
	Queries int
	Hits    int
	Denied  int
	Errors  int
	Empty   int
	Bytes   int64
}

// PeerFedStat is a snapshot of one peer's federation counters (TUI/CLI view).
type PeerFedStat struct {
	Peer    string
	Queries int
	Hits    int
	Denied  int
	Errors  int
	Empty   int
	Bytes   int64
}

// recordFed tallies one fan-out outcome for a peer. Thread-safe (its own mutex,
// independent of m.mu so it never contends with dispatch/registration).
func (m *toolMux) recordFed(peer string, outcome fedOutcome, bytes int) {
	m.fedMu.Lock()
	defer m.fedMu.Unlock()
	if m.fedStats == nil {
		m.fedStats = map[string]*peerFedStat{}
	}
	s := m.fedStats[peer]
	if s == nil {
		s = &peerFedStat{}
		m.fedStats[peer] = s
	}
	s.Queries++
	switch outcome {
	case fedHit:
		s.Hits++
		s.Bytes += int64(bytes)
	case fedDenied:
		s.Denied++
	case fedError:
		s.Errors++
	case fedEmpty:
		s.Empty++
	}
}

// FederationStats returns a name-sorted snapshot of every peer's fan-out tally.
func (m *toolMux) FederationStats() []PeerFedStat {
	m.fedMu.Lock()
	defer m.fedMu.Unlock()
	out := make([]PeerFedStat, 0, len(m.fedStats))
	for name, s := range m.fedStats {
		out = append(out, PeerFedStat{
			Peer: name, Queries: s.Queries, Hits: s.Hits, Denied: s.Denied,
			Errors: s.Errors, Empty: s.Empty, Bytes: s.Bytes,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Peer < out[j].Peer })
	return out
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

// peersForReadFallback snapshots peers (name-sorted) for a read-on-miss tool.
// Returns nil for non-read-fallback tools or when no peers are connected.
func (m *toolMux) peersForReadFallback(toolName string) (counterpart string, peers []namedDisp) {
	counterpart = peerReadByMiss[toolName]
	if counterpart == "" || len(m.peerDisp) == 0 {
		return "", nil
	}
	for name, d := range m.peerDisp {
		peers = append(peers, namedDisp{name: name, disp: d})
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].name < peers[j].name })
	return counterpart, peers
}

// peerReadFallback asks each connected peer (in order) for the item the local
// lookup missed, returning the FIRST that has it — flagged trust-but-verify.
// Denials / offline / empty peers are skipped and recorded for drift tracking.
// Returns ok=false when no peer has it (caller keeps the local miss result).
func (m *toolMux) peerReadFallback(ctx context.Context, counterpart string, args json.RawMessage, peers []namedDisp) (string, bool) {
	for _, p := range peers {
		res, isErr, err := p.disp.Dispatch(ctx, model.ToolCall{Name: counterpart, Arguments: args})
		switch {
		case err != nil:
			m.recordFed(p.name, fedError, 0)
		case isErr:
			m.recordFed(p.name, fedDenied, 0) // share gate refused, or peer lacks the tool
		case !peerReadHasContent(res):
			m.recordFed(p.name, fedEmpty, 0) // peer doesn't have this note
		default:
			res = strings.TrimSpace(res)
			m.recordFed(p.name, fedHit, len(res))
			return fmt.Sprintf("📄 From peer %q — trust but verify:\n\n%s", p.name, res), true
		}
	}
	return "", false
}
