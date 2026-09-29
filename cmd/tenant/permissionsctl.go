package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"slices"
	"sort"
	"strings"
)

const (
	permissionSurfaceLocal    = "local"
	permissionSurfaceDiscord  = "discord"
	permissionSurfaceIMessage = "imessage"
)

type effectiveCategory struct {
	Mode   string `json:"mode"`
	Source string `json:"source"`
}

// effectiveExecution is where the surface's commands run: its exec profile,
// resolved as the spawn site resolves it, and what that backend enforces here.
type effectiveExecution struct {
	Backend  string       `json:"backend"`  // host | restricted
	Isolated bool         `json:"isolated"` // a confining backend that runs on this host
	Enabled  bool         `json:"enabled"`
	Profile  surfaceProbe `json:"profile"` // mode, network, roots, probed capabilities, exceptions
}

// surfaceExecution resolves and probes surface's exec profile.
func surfaceExecution(c *commonFlags, surface string, enabled bool) effectiveExecution {
	p := probeSurface(context.Background(), surface, c.cfgDir, c.dataDir)
	return effectiveExecution{Backend: p.Backend, Isolated: p.Caps != nil && !p.unconfined(), Enabled: enabled, Profile: p}
}

type effectiveRemoteMCP struct {
	Servers          int  `json:"servers"`
	TrustAnnotations bool `json:"trust_annotations"`
	AllowWrite       bool `json:"allow_write_without_approval"`
}

type effectivePermissions struct {
	Agent           string                       `json:"agent"`
	Surface         string                       `json:"surface"`
	Preset          string                       `json:"preset,omitempty"`
	Categories      map[string]effectiveCategory `json:"categories"`
	ToolRules       map[string]string            `json:"tool_rules"`
	ConfiguredTools []string                     `json:"configured_tool_groups"`
	ToolOverrides   map[string]bool              `json:"tool_overrides"`
	Execution       effectiveExecution           `json:"execution"`
	RemoteMCP       effectiveRemoteMCP           `json:"remote_mcp"`
	// Integrations says where each integration's code runs (TEN-318): the
	// first-party plugins in Tenant's process, MCP servers out of it.
	Integrations     []effectiveIntegration `json:"integrations"`
	AvailablePresets []string               `json:"available_presets"`
}

// The presets set category modes; where commands run is exec_profiles'. With
// the built-in profile that is the restricted sandbox wherever it can run, so
// full's exec allow is not a naked host shell there, and the audit's
// permissions.host-exec-allow flags it where it is.
var permissionPresets = map[string]map[string]string{
	"safe":     uniformPreset(modeDeny),
	"balanced": uniformPreset(modeAsk),
	"full":     fullPreset(),
}

// fullPreset allows every category but sandbox, which asks: full runs
// commands without approval inside the sandbox, never a refused one outside
// it (TEN-347). With sandbox allowed too, every refusal re-ran on the host
// unasked, and the sandbox was advisory.
func fullPreset() map[string]string {
	p := uniformPreset(modeAllow)
	p[catSandbox] = modeAsk.String()
	return p
}

// uniformPreset puts every category on one mode, so a new category joins each
// preset without an edit here.
func uniformPreset(m permMode) map[string]string {
	p := make(map[string]string, len(catOrder))
	for _, c := range catOrder {
		p[c] = m.String()
	}
	return p
}

func cmdPermissions(_ context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: tenant permissions <effective|preset> [flags]")
	}
	switch args[0] {
	case "effective":
		return cmdPermissionsEffective(args[1:])
	case "preset":
		return cmdPermissionsPreset(args[1:])
	default:
		return fmt.Errorf("unknown permissions command %q (want effective or preset)", args[0])
	}
}

func cmdPermissionsEffective(args []string) error {
	fs := flag.NewFlagSet("permissions effective", flag.ContinueOnError)
	c := bindCommon(fs)
	pf := bindPluginFlags(fs)
	surface := fs.String("surface", permissionSurfaceLocal, "authority surface: local|discord|imessage")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c.auditOnly = true // a report: it must not create or tighten anything
	if err := c.resolve(); err != nil {
		return err
	}
	applyPluginConfig(c, pf)
	_, _, _ = loadExecProfilesFromSettings(c.cfgDir, c.agent) // an unreadable file fails the compose below
	report, err := composeEffectivePermissions(c, pf, *surface)
	if err != nil {
		return err
	}
	if *asJSON {
		b, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}
	printEffectivePermissions(report)
	return nil
}

