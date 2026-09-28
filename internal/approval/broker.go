package approval

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"tenant/internal/agent"
	"tenant/internal/control"
	"tenant/internal/logs/catalog"
	"tenant/internal/logs/event"
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
	known    map[string]bool // categories the operator had a say in; one that asks and isn't here is New (TEN-343)
	requests chan control.ApprovalRequest
	persist  func(modes map[string]string, known []string) // save modes + the known marker (nil = no persist)
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
		known:      knownSet(Categories),
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
		known:      knownSet(Categories),
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
		known:      knownSet(Categories),
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

// SetPersist installs the hook that saves the category modes, and the
// categories the operator has chosen (the known_categories marker), whenever
// the operator changes one (nil: nothing is saved).
func (b *Broker) SetPersist(persist func(modes map[string]string, known []string)) {
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
// Resolve is the terminal's (its records name the tui surface); Decide the
// dashboard's.
func (b *Broker) Pending() []control.PendingApproval { return b.reg.Pending() }
func (b *Broker) Resolve(id string, d control.ApprovalDecision) bool {
	return b.reg.Resolve(id, d, "tui")
}
func (b *Broker) DenyAll() { b.reg.DenyAll() }

// Decide resolves id from a REST decision string. Deny-by-default: an unknown
// decision string is rejected, not approved.
func (b *Broker) Decide(id, decision string) error {
	d, err := parseDecision(decision)
	if err != nil {
		return err
	}
	if !b.reg.Resolve(id, d, "dashboard") {
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
// without persisting them again, and the known_categories marker saved beside
// them. Without a marker, a fresh install (nothing saved) knows every
// category, and an older save knows only OriginalCategories.
func (b *Broker) LoadModes(saved map[string]string, known []string) {
	modes := SavedModes(saved)
	if len(known) == 0 {
		known = OriginalCategories
		if len(modes) == 0 {
			known = Categories
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for cat, m := range modes {
		b.modes[cat] = m
	}
	b.known = knownSet(known)
}

func knownSet(cats []string) map[string]bool {
	out := make(map[string]bool, len(cats))
	for _, c := range cats {
		out[c] = true
	}
	return out
}

// knownList is the known marker as saved: known categories in display order.
func (b *Broker) knownList() []string {
	var out []string
	for _, c := range Categories {
		if b.known[c] {
			out = append(out, c)
		}
	}
	return out
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
// One line per decision (auto-decided or prompted) makes the log the record,
// joined to the turn that asked by ctx's trace.
func (b *Broker) logInfo(ctx context.Context, msg string, kv ...any) {
	if b.log != nil {
		b.log.InfoContext(ctx, msg, kv...)
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
		b.logInfo(ctx, "approval refused: locked on this surface", "origin", b.origin, "category", cat, "action", action)
		return verdict(ctx, cat, action, detail, b.origin, "lock", "denied_policy", control.DenyOnce)
	}

	// A per-tool rule is the operator's explicit word for THIS tool and wins
	// over the category: allow/deny decide silently; ask always prompts, even
	// past a category allow or a session grant.
	if b.rules != nil {
		if rmode, pat, ok := b.rules.Lookup(action); ok {
			switch rmode {
			case "allow":
				b.logInfo(ctx, "approval allowed by tool rule", "origin", b.origin, "category", cat, "action", action, "rule", pat)
				return verdict(ctx, cat, action, detail, b.origin, "rule", "", control.ApproveAlways)
			case "deny":
				b.logInfo(ctx, "approval denied by tool rule", "origin", b.origin, "category", cat, "action", action, "rule", pat)
				return verdict(ctx, cat, action, detail, b.origin, "rule", "", control.DenyOnce)
			}
			b.logInfo(ctx, "approval prompt forced by tool rule", "origin", b.origin, "category", cat, "action", action, "rule", pat)
			switch b.askDecision(ctx, "agent", cat, action, detail, false) {
			case control.ApproveAlways:
				// "Always" on a rule-forced prompt means THIS tool, not its category.
				// A failed save is told to the operator by the settings persist hook.
				if err := b.rules.Set(pat, "allow"); err != nil && b.log != nil {
					b.log.Warn("approval: always-allow rule not set", "origin", b.origin, "rule", pat, "err", err)
				}
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
		b.logInfo(ctx, "approval auto-allowed by category mode", "origin", b.origin, "category", cat, "action", action)
		return verdict(ctx, cat, action, detail, b.origin, "mode", "approved_always", control.ApproveAlways)
	case ModeDeny:
		b.logInfo(ctx, "approval auto-denied by category mode", "origin", b.origin, "category", cat, "action", action)
		return verdict(ctx, cat, action, detail, b.origin, "mode", "denied_policy", control.DenyOnce)
	}
	if sess {
		b.logInfo(ctx, "approval allowed by session grant", "origin", b.origin, "category", cat, "action", action)
		return verdict(ctx, cat, action, detail, b.origin, "session", "approved_session", control.ApproveSession)
	}

	switch b.askDecision(ctx, "agent", cat, action, detail, true) {
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
	return b.askFrom(ctx, "peer", "pairing", "peer pairing", detail)
}

// Ask raises an Approve/Deny prompt that no category mode, tool rule or
// session grant answers: the operator decides every time, and it is denied
// when nobody answers before ctx ends or no surface can reach the operator.
// For requests that are not a tool's action and that Tenant itself raises:
// an alert's agent investigation (TEN-400).
func (b *Broker) Ask(ctx context.Context, category, action, detail string) bool {
	return b.askFrom(ctx, "tenant", category, action, detail)
}

// askFrom is Ask for a request raised by by (a peer, Tenant).
func (b *Broker) askFrom(ctx context.Context, by, category, action, detail string) bool {
	switch b.askDecision(ctx, by, category, action, detail, false) {
	case control.ApproveAlways, control.ApproveSession, control.ApproveOnce:
		return true
	default:
		return false
	}
}

// askDecision raises an "ask"-mode prompt from by (the agent, a peer) and
// returns the operator's decision. sticky says an "approve for this session"
// becomes a category grant; otherwise it covers only this request.
// The custom backend (b.ask, e.g. Discord buttons) takes precedence; otherwise
// the default backend posts to the host request channel and blocks. With
// NEITHER wired it fails closed (DenyOnce) — a broker with no way to reach the
// operator must never silently allow. The prompt and its outcome are Security
// records (audit.go); a prompt whose record can't be written under the deny
// policy is never raised.
func (b *Broker) askDecision(ctx context.Context, by, cat, action, detail string, sticky bool) control.ApprovalDecision {
	req := control.ApprovalRequest{Category: cat, Action: action, Detail: detail}
	start := time.Now()
	b.logInfo(ctx, "approval pending", "origin", b.origin, "category", cat, "action", action)
	logOutcome := func(outcome, via string) {
		b.logInfo(ctx, "approval resolved", "origin", b.origin, "category", cat, "action", action,
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
		id := policyID(cat, action) // one prompt, one record: pending, then its outcome
		rec := approvalRec{id, cat, action, detail, origin}
		if rec.asked(ctx, by) != nil {
			logOutcome("denied", "security log unwritable")
			return control.DenyOnce
		}
		b.reg.emitBeside(ctx, agent.EventApprovalPending, req, origin, "")
		recordPolicyApproval(ctx, id, cat, action, detail, origin, "pending")
		// The backend names who decided (DecidedBy); nobody means Tenant did:
		// it expired, or no operator could be reached.
		who := &decider{actor: actorTenant}
		d := ask(context.WithValue(event.WithApproval(ctx, id), deciderKey{}, who), req)
		via := "operator"
		if who.actor == actorTenant {
			via = "no_surface"
		}
		d = rec.decided(ctx, d, who.actor, origin, via, who.kv...)
		b.reg.emitBeside(ctx, agent.EventApprovalResolved, req, origin, outcomeOf(d))
		recordPolicyApproval(ctx, id, cat, action, detail, origin, outcomeOf(d))
		logOutcome(outcomeOf(d), "custom backend")
		return d
	}

	// Fail-closed: with no custom backend (handled above) AND no notify channel,
	// there is no surface to reach an operator — deny BEFORE registering rather
	// than block forever on a request nobody can see. (The registry alone is just
	// storage; it is not an observer.)
	if b.requests == nil {
		logOutcome("denied", "no operator surface")
		verdict(ctx, cat, action, detail, b.origin, "no_surface", "denied_no_surface", control.DenyOnce)
		return control.DenyOnce
	}

	id := "ap-" + nextID()
	rec := approvalRec{id, cat, action, detail, b.origin}
	if rec.asked(ctx, by) != nil {
		logOutcome("denied", "security log unwritable")
		return control.DenyOnce
	}
	// Register in the shared id-keyed registry (the source of truth) + emit the
	// pending event. Both observers (TUI + dashboard) now see it and can resolve
	// by id; the agent goroutine blocks ONLY on its own per-id reply chan.
	p := b.reg.register(ctx, id, req, b.origin, sticky)
	req.ID = id
	req.Reply = p.reply

	// Best-effort NOTIFY the on-host TUI via its channel — non-blocking, so a
	// full buffer (or no drainer, e.g. serve) never wedges the agent. The event
	// stream + Pending() are the authoritative refresh; this is just the fast path.
	select {
	case b.requests <- req:
	default:
	}

	select {
	case d := <-p.reply:
		// Resolve named the surface; a reply sent straight to req.Reply is the
		// terminal's fallback, and a shutdown is Tenant's.
		actor, surface := "operator", p.by
		if surface == "" {
			surface = "tui"
		}
		if d == control.DenyShutdown {
			actor, surface = actorTenant, b.origin
		}
		d = rec.decided(ctx, d, actor, surface, "operator")
		logOutcome(outcomeOf(d), "host surface")
		return d
	case <-ctx.Done():
		// Turn cancelled/timed out before anyone resolved → deny-by-default, and
		// drop the stale entry so Pending() is honest + a late Resolve is a no-op.
		b.reg.removeIfPending(id)
		rec.decided(ctx, control.Expired, actorTenant, b.origin, "")
		logOutcome("expired", "turn ended")
		return control.DenyOnce
	}
}

// policyID names one approval the registry never holds (a policy verdict, or a
// prompt a channel backend answers). The per-prompt suffix keeps two prompts
// for the same action in one turn two records.
func policyID(category, action string) string {
	return "policy:" + category + ":" + action + ":" + nextID()
}

func recordPolicyApproval(ctx context.Context, id, category, action, detail, origin, outcome string) {
	if span := runtrace.SpanFromContext(ctx); span != nil {
		span.AddApproval(runtrace.Approval{
			ID: id, Category: category,
			Action: action, Detail: detail, Origin: origin, Outcome: outcome,
		})
	}
}

// setMode is an operator's choice: it also marks the category known.
func (b *Broker) setMode(cat string, m Mode) {
	b.mu.Lock()
	b.modes[cat] = m
	b.known[cat] = true
	snap, known := b.modesSnapshot(), b.knownList()
	persist := b.persist
	b.mu.Unlock()
	if persist != nil {
		persist(snap, known)
	}
}

// --- the terminal UI's PermissionControl ---

func (b *Broker) Permissions() []control.PermissionInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]control.PermissionInfo, 0, len(Categories))
	for _, c := range Categories {
		m := b.modes[c]
		out = append(out, control.PermissionInfo{
			Category: c, Mode: m.String(), Desc: CategoryDesc[c],
			Title: CategoryPlain[c].Title, Can: CategoryPlain[c].Can,
			New:     m == ModeAsk && !b.known[c],
			Session: m == ModeAsk && b.session[c], // a grant only skips prompts while the category asks
		})
	}
	return out
}

// NewCategories lists, in display order, the categories an update added that
// the operator never chose and that ask (TEN-343).
func (b *Broker) NewCategories() []string {
	var out []string
	for _, in := range b.Permissions() {
		if in.New {
			out = append(out, in.Category)
		}
	}
	return out
}

// SetPermission sets a category's mode from the terminal; SetPermissionFrom
// names the surface the operator changed it on, for its Security record.
func (b *Broker) SetPermission(category, mode string) (bool, error) {
	return b.SetPermissionFrom("tui", category, mode)
}

func (b *Broker) SetPermissionFrom(surface, category, mode string) (bool, error) {
	m, ok := ParseMode(mode)
	if !ok {
		return false, fmt.Errorf("mode must be ask|allow|deny")
	}
	b.mu.Lock()
	was, known := b.modes[category]
	b.mu.Unlock()
	if !known {
		return false, nil
	}
	b.setMode(category, m)
	_ = catalog.ModeChanged.Audit(context.Background(), "broker", b.origin, "category", category, "mode", m.String(), "was", was.String(),
		"actor", "operator", "surface", surface)
	// Reconfiguring a category clears any prior "session" grant — the
	// explicit setting is now the source of truth.
	b.mu.Lock()
	delete(b.session, category)
	b.mu.Unlock()
	return true, nil
}
