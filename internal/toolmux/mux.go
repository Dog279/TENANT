// Package toolmux is the process's tool multiplexer (TEN-314): it merges the
// plugins' tools into one registry and dispatcher for the agent, enables and
// disables them at runtime, activates stubbed plugins on first use, ranks the
// catalog by relevance, federates knowledge searches to paired peers, adopts
// remote MCP connectors, and counts every dispatch. What to register, and
// how each plugin is built, is the composition root's business.
package toolmux

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"tenant/internal/agent"
	"tenant/internal/control"
	"tenant/internal/memory/cosine"
	"tenant/internal/model"
	"tenant/internal/peering"
	"tenant/internal/plugins/mcpremote"
)

// plugin is a dispatcher that also advertises its tool specs — every
// Tenant plugin satisfies this.
type Plugin interface {
	agent.ToolDispatcher
	Tools() []model.ToolSpec
}

// toolEntry is one tool in the mux: its spec, the plugin that owns it,
// a plugin label for group toggling, and whether it's currently active.
type toolEntry struct {
	spec    model.ToolSpec
	owner   agent.ToolDispatcher
	Plugin  string
	enabled bool
}

// Mux merges several plugins and is BOTH the agent's ToolRegistry
// and its ToolDispatcher. Tools can be enabled/disabled at runtime
// (via slash commands in the TUI): a disabled tool drops out of Search
// (so it leaves the prompt entirely — no context or compute) and Get
// (so it's uncallable). Thread-safe: the TUI toggles while the agent
// turn goroutine reads. Routing is by tool name (distinct per-plugin
// prefixes ⇒ no collision; first registrant wins on dup).
type Mux struct {
	mu     sync.RWMutex
	order  []string // tool names, registration order (deterministic Search)
	byName map[string]*toolEntry
	// activators turn a stubbed, config-free plugin into the real thing on
	// first /enable — e.g. web spawns Chrome at runtime instead of needing
	// --web at launch. Keyed by plugin label; runs once (see activated).
	activators map[string]func() (Plugin, func(), error)
	activated  map[string]bool
	cleanups   []func() // teardown for everything built (launch + lazy)
	// Remote MCP connectors (TEN-162/164): deps for building gated connectors
	// (set once in buildToolMux), and label→URL for every registered server so
	// /mcp add|list|remove can manage them at runtime.
	mcpDeps    MCPDeps
	mcpServers map[string]string
	mcpTrust   map[string]string // label → live trust mode (ask omitted); see SetMCPTrust
	// Federated knowledge search (TEN-243): paired peers' dispatchers are kept
	// here (name→dispatcher) instead of exposing their peer_*_search tools to the
	// agent. When the agent calls a local search (e.g. wiki_search) it fans out to
	// these and appends the results (trust-but-verify). baseDesc remembers each
	// federated tool's original description so the peer-awareness suffix can be
	// rebuilt as peers connect/disconnect.
	peerDisp map[string]Plugin
	baseDesc map[string]string
	// Federation drift metrics (TEN-243): per-peer fan-out outcome counters,
	// guarded by their own mutex so recording never contends with m.mu.
	fedMu    sync.Mutex
	fedStats map[string]*peerFedStat
	// onChange fires after any enable/disable, with a full name→enabled
	// snapshot, so the caller can persist the curation. Set AFTER restore
	// (restore must not re-trigger a save of what it just loaded).
	onChange func(map[string]bool)

	// stats, when set (enableStats), counts every dispatch outcome per tool.
	// nil = off (the eval harness stays uninstrumented on purpose - synthetic
	// task traffic must not drown the real usage signal).
	stats *Stats

	// savedStates is the operator's persisted enable/disable curation, captured
	// by restore(). Consulted by add() so tools that register LATE still land on
	// the operator's choice rather than the registration default: remote-MCP
	// plugins publish their tools from an activator that runs AFTER restore, so
	// restore skips them ("tool no longer registered") and, without this, every
	// bounce silently re-enabled tools the operator had turned off. Kept in sync
	// by SetEnabled so a newer toggle is never reverted by a later registration.
	savedStates map[string]bool

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
// Notion/Microsoft tooling will push catalog past 50). See the scar in the
// Search() comment below for why earlier "cap by registration order"
// was wrong — cosine ranking targets relevance, not registration race.
const RankActivateThreshold = 20

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
	// The Gmail READ path: gmail_read is the only way to see a message BODY —
	// gmail_search returns headers + snippet only. Brief/digest-shaped prompts
	// ("send me a daily brief covering my email") embed close to gmail_send and
	// gmail_draft, so on a 75-tool catalog with a 12-slot MaxToolsPerCall ceiling
	// the cosine rank surfaced the WRITE half of Gmail and trimmed the read half
	// — the agent then truthfully reported "I cannot read your Gmail (I only have
	// draft and send)" and answered from subject lines. Same scar as wiki_search,
	// same remedy: reading the user's own mail is a capability the operator opted
	// into, never something a per-turn relevance score should be able to hide.
	"gmail_search": true,
	"gmail_read":   true,
}

