package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"tenant/internal/agent"
	"tenant/internal/model"
	"tenant/internal/peering"
	"tenant/internal/plugins/atlassian"
	"tenant/internal/plugins/discord"
	"tenant/internal/plugins/gsuite"
	"tenant/internal/plugins/imessage"
	"tenant/internal/plugins/mcpremote"
	"tenant/internal/plugins/osys"
	sqlp "tenant/internal/plugins/sql"
	"tenant/internal/plugins/web"
	"tenant/internal/plugins/wiki"
	xp "tenant/internal/plugins/x"
	"tenant/internal/tui"
)

// plugin is a dispatcher that also advertises its tool specs — every
// Tenant plugin satisfies this.
type plugin interface {
	agent.ToolDispatcher
	Tools() []model.ToolSpec
}

// toolEntry is one tool in the mux: its spec, the plugin that owns it,
// a plugin label for group toggling, and whether it's currently active.
type toolEntry struct {
	spec    model.ToolSpec
	owner   agent.ToolDispatcher
	plugin  string
	enabled bool
}

// toolMux merges several plugins and is BOTH the agent's ToolRegistry
// and its ToolDispatcher. Tools can be enabled/disabled at runtime
// (via slash commands in the TUI): a disabled tool drops out of Search
// (so it leaves the prompt entirely — no context or compute) and Get
// (so it's uncallable). Thread-safe: the TUI toggles while the agent
// turn goroutine reads. Routing is by tool name (distinct per-plugin
// prefixes ⇒ no collision; first registrant wins on dup).
type toolMux struct {
	mu     sync.RWMutex
	order  []string // tool names, registration order (deterministic Search)
	byName map[string]*toolEntry
	// activators turn a stubbed, config-free plugin into the real thing on
	// first /enable — e.g. web spawns Chrome at runtime instead of needing
	// --web at launch. Keyed by plugin label; runs once (see activated).
	activators map[string]func() (plugin, func(), error)
	activated  map[string]bool
	cleanups   []func() // teardown for everything built (launch + lazy)
	// Remote MCP connectors (TEN-162/164): deps for building gated connectors
	// (set once in buildToolMux), and label→URL for every registered server so
	// /mcp add|list|remove can manage them at runtime.
	mcpDeps    mcpRuntimeDeps
	mcpServers map[string]string
	// Federated knowledge search (TEN-243): paired peers' dispatchers are kept
	// here (name→dispatcher) instead of exposing their peer_*_search tools to the
	// agent. When the agent calls a local search (e.g. wiki_search) it fans out to
	// these and appends the results (trust-but-verify). baseDesc remembers each
	// federated tool's original description so the peer-awareness suffix can be
	// rebuilt as peers connect/disconnect.
	peerDisp map[string]plugin
	baseDesc map[string]string
	// Federation drift metrics (TEN-243): per-peer fan-out outcome counters,
	// guarded by their own mutex so recording never contends with m.mu.
	fedMu    sync.Mutex
	fedStats map[string]*peerFedStat
	// onChange fires after any enable/disable, with a full name→enabled
	// snapshot, so the caller can persist the curation. Set AFTER restore
	// (restore must not re-trigger a save of what it just loaded).
	onChange func(map[string]bool)

	// --- Embedding-ranked tool selection (lazy, opt-in via SetEmbedder) ---
	// When the registered catalog reaches rankActivateThreshold AND an
	// embedder has been installed, Search switches from "return every
	// enabled tool" to "return the top max(rankMinKeepFloor, 3/4) by
	// cosine similarity to the query embedding." Best-effort: any failure
	// (nil embedder, embed error, dim mismatch) silently falls back to the
	// unranked path. See docs at the Search() method.
	embedder            model.Embedder       // nil = ranking off
	embedderFingerprint string               // invalidated on `/model use` swap
	toolEmbeddings      map[string][]float32 // tool name → description embedding

	// lastRank records what the most recent Search did + WHY (TEN-225 diag):
	// whether cosine ranking actually trimmed the catalog, or it fell back to
	// the full enabled set and the reason. Surfaced per-turn via RankingStatus
	// so the operator can SEE when ranking is silently off (the common cause of
	// a full tool catalog hitting the prompt every message). Best-effort /
	// last-writer-wins under a shared mux (concurrent sub-agent Searches) — it's
	// a diagnostic, not selection state.
	lastRank rankStatus
}

// rankStatus is the diagnostic snapshot of one Search (TEN-225).
type rankStatus struct {
	ranked   bool   // true = cosine ranking trimmed; false = full enabled set
	surfaced int    // tools returned this Search
	catalog  int    // enabled tools available (the denominator)
	reason   string // why ranking was inactive (empty when ranked)
}

// rankActivateThreshold — minimum number of REGISTERED tools to enable
// ranking. Below this, Search returns the full enabled set (today's
// behavior). 20 chosen because today's 7-plugin catalog (~25 tools)
// already trips it, surfacing the behavior change while still small;
// it scales naturally as plugins are added (Obsidian/Discord/Telegram/
// Notion/Microsoft tooling will push catalog past 50). See the scar at
// toolmux.go Search() comment for why earlier "cap by registration order"
// was wrong — cosine ranking targets relevance, not registration race.
const rankActivateThreshold = 20

// rankMinKeepFloor — hard minimum kept after ranking. Below this, we
// don't trim at all (a small catalog above threshold still returns
// everything). Chosen so the model always has comfortable breathing room
// across the GStack-typical tool calls (search / read / write / list).
const rankMinKeepFloor = 12

// rankDropFraction — fraction of the catalog to drop when ranking is
// active. 4 means "drop the bottom 25%, keep top 75%." Conservative on
// purpose — the failure mode (model needed a tool we hid) is worse than
// the cost it mitigates (prompt-token bloat).
const rankDropFraction = 4

// peerKnowledgeBoost (TEN-249) lifts the federated knowledge tools
// (wiki_search/memory_search) up the ranked list WHEN A PEER IS CONNECTED, so
// the agent's own peer-extended knowledge is salient rather than buried under
// web_search. It is a SALIENCE nudge, not a hard route: the system prompt
// decides WHEN to prefer the knowledge tier (internal vs open-world), and the
// boost is bounded so a genuinely web-relevant query keeps web_search surfaced.
// Sized to clear a typical tool-description cosine gap without dominating.
const peerKnowledgeBoost = 0.25

// coreToolNames are surfaced EVERY ranked turn regardless of cosine score (when
// enabled) — the agent's "hands" (local file ops + shell) and its memory recall,
// which a turn may need for its NEXT step even when the CURRENT query doesn't
// mention them. Relevance ranking + MaxToolsPerCall trim the long tail, but never
// these. Cardinal rule (TEN-225/226): hiding a tool the model needed costs more
// than its schema tokens. Membership is by exact tool name; only enabled+
// registered names are force-included, so listing a name that isn't present is a
// harmless no-op.
var coreToolNames = map[string]bool{
	"os_read_file":  true,
	"os_write_file": true,
	"os_edit_file":  true,
	"os_list_dir":   true,
	"os_exec":       true,
	"memory_search": true,
	"memory_recall": true,
	// The agent's own knowledge tier (TEN-249): wiki_search joins memory_search
	// as always-surfaced so the model can never be steered to web_search simply
	// because its own (peer-extended) wiki got ranked out of a large catalog.
	"wiki_search": true,
}

// setOnChange installs the persistence hook. Call it after restore.
func (m *toolMux) setOnChange(fn func(map[string]bool)) {
	m.mu.Lock()
	m.onChange = fn
	m.mu.Unlock()
}

// enabledSnapshot captures every tool's current enabled state by name.
func (m *toolMux) enabledSnapshot() map[string]bool {
	out := make(map[string]bool, len(m.order))
	for _, n := range m.order {
		out[n] = m.byName[n].enabled
	}
	return out
}

// errNotConfigured marks a lazy plugin that can't activate simply because
// the operator hasn't set it up yet — as opposed to a genuine failure (bad
// creds, a missing binary, an unsupported auth). restore() swallows it: an
// unconfigured optional plugin (e.g. gsuite before /configure) shouldn't
// print a "could not restore" line at every launch. Activators wrap it with
// %w so the interactive /enable path still shows a clean, actionable message
// while restore can detect it via errors.Is. Real failures still surface.
var errNotConfigured = errors.New("not configured")

