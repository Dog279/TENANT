// Package approval is the control plane's decision point for dangerous
// actions (TEN-314). A Broker resolves an action to a safety category, applies
// the operator's per-tool rules and per-category modes (ask, allow, deny) and
// session grants, and for "ask" raises a request that any operator surface
// (the terminal UI, the dashboard, Discord, iMessage) can resolve by id.
//
// The package knows no plugin and no surface. The command wires in which
// action belongs to which category, how modes persist, and which backend asks
// the operator.
package approval

import "strings"

// Safety categories: the granular knobs the operator controls. Every dangerous
// action maps to exactly one of them.
const (
	CatExec        = "exec"        // run shell/python/code
	CatWrite       = "write"       // create/modify files on disk
	CatDestructive = "destructive" // irreversible: rm -rf, format, DROP, web purchase/delete
	CatWeb         = "web"         // act on the web unprompted: click/fill/select
	CatSend        = "send"        // outbound comms: email / iMessage / X
	CatEgress      = "egress"      // sensitive data crossing a trust boundary
	// CatMCP governs the agent adopting or reconfiguring tool servers
	// (mcp_add / mcp_remove / mcp_trust): "allow" is the hands-off automation
	// switch — the agent can build a tool server, connect it, and have its tools
	// trusted without a prompt. Each connector's OWN calls are governed by its
	// live trust mode (ask / allow / deny), not by this category.
	CatMCP = "mcp"
	// CatSandbox governs re-running a command OUTSIDE the execution sandbox after
	// the sandbox refused it (action os_exec_unsandboxed). Its own category so
	// "approve session/always" on an escalation grants only further escalations;
	// filed under destructive, it granted every irreversible action.
	CatSandbox = "sandbox"
)

// Categories lists every category in display order.
var Categories = []string{CatExec, CatWrite, CatDestructive, CatWeb, CatSend, CatEgress, CatMCP, CatSandbox}

// CategoryDesc describes each category for /permissions and the dashboard.
var CategoryDesc = map[string]string{
	CatExec:        "run shell / python / code (os_exec)",
	CatWrite:       "create / modify files (os_write/edit/append/mkdir)",
	CatDestructive: "irreversible: rm -rf, format, DROP/ALTER, web purchase/delete",
	CatWeb:         "act on the web unprompted: click, fill, select",
	CatSend:        "send outbound comms: email, iMessage, X posts",
	CatEgress:      "send sensitive tool results across a trust boundary",
	CatMCP:         "adopt / reconfigure tool servers (mcp_add, mcp_remove, mcp_trust)",
	CatSandbox:     "re-run a command outside the execution sandbox after it refused it",
}

// CategoryPlain names each category in plain words for the dashboard and the
// new-category notices: Title is its name, Can what Allow lets the agent do.
var CategoryPlain = map[string]struct{ Title, Can string }{
	CatExec:        {"Run commands", "run commands and programs on this computer"},
	CatWrite:       {"Change files", "create and change files on this computer"},
	CatDestructive: {"Irreversible actions", "do things that can't be undone, like deleting files, dropping database tables or buying something"},
	CatWeb:         {"Act on websites", "click, type and submit forms on websites"},
	CatSend:        {"Send messages", "send email, texts and posts in your name"},
	CatEgress:      {"Share private data", "send private information it has read (mail, files, tickets) to an outside service"},
	CatMCP:         {"Connect remote services", "connect new remote services and trust their tools"},
	CatSandbox:     {"Leave the sandbox", "run a command outside the safety box after the box stopped it"},
}

// OriginalCategories are the categories that predate the known_categories
// marker (TEN-343). A saved permissions map without the marker counts only
// these as chosen: every save since the other three arrived wrote all eight,
// so a key's presence can't say whether the operator ever chose it.
var OriginalCategories = []string{CatExec, CatWrite, CatDestructive, CatWeb, CatSend}

// Categorize maps an action id to its category through the given tables, in
// order. An action no table knows is destructive: the safest default, since
// destructive always asks unless the operator opened it.
func Categorize(action string, tables ...func(action string) (string, bool)) string {
	for _, table := range tables {
		if cat, ok := table(action); ok {
			return cat
		}
	}
	return CatDestructive
}

// Mode is a category's setting.
type Mode int

const (
	ModeAsk   Mode = iota // prompt the operator (default — safe)
	ModeAllow             // always allow, no prompt
	ModeDeny              // always block
)

func (m Mode) String() string {
	switch m {
	case ModeAllow:
		return "allow"
	case ModeDeny:
		return "deny"
	default:
		return "ask"
	}
}

// ParseMode reads a mode and its synonyms. ok is false for anything else.
func ParseMode(s string) (Mode, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "ask", "prompt", "confirm":
		return ModeAsk, true
	case "allow", "on", "always", "yes":
		return ModeAllow, true
	case "deny", "off", "block", "no":
		return ModeDeny, true
	}
	return ModeAsk, false
}

// SavedModes keeps the entries of a persisted category → mode map that name a
// known category and a valid mode: the ones LoadModes applies.
func SavedModes(saved map[string]string) map[string]Mode {
	out := map[string]Mode{}
	for cat, ms := range saved {
		if !isCategory(cat) {
			continue
		}
		if m, ok := ParseMode(ms); ok {
			out[cat] = m
		}
	}
	return out
}

func isCategory(cat string) bool {
	for _, c := range Categories {
		if c == cat {
			return true
		}
	}
	return false
}
