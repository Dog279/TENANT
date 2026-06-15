package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"tenant/internal/tui"
)

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
}

func newApprovalBroker(log *slog.Logger) *approvalBroker {
	b := &approvalBroker{
		modes:    map[string]permMode{},
		session:  map[string]bool{},
		requests: make(chan tui.ApprovalRequest, 4),
		log:      log,
	}
	for _, c := range catOrder {
		b.modes[c] = modeAsk // safe by default: everything prompts
	}
	return b
}

// Requests is the channel the TUI drains for approval prompts.
func (b *approvalBroker) Requests() <-chan tui.ApprovalRequest { return b.requests }

// newOffsiteApprovalBroker builds a SECOND broker for an offsite channel (the
// iMessage responder, TEN-230): its own per-category modes — default DENY
// (offsite deny-by-default; the operator opens categories explicitly) — but it
// SHARES the on-host TUI's approval request channel, so an "ask" prompts the
// operator at the Mac console exactly like the global /permissions. It satisfies
// tui.PermissionControl, so /imessage permissions drives it with the same syntax.
func newOffsiteApprovalBroker(log *slog.Logger, requests chan tui.ApprovalRequest) *approvalBroker {
	b := &approvalBroker{
		modes:    map[string]permMode{},
		session:  map[string]bool{},
		requests: requests,
		log:      log,
	}
	for _, c := range catOrder {
		b.modes[c] = modeDeny
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

	reply := make(chan tui.ApprovalDecision, 1)
	req := tui.ApprovalRequest{Category: cat, Action: action, Detail: detail, Reply: reply}
	select {
	case b.requests <- req:
	case <-ctx.Done():
		return false
	}
	select {
	case d := <-reply:
		switch d {
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
	case <-ctx.Done():
		return false
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