// restore applies a persisted name→enabled snapshot over the flag-derived
// defaults, so /enable + /disable survive restarts. Only tools that still
// exist are touched (stale names from removed plugins are ignored). An
// enabled activator-backed tool (e.g. web) is activated here — best
// effort: a failure (Chrome missing) is reported, not fatal. Returns
// human-readable notes for the launch feed.
func (m *toolMux) restore(saved map[string]bool) []string {
	if len(saved) == 0 {
		return nil
	}
	names := make([]string, 0, len(saved))
	for n := range saved {
		names = append(names, n)
	}
	sort.Strings(names) // deterministic order (activation side effects)
	var notes []string
	applied := 0
	// Activation failures are per-PLUGIN, not per-tool: one shared activator
	// backs every tool a plugin owns (e.g. gsuite's 8 tools), so a failure
	// (unsupported/unconfigured auth) hits identically for each. maybeActivate
	// only memoizes success, so without this we'd re-run the failing activator
	// and emit the same note once per enabled tool — the nine-line "unknown
	// auth" wall at launch. Attempt once, report once, skip the rest.
	failedPlugins := map[string]bool{}
	for _, n := range names {
		m.mu.RLock()
		e, ok := m.byName[n]
		m.mu.RUnlock()
		if !ok {
			continue // tool no longer registered
		}
		// plugin label is immutable after registration — safe to read here.
		if saved[n] && e.plugin != "" && failedPlugins[e.plugin] {
			continue // this plugin already failed to activate this restore
		}
		if _, _, err := m.SetEnabled(n, saved[n]); err != nil {
			if e.plugin != "" {
				failedPlugins[e.plugin] = true // dedupe regardless of error kind
			}
			// A plugin the operator simply hasn't configured yet is an
			// expected state, not a restore error — swallow it (no launch
			// nag). Genuine failures still report, deduped per plugin.
			if errors.Is(err, errNotConfigured) {
				continue
			}
			if e.plugin != "" {
				notes = append(notes, fmt.Sprintf("could not restore %s tools: %v", e.plugin, err))
			} else {
				notes = append(notes, fmt.Sprintf("could not restore %s: %v", n, err))
			}
			continue
		}
		applied++
	}
	if applied > 0 {
		notes = append([]string{fmt.Sprintf("restored %d tool state(s)", applied)}, notes...)
	}
	return notes
}

func newToolMux() *toolMux {
	return &toolMux{
		byName:     map[string]*toolEntry{},
		peerDisp:   map[string]plugin{},
		baseDesc:   map[string]string{},
		activators: map[string]func() (plugin, func(), error){},
		activated:  map[string]bool{},
		mcpServers: map[string]string{},
	}
}

// mcpRuntimeDeps captures everything a remote-MCP activator needs, so the
// launch loop and runtime `/mcp add` build IDENTICAL (gated) connectors.
// Set once in buildToolMux (immutable after) — safe to read lock-free.
type mcpRuntimeDeps struct {
	ctx              context.Context
	callback         string
	openBrowser      func(string) error
	trustAnnotations bool
	allowWrite       bool
	confirm          func(context.Context, string, string) bool
	cacheDir         string       // 0600 OAuth token cache dir (persistence across restarts)
	log              *slog.Logger // connect/refresh failure logging (TEN-166)
}

// mcpServerStatus is one remote MCP connector's runtime state (for /mcp list).
type mcpServerStatus struct {
	Label     string
	URL       string
	Enabled   bool
	ToolCount int
}

// registerMCPRemote registers a disabled stub + activator for a remote MCP
// server using the mux's stored deps (idempotent). Activation — the browser
// OAuth flow — happens later on SetEnabled. Returns the derived label.
func (m *toolMux) registerMCPRemote(url string) string {
	label := mcpLabel(url)
	if m.hasPlugin(label) {
		return label
	}
	d := m.mcpDeps
	m.add(label, stubPlugin{hint: "`/enable " + label + "` connects to " + url + " and opens a browser to authorize"})
	m.SetEnabled(label, false)
	m.registerActivator(label, func() (plugin, func(), error) {
		return mcpremote.Open(d.ctx, mcpremote.Config{
			ServerURL:    url,
			Label:        label,
			CallbackAddr: d.callback,
			OpenBrowser:  d.openBrowser,
			CacheDir:     d.cacheDir,
			Interactive:  true, // /enable or /mcp add may sign in via browser (cached token ⇒ silent)
			Logger:       d.log,
		}, d.trustAnnotations, mcpremote.Policy{
			AllowWrite: d.allowWrite,
			Confirm:    d.confirm,
		})
	})
	m.mu.Lock()
	m.mcpServers[label] = url
	m.mu.Unlock()
	return label
}

// reconnectMCPSilently attempts a NON-interactive reconnect (cached token, no
// browser) at launch and, on success, brings the server's tools live + enabled.
// A missing/expired token fails cleanly and leaves the disabled stub in place
// (the interactive activator can sign in later). Safe to run in a goroutine.
func (m *toolMux) reconnectMCPSilently(url string) {
	label := mcpLabel(url)
	d := m.mcpDeps
	// Bound the launch dial so a hung/slow server can't park this goroutine for
	// the whole session. The SDK runs the session on its own background context
	// (not this one), so the timeout caps only the initialize handshake — a
	// connected session survives after Open returns.
	dialCtx, cancel := context.WithTimeout(d.ctx, 30*time.Second)
	defer cancel()
	disp, cleanup, err := mcpremote.Open(dialCtx, mcpremote.Config{
		ServerURL:    url,
		Label:        label,
		CallbackAddr: d.callback,
		CacheDir:     d.cacheDir,
		Interactive:  false, // launch: never pop a browser
		Logger:       d.log,
	}, d.trustAnnotations, mcpremote.Policy{
		AllowWrite: d.allowWrite,
		Confirm:    d.confirm,
	})
	if err != nil {
		// Previously swallowed — that's why "check the logs" showed nothing for a
		// broken MCP server (TEN-166). errNoCachedSession (no token yet) is the
		// normal first-run case → debug; anything else is a real failure → warn.
		if d.log != nil {
			if strings.Contains(err.Error(), "no usable cached session") {
				d.log.Debug("mcp: silent reconnect skipped (no cached session)", "server", url)
			} else {
				d.log.Warn("mcp: silent reconnect failed", "server", url, "err", err.Error())
			}
		}
		return // stays a disabled stub; interactive activator can sign in later
	}
	m.adoptLiveMCP(label, disp, cleanup)
}

// adoptLiveMCP merges an already-connected remote plugin's tools into the mux
// ENABLED (first-registrant-wins, so it can't shadow a local tool) and marks the
// label activated so the interactive activator won't double-run.
func (m *toolMux) adoptLiveMCP(label string, p plugin, cleanup func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.activated[label] {
		if cleanup != nil {
			go cleanup() // someone already brought it live; drop this connection
		}
		return
	}
	added := false
	for _, spec := range p.Tools() {
		if _, exists := m.byName[spec.Name]; exists {
			continue
		}
		m.order = append(m.order, spec.Name)
		m.byName[spec.Name] = &toolEntry{spec: spec, owner: p, plugin: label, enabled: true}
		added = true
	}
	if added {
		// Invalidate the ranking cache so the newly-adopted tools (e.g. a 31-tool
		// remote MCP) get real description embeddings on the next Search instead of
		// sitting at the neutral sim=0.5 — which would let them crowd out genuinely
		// relevant tools. (TEN-226 step 6; mirrors add().)
		m.toolEmbeddings = nil
	}
	m.activated[label] = true
	if cleanup != nil {
		m.cleanups = append(m.cleanups, cleanup)
	}
}

