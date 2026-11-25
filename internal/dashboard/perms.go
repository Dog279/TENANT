package dashboard

// perms.go is the input of the two controls a safety setting and a rule list
// are changed with, wherever they are (TEN-546 ask 2, parity): perm-seg, the
// Ask / Allow / Deny segmented control of /safety, a fleet machine's page and
// the fleet's defaults; and rule-editor, the rule list and its add form of
// /tools, a fleet machine, the fleet's defaults and a group. The partials are
// in templates/ui.html, so every page set has them.

import "strings"

// PermSeg is one safety category's segmented control. Action is the route
// its forms post to, always a literal of this package (F1); Hidden are the
// fields each form carries besides category and mode (a machine's id).
// Mode "" is unset, which only a control with Unset offers: the fleet's
// defaults, whose fourth state is "From this Tenant".
type PermSeg struct {
	Action                string
	Category, Title, Mode string
	Allow                 string // the Allow step's consequence
	Hidden                []HiddenField
	Unset                 bool
	UnsetSaid             string // the From this Tenant step's consequence
}

// AllowStep is the confirm step of Allow (DESIGN.md §5 (c)).
func (p PermSeg) AllowStep() (ConfirmView, error) {
	v, err := confirmGo(p.Action, "Allow", p.Allow, append(flat(p.Hidden), "category", p.Category, "mode", "allow")...)
	return inlineStep(v), err
}

// UnsetStep is the confirm step of From this Tenant: this Tenant's own mode
// may be Allow.
func (p PermSeg) UnsetStep() (ConfirmView, error) {
	v, err := confirmGo(p.Action, "From this Tenant", p.UnsetSaid, append(flat(p.Hidden), "category", p.Category, "mode", "unset")...)
	return inlineStep(v), err
}

// RuleEditor is a rule list with its add form. Add and Remove are the
// routes, literals of this package; Hidden the fields each form carries
// besides pattern and mode. AllowSaid is the Don't ask step's consequence,
// "" where adding an allow rule moves nothing (a group's). RemoveSaid is the
// consequence of removing a rule that is not allow, "%s" standing for the
// rule; "" removes every rule in one tap.
type RuleEditor struct {
	Add, Remove        string
	Hidden             []HiddenField
	Rules              []ToolRuleRow
	Empty, Placeholder string
	AllowSaid          string
	RemoveSaid         string
}

// RemoveStep is the confirm step of removing r.
func (e RuleEditor) RemoveStep(r ToolRuleRow) (ConfirmView, error) {
	v, err := confirmGo(e.Remove, "Remove", strings.ReplaceAll(e.RemoveSaid, "%s", r.Plain()), append(flat(e.Hidden), "pattern", r.Pattern)...)
	return inlineStep(v), err
}

func flat(fields []HiddenField) []string {
	out := make([]string, 0, 2*len(fields)+4)
	for _, f := range fields {
		out = append(out, f.Name, f.Value)
	}
	return out
}

// Seg is the row's segmented control on /safety.
func (m ModeRow) Seg() PermSeg {
	return PermSeg{Action: "/safety/mode", Category: m.Category, Title: m.Title, Mode: m.Mode, Allow: m.Allow}
}

// Editor is the Tools page's rule editor.
func (d toolsData) Editor() RuleEditor {
	return RuleEditor{Add: "/tools/rules/add", Remove: "/tools/rules/remove", Rules: d.Rules,
		Empty:       "No rules yet. A tool your safety settings don't cover asks you every time; add its name here to let it run, block it, or always ask.",
		Placeholder: "a tool name, for example write_note or delete_*",
		AllowSaid:   "Your agent can use the tools this rule matches without asking you, whatever their safety setting says.",
		RemoveSaid:  "%s then follows its safety setting, which may let your agent use it without asking you."}
}
