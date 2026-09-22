package main

// rules_dashctl.go adapts the process-wide tool rule set to
// dashboard.RuleControl — the "Tool rules" card on the dashboard's Tools page.

import (
	"tenant/internal/dashboard"
)

type dashRules struct{ r *toolRules }

func (d dashRules) Rules() []dashboard.ToolRuleRow {
	in := d.r.Rules()
	out := make([]dashboard.ToolRuleRow, 0, len(in))
	for _, ri := range in {
		out = append(out, dashboard.ToolRuleRow{Pattern: ri.Pattern, Mode: ri.Mode, Kind: ri.Kind})
	}
	return out
}
func (d dashRules) SetRule(pattern, mode string) error      { return d.r.Set(pattern, mode) }
func (d dashRules) RemoveRule(pattern string) (bool, error) { return d.r.Remove(pattern), nil }
