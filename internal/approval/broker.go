package approval

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"tenant/internal/agent"
	"tenant/internal/control"
	"tenant/internal/runtrace"
)

// Broker is the single decision point for every dangerous action. Plugins
// call Confirm; the broker resolves the action's category, checks the
// configured mode (+ session grants), and for "ask" raises a request to the
// operator and blocks until the operator decides. It also implements the
// terminal UI's PermissionControl and ToolRuleControl and the dashboard's
// ApprovalControl.
type Broker struct {
	mu       sync.Mutex
	modes    map[string]Mode
	session  map[string]bool // per-category "approve session" grants
	requests chan control.ApprovalRequest
	persist  func(map[string]string) // save modes to settings (nil = no persist)
	log      *slog.Logger
	rules    *Rules // per-tool permission rules, shared process-wide; nil in bare tests

	// categorize maps an action id to its category (see Categorize).
	categorize func(action string) string

	// reg is the SHARED id-keyed pending registry (TEN-203). The host broker owns
	// it; the iMessage offsite broker shares the SAME registry (so its approvals
	// are resolvable by id from any surface). Pending/Resolve/Decide/DenyAll all
	// delegate here. Always non-nil for brokers built via the constructors.
	reg *registry
	// origin tags which surface this broker fronts (local|imessage|discord) for display.
	origin string

	// ask, when non-nil, REPLACES the on-host TUI prompt as the backend for an
	// "ask"-mode action: it raises the request through a channel-specific
	// mechanism (e.g. the Discord button approver, TEN-231) and returns the
	// operator's decision. nil ⇒ the default backend posts to requests and
	// blocks on the TUI. This is what lets one broker model (per-category
	// ask|allow|deny) front every channel — only the "ask" plumbing differs.
	ask func(ctx context.Context, req control.ApprovalRequest) control.ApprovalDecision

	// locked, when set, is consulted before anything else in Confirm: an action
	// it reports is refused whatever a tool rule, the category mode or a session
	// grant says. The Discord relay's exec mode drives it (TEN-338), so
	// relay.allow_exec=false keeps dangerous actions out of reach offsite.
	locked func(action, cat string) bool
}

// NewBroker builds the host's broker: every category asks, and "ask" prompts
// through Requests. categorize maps an action id to its category; nil files
// every action under destructive.
func NewBroker(log *slog.Logger, categorize func(action string) string) *Broker {
	b := &Broker{
		modes:      map[string]Mode{},
		session:    map[string]bool{},
		requests:   make(chan control.ApprovalRequest, 4),
		reg:        newRegistry(),
		origin:     "local",
		log:        log,
		rules:      DefaultRules,
		categorize: categorizeOrDestructive(categorize),
	}
	for _, c := range Categories {
		b.modes[c] = ModeAsk // safe by default: everything prompts
	}
	return b
}

// NewOffsiteBroker builds a SECOND broker for an offsite channel (the
// iMessage responder, TEN-230): its own per-category modes — default DENY
// (offsite deny-by-default; the operator opens categories explicitly) — but it
// SHARES the host's approval request channel and registry, so an "ask" prompts
// the operator at the host exactly like the global /permissions, and is
// resolvable by id from any surface (TEN-203).
func NewOffsiteBroker(log *slog.Logger, host *Broker) *Broker {
	b := &Broker{
		modes:      map[string]Mode{},
		session:    map[string]bool{},
		requests:   host.requests, // share the on-host TUI notify channel
		reg:        host.reg,      // SHARE the registry so offsite approvals are resolvable by id
		origin:     "imessage",
		log:        log,
		rules:      DefaultRules, // tool rules are about the tool, not the surface: one set everywhere
		categorize: host.categorize,
	}
	for _, c := range Categories {
		b.modes[c] = ModeDeny
	}
	return b
}

// NewDiscordBroker builds the per-category broker for the Discord relay
// (TEN-231). Its "ask" backend is the Discord BUTTON approver (the operator is
// reachable in the channel), not the host's prompt, so the default mode is ASK
// rather than the iMessage deny-by-default. The caller installs the button
// backend with SetAsk once the relay manager exists; until then, and with no
// request channel, an "ask" fails closed.
func NewDiscordBroker(log *slog.Logger, categorize func(action string) string) *Broker {
	b := &Broker{
		modes:      map[string]Mode{},
		session:    map[string]bool{},
		reg:        newRegistry(), // own registry; SetEvents may point it at the shared stream for the visibility bridge
		origin:     "discord",
		rules:      DefaultRules,
		log:        log,
		categorize: categorizeOrDestructive(categorize),
	}
	for _, c := range Categories {
		b.modes[c] = ModeAsk
	}
	return b
}