func cmdPermissionsPreset(args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: tenant permissions preset <safe|balanced|full> --surface <surface> [--agent <id>]")
	}
	name := strings.ToLower(strings.TrimSpace(args[0]))
	modes, ok := permissionPresets[name]
	if !ok {
		return fmt.Errorf("unknown permission preset %q (want safe, balanced, or full)", name)
	}
	fs := flag.NewFlagSet("permissions preset", flag.ContinueOnError)
	c := bindCommon(fs)
	surface := fs.String("surface", permissionSurfaceLocal, "authority surface: local|discord|imessage")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := c.resolve(); err != nil {
		return err
	}
	normalized, err := normalizePermissionSurface(*surface)
	if err != nil {
		return err
	}
	if err := applyPermissionPreset(c, normalized, name, modes); err != nil {
		return err
	}
	if *asJSON {
		b, _ := json.MarshalIndent(map[string]string{"agent": c.agent, "surface": normalized, "preset": name}, "", "  ")
		fmt.Println(string(b))
	} else {
		fmt.Printf("applied %s permission preset to agent=%s surface=%s\n", name, c.agent, normalized)
	}
	return nil
}

// composeEffectivePermissions reports what a surface's approval broker would
// decide, built the way the runtime builds it (TEN-338): the surface's real
// constructor defaults, then the launch --*-allow-* flags (local only), then the
// saved modes, then the tool rules Confirm checks first, then the surface's
// lock (the Discord exec mode), which outranks all of them. Pass the flags the
// surface is launched with: an audit run without --os-allow-exec cannot know
// that a running `tenant serve --os-allow-exec` has it.
func composeEffectivePermissions(c *commonFlags, pf *pluginFlags, surface string) (effectivePermissions, error) {
	normalized, err := normalizePermissionSurface(surface)
	if err != nil {
		return effectivePermissions{}, err
	}
	if pf == nil {
		pf = &pluginFlags{}
	}
	lc := c.lc
	if lc == nil {
		lc = &launchConfig{}
	}
	stg, err := loadSettings(c.cfgDir, c.agent)
	if err != nil {
		return effectivePermissions{}, err
	}
	rules := newToolRules()
	rules.Load(stg.ToolRules)

	var locked func(action, cat string) bool
	lockSource := ""
	if normalized == permissionSurfaceDiscord && !lc.Relay.AllowExec {
		locked, lockSource = relayExecLocks, "relay.allow_exec=false"
	}
	categories := foldAuthority(surfaceModes(normalized, pf, stg, lc), rules, locked, lockSource)

	toolOverrides := stg.Tools
	if toolOverrides == nil {
		toolOverrides = map[string]bool{}
	}
	tools := configuredToolGroups(c, pf, toolOverrides)
	report := effectivePermissions{
		Agent:           c.agent,
		Surface:         normalized,
		Categories:      categories,
		ToolRules:       rules.Snapshot(),
		ConfiguredTools: tools,
		ToolOverrides:   toolOverrides,
		Execution: surfaceExecution(c, normalized,
			containsString(tools, "os") && categories[catExec].Mode != modeDeny.String()),
		RemoteMCP: effectiveRemoteMCP{
			Servers: len(lc.MCPRemotes) + len(pf.mcpRemotes), TrustAnnotations: pf.mcpTrustAnnotations,
			AllowWrite: pf.mcpAllowWrite,
		},
		Integrations:     effectiveIntegrations(tools, lc, append(append([]string{}, lc.MCPRemotes...), pf.mcpRemotes...), c.cfgDir, c.dataDir),
		AvailablePresets: []string{"safe", "balanced", "full"},
	}
	report.Preset = matchingPermissionPreset(categories)
	return report, nil
}

// surfaceModes builds the surface's broker the way the runtime does (its
// constructor, seedFromFlags on the local surface, then LoadModes) and names
// the layer that set each category's mode.
func surfaceModes(surface string, pf *pluginFlags, stg *settings, lc *launchConfig) map[string]effectiveCategory {
	var (
		b                 *approvalBroker
		base, savedSource string
		saved             map[string]string
		known             []string
		flags             map[string]string
	)
	switch surface {
	case permissionSurfaceDiscord:
		b, base = newDiscordApprovalBroker(nil), "discord default"
		saved, known, savedSource = lc.Relay.Permissions, lc.Relay.KnownCategories, "relay.permissions"
	case permissionSurfaceIMessage:
		b, base = newOffsiteApprovalBroker(nil, newApprovalBroker(nil)), "imessage deny-by-default"
		saved, known, savedSource = lc.IMessage.Permissions, lc.IMessage.KnownCategories, "imessage.permissions"
	default:
		b, base = newApprovalBroker(nil), "local default"
		saved, known, savedSource = stg.Permissions, stg.KnownCategories, "settings permissions"
		flags = launchFlagGrants(pf)
		seedFromFlags(b, pf)
	}
	b.LoadModes(saved, known)
	modes, persisted := b.Modes(), savedModes(saved)
	out := make(map[string]effectiveCategory, len(catOrder))
	for _, cat := range catOrder {
		source := base
		if flag, ok := flags[cat]; ok {
			source = "launch flag " + flag
		}
		if _, ok := persisted[cat]; ok {
			source = savedSource
		}
		out[cat] = effectiveCategory{Mode: modes[cat], Source: source}
	}
	return out
}