// reconnectPeersSilently dials every paired peer we hold a token for (TEN-186)
// and brings its shared knowledge tools (peer_wiki_search / peer_memory_search)
// live for the agent — namespaced "peer:<name>". Same non-blocking, no-prompt
// pattern as reconnectMCPSilently: a peer that's offline or whose token was
// revoked just stays absent (logged), never stalling launch. Single-peer is the
// trial target; with multiple peers the un-prefixed tool names collide and
// first-registrant-wins (adoptLiveMCP), so a 2nd peer's identically-named tools
// are shadowed — acceptable for the trial, namespaced fully in a follow-up.
func (m *toolMux) reconnectPeersSilently(cfgDir string) {
	store, err := peering.LoadStore(cfgDir)
	if err != nil {
		return
	}
	for _, peer := range store.List() {
		if !peer.Dial || peer.URL == "" || peer.Token == "" {
			continue // we don't dial this peer, or it's revoked
		}
		go func(p *peering.Peer) {
			if m.hasPeer(p.Name) {
				return // already adopted for federated search
			}
			label := "peer:" + p.Name
			d := m.mcpDeps
			dialCtx, cancel := context.WithTimeout(d.ctx, 30*time.Second)
			defer cancel()
			disp, cleanup, derr := mcpremote.OpenStatic(dialCtx, mcpremote.StaticConfig{
				ServerURL: p.URL,
				Token:     p.Token,
				Label:     label,
				TLS:       peering.PinnedTLSClientConfig(p.Fingerprint), // nil ⇒ plain HTTP (overlay)
			}, mcpremote.Policy{Confirm: d.confirm})
			if derr != nil {
				if d.log != nil {
					d.log.Warn("peer: silent reconnect failed", "peer", p.Name, "err", derr.Error())
				}
				return
			}
			// Fold the peer in for federated search instead of exposing its
			// peer_*_search tools to the agent. (TEN-243)
			m.adoptPeer(p.Name, disp, cleanup)
		}(peer)
	}
}

// AddRemoteMCP registers AND activates a remote MCP connector at runtime (the
// `/mcp add` path). Activation opens the browser and blocks on the OAuth
// callback. Returns the label + how many tools came live. On failure it cleans
// up a freshly-registered stub so a retry works.
func (m *toolMux) AddRemoteMCP(url string) (label string, toolCount int, err error) {
	label = mcpLabel(url)
	already := m.hasPlugin(label)
	m.registerMCPRemote(url)
	n, _, aerr := m.SetEnabled(label, true) // triggers maybeActivate → browser
	if aerr != nil {
		if !already {
			m.forgetPlugin(label)
		}
		return label, 0, aerr
	}
	return label, n, nil
}

// forgetPlugin removes a plugin's tools + activator from the mux entirely (the
// inverse of add). Used by `/mcp remove` and failed-add cleanup.
func (m *toolMux) forgetPlugin(label string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.order[:0]
	for _, n := range m.order {
		if e := m.byName[n]; e != nil && e.plugin == label {
			delete(m.byName, n)
			continue
		}
		kept = append(kept, n)
	}
	m.order = kept
	delete(m.activators, label)
	delete(m.activated, label)
	delete(m.mcpServers, label)
}

// RemoteMCPList reports every registered remote MCP connector + its state.
func (m *toolMux) RemoteMCPList() []mcpServerStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]mcpServerStatus, 0, len(m.mcpServers))
	for label, url := range m.mcpServers {
		st := mcpServerStatus{Label: label, URL: url}
		for _, n := range m.order {
			if e := m.byName[n]; e != nil && e.plugin == label {
				st.ToolCount++
				if e.enabled {
					st.Enabled = true
				}
			}
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// addCleanup registers a teardown func (closes a browser/db handle), run
// in reverse order by Close.
func (m *toolMux) addCleanup(fn func()) {
	if fn == nil {
		return
	}
	m.mu.Lock()
	m.cleanups = append(m.cleanups, fn)
	m.mu.Unlock()
}

// Close tears down every plugin the mux built (launch-time and lazily
// activated), in reverse order. Safe to call once at shutdown.
func (m *toolMux) Close() {
	m.mu.Lock()
	cs := m.cleanups
	m.cleanups = nil
	m.mu.Unlock()
	for i := len(cs) - 1; i >= 0; i-- {
		cs[i]()
	}
}

// registerActivator records how to build a config-free plugin on demand.
// Only registered for stubbed plugins that need no operator input (web).
func (m *toolMux) registerActivator(label string, fn func() (plugin, func(), error)) {
	m.mu.Lock()
	m.activators[label] = fn
	m.mu.Unlock()
}

// add registers a plugin's tools under a label (used for group toggling
// like `/disable os`). Tools start enabled. Invalidates the precomputed
// tool embeddings (if any) so the next Search re-precomputes — necessary
// because activators install plugins LATE (e.g. `/enable web` builds
// Chrome long after construction), and a stale embedding cache would
// silently rank the new tool at sim=0.
func (m *toolMux) add(label string, p plugin) {
	m.mu.Lock()
	defer m.mu.Unlock()
	added := false
	for _, sp := range p.Tools() {
		if _, dup := m.byName[sp.Name]; dup {
			continue
		}
		m.byName[sp.Name] = &toolEntry{spec: sp, owner: p, plugin: label, enabled: true}
		m.order = append(m.order, sp.Name)
		added = true
	}
	if added {
		// Cache stale — next Search re-precomputes (best-effort).
		m.toolEmbeddings = nil
	}
}

// --- agent.ToolRegistry (enabled tools only) ---

func (m *toolMux) Get(name string) (model.ToolSpec, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.byName[name]
	if !ok || !e.enabled {
		return model.ToolSpec{}, false
	}
	return e.spec, true
}

// Search returns the set of enabled tools to surface in the prompt for
// this turn. Two modes, chosen by catalog size + embedder availability:
//
//  1. UNRANKED (today's historical behavior, preserved as the floor).
//     Returns every enabled tool. Used when:
//     • registered tool count < rankActivateThreshold, OR
//     • no embedder has been installed via SetEmbedder, OR
//     • the query embedding is nil (caller had no embedder), OR
//     • precomputing tool-description embeddings failed
//     This branch carries the historical contract: the enabled set IS
//     the curation (via /enable + /disable); we never silently drop a
//     tool the operator opted into. The earlier "blind top-k by
//     registration order" bug (which made `/enable web` silently fail
//     when sql+wiki+os filled the cap first) lives in the test
//     TestToolMux_SearchReturnsAllEnabledIgnoringCap as a drift guard.
//
//  2. RANKED (lazy, opt-in, scale path). When the catalog crosses
//     rankActivateThreshold AND an embedder is installed, embed every
//     tool description once (cached), then per-turn return the top
//     max(rankMinKeepFloor, catalog × (1 - 1/rankDropFraction)) by
//     cosine similarity to the query embedding. The signal here is
//     RELEVANCE, not registration order — fixing the original bug at
//     the level the fix actually wants.
//
// Best-effort throughout: any failure in mode 2 falls back to mode 1
// silently. Callers cannot tell the difference (they always get a
// non-error result with the enabled set as the floor).
func (m *toolMux) Search(ctx context.Context, queryEmb []float32, maxTools int) ([]model.ToolSpec, error) {
	m.mu.RLock()
	catalogSize := len(m.order)
	haveEmbedder := m.embedder != nil
	haveCache := len(m.toolEmbeddings) > 0
	m.mu.RUnlock()

	// fallback returns the full enabled set and records WHY ranking didn't run
	// (TEN-225) so the operator can see when the whole catalog is hitting the
	// prompt every turn — the reasons map 1:1 to the known root causes.
	fallback := func(reason string) ([]model.ToolSpec, error) {
		out := m.searchAll()
		m.recordRank(false, len(out), len(out), reason)
		return out, nil
	}

	// Fast path: ranking inactive → today's full enabled set.
	switch {
	case catalogSize < rankActivateThreshold:
		return fallback(fmt.Sprintf("catalog below ranking threshold (%d<%d tools)", catalogSize, rankActivateThreshold))
	case queryEmb == nil:
		return fallback("no query embedding this turn (retrieval degraded — check the embedder)")
	case !haveEmbedder:
		return fallback("no embedder installed for tool ranking on this agent")
	}
	if !haveCache {
		// Slow path: precompute once for this embedder. Best-effort —
		// failure leaves cache empty, we fall back to unranked below.
		m.precomputeEmbeddings(ctx)
		m.mu.RLock()
		haveCache = len(m.toolEmbeddings) > 0
		m.mu.RUnlock()
		if !haveCache {
			return fallback("tool-embedding precompute failed (embedder error or dim mismatch)")
		}
	}
	// Dim guard (TEN-226 step 3): if the query embedding's dimension doesn't
	// match the cached tool embeddings (e.g. cache built by a 128-d echo embedder,
	// query now 768-d after a /model swap), cosineSimF32 returns 0 for EVERY tool
	// → the rank collapses to alphabetical and silently keeps the wrong tools.
	// Fall back to the full enabled set (with a visible reason) instead of ranking
	// on garbage; `/model reload` or `tenant memory reembed` rebuilds the cache.
	if d := m.cachedEmbDim(); d > 0 && d != len(queryEmb) {
		return fallback(fmt.Sprintf("embedding dim mismatch (query %d-d vs tool cache %d-d) — run /model reload", len(queryEmb), d))
	}
	out := m.searchRanked(queryEmb, maxTools)
	m.recordRank(true, len(out), m.enabledCount(), "")
	return out, nil
}

// cachedEmbDim returns the dimension of any cached tool embedding (0 if the
// cache is empty). Used by the Search dim guard.
func (m *toolMux) cachedEmbDim() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, v := range m.toolEmbeddings {
		return len(v)
	}
	return 0
}

// recordRank stores the most recent Search's ranking diagnostic (TEN-225).
func (m *toolMux) recordRank(ranked bool, surfaced, catalog int, reason string) {
	m.mu.Lock()
	m.lastRank = rankStatus{ranked: ranked, surfaced: surfaced, catalog: catalog, reason: reason}
	m.mu.Unlock()
}

// RankingStatus reports what the most recent Search did and why (TEN-225) —
// read by the agent loop to emit a per-turn tool-catalog diagnostic. ok is
// false until the first Search of the session.
func (m *toolMux) RankingStatus() (ranked bool, surfaced, catalog int, reason string, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := m.lastRank
	return s.ranked, s.surfaced, s.catalog, s.reason, s.catalog > 0 || s.reason != ""
}

// enabledCount is the number of currently-enabled tools (the ranking denominator).
func (m *toolMux) enabledCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for _, name := range m.order {
		if e := m.byName[name]; e != nil && e.enabled {
			n++
		}
	}
	return n
}

