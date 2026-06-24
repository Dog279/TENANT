package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tenant/internal/agent"
	"tenant/internal/dashboard"
	"tenant/internal/tui"
)

// approvalRegistry is the id-keyed source of truth for pending dangerous-action
// approvals (TEN-203). It is SHARED across the brokers that front the same
// operator (the host broker + the iMessage offsite broker share one), so any
// observer — the interactive TUI and the web dashboard — sees every pending
// request and can resolve it by id. First-resolver-wins via delete-under-lock;
// the per-request reply chan (buffered cap 1) is ALWAYS sent OUTSIDE the lock so
// the lock is never held across a channel op.
type approvalRegistry struct {
	mu      sync.Mutex
	pending map[string]*pendingReq
	seq     atomic.Uint64
	// emit publishes pending/resolved events to the live event stream so both
	// observers refresh. Set once at wiring (nil ⇒ no event surfacing — fail-safe).
	emit func(agent.Event)
}

type pendingReq struct {
	id     string
	req    tui.ApprovalRequest
	origin string
	at     time.Time
	reply  chan tui.ApprovalDecision
}

func newApprovalRegistry() *approvalRegistry {
	return &approvalRegistry{pending: map[string]*pendingReq{}}
}

// register inserts a new pending request and emits EventApprovalPending. Returns
// the assigned id + the buffered reply chan the caller blocks on.
func (r *approvalRegistry) register(req tui.ApprovalRequest, origin string) (string, chan tui.ApprovalDecision) {
	id := fmt.Sprintf("ap-%d", r.seq.Add(1)) // atomic — no lock needed for the id
	reply := make(chan tui.ApprovalDecision, 1)
	req.ID = id
	req.Reply = reply
	p := &pendingReq{id: id, req: req, origin: origin, at: time.Now(), reply: reply}
	r.mu.Lock()
	r.pending[id] = p
	r.mu.Unlock()
	r.emitEvent(agent.EventApprovalPending, p, "")
	return id, reply
}

// Resolve fires the decision for id — first-resolver-wins. Returns false (clean
// no-op) for an unknown / already-resolved id, so the loser of a TUI/dashboard
// race is harmless. The reply send is buffered + outside the lock.
func (r *approvalRegistry) Resolve(id string, d tui.ApprovalDecision) bool {
	r.mu.Lock()
	p := r.pending[id]
	if p != nil {
		delete(r.pending, id)
	}
	r.mu.Unlock()
	if p == nil {
		return false
	}
	p.reply <- d // cap-1 buffer ⇒ never blocks, fires exactly once (delete-under-lock gated)
	r.emitEvent(agent.EventApprovalResolved, p, outcomeOf(d))
	return true
}

// removeIfPending drops id (if still pending) without an operator decision — used
// when the requesting turn's ctx is cancelled, so Pending() stays honest and a
// late dashboard Resolve is a clean no-op. Emits Resolved(expired).
func (r *approvalRegistry) removeIfPending(id string) {
	r.mu.Lock()
	p := r.pending[id]
	if p != nil {
		delete(r.pending, id)
	}
	r.mu.Unlock()
	if p != nil {
		r.emitEvent(agent.EventApprovalResolved, p, "expired")
	}
}

// DenyAll denies every still-pending request (process/serve teardown), so no
// agent goroutine stays blocked. Buffered, non-blocking sends.
func (r *approvalRegistry) DenyAll() {
	r.mu.Lock()
	pend := r.pending
	r.pending = map[string]*pendingReq{}
	r.mu.Unlock()
	for _, p := range pend {
		select {
		case p.reply <- tui.DenyOnce:
		default:
		}
		r.emitEvent(agent.EventApprovalResolved, p, "denied")
	}
}

// Pending is the snapshot the dashboard / TUI read. Never nil (stable REST shape).
func (r *approvalRegistry) Pending() []dashboard.PendingApproval {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]dashboard.PendingApproval, 0, len(r.pending))
	now := time.Now()
	for _, p := range r.pending {
		out = append(out, dashboard.PendingApproval{
			ID:       p.id,
			Category: p.req.Category,
			Action:   p.req.Action,
			Detail:   p.req.Detail,
			AgeSecs:  int(now.Sub(p.at).Seconds()),
		})
	}
	return out
}