// openCategories also take action ids allActionCategories does not list:
// destructive is where every unknown id lands (a remote connector's tools, a
// plugin's own ids) and web takes any web_ id. A tool rule for those ids cannot
// be checked here, so the category mode stands for them.
var openCategories = map[string]bool{catDestructive: true, catWeb: true}

// foldAuthority applies what Confirm checks before the category mode — the
// surface lock, then tool rules — to each action a category holds, and reports
// the most permissive result: the authority a caller can actually reach in
// that category. The source names the layer that produced it.
func foldAuthority(modes map[string]effectiveCategory, rules *toolRules, locked func(action, cat string) bool, lockSource string) map[string]effectiveCategory {
	actions := map[string][]string{}
	for action, cat := range allActionCategories() {
		actions[cat] = append(actions[cat], action)
	}
	type outcome struct {
		effectiveCategory
		priority int // breaks a tie in mode: the lock, then the category mode, then a rule
	}
	out := make(map[string]effectiveCategory, len(catOrder))
	for _, cat := range catOrder {
		var best outcome
		found, lockedSome := false, false
		consider := func(o outcome) {
			if !found || modeRank(o.Mode) > modeRank(best.Mode) ||
				(modeRank(o.Mode) == modeRank(best.Mode) && o.priority > best.priority) {
				best, found = o, true
			}
		}
		decide := func(action string, checkRules bool) {
			if locked != nil && locked(action, cat) {
				lockedSome = true
				consider(outcome{effectiveCategory{Mode: modeDeny.String(), Source: lockSource}, 2})
				return
			}
			if checkRules {
				if mode, pattern, ok := rules.Lookup(action); ok {
					consider(outcome{effectiveCategory{Mode: mode, Source: "tool rule " + pattern}, 0})
					return
				}
			}
			consider(outcome{modes[cat], 1})
		}
		sort.Strings(actions[cat])
		for _, action := range actions[cat] {
			decide(action, true)
		}
		if openCategories[cat] {
			decide("", false) // an id the table does not list
		}
		result := best.effectiveCategory
		if lockedSome && result.Source != lockSource {
			result.Source += "; other actions refused by " + lockSource
		}
		out[cat] = result
	}
	return out
}

// modeRank orders modes by how much they let through.
func modeRank(mode string) int {
	switch mode {
	case "allow":
		return 2
	case "ask":
		return 1
	}
	return 0
}

// authoritySurface is a surface a turn can arrive from, and whether this
// configuration has it live.
type authoritySurface struct {
	Surface string `json:"surface"`
	Active  bool   `json:"active"`
	Reason  string `json:"reason"`
}

// authoritySurfaces lists every surface with the setting that makes it live
// or not. goos is runtime.GOOS: the iMessage responder is macOS-only.
func authoritySurfaces(lc *launchConfig, goos string) []authoritySurface {
	if lc == nil {
		lc = &launchConfig{}
	}
	out := []authoritySurface{{Surface: permissionSurfaceLocal, Active: true, Reason: "the TUI, dashboard and serve chat at this host"}}

	relay := authoritySurface{Surface: permissionSurfaceDiscord, Reason: "relay.enabled=false"}
	if lc.Relay.Enabled {
		relay.Active, relay.Reason = true, "relay.enabled=true"
	}
	out = append(out, relay)

	imsg := authoritySurface{Surface: permissionSurfaceIMessage}
	switch {
	case !lc.IMessage.Enabled:
		imsg.Reason = "imessage.enabled=false"
	case goos != "darwin":
		imsg.Reason = "imessage.enabled=true, but the responder runs on macOS only"
	case len(lc.IMessage.AllowFrom) == 0:
		imsg.Reason = "imessage.enabled=true, but imessage.allow_from is empty, so nobody can drive it"
	default:
		imsg.Active = true
		imsg.Reason = fmt.Sprintf("imessage.enabled=true, %d allowed handle(s)", len(lc.IMessage.AllowFrom))
	}
	return append(out, imsg)
}

