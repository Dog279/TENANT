package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
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

type effectiveExecution struct {
	Backend  string `json:"backend"`
	Isolated bool   `json:"isolated"`
	Enabled  bool   `json:"enabled"`
}

type effectiveRemoteMCP struct {
	Servers          int  `json:"servers"`
	TrustAnnotations bool `json:"trust_annotations"`
	AllowWrite       bool `json:"allow_write_without_approval"`
}

type effectivePermissions struct {
	Agent            string                       `json:"agent"`
	Surface          string                       `json:"surface"`
	Preset           string                       `json:"preset,omitempty"`
	Categories       map[string]effectiveCategory `json:"categories"`
	ConfiguredTools  []string                     `json:"configured_tool_groups"`
	ToolOverrides    map[string]bool              `json:"tool_overrides"`
	Execution        effectiveExecution           `json:"execution"`
	RemoteMCP        effectiveRemoteMCP           `json:"remote_mcp"`
	AvailablePresets []string                     `json:"available_presets"`
}

var permissionPresets = map[string]map[string]string{
	"safe":     uniformPreset(modeDeny),
	"balanced": uniformPreset(modeAsk),
	"full":     uniformPreset(modeAllow),
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
	if err := c.resolve(); err != nil {
		return err
	}
	applyPluginConfig(c, pf)
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

func composeEffectivePermissions(c *commonFlags, pf *pluginFlags, surface string) (effectivePermissions, error) {
	normalized, err := normalizePermissionSurface(surface)
	if err != nil {
		return effectivePermissions{}, err
	}

	defaults := modeAsk.String()
	source := "local default"
	var overrides map[string]string
	var toolOverrides map[string]bool
	switch normalized {
	case permissionSurfaceLocal:
		stg, loadErr := loadSettings(c.cfgDir, c.agent)
		if loadErr != nil {
			return effectivePermissions{}, loadErr
		}
		overrides = stg.Permissions
		toolOverrides = stg.Tools
	case permissionSurfaceDiscord:
		overrides = c.lc.Relay.Permissions
		source = "discord default"
	case permissionSurfaceIMessage:
		defaults = modeDeny.String()
		source = "imessage deny-by-default"
		overrides = c.lc.IMessage.Permissions
	}

	categories := make(map[string]effectiveCategory, len(catOrder))
	for _, category := range catOrder {
		categories[category] = effectiveCategory{Mode: defaults, Source: source}
		if raw, ok := overrides[category]; ok {
			if mode, valid := parseMode(raw); valid {
				categories[category] = effectiveCategory{Mode: mode.String(), Source: normalized + " override"}
			}
		}
	}
	// Discord has an outer tool-surface kill switch in addition to its broker.
	// Compose it here so an "allow" mode never overstates effective authority.
	if normalized == permissionSurfaceDiscord && !c.lc.Relay.AllowExec {
		for _, category := range []string{catExec, catWrite, catDestructive} {
			categories[category] = effectiveCategory{Mode: modeDeny.String(), Source: "relay.allow_exec=false"}
		}
	}

	if toolOverrides == nil {
		toolOverrides = map[string]bool{}
	}
	tools := configuredToolGroups(c, pf, toolOverrides)
	report := effectivePermissions{
		Agent:           c.agent,
		Surface:         normalized,
		Categories:      categories,
		ConfiguredTools: tools,
		ToolOverrides:   toolOverrides,
		Execution: effectiveExecution{
			Backend: "host", Isolated: false,
			Enabled: containsString(tools, "os") && categories[catExec].Mode != modeDeny.String(),
		},
		RemoteMCP: effectiveRemoteMCP{
			Servers: len(c.lc.MCPRemotes) + len(pf.mcpRemotes), TrustAnnotations: pf.mcpTrustAnnotations,
			AllowWrite: pf.mcpAllowWrite,
		},
		AvailablePresets: []string{"safe", "balanced", "full"},
	}
	report.Preset = matchingPermissionPreset(categories)
	return report, nil
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

func applyPermissionPreset(c *commonFlags, surface, name string, modes map[string]string) error {
	copyModes := cloneStringMap(modes)
	switch surface {
	case permissionSurfaceLocal:
		stg, err := loadSettings(c.cfgDir, c.agent)
		if err != nil {
			return err
		}
		stg.Permissions = copyModes
		return stg.save(c.cfgDir, c.agent)
	case permissionSurfaceDiscord:
		c.lc.Relay.Permissions = copyModes
		c.lc.Relay.AllowExec = name != "safe"
	case permissionSurfaceIMessage:
		c.lc.IMessage.Permissions = copyModes
	}
	return c.lc.save(c.cfgDir)
}

func configuredToolGroups(c *commonFlags, pf *pluginFlags, toolOverrides map[string]bool) []string {
	configured := map[string]bool{"memory": true}
	add := func(enabled bool, name string) {
		if enabled {
			configured[name] = true
		}
	}
	add(pf.wikiDir != "", "wiki")
	add(pf.sqlDB != "", "sql")
	add(pf.web, "web")
	add(pf.gsuite, "gsuite")
	add(pf.atlassian, "atlassian")
	add(pf.x, "x")
	add(pf.imsg, "imessage")
	add(pf.discord, "discord")
	add(pf.osEnable, "os")
	if len(c.lc.MCPRemotes)+len(pf.mcpRemotes) > 0 {
		configured["remote-mcp"] = true
	}
	for tool, enabled := range toolOverrides {
		if !enabled {
			continue
		}
		for prefix, group := range map[string]string{
			"wiki_": "wiki", "sql_": "sql", "web_": "web", "gsuite_": "gsuite",
			"jira_": "atlassian", "x_": "x", "imessage_": "imessage",
			"discord_": "discord", "os_": "os", "memory_": "memory",
		} {
			if strings.HasPrefix(tool, prefix) {
				configured[group] = true
				break
			}
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
	fmt.Printf("  tools: %s\n", strings.Join(report.ConfiguredTools, ", "))
	fmt.Printf("  execution: %s (isolated=%t, enabled=%t)\n", report.Execution.Backend, report.Execution.Isolated, report.Execution.Enabled)
	fmt.Printf("  remote MCP: %d server(s), trust_annotations=%t, allow_write=%t\n",
		report.RemoteMCP.Servers, report.RemoteMCP.TrustAnnotations, report.RemoteMCP.AllowWrite)
}