func categorizeOrDestructive(categorize func(string) string) func(string) string {
	if categorize == nil {
		return func(string) string { return CatDestructive }
	}
	return categorize
}

// SetPersist installs the hook that saves the category modes whenever the
// operator changes one (nil: nothing is saved).
func (b *Broker) SetPersist(persist func(modes map[string]string)) {
	b.mu.Lock()
	b.persist = persist
	b.mu.Unlock()
}

// SetEvents points the broker's pending/resolved events at the live event
// stream, so every observer refreshes. A broker that shares its registry
// (the offsite one) shares this too. Call it while wiring, before the broker
// is in use: the registry reads it without a lock.
func (b *Broker) SetEvents(emit func(agent.Event)) { b.reg.emit = emit }

// SetAsk replaces the host prompt with a channel-specific "ask" backend (the
// Discord buttons, the iMessage reply). nil restores the host prompt.
func (b *Broker) SetAsk(ask func(ctx context.Context, req control.ApprovalRequest) control.ApprovalDecision) {
	b.mu.Lock()
	b.ask = ask
	b.mu.Unlock()
}

// SetLock installs the surface's lock: an action it reports is refused before
// any rule, mode or grant is consulted.
func (b *Broker) SetLock(locked func(action, cat string) bool) {
	b.mu.Lock()
	b.locked = locked
	b.mu.Unlock()
}

// Locked reports whether this surface's lock refuses action outright.
func (b *Broker) Locked(action string) bool {
	b.mu.Lock()
	locked := b.locked
	b.mu.Unlock()
	return locked != nil && locked(action, b.categorize(action))
}

// SetRules replaces the per-tool rule set this broker consults (DefaultRules
// unless changed); nil consults none. Call it while wiring, before the broker
// is in use.
func (b *Broker) SetRules(rules *Rules) { b.rules = rules }

// Requests is the channel the TUI drains for approval prompts. It is a
// best-effort NOTIFIER (the registry is the source of truth); the broker posts
// to it non-blocking so a full buffer never wedges the agent (TEN-203).
func (b *Broker) Requests() <-chan control.ApprovalRequest { return b.requests }

// Pending / Resolve / Decide / DenyAll delegate to the shared registry, so the
// broker IS the dashboard's ApprovalControl and the TUI's resolve-by-id target.
func (b *Broker) Pending() []control.PendingApproval { return b.reg.Pending() }
func (b *Broker) Resolve(id string, d control.ApprovalDecision) bool {
	return b.reg.Resolve(id, d)
}
func (b *Broker) DenyAll() { b.reg.DenyAll() }

// Decide resolves id from a REST decision string. Deny-by-default: an unknown
// decision string is rejected, not approved.
func (b *Broker) Decide(id, decision string) error {
	d, err := parseDecision(decision)
	if err != nil {
		return err
	}
	if !b.reg.Resolve(id, d) {
		return fmt.Errorf("approval %q not found (already resolved or expired)", id)
	}
	return nil
}

// parseDecision maps the REST decision string to a broker decision.
// Deny-by-default: an empty/unknown string is rejected rather than approving.
func parseDecision(s string) (control.ApprovalDecision, error) {
	switch s {
	case "approve", "approve_once", "once":
		return control.ApproveOnce, nil
	case "approve_session", "session":
		return control.ApproveSession, nil
	case "approve_always", "always":
		return control.ApproveAlways, nil
	case "deny", "deny_once", "reject":
		return control.DenyOnce, nil
	default:
		return control.DenyOnce, fmt.Errorf("unknown decision %q (want approve|approve_session|approve_always|deny)", s)
	}
}

// Allow sets categories to allow: how the launch --*-allow-* flags pre-approve
// whole categories.
func (b *Broker) Allow(categories ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, cat := range categories {
		b.modes[cat] = ModeAllow
	}
}

// LoadModes applies persisted per-category modes (override flag defaults),
// without persisting them again.
func (b *Broker) LoadModes(saved map[string]string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for cat, m := range SavedModes(saved) {
		b.modes[cat] = m
	}
}

// Modes snapshots the category → mode map.
func (b *Broker) Modes() map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.modesSnapshot()
}

func (b *Broker) modesSnapshot() map[string]string {
	out := make(map[string]string, len(b.modes))
	for c, m := range b.modes {
		out[c] = m.String()
	}
	return out
}