func (r *approvalRegistry) emitEvent(kind agent.EventKind, p *pendingReq, outcome string) {
	if r.emit == nil {
		return
	}
	r.emit(agent.Event{
		Kind: kind,
		Text: fmt.Sprintf("%s: %s", p.req.Action, p.req.Detail),
		Approval: &agent.ApprovalEvent{
			ID: p.id, Category: p.req.Category, Action: p.req.Action,
			Detail: p.req.Detail, Origin: p.origin, Outcome: outcome,
		},
	})
}

// emitBeside surfaces a pending/resolved event for a request handled by a
// REPLACEMENT backend (the Discord button approver) that is NOT in the
// resolvable registry — observers SEE it (Origin=discord) but can't resolve it
// from the dashboard, because that path owns its own resolution (see askDecision).
func (r *approvalRegistry) emitBeside(kind agent.EventKind, req tui.ApprovalRequest, origin, outcome string) {
	if r == nil || r.emit == nil {
		return
	}
	r.emit(agent.Event{
		Kind: kind,
		Text: fmt.Sprintf("%s: %s", req.Action, req.Detail),
		Approval: &agent.ApprovalEvent{
			ID: "beside-" + origin, Category: req.Category, Action: req.Action,
			Detail: req.Detail, Origin: origin, Outcome: outcome,
		},
	})
}

func outcomeOf(d tui.ApprovalDecision) string {
	switch d {
	case tui.ApproveOnce:
		return "approved"
	case tui.ApproveSession:
		return "approved_session"
	case tui.ApproveAlways:
		return "approved_always"
	default:
		return "denied"
	}
}

// Safety categories — the granular knobs the operator controls. Every
// plugin's dangerous action maps to exactly one of these (see categorize).
const (
	catExec        = "exec"        // run shell/python/code
	catWrite       = "write"       // create/modify files on disk
	catDestructive = "destructive" // irreversible: rm -rf, format, DROP, web purchase/delete
	catWeb         = "web"         // act on the web unprompted: click/fill/select
	catSend        = "send"        // outbound comms: email / iMessage / X
)

var catOrder = []string{catExec, catWrite, catDestructive, catWeb, catSend}

var catDesc = map[string]string{
	catExec:        "run shell / python / code (os_exec)",
	catWrite:       "create / modify files (os_write/edit/append/mkdir)",
	catDestructive: "irreversible: rm -rf, format, DROP/ALTER, web purchase/delete",
	catWeb:         "act on the web unprompted: click, fill, select",
	catSend:        "send outbound comms: email, iMessage, X posts",
}

// categorize maps a plugin action id to a safety category. Unknown
// actions fall through to destructive (the safest default — always ask).
func categorize(action string) string {
	switch action {
	case "os_exec":
		return catExec
	case "os_write", "os_append", "os_edit", "os_mkdir":
		return catWrite
	case "os_exec_dangerous", "sql_ddl", "web_transact":
		return catDestructive
	case "gsuite_send", "imessage_send", "x_post":
		return catSend
	}
	if strings.HasPrefix(action, "web_") {
		return catWeb
	}
	return catDestructive
}

type permMode int

const (
	modeAsk   permMode = iota // prompt the user (default — safe)
	modeAllow                 // always allow, no prompt
	modeDeny                  // always block
)

func (m permMode) String() string {
	switch m {
	case modeAllow:
		return "allow"
	case modeDeny:
		return "deny"
	default:
		return "ask"
	}
}

func parseMode(s string) (permMode, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "ask", "prompt", "confirm":
		return modeAsk, true
	case "allow", "on", "always", "yes":
		return modeAllow, true
	case "deny", "off", "block", "no":
		return modeDeny, true
	}
	return modeAsk, false
}

