package main

import (
	"context"
	"errors"
)

// plugin_events.go writes each plugin's lifecycle through its own catalog,
// its descriptor's Events (TEN-393). The plugin writes the rest itself.

// logStart logs how building the plugin went, at launch or on its first
// /enable. Not being configured yet is a state, not a failure.
func (d *pluginDescriptor) logStart(ctx context.Context, err error) {
	switch {
	case d.Events == nil || errors.Is(err, errNotConfigured):
	case err != nil:
		d.Events.StartFailed.Log(ctx, "err", err)
	default:
		d.Events.Started.Log(ctx)
	}
}

// logPluginToggle is every mux's toggle hook: it logs the operator turning a
// plugin, or one of its tools, on or off.
func logPluginToggle(label, target string, on bool, tools int) {
	d, ok := plugins().Get(label)
	if !ok || d.Events == nil {
		return // a connector, a peer or a host tool
	}
	def := d.Events.Disabled
	if on {
		def = d.Events.Enabled
	}
	def.Log(context.Background(), "target", target, "tools", tools)
}
