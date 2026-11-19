package main

import (
	"encoding/json"
	"log/slog"
	"sort"
	"sync"

	"tenant/internal/approval"
)

// approval.go wires the approval broker (internal/approval, TEN-314) to this
// command: which category each action id belongs to (the host's own actions
// and every plugin's), and what the launch allow-flags pre-approve.

// The broker's names as package main knows them.
type (
	approvalBroker = approval.Broker
	permMode       = approval.Mode
	toolRules      = approval.Rules
)

const (
	catExec        = approval.CatExec
	catWrite       = approval.CatWrite
	catDestructive = approval.CatDestructive
	catWeb         = approval.CatWeb
	catSend        = approval.CatSend
	catEgress      = approval.CatEgress
	catMCP         = approval.CatMCP
	catSandbox     = approval.CatSandbox

	modeAsk   = approval.ModeAsk
	modeAllow = approval.ModeAllow
	modeDeny  = approval.ModeDeny
)

var (
	catOrder         = approval.Categories
	defaultToolRules = approval.DefaultRules
	// defaultFleet holds each fleet machine's approval scheme and the fleet's
	// rules for new machines (internal/approval/fleet.go). Process-wide, like
	// the tool rules: serve and the TUI load it from settings and set its
	// persist hook (loadFleetSettings), and the settings watcher re-reads it.
	defaultFleet = approval.NewFleet(categorize)
)

// loadFleetSettings loads the fleet's approval schemes from the settings
// file's fleet_schemes and fleet_rules, tells the operator about each entry
// that could not be read (that machine is refused everything until it is
// fixed), and saves them back through stg whenever one changes: "always" on a
// machine's card, an edit in the permissions editor, a machine confirmed or
// revoked. The snapshot is taken, and stg and the file change, in one hold of
// mu, the lock applySettingsFile reads the file under: the file always gets
// the fleet's latest state, and the watcher never reads one that is older
// than what the fleet believes it wrote.
func loadFleetSettings(stg *settings, mu *sync.Mutex, cfgDir, agentID string, tell func(string)) {
	_, problems := defaultFleet.Load(stg.FleetSchemes, stg.FleetRules)
	for _, p := range problems {
		tell("permissions: " + p)
	}
	defaultFleet.SetPersist(func(snapshot func() (map[string]json.RawMessage, json.RawMessage)) {
		mu.Lock()
		defer mu.Unlock()
		stg.FleetSchemes, stg.FleetRules = snapshot()
		saveSettingsOrTell(stg, cfgDir, agentID, tell)
	})
}

// readOnlyExec judges a shell command that only looks (ls, git status, rg …):
// the OS plugin installs its classifier at init (plugin_os.go), since the
// switchboards read the plugin registry rather than a plugin. Such a command
// runs without a prompt here and on Discord (TEN-491, D4); the iMessage
// broker stays deny-by-default.
var readOnlyExec func(command string) bool

// newApprovalBroker builds the host's broker, filing actions by categorize.
func newApprovalBroker(log *slog.Logger) *approvalBroker {
	b := approval.NewBroker(log, categorize)
	b.SetReadOnly(readOnlyExec)
	return b
}

func newOffsiteApprovalBroker(log *slog.Logger, host *approvalBroker) *approvalBroker {
	return approval.NewOffsiteBroker(log, host)
}

func newDiscordApprovalBroker(log *slog.Logger) *approvalBroker {
	b := approval.NewDiscordBroker(log, categorize)
	b.SetReadOnly(readOnlyExec)
	return b
}

func newToolRules() *toolRules { return approval.NewRules() }

func parseMode(s string) (permMode, bool) { return approval.ParseMode(s) }

func savedModes(saved map[string]string) map[string]permMode { return approval.SavedModes(saved) }

// hostActionCategories maps the approval action ids the host raises itself to
// their category. Every plugin declares its own in its descriptor
// (plugin_<id>.go). categorize and `tenant permissions effective` both read
// them through allActionCategories, so the two cannot disagree about which
// actions a category holds (TEN-338).
var hostActionCategories = map[string]string{
	// Re-running a command OUTSIDE the execution sandbox after it refused
	// something (execprofiles.go): its own category (ask by default; deny on
	// iMessage), never swept into exec or destructive grants.
	actionExecUnsandboxed: catSandbox,

	"mcp_add": catMCP, "mcp_remove": catMCP, "mcp_trust": catMCP,

	"sensitive_egress": catEgress,
}

// allActionCategories is every known action id with its category: the host's
// own and every plugin's.
func allActionCategories() map[string]string {
	out := plugins().Actions()
	for action, cat := range hostActionCategories {
		out[action] = cat
	}
	return out
}

// categorize maps an action id to a safety category: the host's own table,
// then the plugins'. An action neither knows is destructive, the safest
// default (approval.Categorize).
func categorize(action string) string {
	return approval.Categorize(action, hostActionCategory, plugins().ActionCategory)
}

func hostActionCategory(action string) (string, bool) {
	cat, ok := hostActionCategories[action]
	return cat, ok
}

// launchFlagGrants maps each category a launch --*-allow-* flag pre-approves
// to the flag that does it. seedFromFlags applies it and `tenant permissions
// effective` reports it (TEN-338), so both read the flags the same way.
// Destructive is never here: no flag blanket-approves irreversible actions.
func launchFlagGrants(pf *pluginFlags) map[string]string {
	grants := map[string]string{}
	if pf == nil {
		return grants
	}
	// Each plugin declares what its allow-flags grant; the first flag to
	// grant a category is the one reported.
	for _, d := range plugins().All() {
		if d.Grants == nil {
			continue
		}
		g := d.Grants(pf)
		cats := make([]string, 0, len(g))
		for cat := range g {
			cats = append(cats, cat)
		}
		sort.Strings(cats)
		for _, cat := range cats {
			if _, ok := grants[cat]; !ok {
				grants[cat] = g[cat]
			}
		}
	}
	return grants
}

// seedFromFlags lifts the launch --allow-* flags into category modes, so
// existing flags still work: they pre-approve a whole category.
func seedFromFlags(b *approvalBroker, pf *pluginFlags) {
	for cat := range launchFlagGrants(pf) {
		b.Allow(cat)
	}
}