// logInfo is the broker's persistent audit line. Approval events otherwise live
// only in the in-memory event ring, which every restart wipes — after which
// "did that prompt ever fire, where, and how did it end?" cannot be answered.
// One line per decision (auto-decided or prompted) makes the log the record.
func (b *Broker) logInfo(msg string, kv ...any) {
	if b.log != nil {
		b.log.Info(msg, kv...)
	}
}

// Confirm is the hook handed to every plugin Policy. Returns true if the
// action may proceed. Blocks (on the agent goroutine) for an "ask" while
// the operator decides; respects context cancellation.
func (b *Broker) Confirm(ctx context.Context, action, detail string) bool {
	cat := b.categorize(action)

	b.mu.Lock()
	locked := b.locked
	b.mu.Unlock()
	// The surface's lock (the Discord exec mode) outranks every setting below:
	// a tool rule or a category allow must not reopen what it closed.
	if locked != nil && locked(action, cat) {
		b.logInfo("approval refused: locked on this surface", "origin", b.origin, "category", cat, "action", action)
		recordPolicyApproval(ctx, cat, action, detail, b.origin, "denied_policy")
		return false
	}

	// A per-tool rule is the operator's explicit word for THIS tool and wins
	// over the category: allow/deny decide silently; ask always prompts, even
	// past a category allow or a session grant.
	if b.rules != nil {
		if rmode, pat, ok := b.rules.Lookup(action); ok {
			switch rmode {
			case "allow":
				b.logInfo("approval allowed by tool rule", "origin", b.origin, "category", cat, "action", action, "rule", pat)
				return true
			case "deny":
				b.logInfo("approval denied by tool rule", "origin", b.origin, "category", cat, "action", action, "rule", pat)
				return false
			}
			b.logInfo("approval prompt forced by tool rule", "origin", b.origin, "category", cat, "action", action, "rule", pat)
			switch b.askDecision(ctx, cat, action, detail) {
			case control.ApproveAlways:
				// "Always" on a rule-forced prompt means THIS tool, not its category.
				_ = b.rules.Set(pat, "allow")
				return true
			case control.ApproveSession, control.ApproveOnce:
				return true // a rule that says ask keeps asking: session = this time
			default:
				return false
			}
		}
	}

	b.mu.Lock()
	mode := b.modes[cat]
	sess := b.session[cat]
	b.mu.Unlock()

	switch mode {
	case ModeAllow:
		b.logInfo("approval auto-allowed by category mode", "origin", b.origin, "category", cat, "action", action)
		recordPolicyApproval(ctx, cat, action, detail, b.origin, "approved_always")
		return true
	case ModeDeny:
		b.logInfo("approval auto-denied by category mode", "origin", b.origin, "category", cat, "action", action)
		recordPolicyApproval(ctx, cat, action, detail, b.origin, "denied_policy")
		return false
	}
	if sess {
		b.logInfo("approval allowed by session grant", "origin", b.origin, "category", cat, "action", action)
		recordPolicyApproval(ctx, cat, action, detail, b.origin, "approved_session")
		return true
	}

	switch b.askDecision(ctx, cat, action, detail) {
	case control.ApproveAlways:
		b.setMode(cat, ModeAllow)
		return true
	case control.ApproveSession:
		b.mu.Lock()
		b.session[cat] = true
		b.mu.Unlock()
		return true
	case control.ApproveOnce:
		return true
	default:
		return false
	}
}

// --- per-tool rules (the set is shared by every broker) ---

func (b *Broker) Rules() []control.ToolRuleInfo {
	if b.rules == nil {
		return nil
	}
	return b.rules.Rules()
}

func (b *Broker) SetRule(pattern, mode string) error {
	if b.rules == nil {
		return fmt.Errorf("tool rules are not available")
	}
	return b.rules.Set(pattern, mode)
}

func (b *Broker) RemoveRule(pattern string) (bool, error) {
	if b.rules == nil {
		return false, fmt.Errorf("tool rules are not available")
	}
	return b.rules.Remove(pattern), nil
}

// AskPairing raises an Approve/Deny prompt for an inbound peer pairing request
// (TEN-239). Unlike Confirm it NEVER short-circuits on a category mode — pairing
// is far too sensitive to auto-allow — and never persists an auto-allow: it asks
// every time and fails closed (deny) when there's no operator channel. detail
// carries the inviter name/addr + the PIN the operator must match.
func (b *Broker) AskPairing(ctx context.Context, detail string) bool {
	switch b.askDecision(ctx, "pairing", "peer pairing", detail) {
	case control.ApproveAlways, control.ApproveSession, control.ApproveOnce:
		return true
	default:
		return false
	}
}