func normalizePermissionSurface(surface string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(surface)) {
	case "", "local", "host", "tui", "dashboard", "serve":
		return permissionSurfaceLocal, nil
	case "discord", "relay":
		return permissionSurfaceDiscord, nil
	case "imessage", "message":
		return permissionSurfaceIMessage, nil
	default:
		return "", fmt.Errorf("unknown permission surface %q (want local, discord, or imessage)", surface)
	}
}

// applyPermissionPreset saves a preset's modes. A preset chooses every
// category, so all of them are known (TEN-343): none is marked new after it.
func applyPermissionPreset(c *commonFlags, surface, name string, modes map[string]string) error {
	copyModes, known := cloneStringMap(modes), slices.Clone(catOrder)
	switch surface {
	case permissionSurfaceLocal:
		stg, err := loadSettings(c.cfgDir, c.agent)
		if err != nil {
			return err
		}
		stg.Permissions, stg.KnownCategories = copyModes, known
		return stg.Save(c.cfgDir, c.agent)
	case permissionSurfaceDiscord:
		c.lc.Relay.Permissions, c.lc.Relay.KnownCategories = copyModes, known
		c.lc.Relay.AllowExec = name != "safe"
	case permissionSurfaceIMessage:
		c.lc.IMessage.Permissions, c.lc.IMessage.KnownCategories = copyModes, known
	}
	return c.lc.Save(c.cfgDir)
}

func configuredToolGroups(c *commonFlags, pf *pluginFlags, toolOverrides map[string]bool) []string {
	configured := map[string]bool{"memory": true}
	for _, d := range plugins().All() {
		if d.On != nil && d.On(pf) {
			configured[d.ID] = true
		}
	}
	if len(c.lc.MCPRemotes)+len(pf.mcpRemotes) > 0 {
		configured["remote-mcp"] = true
	}
	for tool, enabled := range toolOverrides {
		if !enabled {
			continue
		}
		if id, ok := plugins().OwnerOfTool(tool); ok {
			configured[id] = true
		} else if strings.HasPrefix(tool, "memory_") {
			configured["memory"] = true
		}
	}
	groups := make([]string, 0, len(configured))
	for group := range configured {
		groups = append(groups, group)
	}
	sort.Strings(groups)
	return groups
}

func matchingPermissionPreset(categories map[string]effectiveCategory) string {
	for _, name := range []string{"safe", "balanced", "full"} {
		preset := permissionPresets[name]
		match := true
		for _, category := range catOrder {
			if categories[category].Mode != preset[category] {
				match = false
				break
			}
		}
		if match {
			return name
		}
	}
	return "custom"
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func printEffectivePermissions(report effectivePermissions) {
	fmt.Printf("effective permissions — agent=%s surface=%s preset=%s\n", report.Agent, report.Surface, report.Preset)
	for _, category := range catOrder {
		p := report.Categories[category]
		fmt.Printf("  %-12s %-5s  (%s)\n", category, p.Mode, p.Source)
	}
	if len(report.ToolRules) > 0 {
		patterns := make([]string, 0, len(report.ToolRules))
		for pattern := range report.ToolRules {
			patterns = append(patterns, pattern)
		}
		sort.Strings(patterns)
		fmt.Println("  tool rules (every surface; checked before the category):")
		for _, pattern := range patterns {
			fmt.Printf("    %-40s %s\n", pattern, report.ToolRules[pattern])
		}
	}
	fmt.Printf("  tools: %s\n", strings.Join(report.ConfiguredTools, ", "))
	fmt.Printf("  execution: %s (isolated=%t, enabled=%t)\n", report.Execution.Backend, report.Execution.Isolated, report.Execution.Enabled)
	fmt.Println("    " + execProfileState(report.Execution.Profile))
	if len(report.Execution.Profile.Exceptions) > 0 {
		fmt.Println("    exceptions (exec_exceptions; delete one there to remove it):")
		for _, e := range report.Execution.Profile.Exceptions {
			fmt.Println("      " + e)
		}
	}
	fmt.Printf("  remote MCP: %d server(s), trust_annotations=%t, allow_write=%t\n",
		report.RemoteMCP.Servers, report.RemoteMCP.TrustAnnotations, report.RemoteMCP.AllowWrite)
	if len(report.Integrations) > 0 {
		fmt.Println("  integrations (where their code runs):")
		for _, in := range report.Integrations {
			fmt.Println("    " + integrationLine(in))
		}
	}
}
