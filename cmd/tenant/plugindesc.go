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
	"flag"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
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
	// SetupOrder is the plugin's place in `tenant setup` (0: not offered).
	SetupOrder int

	// Flags binds the plugin's launch flags and returns where they land: the
	// plugin's own struct, read back with flagsOf.
	Flags func(fs *flag.FlagSet) any
	// On reports whether the launch flags turn the plugin on.
	On func(pf *pluginFlags) bool
	// ApplyConfig folds the plugin's saved config into its launch flags
	// wherever the operator passed none: explicit flags always win.
	ApplyConfig func(pf *pluginFlags, s savedConfig)
	// ExpandPaths expands ~ in the plugin's path-valued flags.
	ExpandPaths func(pf *pluginFlags)
	// Grants maps each approval category the plugin's allow-flags
	// pre-approve to the flag that does it.
	Grants func(pf *pluginFlags) map[string]string
	// Launch builds the plugin at launch when its flags configure it.
	// Nothing built means "not configured here": the stub stands in. A
	// plugin built but not turned on by its flags (On false) is registered
	// disabled.
	Launch func(ctx context.Context, e *pluginEnv) (built []plugin, cleanup func(), err error)
	// Prepare creates what the plugin needs on first run, reporting each
	// action (ensureSetup); ensureDir makes a directory and reports it.
	Prepare func(pf *pluginFlags, ensureDir func(label, dir string), lines *[]string)
	// Eval turns the plugin on for `tenant eval` when the operator set none
	// of its flags, and reports whether it did.
	Eval func(pf *pluginFlags, c *commonFlags, explicit map[string]bool) bool

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
	// Relaunch, for a plugin with no Activate, rebuilds it on /enable by
	// re-reading its saved config and re-running Launch (TEN-483). Only for a
	// Launch that builds one plugin and touches nothing else in pluginEnv.
	Relaunch bool

	// AdaptConfigure adjusts the plugin's config schema to this install
	// (embedded OAuth credentials, a cache dir, a connector), for /configure
	// and `tenant setup` alike.
	AdaptConfigure func(k skillKind, h configureHost) skillKind
	// SelfEnables reports that a configure with these settings already
	// brought the plugin's tools live some other way, so /configure must not
	// auto-enable the plugin itself.
	SelfEnables func(settings map[string]string) bool
	// OnConfigured runs after a configure whose probe passed; what it writes
	// to out is shown to the operator.
	OnConfigured func(sc *skillCfgControl, lc *launchConfig, creds *credentials, out *strings.Builder)

	// Connector marks a plugin that stores credentials or files for a
	// connected service: `tenant privacy forget --connector <id>` removes its
	// skill:<id>:* credentials and its Forget files, and privacy status and
	// export list it.
	Connector bool
	// Forget lists the files, besides its credentials, that forgetting the
	// connector deletes: token caches and the plugin's own data.
	Forget func(cfgDir, dataDir string) []string
	// ConfigSecrets and DataSecrets list the plugin's at-rest secret files in
	// the config and data dirs (token caches, an OAuth client). doctor audits
	// and repairs their protection, the file tools refuse them, and backup
	// files them under their directory.
	ConfigSecrets func(cfgDir string) []string
	DataSecrets   func(dataDir string) []string
	// ConfigFiles lists the plugin's own settings files in the config dir that
	// are not secret but decide what the agent may do (the GitHub merge rules):
	// the file tools may read them but never write them, and backup files them
	// under the config dir.
	ConfigFiles func(cfgDir string) []string
	// PointedSecrets lists secret files the operator pointed the plugin at (a
	// service-account key, an OAuth client), by flag or saved setting: the
	// file tools refuse them.
	PointedSecrets func(pf *pluginFlags, settings map[string]string) []string
	// External names what the plugin uses outside Tenant's directories (a
	// wiki vault, a database), path to what it is, from its saved settings,
	// so backup can say it does not reach it.
	External func(settings map[string]string) map[string]string
	// Keys are the plugin's rows on the keys page: credentials an operator
	// pastes in.
	Keys []keySpec

	// Doctor lists the plugin's health checks, in the order doctor runs them.
	Doctor []doctorCheck

	// CLI lists the plugin's commands.
	CLI []*cliCommand

	// Events is the plugin's event catalog (TEN-393): its provider,
	// plugin.<id>, declared in its package with everything it logs. The host
	// writes its lifecycle through it (plugin_events.go). Nil only for the
	// channels, whose events are their own providers'.
	Events *pluginreg.Events
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

func (d *pluginDescriptor) PluginEvents() *pluginreg.Events { return d.Events }

// pluginEnv is what a descriptor's lifecycle hooks get from the host.
type pluginEnv struct {
	c  *commonFlags
	pf *pluginFlags
	// confirm is the approval gate for the plugin's gated actions; nil when
	// the command runs without one (one-shot CLI turns).
	confirm func(context.Context, string, string) bool
	log     *slog.Logger
	router  *model.Router

	// wiki is the live wiki dir and index, set by the wiki plugin's launch
	// (and a rebuild), for the callers that write a note and reindex (TEN-44).
	wiki *wikiLive
}

// savedConfig is one plugin's saved config (config.json skills.<id> and its
// credentials), as ApplyConfig reads it.
type savedConfig struct {
	settings map[string]string
	secret   func(field string) string // the value of skill:<id>:<field>
	set      map[string]bool           // the launch flags passed explicitly
}

func (s savedConfig) get(key string) string { return s.settings[key] }

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