// approvalBroker is the single decision point for every dangerous action.
// Plugins call Confirm; the broker resolves the action's category, checks
// the configured mode (+ session grants), and for "ask" raises a request
// to the TUI and blocks until the user decides. It also implements
// tui.PermissionControl for the /permissions command.
type approvalBroker struct {
	mu       sync.Mutex
	modes    map[string]permMode
	session  map[string]bool // per-category "approve session" grants
	requests chan tui.ApprovalRequest
	persist  func(map[string]string) // save modes to settings (nil = no persist)
	log      *slog.Logger

	// reg is the SHARED id-keyed pending registry (TEN-203). The host broker owns
	// it; the iMessage offsite broker shares the SAME registry (so its approvals
	// are resolvable by id from any surface). Pending/Resolve/Decide/DenyAll all
	// delegate here. Always non-nil for brokers built via the constructors.
	reg *approvalRegistry
	// origin tags which surface this broker fronts (local|imessage) for display.
	origin string

	// ask, when non-nil, REPLACES the on-host TUI prompt as the backend for an
	// "ask"-mode action: it raises the request through a channel-specific
	// mechanism (e.g. the Discord button approver, TEN-231) and returns the
	// operator's decision. nil ⇒ the default backend posts to requests and
	// blocks on the TUI. This is what lets one broker model (per-category
	// ask|allow|deny) front every channel — only the "ask" plumbing differs.
	ask func(ctx context.Context, req tui.ApprovalRequest) tui.ApprovalDecision
}

func newApprovalBroker(log *slog.Logger) *approvalBroker {
	b := &approvalBroker{
		modes:    map[string]permMode{},
		session:  map[string]bool{},
		requests: make(chan tui.ApprovalRequest, 4),
		reg:      newApprovalRegistry(),
		origin:   "local",
		log:      log,
	}
	for _, c := range catOrder {
		b.modes[c] = modeAsk // safe by default: everything prompts
	}
	return b
}

// Requests is the channel the TUI drains for approval prompts. It is now a
// best-effort NOTIFIER (the registry is the source of truth); the broker posts
// to it non-blocking so a full buffer never wedges the agent (TEN-203).
func (b *approvalBroker) Requests() <-chan tui.ApprovalRequest { return b.requests }

// Pending / Resolve / Decide / DenyAll delegate to the shared registry, so the
// broker IS the dashboard.ApprovalControl and the TUI's resolve-by-id target.
func (b *approvalBroker) Pending() []dashboard.PendingApproval { return b.reg.Pending() }
func (b *approvalBroker) Resolve(id string, d tui.ApprovalDecision) bool {
	return b.reg.Resolve(id, d)
}
func (b *approvalBroker) DenyAll() { b.reg.DenyAll() }

// Decide implements dashboard.ApprovalControl: resolve id from a REST decision
// string. Deny-by-default: an unknown decision string is rejected, not approved.
func (b *approvalBroker) Decide(id, decision string) error {
	d, err := parseApprovalDecision(decision)
	if err != nil {
		return err
	}
	if !b.reg.Resolve(id, d) {
		return fmt.Errorf("approval %q not found (already resolved or expired)", id)
	}
	return nil
}

// parseApprovalDecision maps the REST decision string to a broker decision.
// Deny-by-default: an empty/unknown string is rejected rather than approving.
func parseApprovalDecision(s string) (tui.ApprovalDecision, error) {
	switch s {
	case "approve", "approve_once", "once":
		return tui.ApproveOnce, nil
	case "approve_session", "session":
		return tui.ApproveSession, nil
	case "approve_always", "always":
		return tui.ApproveAlways, nil
	case "deny", "deny_once", "reject":
		return tui.DenyOnce, nil
	default:
		return tui.DenyOnce, fmt.Errorf("unknown decision %q (want approve|approve_session|approve_always|deny)", s)
	}
}

// newOffsiteApprovalBroker builds a SECOND broker for an offsite channel (the
// iMessage responder, TEN-230): its own per-category modes — default DENY
// (offsite deny-by-default; the operator opens categories explicitly) — but it
// SHARES the on-host TUI's approval request channel, so an "ask" prompts the
// operator at the Mac console exactly like the global /permissions. It satisfies
// tui.PermissionControl, so /imessage permissions drives it with the same syntax.
func newOffsiteApprovalBroker(log *slog.Logger, host *approvalBroker) *approvalBroker {
	b := &approvalBroker{
		modes:    map[string]permMode{},
		session:  map[string]bool{},
		requests: host.requests, // share the on-host TUI notify channel
		reg:      host.reg,      // SHARE the registry so iMessage approvals are resolvable by id (TEN-203)
		origin:   "imessage",
		log:      log,
	}
	for _, c := range catOrder {
		b.modes[c] = modeDeny
	}
	return b
}

