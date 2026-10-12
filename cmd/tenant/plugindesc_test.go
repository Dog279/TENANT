package main

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// Every descriptor declares what AC1 of TEN-321 asks of a plugin: identity,
// version, tools and permission classes; and a plugin that gates anything
// declares the class of every gated tool.
func TestEveryPluginDeclaresItself(t *testing.T) {
	for _, d := range plugins().All() {
		if d.ID == "" || d.Title == "" || d.Version < 1 {
			t.Errorf("plugin %q lacks identity or version: %+v", d.ID, d)
		}
		tools := d.Tools()
		if len(tools) == 0 {
			t.Errorf("plugin %q declares no tools", d.ID)
		}
		gated := 0
		for _, spec := range tools {
			if spec.Gated {
				gated++
			}
		}
		if gated > 0 && len(d.Actions) == 0 {
			t.Errorf("plugin %q has %d gated tool(s) but declares no permission class", d.ID, gated)
		}
		if !d.PerAgent && d.ID != "os" && d.StubHint == "" {
			t.Errorf("plugin %q has no stub hint: an unconfigured plugin would be invisible", d.ID)
		}
		if d.Configure != nil {
			if k := d.Configure(); k.ID != d.ID {
				t.Errorf("plugin %q's config schema is filed as %q", d.ID, k.ID)
			}
		}
	}
}

// The host's own approval actions and the plugins' never overlap, so a
// category is never decided by which table was read first.
func TestHostAndPluginActionsAreDisjoint(t *testing.T) {
	for action := range hostActionCategories {
		if _, ok := plugins().ActionCategory(action); ok {
			t.Errorf("action %q is declared by the host and by a plugin", action)
		}
	}
	all := allActionCategories()
	for action, cat := range all {
		if got := categorize(action); got != cat {
			t.Errorf("categorize(%q) = %q, but the table says %q", action, got, cat)
		}
	}
}