// plugins is the registry of first-party plugins, in the order they are
// built at launch and listed by the stub catalog, doctor and the CLI. Adding a plugin means
// writing its plugin_<id>.go and naming its descriptor here.
func plugins() *pluginreg.Registry[*pluginDescriptor] {
	pluginsOnce.Do(func() {
		pluginsReg = pluginreg.MustNew(pluginreg.Rules{Categories: catOrder, Reserved: hostToolLabels},
			sqlPlugin(), wikiPlugin(), webPlugin(), gsuitePlugin(), atlassianPlugin(), githubPlugin(),
			xPlugin(), imessagePlugin(), discordPlugin(), osPlugin(), crmPlugin(), simplefinPlugin(),
		)
	})
	return pluginsReg
}

// setupPlugins lists the plugins `tenant setup` offers, in its order.
func setupPlugins() []*pluginDescriptor {
	var out []*pluginDescriptor
	for _, d := range plugins().All() {
		if d.SetupOrder > 0 && d.Configure != nil {
			out = append(out, d)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].SetupOrder < out[j].SetupOrder })
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

// activatorFor builds d from its saved config on /enable (and again after a
// /disable, so config edits apply without a restart).
func activatorFor(ctx context.Context, d *pluginDescriptor, e *pluginEnv) func() (plugin, func(), error) {
	activate := d.Activate
	if activate == nil {
		activate = func(ctx context.Context, e *pluginEnv) (plugin, func(), error) { return relaunchFromConfig(ctx, d, e) }
	}
	return func() (plugin, func(), error) {
		p, cleanup, err := activate(ctx, e)
		d.logStart(ctx, err)
		return p, cleanup, err
	}
}

// relaunchFromConfig builds d the way the launch did, from what config.json and
// the credentials say now, so a changed setting (the SQL file, the BlueBubbles
// URL) applies on the next /enable. Launch-only flags such as --sql-allow-write
// fall back to their defaults, the stricter setting. A plugin with no saved
// config keeps its launch flags.
func relaunchFromConfig(ctx context.Context, d *pluginDescriptor, e *pluginEnv) (plugin, func(), error) {
	lc, err := loadLaunchConfig(e.c.cfgDir)
	if err != nil {
		return nil, nil, fmt.Errorf("%s plugin: load config: %w", d.ID, err)
	}
	re := *e
	if sc := lc.Skills[d.ID]; sc != nil && d.ApplyConfig != nil {
		re.pf = &pluginFlags{}
		id := d.ID
		d.ApplyConfig(re.pf, savedConfig{settings: sc.Settings, secret: func(field string) string {
			return resolveSecretWith(e.c.credentialsLoader(), e.c.cfgDir, skillSecretID(id, field), authCfg{Stored: true})
		}})
		if d.ExpandPaths != nil {
			d.ExpandPaths(re.pf)
		}
	}
	built, cleanup, err := d.Launch(ctx, &re)
	if err != nil {
		return nil, nil, err
	}
	if len(built) != 1 {
		if cleanup != nil {
			cleanup()
		}
		return nil, nil, fmt.Errorf("%s plugin: %w yet - %s", d.ID, errNotConfigured, d.StubHint)
	}
	return built[0], cleanup, nil
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
		if d.Activate != nil || d.Relaunch {
			mux.RegisterActivator(d.ID, activatorFor(ctx, d, e))
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

// launchPlugins builds every plugin its launch flags configure, in registry
// order, and registers it on mux. The first plugin that fails to build fails
// the launch: better to tell the operator their flag is wrong than to drop
// the tool silently.
func launchPlugins(ctx context.Context, mux *toolMux, e *pluginEnv) error {
	for _, d := range plugins().All() {
		if d.Launch == nil {
			continue
		}
		built, cleanup, err := d.Launch(ctx, e)
		if err != nil || len(built) > 0 {
			d.logStart(ctx, err)
		}
		if err != nil {
			return err
		}
		for _, p := range built {
			mux.Add(d.ID, p)
		}
		if (d.Activate != nil || d.Relaunch) && len(built) == 1 {
			mux.AdoptLaunched(d.ID, activatorFor(ctx, d, e), cleanup) // disable→enable rebuilds from config
		} else {
			mux.AddCleanup(cleanup)
		}
		if len(built) > 0 && d.On != nil && !d.On(e.pf) {
			mux.SetEnabled(d.ID, false)
		}
	}
	return nil
}

// expandPluginPaths expands ~ in every plugin's path-valued launch flags.
func expandPluginPaths(pf *pluginFlags) {
	for _, d := range plugins().All() {
		if d.ExpandPaths != nil {
			d.ExpandPaths(pf)
		}
	}
}

// pluginConfigSecrets and pluginDataSecrets list every plugin's at-rest
// secret files in the config and data dirs.
func pluginConfigSecrets(cfgDir string) []string {
	var out []string
	for _, d := range plugins().All() {
		if d.ConfigSecrets != nil {
			out = append(out, d.ConfigSecrets(cfgDir)...)
		}
	}
	return out
}

func pluginConfigFiles(cfgDir string) []string {
	var out []string
	for _, d := range plugins().All() {
		if d.ConfigFiles != nil {
			out = append(out, d.ConfigFiles(cfgDir)...)
		}
	}
	return out
}

func pluginDataSecrets(dataDir string) []string {
	var out []string
	for _, d := range plugins().All() {
		if d.DataSecrets != nil {
			out = append(out, d.DataSecrets(dataDir)...)
		}
	}
	return out
}

// pluginSecretName reports whether name is the file name of a plugin secret
// that list places directly in its directory.
func pluginSecretName(list func(dir string) []string, name string) bool {
	const dir = "dir"
	for _, p := range list(dir) {
		if filepath.Dir(p) == dir && filepath.Base(p) == name {
			return true
		}
	}
	return false
}

// configureHost is what AdaptConfigure may use from the configure surface.
type configureHost struct {
	cfgDir     string
	mcpConnect atlassianMCPConnector // nil where no MCP connector is wired
}