// SetOnChange installs the persistence hook. Call it after restore.
func (m *Mux) SetOnChange(fn func(map[string]bool)) {
	m.mu.Lock()
	m.onChange = fn
	m.mu.Unlock()
}

// enabledSnapshot captures every tool's current enabled state by name.
func (m *Mux) enabledSnapshot() map[string]bool {
	out := make(map[string]bool, len(m.order)+len(m.savedStates))
	// Seed from the persisted curation so a toggle that happens BEFORE a
	// late-registering plugin loads (remote MCP) doesn't drop that plugin's
	// tools out of the settings file entirely — losing the operator's choice
	// for them permanently. Live registry state overrides it below.
	for n, v := range m.savedStates {
		out[n] = v
	}
	for _, n := range m.order {
		out[n] = m.byName[n].enabled
	}
	return out
}

// ErrNotConfigured marks a lazy plugin that can't activate simply because
// the operator hasn't set it up yet — as opposed to a genuine failure (bad
// creds, a missing binary, an unsupported auth). restore() swallows it: an
// unconfigured optional plugin (e.g. gsuite before /configure) shouldn't
// print a "could not restore" line at every launch. Activators wrap it with
// %w so the interactive /enable path still shows a clean, actionable message
// while restore can detect it via errors.Is. Real failures still surface.
var ErrNotConfigured = errors.New("not configured")