// searchAll is the unranked path — every enabled tool, deterministic
// registration order. Lifted out so Search can return it cleanly from
// any fallback branch.
func (m *toolMux) searchAll() []model.ToolSpec {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]model.ToolSpec, 0, len(m.order))
	for _, n := range m.order {
		if e := m.byName[n]; e.enabled {
			out = append(out, e.spec)
		}
	}
	return out
}

// searchRanked is the cosine-ranked path. Sorts enabled tools by
// similarity to queryEmb, then keeps the top max(rankMinKeepFloor,
// catalog × 3/4). Stable tie-break (alphabetical by tool name) so two
// adjacent queries with identical sims produce identical orderings —
// transcripts stay reproducible.
//
// A tool with no precomputed embedding (rare — only if add() raced
// precompute) is treated as sim=0.5 so it neither leads nor lags by
// default. Next Search will re-precompute via the cache-invalidation in
// add().
func (m *toolMux) searchRanked(queryEmb []float32, maxTools int) []model.ToolSpec {
	m.mu.RLock()
	defer m.mu.RUnlock()
	type scored struct {
		spec model.ToolSpec
		sim  float64
	}
	// Peer-connected salience boost (TEN-249): when at least one peer is
	// connected, the federated knowledge tools get lifted up the ranking so the
	// agent's own (peer-extended) wiki/memory isn't buried under web_search.
	peersConnected := len(m.peerDisp) > 0
	rows := make([]scored, 0, len(m.order))
	for _, n := range m.order {
		e := m.byName[n]
		if !e.enabled {
			continue
		}
		emb := m.toolEmbeddings[n]
		sim := 0.5 // neutral default for race-installed tools
		if emb != nil {
			sim = cosineSimF32(queryEmb, emb)
		}
		if peersConnected && federatableTools[n] != "" {
			sim += peerKnowledgeBoost
		}
		rows = append(rows, scored{spec: e.spec, sim: sim})
	}
	sort.SliceStable(rows, func(a, b int) bool {
		if rows[a].sim != rows[b].sim {
			return rows[a].sim > rows[b].sim
		}
		return rows[a].spec.Name < rows[b].spec.Name
	})

	// keep starts at the historical "drop the bottom 1/rankDropFraction, never
	// below the floor" target.
	keep := rankMinKeepFloor
	if drop := len(rows) / rankDropFraction; len(rows)-drop > keep {
		keep = len(rows) - drop
	}
	// Honor the profile's MaxToolsPerCall as a CEILING — the real token cut
	// (TEN-226 step 5). The FLOOR WINS: a tiny cap (e.g. the small profile's 3)
	// is raised to rankMinKeepFloor so it can never starve the agent. maxTools<=0
	// means "no cap configured" → keep the historical target.
	if maxTools > 0 {
		ceiling := maxTools
		if ceiling < rankMinKeepFloor {
			ceiling = rankMinKeepFloor
		}
		if ceiling < keep {
			keep = ceiling
		}
	}
	if keep > len(rows) {
		keep = len(rows)
	}

	// Take the top-`keep` by relevance, then UNION the always-on core set so a
	// core capability (the agent's hands + memory) is never ranked/capped out —
	// the cardinal rule. Core tools that didn't make the cut are appended in rank
	// order, so the surfaced count is keep + (enabled core not already kept).
	out := make([]model.ToolSpec, 0, keep+len(coreToolNames))
	chosen := make(map[string]bool, keep+len(coreToolNames))
	for i := 0; i < keep; i++ {
		out = append(out, rows[i].spec)
		chosen[rows[i].spec.Name] = true
	}
	for _, r := range rows {
		if chosen[r.spec.Name] || !coreToolNames[r.spec.Name] {
			continue
		}
		out = append(out, r.spec)
		chosen[r.spec.Name] = true
	}
	return out
}

// SetEmbedder installs (or replaces) the embedder used for tool-catalog
// ranking. Call once after construction. Re-call with a different
// fingerprint (e.g. after `/model use ...` swaps the embedder) to
// invalidate the precomputed embedding cache — required because two
// different embedders may produce vectors with different dimensions, and
// a cached vector from the OLD embedder would silently produce wrong
// cosines (zero or garbage) against the new query embedding.
//
// The fingerprint convention is the embedder profile ID. Two embedders
// with the same ID are assumed equivalent (Tenant's model.Profile.ID is
// a stable string per provider+model pair).
//
// Passing a nil embedder disables ranking (Search returns to the full
// enabled set).
func (m *toolMux) SetEmbedder(fingerprint string, e model.Embedder) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if fingerprint != m.embedderFingerprint || e == nil {
		m.toolEmbeddings = nil
	}
	m.embedderFingerprint = fingerprint
	m.embedder = e
}

// precomputeEmbeddings batch-embeds every enabled tool's description.
// Lazy: called from the Search slow path the first time ranking is
// active for a given embedder. Best-effort: an error or a partial result
// leaves m.toolEmbeddings empty so Search degrades to the unranked path.
// Lock discipline: snapshot the inputs under RLock, do the network call
// without any lock held, then write the cache under Lock.
func (m *toolMux) precomputeEmbeddings(ctx context.Context) {
	m.mu.RLock()
	emb := m.embedder
	if emb == nil || len(m.order) == 0 {
		m.mu.RUnlock()
		return
	}
	names := make([]string, 0, len(m.order))
	descs := make([]string, 0, len(m.order))
	for _, n := range m.order {
		e := m.byName[n]
		// Embed description for ALL registered tools (enabled or not);
		// /enable could flip them on later and we'd avoid a re-embed.
		names = append(names, n)
		descs = append(descs, e.spec.Description)
	}
	m.mu.RUnlock()

	vecs, err := emb.Embed(ctx, descs)
	if err != nil || len(vecs) != len(names) {
		return // best-effort — empty cache → Search falls back to unranked
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Re-check: another goroutine may have precomputed or invalidated
	// concurrently. We only install if the cache is still empty.
	if m.toolEmbeddings != nil {
		return
	}
	m.toolEmbeddings = make(map[string][]float32, len(names))
	for i, n := range names {
		m.toolEmbeddings[n] = vecs[i]
	}
}

func (m *toolMux) All() []model.ToolSpec {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]model.ToolSpec, 0, len(m.order))
	for _, n := range m.order {
		if e := m.byName[n]; e.enabled {
			out = append(out, e.spec)
		}
	}
	return out
}

// --- agent.ToolDispatcher ---