// The shared mux built with no plugin configured carries a stub for exactly
// the registry's shared plugins, each under its declared id with its declared
// tools, disabled; os is registered for real; web is left to each agent.
func TestTheStubCatalogIsTheRegistry(t *testing.T) {
	c := &commonFlags{cfgDir: t.TempDir(), dataDir: t.TempDir()}
	mux, _, cleanup, err := buildToolMux(context.Background(), c, nil, &pluginFlags{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	byPlugin := map[string][]string{}
	for _, ti := range mux.ToolList() {
		byPlugin[ti.Plugin] = append(byPlugin[ti.Plugin], ti.Name)
		if ti.Enabled && ti.Plugin != "memory" && ti.Plugin != "ui" { // host tools, on by default
			t.Errorf("%s (%s) is enabled with nothing configured", ti.Name, ti.Plugin)
		}
	}
	for _, d := range plugins().All() {
		got := byPlugin[d.ID]
		if d.PerAgent {
			if len(got) != 0 {
				t.Errorf("per-agent plugin %q is on the shared mux: %v", d.ID, got)
			}
			continue
		}
		want := d.ToolNames()
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("plugin %q registered %v, declares %v", d.ID, got, want)
		}
	}
}

// /configure lists exactly the plugins whose schema it manages; `tenant setup`
// offers its plugins in their declared order.
func TestTheConfigureSurfacesAreTheRegistry(t *testing.T) {
	for _, d := range plugins().All() {
		_, inConfigure := skillKinds[d.ID]
		want := d.Configure != nil && !d.SetupOnly
		if inConfigure != want {
			t.Errorf("plugin %q: in /configure = %v, want %v", d.ID, inConfigure, want)
		}
		_, setupOnly := setupOnlyKinds()[d.ID]
		if setupOnly != (d.SetupOnly && d.Configure != nil) {
			t.Errorf("plugin %q: setup-only = %v", d.ID, setupOnly)
		}
	}
	var ids []string
	for _, k := range wizardCatalog(t.TempDir()) {
		ids = append(ids, k.ID)
	}
	if got := strings.Join(ids, ","); got != "wiki,sql,gsuite,x,imessage,discord,github,web,os" {
		t.Errorf("tenant setup order = %s", got)
	}
}

// Each plugin's CLI commands and doctor checks reach the CLI and doctor.
func TestPluginCommandsAndChecksAreWired(t *testing.T) {
	for _, d := range plugins().All() {
		for _, c := range d.CLI {
			if got := cliRegistry().Lookup(c.Names[0]); got == nil {
				t.Errorf("plugin %q's command %q is not in the CLI", d.ID, c.Names[0])
			}
		}
	}
	names := map[string]bool{}
	for _, ck := range pluginDoctorChecks() {
		names[ck.Name] = true
	}
	for _, want := range []string{"wiki dir (if configured)", "chrome (if web enabled)", "gsuite (if enabled)", "os_processes tool"} {
		if !names[want] {
			t.Errorf("doctor lacks the plugin check %q", want)
		}
	}
}

// A tool override counts toward its plugin's group by the registry's exact
// tool index, not by a name prefix (the gsuite_ prefix matched no tool).
func TestToolGroupsUseTheRegistry(t *testing.T) {
	c := &commonFlags{lc: &launchConfig{}}
	groups := configuredToolGroups(c, &pluginFlags{}, map[string]bool{"gmail_send": true, "github_get_pr": true, "money_spend": true, "memory_search": true})
	for _, want := range []string{"gsuite", "github", "simplefin", "memory"} {
		if !slices.Contains(groups, want) {
			t.Errorf("groups = %v, lacks %q", groups, want)
		}
	}
}

// pluginOwnedFiles are the surfaces only one plugin has, beside its
// plugin_<id>*.go and skillctl_<id>*.go.
var pluginOwnedFiles = map[string][]string{
	"github":    {"githubtuictl.go", "githubdashctl.go"},
	"atlassian": {"atlassianctl.go"},
	"simplefin": {"commands_simplefin.go"},
	"gsuite":    {"setup_oauth.go"},
}

func ownsFile(id, file string) bool {
	for _, prefix := range []string{"plugin_" + id, "skillctl_" + id} {
		if rest, ok := strings.CutPrefix(file, prefix); ok && (strings.HasPrefix(rest, ".") || strings.HasPrefix(rest, "_")) {
			return true
		}
	}
	return slices.Contains(pluginOwnedFiles[id], file)
}

// A plugin's saved settings, secrets and config schema are named only in its
// own files: adding or changing a plugin touches no central switchboard
// (TEN-321 AC3).
func TestPluginsAreNamedOnlyInTheirOwnFiles(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range plugins().All() {
			if ownsFile(d.ID, f) {
				continue
			}
			for _, pat := range []string{`Skills["` + d.ID + `"]`, `skillSecretID("` + d.ID + `"`, `"skill:` + d.ID + `:`, `kinds["` + d.ID + `"]`} {
				if strings.Contains(string(src), pat) {
					t.Errorf("%s names plugin %q (%s); that belongs in the plugin's own files", f, d.ID, pat)
				}
			}
		}
	}
}

// The files that used to be per-plugin switchboards import no first-party
// plugin: they read the registry. The remote-MCP family is the host's own,
// and the builder hands its callers the wiki index.
func TestTheSwitchboardsImportNoPlugin(t *testing.T) {
	allowed := map[string][]string{
		"doctor.go":        {"mcpremote"},
		"plugindesc.go":    {"wiki"},
		"toolmux_build.go": {"wiki"},
		"eval.go":          {"wiki"}, // eval's own per-task wiki overlay, not a switchboard
	}
	for _, f := range []string{"toolmux_build.go", "plugindesc.go", "approval.go", "permissionsctl.go", "securityaudit.go",
		"doctor.go", "backupctl.go", "privacyconn.go", "secretpaths.go", "keyscatalog.go", "skills_setup.go",
		"skillctl.go", "cliregistry.go", "bootstrap.go", "eval.go"} {
		file, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range file.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			name, ok := strings.CutPrefix(path, "tenant/internal/plugins/")
			if ok && !slices.Contains(allowed[f], name) {
				t.Errorf("%s imports plugin %s; it should read the plugin registry", f, name)
			}
		}
	}
}

