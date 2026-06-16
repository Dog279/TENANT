package main

// integrations_dashctl.go adapts the skill-config control to
// dashboard.IntegrationsControl (TEN-206): the web Integrations page reuses the
// SAME /configure machinery as the TUI. The dashboard's control is built
// WITHOUT the Atlassian-MCP connector — OAuth-server connects go through the
// MCP page (TEN-205) on the hybrid model — so this surface covers key-based
// integrations + probe + clear. No business logic forks.

import "tenant/internal/dashboard"

type dashIntegrations struct{ c *skillCfgControl }

func (d dashIntegrations) Integrations() []dashboard.IntegrationView {
	in := d.c.SkillList()
	out := make([]dashboard.IntegrationView, 0, len(in))
	for _, s := range in {
		if s.Legacy {
			continue // hide wizard-only skills; show the /configure framework catalog
		}
		out = append(out, dashboard.IntegrationView{
			ID:         s.ID,
			Label:      s.Label,
			Configured: s.Configured,
			Enabled:    s.Enabled,
			SetupHint:  s.SetupHint,
		})
	}
	return out
}

func (d dashIntegrations) Fields(id string) ([]dashboard.IntegrationField, error) {
	in, err := d.c.SkillFields(id)
	if err != nil {
		return nil, err
	}
	out := make([]dashboard.IntegrationField, 0, len(in))
	for _, f := range in {
		out = append(out, dashboard.IntegrationField{
			Key:          f.Key,
			Prompt:       f.Prompt,
			Secret:       f.Secret,
			Required:     f.Required,
			Default:      f.Default,
			Options:      f.Options,
			OptionLabels: f.OptionLabels,
		})
	}
	return out, nil
}

// Configure stores credentials only (noEnable=true) so the dashboard request
// never blocks on an auto-probe that might open a host browser. The operator
// verifies with Test (async) afterward.
func (d dashIntegrations) Configure(id string, values map[string]string) (string, error) {
	args := []string{id}
	for k, v := range values {
		args = append(args, k+"="+v)
	}
	return d.c.SkillConfigure(args, true)
}

func (d dashIntegrations) Probe(id string) (string, error) { return d.c.SkillProbe(id) }

// Disconnect clears the integration's first required field — which the
// skill-config layer treats as a disable trigger (clearing a required field
// auto-disables the integration).
func (d dashIntegrations) Disconnect(id string) (string, error) {
	fields, err := d.c.SkillFields(id)
	if err != nil {
		return "", err
	}
	field := ""
	for _, f := range fields {
		if f.Required {
			field = f.Key
			break
		}
	}
	if field == "" && len(fields) > 0 {
		field = fields[0].Key
	}
	return d.c.SkillClear(id, field)
}