func (m *toolMux) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	m.mu.RLock()
	e, ok := m.byName[call.Name]
	// Snapshot the federation peers for this call (nil unless the tool is
	// federatable AND peers are connected) while we hold the lock; the fan-out
	// itself runs lock-free since it does network I/O. (TEN-243)
	counterpart, peers := m.peersForFederation(call.Name)
	readCounterpart, readPeers := m.peersForReadFallback(call.Name)
	m.mu.RUnlock()
	if !ok {
		return "unknown tool: " + call.Name, true, nil
	}
	if !e.enabled {
		return "tool " + call.Name + " is disabled (/enable " + call.Name + ")", true, nil
	}
	out, isErr, err := e.owner.Dispatch(ctx, call)
	// Local-first, peer-appended: only federate a clean local result so the
	// agent isn't handed peer data stapled to a local error. (TEN-243)
	if err == nil && !isErr && len(peers) > 0 {
		out = m.federate(ctx, out, counterpart, call.Arguments, peers)
	}
	// Read-on-miss fallback: a local "read one item" tool that missed (isErr)
	// transparently falls back to the first connected peer that has it, so the
	// agent reads a note by filename whether it's local or on a peer. (TEN-243)
	if err == nil && isErr && len(readPeers) > 0 {
		if peerOut, found := m.peerReadFallback(ctx, readCounterpart, call.Arguments, readPeers); found {
			return peerOut, false, nil
		}
	}
	return out, isErr, err
}

// --- tui.ToolControl (runtime enable/disable from slash commands) ---

func (m *toolMux) ToolList() []tui.ToolInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]tui.ToolInfo, 0, len(m.order))
	for _, n := range m.order {
		e := m.byName[n]
		out = append(out, tui.ToolInfo{Name: n, Plugin: e.plugin, Enabled: e.enabled, Gated: e.spec.Gated})
	}
	return out
}

func (m *toolMux) hasPlugin(label string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, e := range m.byName {
		if e.plugin == label {
			return true
		}
	}
	return false
}

// stubPlugin advertises a plugin's real tool specs but isn't configured
// — calling any of its tools returns a "needs setup" status (like a 401)
// instead of failing to load. This is what makes the framework run even
// when a plugin is unauthenticated, and lets every plugin appear in
// /tools so you know it exists.
type stubPlugin struct {
	specs []model.ToolSpec
	hint  string
}

func (s stubPlugin) Tools() []model.ToolSpec { return s.specs }
func (s stubPlugin) Dispatch(context.Context, model.ToolCall) (string, bool, error) {
	return "this plugin is not configured — " + s.hint, true, nil
}

// SetPluginEnabled is the explicit categorical toggle: forces a plugin-
// label sweep and never matches a single tool name. Used by the TUI's
// `/enable skill <label>` form to give the user a clean "no skill named
// X" error when they typo, instead of falling through to the smart-
// match path. Same activation + notify-onChange contract as SetEnabled.
func (m *toolMux) SetPluginEnabled(label string, on bool) (int, string, error) {
	if on {
		if err := m.maybeActivate(label); err != nil {
			return 0, "", err
		}
	}
	m.mu.Lock()
	n := 0
	for _, e := range m.byName {
		if e.plugin == label {
			e.enabled = on
			n++
		}
	}
	var snap map[string]bool
	if n > 0 && m.onChange != nil {
		snap = m.enabledSnapshot()
	}
	m.mu.Unlock()
	if snap != nil {
		m.onChange(snap)
	}
	scope := ""
	if n > 0 {
		scope = "plugin"
	}
	return n, scope, nil
}

// Plugins returns the sorted unique set of plugin labels currently
// registered on the mux (sql, wiki, web, gsuite, …). Powers the TUI's
// "did you mean" hint when `/enable skill <typo>` finds no match.
func (m *toolMux) Plugins() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	seen := map[string]bool{}
	out := make([]string, 0, 8)
	for _, e := range m.byName {
		if e.plugin == "" || seen[e.plugin] {
			continue
		}
		seen[e.plugin] = true
		out = append(out, e.plugin)
	}
	sort.Strings(out)
	return out
}

// SetEnabled toggles a single tool (exact name) or a whole plugin (by
// label). Returns how many tools changed and the scope ("tool" or
// "plugin"); (0, "", nil) if nothing matched. Enabling a plugin that's
// only a stub lazily activates it (e.g. launches Chrome for web) — a
// build failure is returned as the error and nothing is toggled.
func (m *toolMux) SetEnabled(target string, on bool) (int, string, error) {
	if on {
		if err := m.maybeActivate(target); err != nil {
			return 0, "", err
		}
	}
	m.mu.Lock()
	n, scope := 0, ""
	if e, ok := m.byName[target]; ok {
		e.enabled = on
		n, scope = 1, "tool"
	} else {
		for _, e := range m.byName {
			if e.plugin == target {
				e.enabled = on
				n++
			}
		}
		if n > 0 {
			scope = "plugin"
		}
	}
	// Snapshot under the lock; notify after releasing it (the hook does
	// file I/O — must not run while holding the mux lock).
	var snap map[string]bool
	if n > 0 && m.onChange != nil {
		snap = m.enabledSnapshot()
	}
	m.mu.Unlock()
	if snap != nil {
		m.onChange(snap)
	}
	return n, scope, nil
}