// Every at-rest plugin secret is on the managed list, from the descriptors:
// doctor now repairs github-token.json too (it was missing, D9).
func TestPluginSecretsAreManaged(t *testing.T) {
	cfg, data := t.TempDir(), t.TempDir()
	managed, err := managedSecrets(cfg, data)
	if err != nil {
		t.Fatal(err)
	}
	var labels []string
	for _, m := range managed {
		labels = append(labels, m.label)
	}
	for _, want := range []string{"credentials.json", "atlassian-token.json", "github-token.json", "oauth_client.json", "x-token.json"} {
		if !slices.Contains(labels, want) {
			t.Errorf("managed secrets %v lack %s", labels, want)
		}
	}
	for _, name := range []string{"github-token.json", "atlassian-token.json", "oauth_client.json"} {
		if !privateConfigRootFile(name) {
			t.Errorf("doctor's permission repair skips the config-dir secret %s", name)
		}
	}
	if !privateDataRootFile("x-token.json") {
		t.Error("doctor's permission repair skips the data-dir secret x-token.json")
	}
}

// The file tools refuse the OAuth client JSON the operator pointed gsuite at:
// it holds a client secret (it was unguarded, D10).
func TestPointedSecretsAreGuarded(t *testing.T) {
	lc := &launchConfig{Skills: map[string]*skillConfig{"gsuite": {Enabled: true, Settings: map[string]string{
		"sa_json": "/keys/sa.json", "oauth_creds_json": "/keys/client.json",
	}}}}
	_, secret := fileToolGuards(t.TempDir(), t.TempDir(), testPluginFlags(func(pf *pluginFlags) { gsuiteFlagsOf(pf).saJSON = "/keys/flag.json" }), lc)
	for _, want := range []string{"/keys/sa.json", "/keys/client.json", "/keys/flag.json"} {
		if !slices.ContainsFunc(secret, func(p string) bool {
			return filepath.ToSlash(p) == want || strings.HasSuffix(filepath.ToSlash(p), want)
		}) {
			t.Errorf("the file tools do not refuse %s", want)
		}
	}
}

// The privacy connectors and the keys page are the registry's, the same
// services and keys as before they were declared per plugin.
func TestConnectorsAndKeysComeFromTheRegistry(t *testing.T) {
	var names []string
	for _, c := range connectorDefs() {
		names = append(names, c.name)
	}
	slices.Sort(names)
	if got := strings.Join(names, ","); got != "atlassian,discord,github,gsuite,imessage,mcp,simplefin,web,x" {
		t.Errorf("connectors = %s", got)
	}
	var ids []string
	for _, k := range keyCatalog() {
		ids = append(ids, k.CredID)
	}
	for _, want := range []string{"openai", "sakana", "tavily", "brave_search", "jina", "skill:discord:token", "skill:simplefin:access_url", "skill:github:token"} {
		if !slices.Contains(ids, want) {
			t.Errorf("keys page %v lacks %s", ids, want)
		}
	}
	if len(ids) != len(providerKeys)+6 {
		t.Errorf("keys page has %d keys, want the %d providers and 6 plugin keys", len(ids), len(providerKeys))
	}
}

