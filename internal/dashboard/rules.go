package dashboard

// rules.go: the "Tool rules" card on the Tools page — per-tool permission rules
// (pattern → ask | allow | deny) checked before the safety category on every
// surface. This is where an operator answers "what about THIS tool?" for tools
// the categories cannot know in advance: a newly adopted connector's tools, an
// unusual action id. Same nil-safe pattern as the other controls.

import (
	"net/http"
	"net/url"
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

func (s *Server) handleRuleAddForm(w http.ResponseWriter, r *http.Request) {
	if s.rules == nil {
		http.Redirect(w, r, "/tools", http.StatusSeeOther)
		return
	}
	pattern := strings.TrimSpace(r.FormValue("pattern"))
	mode := strings.ToLower(strings.TrimSpace(r.FormValue("mode")))
	if pattern == "" || (mode != "ask" && mode != "allow" && mode != "deny") {
		redirectTools(w, r, "", "A rule needs a tool name or pattern and a mode (ask, allow or deny).")
		return
	}
	if err := s.rules.SetRule(pattern, mode); err != nil {
		redirectTools(w, r, "", "Couldn't add that rule: "+err.Error())
		return
	}
	redirectTools(w, r, "Rule added: "+pattern+" → "+mode+".", "")
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
		redirectTools(w, r, "", "Couldn't remove that rule: "+err.Error())
	case !removed:
		redirectTools(w, r, "", "No rule "+pattern+".")
	default:
		redirectTools(w, r, "Rule removed: "+pattern+".", "")
	}
}

func redirectTools(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	target := "/tools"
	switch {
	case errMsg != "":
		target += "?err=" + url.QueryEscape(errMsg)
	case msg != "":
		target += "?msg=" + url.QueryEscape(msg)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