// Restore applies a persisted name→enabled snapshot over the flag-derived
// defaults, so /enable + /disable survive restarts. Only tools that still
// exist are touched (stale names from removed plugins are ignored). An
// enabled activator-backed tool (e.g. web) is activated here — best
// effort: a failure (Chrome missing) is reported, not fatal. Returns
// human-readable notes for the launch feed.
func (m *Mux) Restore(saved map[string]bool) []string {
	if len(saved) == 0 {
		return nil
	}
	// Remember the curation BEFORE applying it: the loop below can only touch
	// tools that are already registered, and remote-MCP tools are not yet. add()
	// replays this map for them when their activator finally registers them.
	m.mu.Lock()
	m.savedStates = make(map[string]bool, len(saved))
	for n, v := range saved {
		m.savedStates[n] = v
	}
	m.mu.Unlock()
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
		if saved[n] && e.Plugin != "" && failedPlugins[e.Plugin] {
			continue // this plugin already failed to activate this restore
		}
		if _, _, err := m.SetEnabled(n, saved[n]); err != nil {
			if e.Plugin != "" {
				failedPlugins[e.Plugin] = true // dedupe regardless of error kind
			}
			// A plugin the operator simply hasn't configured yet is an
			// expected state, not a restore error — swallow it (no launch
			// nag). Genuine failures still report, deduped per plugin.
			if errors.Is(err, ErrNotConfigured) {
				continue
			}
			if e.Plugin != "" {
				notes = append(notes, fmt.Sprintf("could not restore %s tools: %v", e.Plugin, err))
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

func New() *Mux {
	return &Mux{
		byName:     map[string]*toolEntry{},
		peerDisp:   map[string]Plugin{},
		baseDesc:   map[string]string{},
		activators: map[string]func() (Plugin, func(), error){},
		activated:  map[string]bool{},
		mcpServers: map[string]string{},
		mcpTrust:   map[string]string{},
	}
}

// MCPDeps captures everything a remote-MCP activator needs, so the
// launch loop and runtime `/mcp add` build IDENTICAL (gated) connectors.
// Set once in buildToolMux (immutable after) — safe to read lock-free.
type MCPDeps struct {
	Ctx              context.Context
	Callback         string
	OpenBrowser      func(string) error
	TrustAnnotations bool
	AllowWrite       bool
	Confirm          func(context.Context, string, string) bool
	Rule             func(action string) string // per-tool permission rule lookup (approval.Rules); nil ⇒ none
	CacheDir         string                     // 0600 OAuth token cache dir (persistence across restarts)
	Log              *slog.Logger               // connect/refresh failure logging (TEN-166)
	Trust            map[string]string          // persisted per-connector trust by URL (config mcp_trust); applied on register
	// onAuthChange is the only mutable member: installed after the mux is built
	// (SetMCPAuthNotifier, under the mux lock) once the surfaces that can reach
	// the operator exist. Activators read it at FIRE time via fireMCPAuthChange,
	// never from a copy taken at registration.
	onAuthChange func(label, url string, needsReauth bool, err string)
}

// SetMCPDeps installs what every remote-MCP activator needs. Called once
// while the mux is built, before any connector registers; a notifier
// already installed with SetMCPAuthNotifier is kept.
func (m *Mux) SetMCPDeps(d MCPDeps) {
	m.mu.Lock()
	d.onAuthChange = m.mcpDeps.onAuthChange
	m.mcpDeps = d
	m.mu.Unlock()
}

// MCPConfirm is the approval gate remote connectors were built with, for the
// host tools that manage them (mcp_add, mcp_trust, …).
func (m *Mux) MCPConfirm() func(context.Context, string, string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.mcpDeps.Confirm
}

// ServerStatus is one remote MCP connector's runtime state (for /mcp list).
type ServerStatus struct {
	Label     string
	URL       string
	Enabled   bool
	ToolCount int
	Trust     string // ask | allow | deny — the connector's live trust mode
	// Auth health: a connector whose OAuth token died still lists its tools
	// (the count is frozen from connect time), which is exactly how a dead
	// Jira bridge looked healthy for three days. NeedsReauth is the truth.
	NeedsReauth bool
	AuthErr     string
	AuthSince   time.Time
}

// RegisterMCPRemote registers a disabled stub + activator for a remote MCP
// server using the mux's stored deps (idempotent). Activation — the browser
// OAuth flow — happens later on SetEnabled. Returns the derived label.
func (m *Mux) RegisterMCPRemote(url string) string {
	label := m.labelFor(url)
	if m.HasPlugin(label) {
		return label
	}
	d := m.mcpDeps
	m.Add(label, Stub{Hint: "`/enable " + label + "` connects to " + url + " and opens a browser to authorize"})
	m.SetEnabled(label, false)
	m.RegisterActivator(label, func() (Plugin, func(), error) {
		return mcpremote.Open(d.Ctx, mcpremote.Config{
			ServerURL:    url,
			Label:        label,
			CallbackAddr: d.Callback,
			OpenBrowser:  d.OpenBrowser,
			CacheDir:     d.CacheDir,
			Interactive:  true, // /enable or /mcp add may sign in via browser (cached token ⇒ silent)
			Logger:       d.Log,
			OnAuthChange: func(needs bool, err string) { m.fireMCPAuthChange(label, url, needs, err) },
		}, d.TrustAnnotations, mcpremote.Policy{
			AllowWrite: d.AllowWrite,
			Confirm:    d.Confirm,
			Rule:       d.Rule,
		})
	})
	m.mu.Lock()
	m.mcpServers[label] = url
	if t, ok := mcpremote.ParseTrust(d.Trust[url]); ok && t != mcpremote.TrustAsk {
		m.mcpTrust[label] = t // the operator's persisted decision, re-applied on activation
	}
	m.mu.Unlock()
	return label
}

// StdioOpener starts a local MCP server and connects to it (TEN-318). The
// command layer supplies it, since the mux knows no sandbox; it receives the
// approval policy every connector is built with.
type StdioOpener func(ctx context.Context, trustAnnotations bool, policy mcpremote.Policy) (Plugin, func(), error)

// RegisterMCPStdio registers a local MCP server as connector mcp:<name>: a
// stub until /enable, when open starts it. "stdio:<name>" stands in for its
// URL wherever connectors are keyed (persisted trust, the connector list). A
// label another connector or plugin holds is refused.
func (m *Mux) RegisterMCPStdio(name, hint string, open StdioOpener) (string, error) {
	label, key := "mcp:"+name, "stdio:"+name
	m.mu.RLock()
	holder, taken := m.mcpServers[label]
	m.mu.RUnlock()
	if taken {
		if holder == key {
			return label, nil
		}
		return "", fmt.Errorf("toolmux: connector label %s is already %s", label, holder)
	}
	if m.HasPlugin(label) {
		return "", fmt.Errorf("toolmux: %s is already a plugin", label)
	}
	d := m.mcpDeps
	m.Add(label, Stub{Hint: hint})
	m.SetEnabled(label, false)
	m.RegisterActivator(label, func() (Plugin, func(), error) {
		return open(d.Ctx, d.TrustAnnotations, mcpremote.Policy{AllowWrite: d.AllowWrite, Confirm: d.Confirm, Rule: d.Rule})
	})
	m.mu.Lock()
	m.mcpServers[label] = key
	if t, ok := mcpremote.ParseTrust(d.Trust[key]); ok && t != mcpremote.TrustAsk {
		m.mcpTrust[label] = t
	}
	m.mu.Unlock()
	return label, nil
}

// StartMCPStdioSilently starts a registered local server at launch and brings
// its tools live the way a reconnected remote connector's come (adoptLiveMCP):
// enabled, except those the operator disabled, and hidden for a denied
// connector. A failure is logged and leaves the stub for /enable. Safe to run
// in a goroutine.
func (m *Mux) StartMCPStdioSilently(label string, open StdioOpener) {
	d := m.mcpDeps
	disp, cleanup, err := open(d.Ctx, d.TrustAnnotations, mcpremote.Policy{AllowWrite: d.AllowWrite, Confirm: d.Confirm, Rule: d.Rule})
	if err != nil {
		if d.Log != nil {
			d.Log.Warn("mcp: local server did not start", "connector", label, "err", err.Error())
		}
		return
	}
	m.adoptLiveMCP(label, disp, cleanup)
}

// labelFor derives a connector's mux label from its URL, keeping the label of
// an already-registered URL stable and never letting two different URLs share
// one label: the base is "mcp:<host[:port]>" (the port matters — an operator
// or the agent may run several local tool servers on 127.0.0.1); if that base
// already names a DIFFERENT URL, the path is appended ("mcp:127.0.0.1:9000/b").
func (m *Mux) labelFor(url string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for label, u := range m.mcpServers {
		if u == url {
			return label
		}
	}
	base := MCPLabel(url)
	if u, taken := m.mcpServers[base]; !taken || u == url {
		return base
	}
	s := strings.TrimPrefix(strings.TrimPrefix(url, "https://"), "http://")
	path := ""
	if i := strings.IndexByte(s, '/'); i >= 0 {
		path = strings.TrimRight(s[i:], "/")
	}
	label := base + path
	for n := 2; ; n++ {
		if u, taken := m.mcpServers[label]; !taken || u == url {
			return label
		}
		label = fmt.Sprintf("%s%s#%d", base, path, n)
	}
}

// MCPTrust reports a connector's live trust mode ("ask" when unset).
func (m *Mux) MCPTrust(label string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if t := m.mcpTrust[label]; t != "" {
		return t
	}
	return mcpremote.TrustAsk
}

// SetMCPTrust changes a connector's trust mode LIVE: the mode is pushed into
// the running dispatcher (no reconnect), a "deny" also hides the tools from the
// agent, and leaving "deny" shows them again. Unknown label ⇒ error.
func (m *Mux) SetMCPTrust(label, mode string) error {
	t, ok := mcpremote.ParseTrust(mode)
	if !ok {
		return fmt.Errorf("trust must be ask | allow | deny")
	}
	m.mu.Lock()
	if _, known := m.mcpServers[label]; !known {
		m.mu.Unlock()
		return fmt.Errorf("no remote MCP server labeled %q", label)
	}
	if t == mcpremote.TrustAsk {
		delete(m.mcpTrust, label)
	} else {
		m.mcpTrust[label] = t
	}
	m.applyTrustLocked(label)
	activated := m.activated[label]
	m.mu.Unlock()
	if activated {
		// Hide a denied connector's tools; show them again when trust returns.
		// SetEnabled(true) is a no-op activation-wise for an already-live plugin.
		_, _, _ = m.SetEnabled(label, t != mcpremote.TrustDeny)
	}
	return nil
}

// applyTrustLocked pushes the connector's trust mode into its live dispatcher
// (whatever currently owns its tools). Caller holds m.mu.
func (m *Mux) applyTrustLocked(label string) {
	mode := m.mcpTrust[label]
	if mode == "" {
		mode = mcpremote.TrustAsk
	}
	// No dedupe by owner: a dispatcher may be stored by value (its dynamic type
	// may not be hashable), and SetTrust is idempotent and cheap.
	for _, n := range m.order {
		e := m.byName[n]
		if e == nil || e.Plugin != label {
			continue
		}
		if tp, ok := e.owner.(interface{ SetTrust(string) }); ok {
			tp.SetTrust(mode)
		}
	}
}

// ReconnectMCPSilently attempts a NON-interactive reconnect (cached token, no
// browser) at launch and, on success, brings the server's tools live + enabled.
// A missing/expired token fails cleanly and leaves the disabled stub in place
// (the interactive activator can sign in later). Safe to run in a goroutine.
func (m *Mux) ReconnectMCPSilently(url string) {
	label := m.labelFor(url)
	d := m.mcpDeps
	// Bound the launch dial so a hung/slow server can't park this goroutine for
	// the whole session. The SDK runs the session on its own background context
	// (not this one), so the timeout caps only the initialize handshake — a
	// connected session survives after Open returns.
	dialCtx, cancel := context.WithTimeout(d.Ctx, 30*time.Second)
	defer cancel()
	disp, cleanup, err := mcpremote.Open(dialCtx, mcpremote.Config{
		ServerURL:    url,
		Label:        label,
		CallbackAddr: d.Callback,
		CacheDir:     d.CacheDir,
		Interactive:  false, // launch: never pop a browser
		Logger:       d.Log,
		OnAuthChange: func(needs bool, err string) { m.fireMCPAuthChange(label, url, needs, err) },
	}, d.TrustAnnotations, mcpremote.Policy{
		AllowWrite: d.AllowWrite,
		Confirm:    d.Confirm,
		Rule:       d.Rule,
	})
	if err != nil {
		// Previously swallowed — that's why "check the logs" showed nothing for a
		// broken MCP server (TEN-166). errNoCachedSession (no token yet) is the
		// normal first-run case → debug; anything else is a real failure → warn.
		if d.Log != nil {
			if strings.Contains(err.Error(), "no usable cached session") {
				d.Log.Debug("mcp: silent reconnect skipped (no cached session)", "server", url)
			} else {
				d.Log.Warn("mcp: silent reconnect failed", "server", url, "err", err.Error())
			}
		}
		return // stays a disabled stub; interactive activator can sign in later
	}
	m.adoptLiveMCP(label, disp, cleanup)
}

// adoptLiveMCP merges an already-connected remote plugin's tools into the mux
// ENABLED (first-registrant-wins, so it can't shadow a local tool) and marks the
// label activated so the interactive activator won't double-run.
func (m *Mux) adoptLiveMCP(label string, p Plugin, cleanup func()) {
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
		// Adopt ENABLED by default, but never over the operator's restored
		// curation: a remote tool they disabled last session must stay off. This
		// has to happen here because restore() runs before the reconnect
		// goroutine registers these names, so restore can only skip them — which
		// silently re-enabled every disabled remote-MCP tool on each bounce.
		enabled := true
		if v, ok := m.savedStates[spec.Name]; ok {
			enabled = v
		}
		if m.mcpTrust[label] == mcpremote.TrustDeny {
			enabled = false // a denied connector reconnects hidden
		}
		m.byName[spec.Name] = &toolEntry{spec: spec, owner: p, Plugin: label, enabled: enabled}
		added = true
	}
	m.applyTrustLocked(label)
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

// ReconnectPeersSilently dials every paired peer we hold a token for (TEN-186)
// and brings its shared knowledge tools (peer_wiki_search / peer_memory_search)
// live for the agent — namespaced "peer:<name>". Same non-blocking, no-prompt
// pattern as reconnectMCPSilently: a peer that's offline or whose token was
// revoked just stays absent (logged), never stalling launch. Single-peer is the
// trial target; with multiple peers the un-prefixed tool names collide and
// first-registrant-wins (adoptLiveMCP), so a 2nd peer's identically-named tools
// are shadowed — acceptable for the trial, namespaced fully in a follow-up.
func (m *Mux) ReconnectPeersSilently(cfgDir string) {
	store, err := peering.LoadStore(cfgDir)
	if err != nil {
		// An unreadable peers.json turns federation off: say so.
		if m.mcpDeps.Log != nil {
			m.mcpDeps.Log.Warn("peer: peers.json unreadable; no peer is reconnected", "err", err.Error())
		}
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
			dialCtx, cancel := context.WithTimeout(d.Ctx, 30*time.Second)
			defer cancel()
			disp, cleanup, derr := mcpremote.OpenStatic(dialCtx, mcpremote.StaticConfig{
				ServerURL:   p.URL,
				Token:       p.Token,
				Label:       label,
				TLS:         peering.PinnedTLSClientConfig(p.Fingerprint), // nil ⇒ plain HTTP (overlay)
				UngateTools: PeerFederationTools,                          // TEN-252: only the known federation tools ungate
			}, mcpremote.Policy{Confirm: d.Confirm, Rule: d.Rule})
			if derr != nil {
				if d.Log != nil {
					d.Log.Warn("peer: silent reconnect failed", "peer", p.Name, "err", derr.Error())
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
func (m *Mux) AddRemoteMCP(url string) (label string, toolCount int, err error) {
	label = m.labelFor(url)
	already := m.HasPlugin(label)
	m.RegisterMCPRemote(url)
	n, _, aerr := m.SetEnabled(label, true) // triggers maybeActivate → browser only if the server asks for a sign-in
	if aerr != nil {
		if !already {
			m.ForgetPlugin(label)
		}
		return label, 0, aerr
	}
	return label, n, nil
}

// ForgetPlugin removes a plugin's tools + activator from the mux entirely (the
// inverse of add). Used by `/mcp remove` and failed-add cleanup.
func (m *Mux) ForgetPlugin(label string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.order[:0]
	for _, n := range m.order {
		if e := m.byName[n]; e != nil && e.Plugin == label {
			delete(m.byName, n)
			continue
		}
		kept = append(kept, n)
	}
	m.order = kept
	delete(m.activators, label)
	delete(m.activated, label)
	delete(m.mcpServers, label)
	delete(m.mcpTrust, label)
}

// RemoteMCPList reports every registered remote MCP connector + its state.
func (m *Mux) RemoteMCPList() []ServerStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ServerStatus, 0, len(m.mcpServers))
	for label, url := range m.mcpServers {
		st := ServerStatus{Label: label, URL: url, Trust: m.mcpTrust[label]}
		if st.Trust == "" {
			st.Trust = mcpremote.TrustAsk
		}
		for _, n := range m.order {
			if e := m.byName[n]; e != nil && e.Plugin == label {
				st.ToolCount++
				if e.enabled {
					st.Enabled = true
				}
				if !st.NeedsReauth {
					if hp, ok := e.owner.(interface{ AuthHealth() mcpremote.Health }); ok {
						if h := hp.AuthHealth(); h.NeedsReauth {
							st.NeedsReauth, st.AuthErr, st.AuthSince = true, h.Err, h.Since
						}
					}
				}
			}
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// RemoteMCPAuthProblems lists the connectors whose OAuth token is dead: their
// tools fail (loudly) until the operator reconnects them at the host.
func (m *Mux) RemoteMCPAuthProblems() []ServerStatus {
	var out []ServerStatus
	for _, s := range m.RemoteMCPList() {
		if s.NeedsReauth {
			out = append(out, s)
		}
	}
	return out
}

// SetMCPAuthNotifier installs the callback fired when a connector flips into
// or out of the needs-re-auth state (feed line + operator DM). Installed after
// the relay exists; read at fire time, so already-registered connectors see it.
func (m *Mux) SetMCPAuthNotifier(fn func(label, url string, needsReauth bool, err string)) {
	m.mu.Lock()
	m.mcpDeps.onAuthChange = fn
	m.mu.Unlock()
}

// fireMCPAuthChange is what every remote connector's OnAuthChange points at.
// It logs with the fix spelled out (the log line is the persistent audit) and
// forwards to the installed notifier, if any.
func (m *Mux) fireMCPAuthChange(label, url string, needsReauth bool, err string) {
	m.mu.RLock()
	fn := m.mcpDeps.onAuthChange
	log := m.mcpDeps.Log
	m.mu.RUnlock()
	if log != nil {
		if needsReauth {
			log.Warn("mcp: connector needs re-authorization — its tools fail until reconnected",
				"server", label, "url", url, "err", err, "fix", "tenant mcp connect "+url)
		} else {
			log.Info("mcp: connector re-authorized", "server", label)
		}
	}
	if fn != nil {
		fn(label, url, needsReauth, err)
	}
}

// AddCleanup registers a teardown func (closes a browser/db handle), run
// in reverse order by Close.
func (m *Mux) AddCleanup(fn func()) {
	if fn == nil {
		return
	}
	m.mu.Lock()
	m.cleanups = append(m.cleanups, fn)
	m.mu.Unlock()
}

// Close tears down every plugin the mux built (launch-time and lazily
// activated), in reverse order. Safe to call once at shutdown.
func (m *Mux) Close() {
	m.mu.Lock()
	cs := m.cleanups
	m.cleanups = nil
	m.mu.Unlock()
	for i := len(cs) - 1; i >= 0; i-- {
		cs[i]()
	}
}

// RegisterActivator records how to build a config-free plugin on demand.
// Only registered for stubbed plugins that need no operator input (web).
func (m *Mux) RegisterActivator(label string, fn func() (Plugin, func(), error)) {
	m.mu.Lock()
	m.activators[label] = fn
	m.mu.Unlock()
}

// Add registers a plugin's tools under a label (used for group toggling
// like `/disable os`). Tools start enabled. Invalidates the precomputed
// tool embeddings (if any) so the next Search re-precomputes — necessary
// because activators install plugins LATE (e.g. `/enable web` builds
// Chrome long after construction), and a stale embedding cache would
// silently rank the new tool at sim=0.
func (m *Mux) Add(label string, p Plugin) {
	m.mu.Lock()
	defer m.mu.Unlock()
	added := false
	for _, sp := range p.Tools() {
		if _, dup := m.byName[sp.Name]; dup {
			continue
		}
		// Tools start enabled UNLESS the operator's restored curation says
		// otherwise — the late-registration path (remote MCP) depends on this.
		enabled := true
		if v, ok := m.savedStates[sp.Name]; ok {
			enabled = v
		}
		m.byName[sp.Name] = &toolEntry{spec: sp, owner: p, Plugin: label, enabled: enabled}
		m.order = append(m.order, sp.Name)
		added = true
	}
	if added {
		// Cache stale — next Search re-precomputes (best-effort).
		m.toolEmbeddings = nil
	}
}

// Owner returns the dispatcher that serves the named tool, enabled or not.
func (m *Mux) Owner(name string) (agent.ToolDispatcher, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.byName[name]
	if !ok {
		return nil, false
	}
	return e.owner, true
}

// --- agent.ToolRegistry (enabled tools only) ---

func (m *Mux) Get(name string) (model.ToolSpec, bool) {
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
func (m *Mux) Search(ctx context.Context, queryEmb []float32, maxTools int) ([]model.ToolSpec, error) {
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
	case catalogSize < RankActivateThreshold:
		return fallback(fmt.Sprintf("catalog below ranking threshold (%d<%d tools)", catalogSize, RankActivateThreshold))
	case queryEmb == nil:
		return fallback("no query embedding this turn (retrieval degraded — check the embedder)")
	case !haveEmbedder:
		return fallback("no embedder installed for tool ranking on this agent")
	}
	if !haveCache {
		// Slow path: precompute once for this embedder. Best-effort —
		// failure leaves cache empty, we fall back to unranked below.
		m.PrecomputeEmbeddings(ctx)
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
func (m *Mux) cachedEmbDim() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, v := range m.toolEmbeddings {
		return len(v)
	}
	return 0
}

// recordRank stores the most recent Search's ranking diagnostic (TEN-225).
func (m *Mux) recordRank(ranked bool, surfaced, catalog int, reason string) {
	m.mu.Lock()
	m.lastRank = rankStatus{ranked: ranked, surfaced: surfaced, catalog: catalog, reason: reason}
	m.mu.Unlock()
}

// RankingStatus reports what the most recent Search did and why (TEN-225) —
// read by the agent loop to emit a per-turn tool-catalog diagnostic. ok is
// false until the first Search of the session.
func (m *Mux) RankingStatus() (ranked bool, surfaced, catalog int, reason string, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := m.lastRank
	return s.ranked, s.surfaced, s.catalog, s.reason, s.catalog > 0 || s.reason != ""
}

// enabledCount is the number of currently-enabled tools (the ranking denominator).
func (m *Mux) enabledCount() int {
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
func (m *Mux) searchAll() []model.ToolSpec {
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
// Add().
func (m *Mux) searchRanked(queryEmb []float32, maxTools int) []model.ToolSpec {
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
			sim = cosine.Similarity(queryEmb, emb)
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
func (m *Mux) SetEmbedder(fingerprint string, e model.Embedder) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if fingerprint != m.embedderFingerprint || e == nil {
		m.toolEmbeddings = nil
	}
	m.embedderFingerprint = fingerprint
	m.embedder = e
}

// PrecomputeEmbeddings batch-embeds every enabled tool's description.
// Lazy: called from the Search slow path the first time ranking is
// active for a given embedder. Best-effort: an error or a partial result
// leaves m.toolEmbeddings empty so Search degrades to the unranked path.
// Lock discipline: snapshot the inputs under RLock, do the network call
// without any lock held, then write the cache under Lock.
func (m *Mux) PrecomputeEmbeddings(ctx context.Context) {
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

func (m *Mux) All() []model.ToolSpec {
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

func (m *Mux) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	m.mu.RLock()
	e, ok := m.byName[call.Name]
	// Snapshot the federation peers for this call (nil unless the tool is
	// federatable AND peers are connected) while we hold the lock; the fan-out
	// itself runs lock-free since it does network I/O. (TEN-243)
	counterpart, peers := m.peersForFederation(call.Name)
	st := m.stats // snapshot under the RLock; enableStats writes under Lock
	readCounterpart, readPeers := m.peersForReadFallback(call.Name)
	m.mu.RUnlock()
	if !ok {
		return "unknown tool: " + call.Name, true, nil
	}
	if !e.enabled {
		st.record(call.Name, statDisabled)
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
			// A peer satisfied the read: still a successful call of this tool
			// from the agent's point of view - count it, or federated tools
			// look dead in the usage stats.
			st.record(call.Name, statOK)
			return peerOut, false, nil
		}
	}
	outcome := classifyDispatch(out, isErr, err)
	st.record(call.Name, outcome)
	if outcome != statOK {
		msg := out
		if err != nil {
			msg = err.Error()
		}
		st.noteError(call.Name, msg)
	}
	return out, isErr, err
}

// EnableStats turns on per-tool dispatch counting for this mux. Muxes may
// SHARE one *toolStats (serve attaches the same set to the shared and the
// main-agent local mux) so one role file carries every tool the process
// dispatches.
func (m *Mux) EnableStats(s *Stats) {
	m.mu.Lock()
	m.stats = s
	m.mu.Unlock()
}

// --- tui.ToolControl (runtime enable/disable from slash commands) ---

func (m *Mux) ToolList() []control.ToolInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]control.ToolInfo, 0, len(m.order))
	for _, n := range m.order {
		e := m.byName[n]
		out = append(out, control.ToolInfo{Name: n, Plugin: e.Plugin, Enabled: e.enabled, Gated: e.spec.Gated})
	}
	return out
}

func (m *Mux) HasPlugin(label string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, e := range m.byName {
		if e.Plugin == label {
			return true
		}
	}
	return false
}

// Stub advertises a plugin's real tool specs but isn't configured
// — calling any of its tools returns a "needs setup" status (like a 401)
// instead of failing to load. This is what makes the framework run even
// when a plugin is unauthenticated, and lets every plugin appear in
// /tools so you know it exists.
type Stub struct {
	Specs []model.ToolSpec
	Hint  string
}

func (s Stub) Tools() []model.ToolSpec { return s.Specs }
func (s Stub) Dispatch(context.Context, model.ToolCall) (string, bool, error) {
	return "this plugin is not configured — " + s.Hint, true, nil
}

// SetPluginEnabled is the explicit categorical toggle: forces a plugin-
// label sweep and never matches a single tool name. Used by the TUI's
// `/enable skill <label>` form to give the user a clean "no skill named
// X" error when they typo, instead of falling through to the smart-
// match path. Same activation + notify-onChange contract as SetEnabled.
func (m *Mux) SetPluginEnabled(label string, on bool) (int, string, error) {
	if on {
		if err := m.maybeActivate(label); err != nil {
			return 0, "", err
		}
	}
	m.mu.Lock()
	n := 0
	for _, e := range m.byName {
		if e.Plugin == label {
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
func (m *Mux) Plugins() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	seen := map[string]bool{}
	out := make([]string, 0, 8)
	for _, e := range m.byName {
		if e.Plugin == "" || seen[e.Plugin] {
			continue
		}
		seen[e.Plugin] = true
		out = append(out, e.Plugin)
	}
	sort.Strings(out)
	return out
}

// SetEnabled toggles a single tool (exact name) or a whole plugin (by
// label). Returns how many tools changed and the scope ("tool" or
// "plugin"); (0, "", nil) if nothing matched. Enabling a plugin that's
// only a stub lazily activates it (e.g. launches Chrome for web) — a
// build failure is returned as the error and nothing is toggled.
func (m *Mux) SetEnabled(target string, on bool) (int, string, error) {
	if on {
		if err := m.maybeActivate(target); err != nil {
			return 0, "", err
		}
	}
	m.mu.Lock()
	n, scope := 0, ""
	if m.savedStates == nil {
		m.savedStates = map[string]bool{}
	}
	if e, ok := m.byName[target]; ok {
		e.enabled = on
		m.savedStates[target] = on
		n, scope = 1, "tool"
	} else {
		for name, e := range m.byName {
			if e.Plugin == target {
				e.enabled = on
				m.savedStates[name] = on
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
func (m *Mux) maybeActivate(target string) error {
	m.mu.Lock()
	label := target
	if e, ok := m.byName[target]; ok {
		label = e.Plugin
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
		if e := m.byName[n]; e.Plugin == label {
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
		m.byName[spec.Name] = &toolEntry{spec: spec, owner: p, Plugin: label, enabled: false}
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
	m.applyTrustLocked(label) // a remote connector picks up its persisted trust mode
	if cleanup != nil {
		m.cleanups = append(m.cleanups, cleanup)
	}
	m.activated[label] = true
	return nil
}

// mcpLabel derives a stable tool-group label from a remote MCP server URL,
// e.g. https://mcp.atlassian.com/v1/mcp → "mcp:atlassian.com".
// mcpLabel derives the base label "mcp:<host[:port]>". The port is kept so
// several tool servers on one host (127.0.0.1:9000, 127.0.0.1:9001 — the
// agent's own creations) get distinct labels; a hosted service's URL has no
// port, so its label is unchanged ("mcp:mcp.atlassian.com"). labelFor adds a
// path suffix when two URLs still collide.
func MCPLabel(rawURL string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(rawURL, "https://"), "http://")
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		s = rawURL
	}
	return "mcp:" + s
}