// A plugin with an activator is built from its saved config on its first
// /enable: with nothing saved, enabling it asks for /configure instead of
// switching on a stub that can only say "not configured".
func TestActivatorsBuildTheirPluginOnEnable(t *testing.T) {
	t.Setenv("X_BEARER_TOKEN", "")
	c := &commonFlags{cfgDir: t.TempDir(), dataDir: t.TempDir()}
	mux, _, cleanup, err := buildToolMux(context.Background(), c, nil, &pluginFlags{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	n := 0
	for _, d := range plugins().All() {
		// imessage needs no config (native chat.db by default), so enabling it
		// would open the host's real Messages database.
		if d.Activate == nil || d.ID == "imessage" {
			continue
		}
		n++
		if _, _, err := mux.SetEnabled(d.ID, true); !errors.Is(err, errNotConfigured) {
			t.Errorf("enabling unconfigured %q: err = %v, want its activator's %q", d.ID, err, errNotConfigured)
		}
	}
	if n == 0 {
		t.Fatal("no plugin declares an activator")
	}
}

// /configure applies a plugin's own configure hooks. atlassian's "mcp" mode
// probes through this session's remote-MCP connector (AdaptConfigure), and
// the tools that brought live are not enabled again as the native plugin
// (SelfEnables).
func TestConfigureAdaptsToThePlugin(t *testing.T) {
	var connected []string
	connect := func(_ context.Context, url string) (string, error) {
		connected = append(connected, url)
		return "connected over MCP", nil
	}
	counter, enabler := newCountingEnabler(1)
	sc := newSkillCfgControl(t.TempDir(), map[string]skillKind{"atlassian": atlassianSkillKind()}, enabler, connect)
	out, err := sc.SkillConfigure([]string{"atlassian", "auth=mcp"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(connected) != 1 {
		t.Errorf("the probe did not use the session's MCP connector:\n%s", out)
	}
	if n := atomic.LoadInt32(counter); n != 0 {
		t.Errorf("mcp mode also enabled the native plugin (%d call(s)):\n%s", n, out)
	}
}

// A plugin's post-configure hook runs after a passing probe: /configure
// discord hands the relay the bot token and the operator, and turns it on.
func TestConfigureRunsThePluginsFollowUp(t *testing.T) {
	dir := t.TempDir()
	kind := skillKind{ID: "discord", Label: "Discord (fixture)", Wired: true,
		Fields: []skillKindField{
			{Key: "token", Prompt: "bot token", Secret: true, Required: true},
			{Key: "operator_id", Prompt: "operator id", Required: true},
		},
		Probe: func(context.Context, *credentials, map[string]string, func() error) (string, error) {
			return "bot", nil
		},
	}
	sc := newSkillCfgControl(dir, map[string]skillKind{"discord": kind}, nil)
	var token, operator string
	sc.onDiscordConfigured = func(tok, op string) error {
		token, operator = tok, op
		return nil
	}
	if _, err := sc.SkillConfigure([]string{"discord", "token=bot-token", "operator_id=42"}, true); err != nil {
		t.Fatal(err)
	}
	if token != "bot-token" || operator != "42" {
		t.Errorf("the relay got token %q and operator %q", token, operator)
	}
	lc, err := loadLaunchConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !lc.Relay.Enabled || lc.Relay.OperatorID != "42" {
		t.Errorf("relay config = %+v", lc.Relay)
	}
}

// Each plugin expands the ~ in its own path flags.
func TestPluginPathsAreExpanded(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir:", err)
	}
	pf := testPluginFlags(func(pf *pluginFlags) {
		wikiFlagsOf(pf).dir = "~/notes"
		sqlFlagsOf(pf).db = "~/data.db"
		gsuiteFlagsOf(pf).saJSON = "~/sa.json"
	})
	expandPluginPaths(pf)
	for flag, got := range map[string]string{"--wiki-dir": wikiFlagsOf(pf).dir, "--sql-db": sqlFlagsOf(pf).db, "--gsuite-sa-json": gsuiteFlagsOf(pf).saJSON} {
		if !strings.HasPrefix(got, home) {
			t.Errorf("%s = %q, not expanded against %s", flag, got, home)
		}
	}
}
