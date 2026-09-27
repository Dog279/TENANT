package main

// skills_dashctl.go adapts skillControl to dashboard.SkillControl (TEN-202):
// the web Skills page reuses the SAME skill store + auto-accept logic the TUI
// /skills does, mapping tui.SkillInfo to the dashboard's structured view. No
// business logic forks.

import "tenant/internal/dashboard"

type dashSkill struct{ c skillControl }

func (d dashSkill) Skills() []dashboard.SkillView {
	in := d.c.SkillList()
	out := make([]dashboard.SkillView, 0, len(in))
	for _, sk := range in {
		v := dashboard.SkillView{
			Name:        sk.Name,
			Description: sk.Description,
			Status:      sk.Status,
			Enabled:     sk.Enabled,
			Untrusted:   !sk.Trusted,
			Origin:      sk.Origin,
		}
		// An untrusted skill waits for review: show what it would add.
		if v.Untrusted {
			if det, err := d.c.SkillDetail(sk.Name); err == nil && det != nil {
				v.Recipe, v.Source, v.RequiresTools = det.Recipe, det.Source, det.RequiresTools
			}
		}
		out = append(out, v)
	}
	return out
}

func (d dashSkill) SetTrust(name string, trusted bool) (bool, error) {
	return d.c.SetSkillTrust(name, trusted)
}

func (d dashSkill) Accept(name string) (bool, error) { return d.c.AcceptSkill(name) }
func (d dashSkill) SetEnabled(name string, on bool) (bool, error) {
	return d.c.SetSkillEnabled(name, on)
}
func (d dashSkill) Forget(name string) (bool, error) { return d.c.ForgetSkill(name) }
func (d dashSkill) AutoAcceptMode() string           { return d.c.AutoAcceptMode() }
func (d dashSkill) SetAutoAccept(mode string) error  { return d.c.SetAutoAccept(mode) }