// askDecision raises an "ask"-mode prompt and returns the operator's decision.
// The custom backend (b.ask, e.g. Discord buttons) takes precedence; otherwise
// the default backend posts to the host request channel and blocks. With
// NEITHER wired it fails closed (DenyOnce) — a broker with no way to reach the
// operator must never silently allow.
func (b *Broker) askDecision(ctx context.Context, cat, action, detail string) control.ApprovalDecision {
	req := control.ApprovalRequest{Category: cat, Action: action, Detail: detail}
	start := time.Now()
	b.logInfo("approval pending", "origin", b.origin, "category", cat, "action", action)
	logOutcome := func(outcome, via string) {
		b.logInfo("approval resolved", "origin", b.origin, "category", cat, "action", action,
			"outcome", outcome, "via", via, "latency", time.Since(start).Round(time.Millisecond))
	}

	b.mu.Lock()
	ask := b.ask
	b.mu.Unlock()
	// THIRD PATH (Discord buttons, TEN-231): ask is a REPLACEMENT single-owner
	// backend — the operator decides in Discord, not at any other surface. It
	// stays BESIDE the fan-out registry on purpose: routing it through Resolve
	// would create multiple racing resolvers on one reply (the exact hazard
	// TEN-203 exists to contain) and a dashboard can't drive a Discord button
	// anyway. We DO emit pending/resolved (Origin=discord) so the dashboard +
	// activity feed still SEE that a Discord approval is outstanding (visibility
	// only — not resolvable from those surfaces).
	if ask != nil {
		// Tag the beside-event with THIS broker's origin (discord|imessage) so the
		// host surfaces and the activity feed show where the approval lives.
		origin := b.origin
		if origin == "" {
			origin = "discord"
		}
		b.reg.emitBeside(ctx, agent.EventApprovalPending, req, origin, "")
		recordPolicyApproval(ctx, cat, action, detail, origin, "pending")
		d := ask(ctx, req)
		b.reg.emitBeside(ctx, agent.EventApprovalResolved, req, origin, outcomeOf(d))
		recordPolicyApproval(ctx, cat, action, detail, origin, outcomeOf(d))
		logOutcome(outcomeOf(d), "custom backend")
		return d
	}

	// Fail-closed: with no custom backend (handled above) AND no notify channel,
	// there is no surface to reach an operator — deny BEFORE registering rather
	// than block forever on a request nobody can see. (The registry alone is just
	// storage; it is not an observer.)
	if b.requests == nil {
		logOutcome("denied", "no operator surface")
		recordPolicyApproval(ctx, cat, action, detail, b.origin, "denied_no_surface")
		return control.DenyOnce
	}

	// Register in the shared id-keyed registry (the source of truth) + emit the
	// pending event. Both observers (TUI + dashboard) now see it and can resolve
	// by id; the agent goroutine blocks ONLY on its own per-id reply chan.
	id, reply := b.reg.register(ctx, req, b.origin)
	req.ID = id
	req.Reply = reply

	// Best-effort NOTIFY the on-host TUI via its channel — non-blocking, so a
	// full buffer (or no drainer, e.g. serve) never wedges the agent. The event
	// stream + Pending() are the authoritative refresh; this is just the fast path.
	select {
	case b.requests <- req:
	default:
	}

	select {
	case d := <-reply:
		logOutcome(outcomeOf(d), "host surface")
		return d
	case <-ctx.Done():
		// Turn cancelled/timed out before anyone resolved → deny-by-default, and
		// drop the stale entry so Pending() is honest + a late Resolve is a no-op.
		b.reg.removeIfPending(id)
		logOutcome("expired", "turn ended")
		return control.DenyOnce
	}
}

func recordPolicyApproval(ctx context.Context, category, action, detail, origin, outcome string) {
	if span := runtrace.SpanFromContext(ctx); span != nil {
		span.AddApproval(runtrace.Approval{
			ID: "policy:" + category + ":" + action, Category: category,
			Action: action, Detail: detail, Origin: origin, Outcome: outcome,
		})
	}
}

func (b *Broker) setMode(cat string, m Mode) {
	b.mu.Lock()
	b.modes[cat] = m
	snap := b.modesSnapshot()
	persist := b.persist
	b.mu.Unlock()
	if persist != nil {
		persist(snap)
	}
}

// --- the terminal UI's PermissionControl ---

func (b *Broker) Permissions() []control.PermissionInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]control.PermissionInfo, 0, len(Categories))
	for _, c := range Categories {
		out = append(out, control.PermissionInfo{Category: c, Mode: b.modes[c].String(), Desc: CategoryDesc[c]})
	}
	return out
}

func (b *Broker) SetPermission(category, mode string) (bool, error) {
	m, ok := ParseMode(mode)
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
