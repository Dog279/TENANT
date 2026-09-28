package dashboard

// rules.go: the "Tool rules" card on the Tools page — per-tool permission rules
// (pattern → ask | allow | deny) checked before the safety category on every
// surface. This is where an operator answers "what about THIS tool?" for tools
// the categories cannot know in advance: a newly adopted connector's tools, an
// unusual action id. Same nil-safe pattern as the other controls.

import (
	"net/http"
	"strings"
)

// ToolRuleRow is one rule, render-ready.
type ToolRuleRow struct {
	Pattern string
	Mode    string // ask | allow | deny
	Kind    string // exact | name | glob
}

// RuleControl is the per-tool rule surface cmd/tenant wires (dashRules).
type RuleControl interface {
	Rules() []ToolRuleRow
	SetRule(pattern, mode string) error
	RemoveRule(pattern string) (bool, error)
}

// SetRules installs the rule control after construction (mirrors SetCron).
func (s *Server) SetRules(c RuleControl) { s.rules = c }

// Plain is the rule's pattern as the page shows it: a remote service's tool
// id ("mcp:<service>:<tool>", "*" for all of them) is named in words.
func (r ToolRuleRow) Plain() string {
	rest, ok := strings.CutPrefix(r.Pattern, "mcp:")
	i := strings.LastIndexByte(rest, ':')
	if !ok || i <= 0 {
		return r.Pattern
	}
	if tool := rest[i+1:]; tool != "*" {
		return tool + " on " + rest[:i]
	}
	return "every tool on " + rest[:i]
}

// ruleWords says what a rule's mode does to the tools it matches.
var ruleWords = map[string]string{
	"ask":   "asks you first",
	"allow": "runs without asking",
	"deny":  "is blocked",
}

func (s *Server) handleRuleAddForm(w http.ResponseWriter, r *http.Request) {
	if s.rules == nil {
		http.Redirect(w, r, "/tools", http.StatusSeeOther)
		return
	}
	pattern := strings.TrimSpace(r.FormValue("pattern"))
	mode := strings.ToLower(strings.TrimSpace(r.FormValue("mode")))
	if pattern == "" || ruleWords[mode] == "" {
		redirectFlash(w, r, "/tools", "", "A rule needs a tool name or pattern, and Ask first, Don't ask or Block.")
		return
	}
	if err := s.rules.SetRule(pattern, mode); err != nil {
		redirectFlash(w, r, "/tools", "", s.flashFor(r.Context(), "tool rule add", err, "Couldn't add that rule. Check the tool name or pattern."))
		return
	}
	redirectFlash(w, r, "/tools", "Rule added: "+ToolRuleRow{Pattern: pattern}.Plain()+" "+ruleWords[mode]+".", "")
}

func (s *Server) handleRuleRemoveForm(w http.ResponseWriter, r *http.Request) {
	if s.rules == nil {
		http.Redirect(w, r, "/tools", http.StatusSeeOther)
		return
	}
	pattern := strings.TrimSpace(r.FormValue("pattern"))
	removed, err := s.rules.RemoveRule(pattern)
	switch {
	case err != nil:
		redirectFlash(w, r, "/tools", "", s.flashFor(r.Context(), "tool rule remove", err, "Couldn't remove that rule."))
	case !removed:
		redirectFlash(w, r, "/tools", "", "There's no rule for "+pattern+".")
	default:
		redirectFlash(w, r, "/tools", "Rule removed: "+ToolRuleRow{Pattern: pattern}.Plain()+".", "")
	}
}