// maybeActivate builds a stubbed plugin for real the first time it's
// enabled, swapping the stub owner for the live dispatcher. target may be
// a plugin label or one of its tool names. No-op if there's no activator
// or it already ran. The build runs OUTSIDE the lock (spawning Chrome is
// slow) — a concurrent winner is detected after and our build discarded.
func (m *toolMux) maybeActivate(target string) error {
	m.mu.Lock()
	label := target
	if e, ok := m.byName[target]; ok {
		label = e.plugin
	}
	fn, ok := m.activators[label]
	if !ok || m.activated[label] {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	p, cleanup, err := fn()
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.activated[label] { // someone else won the race; drop ours
		if cleanup != nil {
			go cleanup()
		}
		return nil
	}
	for _, n := range m.order {
		if e := m.byName[n]; e.plugin == label {
			e.owner = p
		}
	}
	// Zero-spec stubs (e.g. a remote MCP connector) only learn their tool
	// list AFTER connecting, so the stub registered no specs to swap above.
	// Merge whatever the live plugin now advertises. First-registrant-wins:
	// a name already owned by ANY plugin is left untouched, so a remote
	// server can't shadow a trusted local tool (e.g. gmail_send). Added
	// disabled — the SetEnabled caller flips this plugin's tools on; that
	// also means they don't silently auto-activate at restore (which would
	// pop a browser at startup).
	merged := false
	for _, spec := range p.Tools() {
		if _, exists := m.byName[spec.Name]; exists {
			continue
		}
		m.order = append(m.order, spec.Name)
		m.byName[spec.Name] = &toolEntry{spec: spec, owner: p, plugin: label, enabled: false}
		merged = true
	}
	if merged {
		// New specs learned at activation time (a connected stub plugin) → drop
		// the ranking cache so they get real description embeddings on the next
		// Search, not the neutral sim=0.5. They're added disabled, but precompute
		// embeds ALL registered tools, so they're ready when SetEnabled flips them
		// on. (TEN-226 step 6 — mirrors add()/adoptLiveMCP.)
		m.toolEmbeddings = nil
	}
	if cleanup != nil {
		m.cleanups = append(m.cleanups, cleanup)
	}
	m.activated[label] = true
	return nil
}

// pluginFlags collects the enable/config flags for every plugin. All
// default OFF (memory-only chat). Dangerous actions (write/send/post)
// default OFF even when a plugin is enabled — the operator opts in.
type pluginFlags struct {
	wikiDir string

	sqlDB         string
	sqlAllowWrite bool

	web              bool
	webShow          bool
	webAllowInteract bool

	gsuite          bool
	gsuiteAuth      string
	gsuiteSAJSON    string
	gsuiteSubject   string
	gsuiteAllowSend bool

	atlassian           bool
	atlassianSite       string
	atlassianEmail      string
	atlassianProject    string
	atlassianAllowWrite bool
	atlassianClientID   string
	atlassianCallback   string

	mcpRemotes          []string // --mcp-remote (repeatable): remote MCP server URLs
	mcpTrustAnnotations bool
	mcpAllowWrite       bool
	mcpCallback         string

	x          bool
	xBearer    string
	xAllowPost bool

	imsg          bool
	imsgURL       string
	imsgPass      string
	imsgPrivate   bool
	imsgAllowSend bool

	discord          bool
	discordToken     string
	discordAllowSend bool

	osEnable     bool
	osAllowExec  bool
	osAllowWrite bool
}

func bindPluginFlags(fs *flag.FlagSet) *pluginFlags {
	p := &pluginFlags{}
	fs.StringVar(&p.wikiDir, "wiki-dir", "", "enable the wiki plugin over this markdown dir")
	fs.StringVar(&p.sqlDB, "sql-db", "", "enable the sql plugin over this SQLite file")
	fs.BoolVar(&p.sqlAllowWrite, "sql-allow-write", false, "permit SQL INSERT/UPDATE/DELETE")
	fs.BoolVar(&p.web, "web", false, "enable the web plugin (drives Chrome)")
	fs.BoolVar(&p.webShow, "web-show", false, "run a visible Chrome window")
	fs.BoolVar(&p.webAllowInteract, "web-allow-interact", false, "permit web click/fill/select")
	fs.BoolVar(&p.gsuite, "gsuite", false, "enable Gmail + Calendar")
	fs.StringVar(&p.gsuiteAuth, "gsuite-auth", "gcloud", "gsuite auth: gcloud|sa")
	fs.StringVar(&p.gsuiteSAJSON, "gsuite-sa-json", "", "gsuite service-account JSON (auth=sa)")
	fs.StringVar(&p.gsuiteSubject, "gsuite-subject", "", "gsuite impersonated user (auth=sa)")
	fs.BoolVar(&p.gsuiteAllowSend, "gsuite-allow-send", false, "permit gmail_send/calendar_create")
	fs.BoolVar(&p.atlassian, "atlassian", false, "enable Jira tools (Atlassian)")
	fs.StringVar(&p.atlassianSite, "atlassian-site", "", "Atlassian site URL, e.g. https://you.atlassian.net (Path A)")
	fs.StringVar(&p.atlassianEmail, "atlassian-email", "", "Atlassian account email (Path A; token via $ATLASSIAN_TOKEN)")
	fs.StringVar(&p.atlassianProject, "atlassian-project", "", "default Jira project key (e.g. TEN)")
	fs.BoolVar(&p.atlassianAllowWrite, "atlassian-allow-write", false, "permit jira_create/comment/transition (writes to the board)")
	fs.StringVar(&p.atlassianClientID, "atlassian-client-id", "", "Atlassian OAuth app client id (Path B; secret via $ATLASSIAN_CLIENT_SECRET, login via `tenant atlassian login`)")
	fs.StringVar(&p.atlassianCallback, "atlassian-oauth-callback", "", "OAuth callback bind addr (default 127.0.0.1:8765)")
	fs.Func("mcp-remote", "remote MCP server URL to connect as a client (repeatable; OAuth2.1+DCR browser sign-in via `/enable`)", func(s string) error {
		p.mcpRemotes = append(p.mcpRemotes, s)
		return nil
	})
	fs.BoolVar(&p.mcpTrustAnnotations, "mcp-trust-annotations", false, "trust remote servers' read-only tool annotations (relax the deny-by-default gate)")
	fs.BoolVar(&p.mcpAllowWrite, "mcp-allow-write", false, "permit remote MCP write tools without per-action confirm")
	fs.StringVar(&p.mcpCallback, "mcp-callback", "", "localhost OAuth callback for --mcp-remote (default 127.0.0.1:8765)")
	fs.BoolVar(&p.x, "x", false, "enable X/Twitter")
	fs.StringVar(&p.xBearer, "x-bearer", "", "X app bearer token (or $X_BEARER_TOKEN)")
	fs.BoolVar(&p.xAllowPost, "x-allow-post", false, "permit x_post/x_delete")
	fs.BoolVar(&p.imsg, "imessage", false, "enable iMessage (native chat.db on macOS; --bb-url switches to BlueBubbles)")
	fs.StringVar(&p.imsgURL, "bb-url", "", "BlueBubbles URL (or $BLUEBUBBLES_URL)")
	fs.StringVar(&p.imsgPass, "bb-password", "", "BlueBubbles password (or $BLUEBUBBLES_PASSWORD)")
	fs.BoolVar(&p.imsgPrivate, "bb-private-api", false, "use BlueBubbles private-api send method")
	fs.BoolVar(&p.imsgAllowSend, "imessage-allow-send", false, "permit imessage_send/new_chat")
	fs.BoolVar(&p.discord, "discord", false, "enable the Discord plugin (REST: read/send/react)")
	fs.StringVar(&p.discordToken, "discord-bot-token", "", "Discord bot token (or $DISCORD_BOT_TOKEN)")
	fs.BoolVar(&p.discordAllowSend, "discord-allow-send", false, "permit discord_send_message/discord_react (posts publicly as the bot)")
	fs.BoolVar(&p.osEnable, "os", false, "enable the OS plugin (sysinfo, file read, dir list, processes, gated shell exec)")
	fs.BoolVar(&p.osAllowExec, "os-allow-exec", false, "permit os_exec (run shell commands) — destructive ones still need confirm")
	fs.BoolVar(&p.osAllowWrite, "os-allow-write", false, "permit os_write/os_edit/os_append/os_make_dir (file writes)")
	return p
}

// buildToolMux constructs every enabled plugin and returns the merged
// dispatcher + a cleanup func (closes browser/db handles). An enabled
// plugin that fails to construct returns an error — better to tell the
// operator their flag is wrong than to silently drop the tool.
func buildToolMux(ctx context.Context, c *commonFlags, router *model.Router, pf *pluginFlags, confirm func(context.Context, string, string) bool, log *slog.Logger) (*toolMux, *wiki.Index, func(), error) {
	mux := newToolMux()
	// wikiIx is captured here so callers (cmdTUI / research auto-deposit)
	// can force a reindex after writeWikiReport without having to chase
	// the dispatcher's unexported `ix` field. nil when --wiki-dir is
	// unset. TEN-44.
	var wikiIx *wiki.Index
	fail := func(err error) (*toolMux, *wiki.Index, func(), error) {
		mux.Close()
		return nil, nil, nil, err
	}
	// When an approval broker is wired (TUI), it is the single decision
	// point: per-action gate flags drop to false so every dangerous action
	// flows through Confirm, and the broker's category modes decide. The
	// auth/scope-level flags (gsuite/x Config) are untouched — they pick
	// OAuth scopes, a separate concern from per-action approval.
	gateAllow := func(flag bool) bool {
		if confirm != nil {
			return false
		}
		return flag
	}

	if pf.sqlDB != "" {
		db, err := sqlp.Open(sqlp.Config{Driver: "sqlite", DSN: pf.sqlDB})
		if err != nil {
			return fail(fmt.Errorf("sql plugin: %w", err))
		}
		mux.addCleanup(func() { _ = db.Close() })
		mux.add("sql", sqlp.NewDispatcher(db, sqlp.Policy{AllowWrite: pf.sqlAllowWrite, Confirm: confirm}))
	}

	if pf.wikiDir != "" {
		ix, err := buildWikiIndex(ctx, c, router, pf.wikiDir, log)
		if err != nil {
			return fail(err)
		}
		mux.add("wiki", wiki.NewDispatcher(ix))
		// Gated local-write counterpart so a fetched peer note (or research) can
		// be ingested into the wiki on the user's direction. (TEN-244)
		mux.add("wiki", newWikiSaveDispatcher(pf.wikiDir, ix, confirm))
		wikiIx = ix // exported via the return tuple for TEN-44 auto-reindex
	}

	// Local memory recall tool (TEN-243 Phase B). Enabled by default as of
	// TEN-249: it gives the agent ACTIVE recall of its own long-term memory and,
	// when a memory-sharing peer is connected, federates to that peer — so the
	// knowledge tier is always available and the model isn't pushed to web_search
	// for "what do we know / did we decide" questions. (Relevant memory is still
	// auto-assembled each turn too; this just adds the on-demand search.) The
	// operator can still `/disable memory_search`; a persisted choice wins via
	// restore. Opens its own read-only store handle (SQLite WAL ⇒ safe alongside
	// the live handle); a failure to open just means no memory_search.
	if mst, mclose, merr := openStores(c); merr == nil {
		mux.addCleanup(mclose)
		hostName, _ := os.Hostname()
		var memEmb model.Embedder
		if router != nil { // nil in some headless/test paths; embedder is optional
			if e, _, eerr := router.EmbedderForRole(ctx, model.RoleEmbedder); eerr == nil {
				memEmb = e
			}
		}
		mux.add("memory", newMemorySearchDispatcher(hostName, mst.semantic, mst.episodic, memEmb))
		// add() registers enabled by default; no disable-at-launch (TEN-249).
	} else if log != nil {
		log.Debug("memory_search: store open failed; tool unavailable", "err", merr.Error())
	}

	if pf.web {
		sess, err := web.NewSession(webConfig(c.cfgDir, pf))
		if err != nil {
			return fail(fmt.Errorf("web plugin: %w (is Chrome installed?)", err))
		}
		mux.addCleanup(func() { _ = sess.Close() })
		mux.add("web", web.NewDispatcher(sess, web.Policy{AllowInteract: gateAllow(pf.webAllowInteract), Confirm: confirm},
			filepath.Join(c.dataDir, "screenshots")))
	}

	if pf.gsuite {
		gcfg := gsuite.Config{Auth: pf.gsuiteAuth, Subject: pf.gsuiteSubject, AllowSend: pf.gsuiteAllowSend}
		if pf.gsuiteAuth == "sa" {
			if pf.gsuiteSAJSON == "" {
				return fail(fmt.Errorf("gsuite plugin: --gsuite-auth sa needs --gsuite-sa-json"))
			}
			b, err := os.ReadFile(pf.gsuiteSAJSON)
			if err != nil {
				return fail(fmt.Errorf("gsuite plugin: read sa json: %w", err))
			}
			gcfg.SAJSON = b
		}
		svc, err := gsuite.Open(gcfg)
		if err != nil {
			return fail(fmt.Errorf("gsuite plugin: %w", err))
		}
		mux.add("gsuite", gsuite.NewDispatcher(svc, gsuite.Policy{AllowSend: gateAllow(pf.gsuiteAllowSend), Confirm: confirm}))
	}

	if pf.atlassian {
		svc, err := atlassian.Open(ctx, atlassian.Config{
			SiteURL:       pf.atlassianSite,
			Email:         pf.atlassianEmail,
			Project:       pf.atlassianProject,
			ClientID:      pf.atlassianClientID,
			TokenPath:     atlassianTokenPath(c.cfgDir),
			OAuthCallback: pf.atlassianCallback,
		})
		if err != nil {
			return fail(fmt.Errorf("atlassian plugin: %w", err))
		}
		mux.add("atlassian", atlassian.NewDispatcher(svc, atlassian.Policy{AllowWrite: gateAllow(pf.atlassianAllowWrite), Confirm: confirm}))
	}

	if pf.x {
		bearer := resolveXBearer(c.cfgDir, pf.xBearer)
		svc, err := xp.Open(xp.Config{Bearer: bearer, TokenPath: filepath.Join(c.dataDir, "x-token.json"), AllowPost: pf.xAllowPost})
		if err != nil {
			return fail(fmt.Errorf("x plugin: %w", err))
		}
		mux.add("x", xp.NewDispatcher(svc, xp.Policy{AllowPost: gateAllow(pf.xAllowPost), Confirm: confirm}))
	}

	if pf.imsg {
		url, pass := pf.imsgURL, pf.imsgPass
		if url == "" {
			url = os.Getenv("BLUEBUBBLES_URL")
		}
		if pass == "" {
			pass = os.Getenv("BLUEBUBBLES_PASSWORD")
		}
		pol := imessage.Policy{AllowSend: gateAllow(pf.imsgAllowSend), Confirm: confirm}
		// Transport selection: a BlueBubbles URL (flag or env) is an
		// explicit opt-in to the server bridge; otherwise the default is the
		// native macOS transport (chat.db read + AppleScript send, no
		// server). OpenNative returns a "macOS only" error off darwin.
		if url != "" {
			svc, err := imessage.Open(imessage.Config{URL: url, Password: pass, PrivateAPI: pf.imsgPrivate})
			if err != nil {
				return fail(fmt.Errorf("imessage plugin: %w", err))
			}
			mux.add("imessage", imessage.NewDispatcher(svc, pol))
		} else {
			nat, err := imessage.OpenNative(imessage.NativeConfig{})
			if err != nil {
				return fail(fmt.Errorf("imessage plugin: %w", err))
			}
			mux.addCleanup(func() { _ = nat.Close() })
			mux.add("imessage", imessage.NewDispatcher(nat, pol))
		}
	}

	if pf.discord {
		token := pf.discordToken
		if token == "" {
			token = os.Getenv("DISCORD_BOT_TOKEN")
		}
		svc, err := discord.Open(discord.Config{Token: token})
		if err != nil {
			return fail(fmt.Errorf("discord plugin: %w", err))
		}
		mux.add("discord", discord.NewDispatcher(svc, discord.Policy{AllowSend: gateAllow(pf.discordAllowSend), Confirm: confirm}))
	}

	// OS needs no config, so always register it — it's visible in
	// /tools and `/enable os` activates it instantly at runtime. Starts
	// enabled only if --os was passed (else registered-but-disabled, so
	// it costs nothing until you turn it on).
	if svc, err := osys.Open(osys.Config{}); err == nil {
		mux.add("os", osys.NewDispatcher(svc, osys.Policy{AllowExec: gateAllow(pf.osAllowExec), AllowWrite: gateAllow(pf.osAllowWrite), Confirm: confirm}))
		if !pf.osEnable {
			mux.SetEnabled("os", false)
		}
	}

	// Register stubs for every plugin that wasn't configured, so the
	// whole catalog shows in /tools. A stub advertises the real tool
	// specs (Tools() is static — safe with a nil service) but returns a
	// "needs setup" status if called. Registered disabled: visible,
	// zero context until /enable.
	catalog := []struct {
		label string
		specs []model.ToolSpec
		hint  string
		// activate, if set, builds the real plugin the first time it's
		// enabled (config-free plugins only). Otherwise the stub just tells
		// the operator to relaunch with the configuring flag.
		activate func() (plugin, func(), error)
	}{
		{label: "sql", specs: sqlp.NewDispatcher(nil, sqlp.Policy{}).Tools(), hint: "relaunch with --sql-db <file>"},
		{label: "wiki", specs: wiki.NewDispatcher(nil).Tools(), hint: "relaunch with --wiki-dir <dir>"},
		// Web is intentionally OMITTED from the shared catalog: it's
		// registered per-agent in team.go via addWebTool because each
		// agent gets its own Chrome session (concurrent navigation can't
		// share a tab). Leaving a stub here registered web tools TWICE
		// in the composite view — once via local (the real one) and once
		// via shared — which showed up as duplicate entries in /tools AND
		// broke /enable persistence (composite.SetEnabled hits local
		// first which had no setOnChange callback). Fix: don't register
		// web here at all; persistence is wired on the local mux below.
		{
			label: "gsuite",
			specs: gsuite.NewDispatcher(nil, gsuite.Policy{}).Tools(),
			hint:  "run `/configure gsuite` and pick a sign-in method (or relaunch with --gsuite gcloud/SA flags)",
			activate: func() (plugin, func(), error) {
				// Runtime activator: when the operator has configured
				// gsuite via /configure (saved in
				// launchConfig.Skills["gsuite"]), build the real
				// dispatcher on-demand. Supports all three auth modes:
				// sa (the business primary), gcloud (dev), and oauth
				// (advanced personal).
				lc, err := loadLaunchConfig(c.cfgDir)
				if err != nil {
					return nil, nil, fmt.Errorf("gsuite plugin: load config: %w", err)
				}
				sk, ok := lc.Skills["gsuite"]
				if !ok || sk.Settings == nil || sk.Settings["auth"] == "" {
					return nil, nil, fmt.Errorf("gsuite plugin: %w yet — run `/configure gsuite`", errNotConfigured)
				}
				auth := sk.Settings["auth"]
				gcfg := gsuite.Config{Auth: auth, AllowSend: pf.gsuiteAllowSend}
				switch auth {
				case "sa":
					saPath := sk.Settings["sa_json"]
					subject := sk.Settings["subject"]
					if saPath == "" || subject == "" {
						return nil, nil, fmt.Errorf("gsuite plugin: auth=sa needs sa_json + subject — re-run `/configure gsuite`")
					}
					b, err := os.ReadFile(saPath)
					if err != nil {
						return nil, nil, fmt.Errorf("gsuite plugin: read sa json: %w", err)
					}
					gcfg.SAJSON = b
					gcfg.Subject = subject
				case "gcloud":
					// nothing extra — Open() shells out to gcloud.
				case "oauth":
					credsPath := sk.Settings["oauth_creds_json"]
					if credsPath == "" {
						return nil, nil, fmt.Errorf("gsuite plugin: auth=oauth needs oauth_creds_json — re-run `/configure gsuite`")
					}
					oauthCredsBytes, err := os.ReadFile(credsPath)
					if err != nil {
						return nil, nil, fmt.Errorf("gsuite plugin: read oauth creds: %w", err)
					}
					gcfg.OAuthCreds = oauthCredsBytes
				default:
					// Reached when config.json carries an auth this build
					// doesn't implement (e.g. a stale "composio"). Make it
					// actionable rather than just "unknown".
					return nil, nil, fmt.Errorf("gsuite plugin: auth %q isn't supported in this build — run `/configure gsuite` and pick sa, gcloud, or oauth", auth)
				}
				svc, err := gsuite.Open(gcfg)
				if err != nil {
					return nil, nil, fmt.Errorf("gsuite plugin: %w", err)
				}
				return gsuite.NewDispatcher(svc, gsuite.Policy{
					AllowSend: gateAllow(pf.gsuiteAllowSend),
					Confirm:   confirm,
				}), nil, nil
			},
		},
		{
			label: "x",
			specs: xp.NewDispatcher(nil, xp.Policy{}).Tools(),
			hint:  "run `/configure x <bearer>`, then `/enable x` (or relaunch with --x)",
			activate: func() (plugin, func(), error) {
				// TEN-67: build the X plugin from the /configure-saved bearer
				// (or the launch flag / env). Same resolution as the launch
				// path via resolveXBearer so both pick the same token.
				bearer := resolveXBearer(c.cfgDir, pf.xBearer)
				if bearer == "" {
					return nil, nil, fmt.Errorf("x plugin: %w yet — run `/configure x <bearer>`", errNotConfigured)
				}
				svc, err := xp.Open(xp.Config{Bearer: bearer, TokenPath: filepath.Join(c.dataDir, "x-token.json"), AllowPost: pf.xAllowPost})
				if err != nil {
					return nil, nil, fmt.Errorf("x plugin: %w", err)
				}
				return xp.NewDispatcher(svc, xp.Policy{AllowPost: gateAllow(pf.xAllowPost), Confirm: confirm}), func() {}, nil
			},
		},
		{label: "imessage", specs: imessage.NewDispatcher(nil, imessage.Policy{}).Tools(), hint: "relaunch with --imessage (native chat.db on macOS; or --bb-url for BlueBubbles)"},
		{label: "discord", specs: discord.NewDispatcher(nil, discord.Policy{}).Tools(), hint: "relaunch with --discord --discord-bot-token=<token>"},
		{
			label: "atlassian",
			specs: atlassian.NewDispatcher(nil, atlassian.Policy{}).Tools(),
			hint:  "run `/configure atlassian` (OAuth sign-in or API token), then `/enable atlassian`",
			activate: func() (plugin, func(), error) {
				// TEN-160: build the real Jira plugin from /configure-saved settings.
				lc, err := loadLaunchConfig(c.cfgDir)
				if err != nil {
					return nil, nil, fmt.Errorf("atlassian plugin: load config: %w", err)
				}
				sk, ok := lc.Skills["atlassian"]
				if !ok || sk.Settings == nil || sk.Settings["site"] == "" {
					return nil, nil, fmt.Errorf("atlassian plugin: %w yet — run `/configure atlassian`", errNotConfigured)
				}
				creds, _ := loadCredentials(c.cfgDir)
				acfg := atlassian.Config{
					SiteURL:   sk.Settings["site"],
					Project:   sk.Settings["project"],
					TokenPath: atlassianTokenPath(c.cfgDir),
				}
				switch sk.Settings["auth"] {
				case "token":
					acfg.Email = sk.Settings["email"]
					acfg.APIToken = creds.get(skillSecretID("atlassian", "api_token"))
				default: // oauth
					acfg.ClientID = sk.Settings["client_id"]
					acfg.ClientSecret = creds.get(skillSecretID("atlassian", "client_secret"))
				}
				svc, err := atlassian.Open(ctx, acfg)
				if err != nil {
					return nil, nil, fmt.Errorf("atlassian plugin: %w", err)
				}
				return atlassian.NewDispatcher(svc, atlassian.Policy{AllowWrite: gateAllow(pf.atlassianAllowWrite), Confirm: confirm}), func() {}, nil
			},
		},
	}
	for _, e := range catalog {
		if mux.hasPlugin(e.label) {
			continue
		}
		mux.add(e.label, stubPlugin{specs: e.specs, hint: e.hint})
		mux.SetEnabled(e.label, false)
		if e.activate != nil {
			mux.registerActivator(e.label, e.activate)
		}
	}

	// Remote MCP connectors (TEN-162/164): stash the deps so runtime `/mcp add`
	// builds connectors identical (gated, deny-by-default) to launch-time ones,
	// then register one disabled stub + activator per server — from both
	// --mcp-remote flags AND the persisted launchConfig list. The activator
	// connects (OAuth2.1 + DCR + browser) on `/enable` / `/mcp add`.
	mux.mcpDeps = mcpRuntimeDeps{
		ctx:              ctx,
		callback:         pf.mcpCallback,
		openBrowser:      openBrowser,
		trustAnnotations: pf.mcpTrustAnnotations,
		allowWrite:       gateAllow(pf.mcpAllowWrite),
		confirm:          confirm,
		cacheDir:         filepath.Join(c.cfgDir, "mcp"),
		log:              log,
	}
	mcpURLs := append([]string{}, pf.mcpRemotes...)
	if c.lc != nil {
		mcpURLs = append(mcpURLs, c.lc.MCPRemotes...)
	}
	seenMCP := map[string]bool{}
	for _, url := range mcpURLs {
		if url == "" || seenMCP[mcpLabel(url)] {
			continue
		}
		seenMCP[mcpLabel(url)] = true
		mux.registerMCPRemote(url)
		// Auto-reconnect from a cached token (async — no browser, no launch
		// stall). With a valid/refreshable token the tools come live shortly
		// after launch; otherwise the disabled stub remains for /configure.
		go mux.reconnectMCPSilently(url)
	}
	// TEN-186: bring paired peers' shared knowledge tools live for the agent,
	// same silent/non-blocking pattern.
	mux.reconnectPeersSilently(c.cfgDir)

	return mux, wikiIx, mux.Close, nil
}

// mcpLabel derives a stable tool-group label from a remote MCP server URL,
// e.g. https://mcp.atlassian.com/v1/mcp → "mcp:atlassian.com".
func mcpLabel(rawURL string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(rawURL, "https://"), "http://")
	if i := strings.IndexAny(s, "/:"); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		s = rawURL
	}
	return "mcp:" + s
}

