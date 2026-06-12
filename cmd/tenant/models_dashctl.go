package main

// models_dashctl.go adapts modelControl to dashboard.ModelControl (TEN-204):
// the web Model page reuses the SAME backend registry + live-swap logic the
// TUI /model does, mapping tui.ModelInfo to the dashboard's structured view.
// No business logic forks. Every call is synchronous (no browser), unlike the
// MCP connect flow.

import "tenant/internal/dashboard"

type dashModel struct{ mc *modelControl }

func (d dashModel) Models() []dashboard.ModelView {
	in := d.mc.ModelList()
	out := make([]dashboard.ModelView, 0, len(in))
	for _, m := range in {
		out = append(out, dashboard.ModelView{
			Name:     m.Name,
			Kind:     m.Kind,
			Model:    m.Model,
			Active:   m.Active,
			Degraded: m.Degraded,
		})
	}
	return out
}

// Use switches the active backend (modelOverride "" keeps the saved variant).
func (d dashModel) Use(name string) (string, error) {
	status, _, err := d.mc.UseModel(name, "")
	return status, err
}

func (d dashModel) AddCloud(kind, apiKey string) (string, error) {
	return d.mc.AddCloudModel(kind, apiKey)
}
func (d dashModel) Remove(name string) (string, error)   { return d.mc.RemoveModel(name) }
func (d dashModel) LoopCeiling() int                     { return d.mc.LoopCeiling() }
func (d dashModel) SetLoopCeiling(n int) (string, error) { return d.mc.SetLoopCeiling(n) }
func (d dashModel) ReloadKeys() (string, error)          { return d.mc.ReloadKeys() }