// newDiscordApprovalBroker builds the per-category broker for the Discord relay
// (TEN-231). Like the iMessage broker it satisfies tui.PermissionControl so
// /relay permissions uses the SAME ask|allow|deny syntax as /permissions, but
// its "ask" backend is the Discord BUTTON approver (the operator is reachable in
// the channel), not the Mac TUI — so the default mode is ASK (every dangerous
// action prompts a button, the faithful pre-TEN-231 behavior) rather than the
// iMessage deny-by-default (where the operator is typically away from the host).
// The caller sets b.ask to the live button backend after the relay manager exists.
func newDiscordApprovalBroker(log *slog.Logger) *approvalBroker {
	b := &approvalBroker{
		modes:   map[string]permMode{},
		session: map[string]bool{},
		reg:     newApprovalRegistry(), // own registry; wiring may point reg.emit at the shared stream for the visibility bridge
		origin:  "discord",
		log:     log,
	}
	for _, c := range catOrder {
		b.modes[c] = modeAsk
	}
	return b
}

// seedFromFlags lifts the launch --allow-* flags into category modes, so
// existing flags still work: they pre-approve a whole category. Destructive
// always stays "ask" — no flag blanket-approves irreversible actions.
func (b *approvalBroker) seedFromFlags(pf *pluginFlags) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if pf.osAllowExec {
		b.modes[catExec] = modeAllow
	}
	if pf.osAllowWrite {
		b.modes[catWrite] = modeAllow
	}
	if pf.webAllowInteract {
		b.modes[catWeb] = modeAllow
	}
	if pf.gsuiteAllowSend || pf.xAllowPost || pf.imsgAllowSend {
		b.modes[catSend] = modeAllow
	}
}

// loadModes applies persisted per-category modes (override flag defaults).
func (b *approvalBroker) loadModes(saved map[string]string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for cat, ms := range saved {
		if _, known := b.modes[cat]; !known {
			continue
		}
		if m, ok := parseMode(ms); ok {
			b.modes[cat] = m
		}
	}
}

func (b *approvalBroker) modesSnapshot() map[string]string {
	out := make(map[string]string, len(b.modes))
	for c, m := range b.modes {
		out[c] = m.String()
	}
	return out
}

// Confirm is the hook handed to every plugin Policy. Returns true if the
// action may proceed. Blocks (on the agent goroutine) for an "ask" while
// the user decides in the TUI; respects context cancellation.
func (b *approvalBroker) Confirm(ctx context.Context, action, detail string) bool {
	cat := categorize(action)
	b.mu.Lock()
	mode := b.modes[cat]
	sess := b.session[cat]
	b.mu.Unlock()

	switch mode {
	case modeAllow:
		return true
	case modeDeny:
		return false
	}
	if sess {
		return true
	}

	switch b.askDecision(ctx, cat, action, detail) {
	case tui.ApproveAlways:
		b.setMode(cat, modeAllow)
		return true
	case tui.ApproveSession:
		b.mu.Lock()
		b.session[cat] = true
		b.mu.Unlock()
		return true
	case tui.ApproveOnce:
		return true
	default:
		return false
	}
}

// AskPairing raises an Approve/Deny prompt for an inbound peer pairing request
// (TEN-239). Unlike Confirm it NEVER short-circuits on a category mode — pairing
// is far too sensitive to auto-allow — and never persists an auto-allow: it asks
// every time and fails closed (deny) when there's no operator channel. detail
// carries the inviter name/addr + the PIN the operator must match.
func (b *approvalBroker) AskPairing(ctx context.Context, detail string) bool {
	switch b.askDecision(ctx, "pairing", "peer pairing", detail) {
	case tui.ApproveAlways, tui.ApproveSession, tui.ApproveOnce:
		return true
	default:
		return false
	}
}