// buildWikiIndex constructs the wiki index for the TUI. Unlike the
// `tenant wiki` command (where a dead embedder is fatal — wiki is the
// whole point there), here it must NOT crash the TUI: `wiki.New` is
// offline-safe (validates the dir + loads the sidecar, no network), the
// embedder fingerprint falls back to the profile's declared dim if the
// probe fails, and the initial reindex is best-effort (wiki.Search
// auto-reindexes lazily on first use, so it self-heals when the
// embedder comes back). A search while the embedder is down surfaces a
// clean tool error in the feed instead of a startup crash.
func buildWikiIndex(ctx context.Context, c *commonFlags, router *model.Router, dir string, log *slog.Logger) (*wiki.Index, error) {
	emb, embProfile, err := router.EmbedderForRole(ctx, model.RoleEmbedder)
	if err != nil {
		return nil, fmt.Errorf("wiki plugin: resolve embedder: %w", err)
	}
	dim := embProfile.EmbedDim // declared; preferred = actual (probed) when reachable
	if probe, perr := emb.Embed(ctx, []string{"wiki embedder fingerprint probe"}); perr == nil && len(probe) == 1 && len(probe[0]) > 0 {
		dim = len(probe[0])
	} else {
		log.Warn("wiki: embedder probe failed; using declared dim, reindex deferred to first search",
			"declared_dim", dim, "err", perr)
	}
	embedID := embProfile.Model + "/" + strconv.Itoa(dim)
	absVault, _ := filepath.Abs(dir)
	h := fnv.New64a()
	_, _ = h.Write([]byte(absVault))
	sidecar := filepath.Join(c.dataDir, "wiki", fmt.Sprintf("%x.json", h.Sum64()))
	ix, err := wiki.New(dir, sidecar, embedID, emb)
	if err != nil {
		return nil, fmt.Errorf("wiki plugin: %w", err)
	}
	if _, _, rerr := ix.Reindex(ctx); rerr != nil {
		// Embedder down at launch — don't crash; lazy reindex on first search.
		log.Warn("wiki: initial reindex failed; will reindex on first search", "err", rerr)
	}
	return ix, nil
}
