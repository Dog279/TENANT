package main

// plugindesc.go is the plugin descriptor (TEN-321): everything the host knows
// about one first-party plugin, declared once in that plugin's own
// plugin_<id>.go. The registry (internal/pluginreg) checks the descriptors
// against each other and indexes them; the stub catalog and its activators,
// the approval categories, /configure, the dashboard's Integrations page,
// `tenant setup`, doctor and the CLI all read it instead of keeping their own
// per-plugin switchboards.

import (
	"context"
	"log/slog"
	"sort"
	"sync"

	"tenant/internal/model"
	"tenant/internal/pluginreg"
)

// pluginDescriptor declares one first-party plugin.
type pluginDescriptor struct {
	// ID is the plugin's one name: its tool-mux label, its key under skills in
	// config.json, and the <id> of its secrets (skill:<id>:<field>) in
	// credentials.json.
	ID string
	// Title names the plugin for people.
	Title string
	// Version is the version of the plugin's saved settings shape
	// (config.json skills.<ID>.settings); bump it when that shape changes.
	Version int

	// Tools lists the plugin's tool specs without a configured service: the
	// stub advertises them, and the registry indexes which plugin owns which
	// tool.
	Tools func() []model.ToolSpec

	// Configure returns the plugin's config schema: its settings and secret
	// fields with their validators, its liveness probe and its setup hint,
	// shared by /configure, the Integrations page and `tenant setup`. Nil: the
	// plugin is configured by launch flags alone.
	Configure func() skillKind
	// SetupOnly marks a schema only `tenant setup` walks (path- or flag-driven
	// plugins); /configure lists the plugin as legacy.
	SetupOnly bool
	// Setup is the plugin's place in `tenant setup` (0: not offered there).
	Setup int

	// Actions maps each approval action id the plugin raises to its category:
	// the plugin's permission classes.
	Actions map[string]string

	// StubHint is what an unconfigured plugin's tools answer with.
	StubHint string
	// PerAgent plugins are registered on each agent's own mux and never in
	// the shared stub catalog (web: every agent drives its own browser).
	PerAgent bool
	// Activate builds the plugin from saved config on its first /enable. Nil:
	// enabling it needs a relaunch with the plugin's flags.
	Activate func(ctx context.Context, e *pluginEnv) (plugin, func(), error)

	// Doctor lists the plugin's health checks, in the order doctor runs them.
	Doctor []doctorCheck

	// CLI lists the plugin's commands.
	CLI []*cliCommand
}

// PluginID, ToolNames and ActionCategories make a descriptor a
// pluginreg.Declared.
func (d *pluginDescriptor) PluginID() string { return d.ID }

func (d *pluginDescriptor) ToolNames() []string {
	if d.Tools == nil {
		return nil
	}
	specs := d.Tools()
	names := make([]string, len(specs))
	for i, s := range specs {
		names[i] = s.Name
	}
	return names
}

func (d *pluginDescriptor) ActionCategories() map[string]string { return d.Actions }

// pluginEnv is what a descriptor's lifecycle hooks get from the host.
type pluginEnv struct {
	c  *commonFlags
	pf *pluginFlags
	// confirm is the approval gate for the plugin's gated actions; nil when
	// the command runs without one (one-shot CLI turns).
	confirm func(context.Context, string, string) bool
	log     *slog.Logger
}

// gateAllow is a plugin's allow flag as its policy should see it: when an
// approval broker is wired it is the single decision point, so the per-action
// flag drops to false and every gated action goes through confirm. The
// auth/scope-level settings are untouched.
func (e *pluginEnv) gateAllow(flag bool) bool {
	if e.confirm != nil {
		return false
	}
	return flag
}

// hostToolLabels are the mux labels the host registers itself; no plugin may
// take one.
var hostToolLabels = []string{"memory", "recall", "skills", "orchestrator", "team", "cron", "connectors", "mcp:*", "peer:*"}

var (
	pluginsOnce sync.Once
	pluginsReg  *pluginreg.Registry[*pluginDescriptor]
)

// plugins is the registry of first-party plugins, in the order the stub
// catalog, doctor and the configure surfaces list them. Adding a plugin means
// writing its plugin_<id>.go and naming its descriptor here.
func plugins() *pluginreg.Registry[*pluginDescriptor] {
	pluginsOnce.Do(func() {
		pluginsReg = pluginreg.MustNew(pluginreg.Rules{Categories: catOrder, Reserved: hostToolLabels},
			sqlPlugin(), wikiPlugin(), webPlugin(), osPlugin(), gsuitePlugin(), xPlugin(), imessagePlugin(),
			crmPlugin(), discordPlugin(), atlassianPlugin(), githubPlugin(), simplefinPlugin(),
		)
	})
	return pluginsReg
}

// setupPlugins lists the plugins `tenant setup` offers, in its order.
func setupPlugins() []*pluginDescriptor {
	var out []*pluginDescriptor
	for _, d := range plugins().All() {
		if d.Setup > 0 && d.Configure != nil {
			out = append(out, d)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Setup < out[j].Setup })
	return out
}

// setupOnlyKinds is the config schema of every plugin only `tenant setup`
// configures, by id.
func setupOnlyKinds() map[string]skillKind {
	out := map[string]skillKind{}
	for _, d := range plugins().All() {
		if d.SetupOnly && d.Configure != nil {
			out[d.ID] = d.Configure()
		}
	}
	return out
}

// registerPluginStubs registers a disabled stub for every shared plugin the
// launch path did not build, with its activator when it has one, so the
// whole catalog shows in /tools. A stub advertises the real tool specs but
// answers "needs setup" if called; it costs nothing until /enable.
func registerPluginStubs(ctx context.Context, mux *toolMux, e *pluginEnv) {
	for _, d := range plugins().All() {
		if d.PerAgent || d.StubHint == "" || mux.HasPlugin(d.ID) {
			continue
		}
		mux.Add(d.ID, stubPlugin{Specs: d.Tools(), Hint: d.StubHint})
		mux.SetEnabled(d.ID, false)
		if d.Activate != nil {
			activate := d.Activate
			mux.RegisterActivator(d.ID, func() (plugin, func(), error) { return activate(ctx, e) })
		}
	}
}

// init fills the /configure catalog (skillKinds) from the registry: one entry
// per plugin whose config schema /configure manages. The only init() in this
// package (audit P2 contract from TEN-64).
func init() {
	for _, d := range plugins().All() {
		if d.Configure != nil && !d.SetupOnly {
			skillKinds[d.ID] = d.Configure()
		}
	}
}

// pluginCommands lists every plugin's CLI commands, in registry order.
func pluginCommands() []*cliCommand {
	var out []*cliCommand
	for _, d := range plugins().All() {
		out = append(out, d.CLI...)
	}
	return out
}

// pluginDoctorChecks lists every plugin's doctor checks, in registry order.
func pluginDoctorChecks() []doctorCheck {
	var out []doctorCheck
	for _, d := range plugins().All() {
		out = append(out, d.Doctor...)
	}
	return out
}