// askDecision raises an "ask"-mode prompt and returns the operator's decision.
// The custom backend (b.ask, e.g. Discord buttons) takes precedence; otherwise
// the default backend posts to the on-host TUI request channel and blocks. With
// NEITHER wired it fails closed (DenyOnce) — a broker with no way to reach the
// operator must never silently allow.
func (b *approvalBroker) askDecision(ctx context.Context, cat, action, detail string) tui.ApprovalDecision {
	req := tui.ApprovalRequest{Category: cat, Action: action, Detail: detail}

	// THIRD PATH (Discord buttons, TEN-231): b.ask is a REPLACEMENT single-owner
	// backend — the operator decides in Discord, not at any other surface. It
	// stays BESIDE the fan-out registry on purpose: routing it through Resolve
	// would create multiple racing resolvers on one reply (the exact hazard
	// TEN-203 exists to contain) and a dashboard can't drive a Discord button
	// anyway. We DO emit pending/resolved (Origin=discord) so the dashboard +
	// activity feed still SEE that a Discord approval is outstanding (visibility
	// only — not resolvable from those surfaces).
	if b.ask != nil {
		// Tag the beside-event with THIS broker's origin (discord|imessage) so the
		// dashboard/activity feed shows where the out-of-band approval is pending.
		origin := b.origin
		if origin == "" {
			origin = "discord"
		}
		b.reg.emitBeside(agent.EventApprovalPending, req, origin, "")
		d := b.ask(ctx, req)
		b.reg.emitBeside(agent.EventApprovalResolved, req, origin, outcomeOf(d))
		return d
	}

	// Fail-closed: with no custom backend (handled above) AND no notify channel,
	// there is no surface to reach an operator — deny BEFORE registering rather
	// than block forever on a request nobody can see. (The registry alone is just
	// storage; it is not an observer.)
	if b.requests == nil {
		return tui.DenyOnce
	}

	// Register in the shared id-keyed registry (the source of truth) + emit the
	// pending event. Both observers (TUI + dashboard) now see it and can resolve
	// by id; the agent goroutine blocks ONLY on its own per-id reply chan.
	id, reply := b.reg.register(req, b.origin)
	req.ID = id
	req.Reply = reply

	// Best-effort NOTIFY the on-host TUI via its channel — non-blocking, so a
	// full buffer (or no drainer, e.g. serve) never wedges the agent. The event
	// stream + Pending() are the authoritative refresh; this is just the fast path.
	if b.requests != nil {
		select {
		case b.requests <- req:
		default:
		}
	}

	select {
	case d := <-reply:
		return d
	case <-ctx.Done():
		// Turn cancelled/timed out before anyone resolved → deny-by-default, and
		// drop the stale entry so Pending() is honest + a late Resolve is a no-op.
		b.reg.removeIfPending(id)
		return tui.DenyOnce
	}
}

func (b *approvalBroker) setMode(cat string, m permMode) {
	b.mu.Lock()
	b.modes[cat] = m
	snap := b.modesSnapshot()
	b.mu.Unlock()
	if b.persist != nil {
		b.persist(snap)
	}
}

// --- tui.PermissionControl ---

func (b *approvalBroker) Permissions() []tui.PermissionInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]tui.PermissionInfo, 0, len(catOrder))
	for _, c := range catOrder {
		out = append(out, tui.PermissionInfo{Category: c, Mode: b.modes[c].String(), Desc: catDesc[c]})
	}
	return out
}

func (b *approvalBroker) SetPermission(category, mode string) (bool, error) {
	m, ok := parseMode(mode)
	if !ok {
		return false, fmt.Errorf("mode must be ask|allow|deny")
	}
	b.mu.Lock()
	_, known := b.modes[category]
	b.mu.Unlock()
	if !known {
		return false, nil
	}
	b.setMode(category, m)
	// Reconfiguring a category clears any prior "session" grant — the
	// explicit setting is now the source of truth.
	b.mu.Lock()
	delete(b.session, category)
	b.mu.Unlock()
	return true, nil
}
