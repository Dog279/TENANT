package main

// agentctl — TUI + CLI control for the named-sub-agent registry
// (launchConfig.Agents). Manages the config file AND keeps a live runtime's
// per-profile router cache in sync, so a `/agents add researcher zai`
// applied mid-session takes effect on the very next spawn.

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"tenant/internal/tui"
)

// agentControl implements tui.AgentControl + the CLI handlers. Holds a
// reference to the live TeamRuntime so mutations invalidate its router
// cache; rt may be nil at CLI time (no live runtime, config-only edits).
type agentControl struct {
	mu     sync.Mutex
	cfgDir string
	rt     *TeamRuntime // may be nil for CLI; required for TUI live-apply
}

// List returns the named agent profiles, sorted, mapped to the TUI-facing
// summary type. Empty list when none are configured (returns nil, not an
// error — the TUI shows "no agents configured" hint).
func (ac *agentControl) List() ([]tui.AgentInfo, error) {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	lc, err := loadLaunchConfig(ac.cfgDir)
	if err != nil {
		return nil, err
	}
	// Built-in specialists (TEN-132) listed alongside the operator's profiles.
	agents := effectiveAgents(lc)
	out := make([]tui.AgentInfo, 0, len(agents))
	for _, name := range sortedAgentNames(agents) {
		ap := agents[name]
		pc := lc.Providers[ap.Provider]
		modelLabel := ap.Model
		switch { // nil-safe: a built-in has an empty provider, so pc is nil
		case modelLabel != "":
		case pc != nil && pc.Model != "":
			modelLabel = pc.Model
		case pc != nil:
			if pk, ok := providerKinds[pc.Kind]; ok {
				modelLabel = pk.DefaultModel
			}
		}
		if ap.Builtin {
			modelLabel = "(your model)"
		}
		out = append(out, tui.AgentInfo{
			Name:        name,
			Provider:    ap.Provider,
			Model:       modelLabel,
			Description: ap.Description,
			HasSoul:     strings.TrimSpace(ap.Soul) != "",
			Builtin:     ap.Builtin,
			// Built-ins are always valid (they inherit the primary); else the
			// provider must exist and a model must resolve.
			Valid: ap.Builtin || (pc != nil && modelLabel != ""),
		})
	}
	return out, nil
}

// Add registers (or replaces) a named agent profile. Validates the provider
// exists in launchConfig.Providers and that the resolved model isn't empty
// — a misconfigured profile would silently use the orchestrator's router
// when first spawned, which is the WRONG kind of forgiving.
func (ac *agentControl) Add(name, provider, modelID, description, soul string) (string, error) {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	name = strings.TrimSpace(name)
	provider = strings.TrimSpace(provider)
	if name == "" || provider == "" {
		return "", fmt.Errorf("name and provider are required")
	}
	if !safeAgentName(name) {
		return "", fmt.Errorf("agent name %q has unsafe characters (alphanumerics, dashes, underscores only)", name)
	}
	lc, err := loadLaunchConfig(ac.cfgDir)
	if err != nil {
		return "", err
	}
	pc := lc.Providers[provider]
	if pc == nil {
		return "", fmt.Errorf("provider %q is not configured (see /model or `tenant model list`)", provider)
	}
	// Resolve effective model: profile override > provider default > catalog default.
	mdl := strings.TrimSpace(modelID)
	if mdl == "" {
		mdl = pc.Model
	}
	if mdl == "" {
		if pk, ok := providerKinds[pc.Kind]; ok {
			mdl = pk.DefaultModel
		}
	}
	if mdl == "" {
		return "", fmt.Errorf("provider %q has no Model and the profile didn't set one — pass a model explicitly", provider)
	}
	if lc.Agents == nil {
		lc.Agents = map[string]*agentProfile{}
	}
	// Preserve a previously-set soul/description on partial updates
	// (someone may run `Add` again just to swap the model).
	prior := lc.Agents[name]
	if prior != nil {
		if description == "" {
			description = prior.Description
		}
		if soul == "" {
			soul = prior.Soul
		}
	}
	lc.Agents[name] = &agentProfile{
		Provider:    provider,
		Model:       strings.TrimSpace(modelID),
		Description: strings.TrimSpace(description),
		Soul:        strings.TrimSpace(soul),
	}
	if err := lc.save(ac.cfgDir); err != nil {
		return "", err
	}
	ac.publishRuntime(lc.Agents)
	return fmt.Sprintf("agent %q → %s/%s (effective)", name, provider, mdl), nil
}

// Rename moves a profile from one name to another. The orchestrator's
// spawn_agent(role=<oldname>) stops resolving; spawn_agent(role=<newname>)
// resolves instead. Lossless: all of provider/model/soul/description carry
// over. Refuses if the target name already exists (operator's intent must
// be explicit — call Remove first if they really want to overwrite).
func (ac *agentControl) Rename(oldName, newName string) (string, error) {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	oldName = strings.TrimSpace(oldName)
	newName = strings.TrimSpace(newName)
	if oldName == "" || newName == "" {
		return "", fmt.Errorf("both old and new names are required")
	}
	if oldName == newName {
		return "", fmt.Errorf("old and new names are the same")
	}
	if !safeAgentName(newName) {
		return "", fmt.Errorf("new name %q has unsafe characters (alphanumerics, dashes, underscores only)", newName)
	}
	lc, err := loadLaunchConfig(ac.cfgDir)
	if err != nil {
		return "", err
	}
	ap := lc.Agents[oldName]
	if ap == nil {
		return "", fmt.Errorf("no agent named %q", oldName)
	}
	if _, taken := lc.Agents[newName]; taken {
		return "", fmt.Errorf("an agent named %q already exists — remove it first or pick a different name", newName)
	}
	delete(lc.Agents, oldName)
	lc.Agents[newName] = ap
	if err := lc.save(ac.cfgDir); err != nil {
		return "", err
	}
	ac.publishRuntime(lc.Agents)
	return fmt.Sprintf("renamed %q → %q (identity + model + soul preserved)", oldName, newName), nil
}

// SetModel updates just the provider+model pinning for an existing profile,
// preserving soul and description. The orchestrator's NEXT spawn of this
// name picks up the new model (the runtime's per-profile router cache is
// invalidated). Pass an empty modelID to fall through to the provider's
// configured/catalog default.
func (ac *agentControl) SetModel(name, provider, modelID string) (string, error) {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	name = strings.TrimSpace(name)
	provider = strings.TrimSpace(provider)
	if name == "" || provider == "" {
		return "", fmt.Errorf("name and provider are required")
	}
	lc, err := loadLaunchConfig(ac.cfgDir)
	if err != nil {
		return "", err
	}
	ap := lc.Agents[name]
	if ap == nil {
		return "", fmt.Errorf("no agent named %q (see /agents)", name)
	}
	pc := lc.Providers[provider]
	if pc == nil {
		return "", fmt.Errorf("provider %q is not configured (see /model or `tenant model list`)", provider)
	}
	// Resolve effective model for the success message + error if unresolvable.
	effective := strings.TrimSpace(modelID)
	if effective == "" {
		effective = pc.Model
	}
	if effective == "" {
		if pk, ok := providerKinds[pc.Kind]; ok {
			effective = pk.DefaultModel
		}
	}
	if effective == "" {
		return "", fmt.Errorf("provider %q has no Model and no override was given — pass a model explicitly", provider)
	}
	ap.Provider = provider
	ap.Model = strings.TrimSpace(modelID)
	if err := lc.save(ac.cfgDir); err != nil {
		return "", err
	}
	ac.publishRuntime(lc.Agents) // invalidates the per-profile router cache
	return fmt.Sprintf("agent %q → %s/%s (next spawn uses the new model)", name, provider, effective), nil
}

// SetSoul updates ONLY the identity markdown for an existing profile —
// faster than re-typing the whole Add line. Empty soul clears it (agent
// falls back to the role-stamped base soul, same as an unprofiled spawn).
func (ac *agentControl) SetSoul(name, soulText string) (string, error) {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("agent name is required")
	}
	lc, err := loadLaunchConfig(ac.cfgDir)
	if err != nil {
		return "", err
	}
	ap := lc.Agents[name]
	if ap == nil {
		return "", fmt.Errorf("no agent named %q (see /agents)", name)
	}
	ap.Soul = strings.TrimSpace(soulText)
	if err := lc.save(ac.cfgDir); err != nil {
		return "", err
	}
	ac.publishRuntime(lc.Agents)
	if ap.Soul == "" {
		return fmt.Sprintf("agent %q: soul cleared (falls back to role-stamped base soul)", name), nil
	}
	return fmt.Sprintf("agent %q: soul updated (%d chars)", name, len(ap.Soul)), nil
}

// Remove deletes a named profile. Spawns using that role-name after removal
// fall back to the orchestrator's default (existing behavior).
func (ac *agentControl) Remove(name string) (string, error) {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("agent name is required")
	}
	lc, err := loadLaunchConfig(ac.cfgDir)
	if err != nil {
		return "", err
	}
	if lc.Agents[name] == nil {
		if builtinAgentProfiles()[name] != nil {
			return "", fmt.Errorf("%q is a built-in specialist (read-only); add a profile of the same name to override it", name)
		}
		return "", fmt.Errorf("no agent named %q", name)
	}
	delete(lc.Agents, name)
	if err := lc.save(ac.cfgDir); err != nil {
		return "", err
	}
	ac.publishRuntime(lc.Agents)
	return fmt.Sprintf("removed agent %q", name), nil
}

// Show returns the full profile (incl. soul markdown) for one agent, for
// /agents show in the TUI. Useful for confirming the identity text without
// dropping to the config file.
func (ac *agentControl) Show(name string) (tui.AgentDetail, error) {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	name = strings.TrimSpace(name)
	lc, err := loadLaunchConfig(ac.cfgDir)
	if err != nil {
		return tui.AgentDetail{}, err
	}
	ap := lc.Agents[name]
	if ap == nil {
		ap = effectiveAgents(lc)[name] // built-in specialists are shown read-only
	}
	if ap == nil {
		return tui.AgentDetail{}, fmt.Errorf("no agent named %q", name)
	}
	pc := lc.Providers[ap.Provider]
	mdl := ap.Model
	if mdl == "" && pc != nil {
		mdl = pc.Model
	}
	if mdl == "" && pc != nil {
		if pk, ok := providerKinds[pc.Kind]; ok {
			mdl = pk.DefaultModel
		}
	}
	if ap.Provider == "" { // built-in inherits the primary
		mdl = "(your model)"
	}
	return tui.AgentDetail{
		Name: name, Provider: ap.Provider, Model: mdl,
		Description: ap.Description, Soul: ap.Soul,
	}, nil
}

// publishRuntime sends the updated registry to the live TeamRuntime so the
// router cache invalidates and the NEXT spawn picks up the new settings.
// No-op when rt is nil (CLI mode).
func (ac *agentControl) publishRuntime(agents map[string]*agentProfile) {
	if ac.rt == nil {
		return
	}
	// Re-merge the built-in specialists so a live /agents edit can't drop them
	// from the running registry (TEN-132).
	ac.rt.SetAgentProfiles(effectiveAgents(&launchConfig{Agents: agents}))
}

// safeAgentName rejects names that would break filename / id semantics
// (spawn ids are formed as `<orchID>-<slug(name)>-<seq>`).
func safeAgentName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			return false
		}
	}
	return true
}

// --- CLI: `tenant agents` subcommands ---------------------------------

// cmdAgents dispatches `tenant agents list|add|remove|show|soul`. All edits
// write straight to launchConfig (no live runtime to publish to from the
// CLI — config-only).
func cmdAgents(_ context.Context, args []string) error {
	if len(args) == 0 {
		return cmdAgentsList(nil)
	}
	sub := args[0]
	rest := args[1:]
	switch strings.ToLower(sub) {
	case "list", "ls":
		return cmdAgentsList(rest)
	case "add", "set":
		return cmdAgentsAdd(rest)
	case "remove", "rm", "delete":
		return cmdAgentsRemove(rest)
	case "show":
		return cmdAgentsShow(rest)
	case "soul":
		return cmdAgentsSoul(rest)
	case "rename", "mv":
		return cmdAgentsRename(rest)
	case "model":
		return cmdAgentsModel(rest)
	default:
		return fmt.Errorf("usage: tenant agents [list|add|model|rename|soul|show|remove]")
	}
}

func cmdAgentsList(_ []string) error {
	c := &commonFlags{}
	if err := c.resolveDirs(); err != nil {
		return err
	}
	ac := &agentControl{cfgDir: c.cfgDir}
	rows, err := ac.List()
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("no named agents configured — try `tenant agents add <name> --provider <id>`")
		return nil
	}
	for _, r := range rows {
		mark := "✓"
		if !r.Valid {
			mark = "⚠"
		}
		soul := ""
		if r.HasSoul {
			soul = " [has soul]"
		}
		desc := ""
		if r.Description != "" {
			desc = " — " + r.Description
		}
		fmt.Printf("  %s %-20s  %s/%s%s%s\n", mark, r.Name, r.Provider, r.Model, soul, desc)
	}
	return nil
}

func cmdAgentsAdd(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: tenant agents add <name> <provider> [model] [--description \"...\"] [--soul \"...\"]\n   or: tenant agents add <name> --provider <id> [--model X] [--description ...] [--soul ...]")
	}
	name := args[0]
	rest := args[1:]
	provider, model, desc, soul := "", "", "", ""
	// Two-pass: first collect named flags, then absorb leftover positionals
	// as <provider> + optional <model> (most-natural shell form).
	var positionals []string
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--provider", "-p":
			i++
			if i < len(rest) {
				provider = rest[i]
			}
		case "--model", "-m":
			i++
			if i < len(rest) {
				model = rest[i]
			}
		case "--description", "-d":
			i++
			if i < len(rest) {
				desc = rest[i]
			}
		case "--soul":
			i++
			if i < len(rest) {
				soul = rest[i]
			}
		default:
			positionals = append(positionals, rest[i])
		}
	}
	// Positionals: first = provider (if not set), second = model (if not set).
	if provider == "" && len(positionals) >= 1 {
		provider = positionals[0]
		positionals = positionals[1:]
	}
	if model == "" && len(positionals) >= 1 {
		model = positionals[0]
	}
	c := &commonFlags{}
	if err := c.resolveDirs(); err != nil {
		return err
	}
	ac := &agentControl{cfgDir: c.cfgDir}
	status, err := ac.Add(name, provider, model, desc, soul)
	if err != nil {
		return err
	}
	fmt.Println(status)
	return nil
}

func cmdAgentsRemove(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: tenant agents remove <name>")
	}
	c := &commonFlags{}
	if err := c.resolveDirs(); err != nil {
		return err
	}
	ac := &agentControl{cfgDir: c.cfgDir}
	status, err := ac.Remove(args[0])
	if err != nil {
		return err
	}
	fmt.Println(status)
	return nil
}

func cmdAgentsShow(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: tenant agents show <name>")
	}
	c := &commonFlags{}
	if err := c.resolveDirs(); err != nil {
		return err
	}
	ac := &agentControl{cfgDir: c.cfgDir}
	d, err := ac.Show(args[0])
	if err != nil {
		return err
	}
	fmt.Printf("name:        %s\nprovider:    %s\nmodel:       %s\ndescription: %s\n",
		d.Name, d.Provider, d.Model, d.Description)
	if d.Soul != "" {
		fmt.Printf("\n--- soul ---\n%s\n", d.Soul)
	}
	return nil
}

func cmdAgentsSoul(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: tenant agents soul <name> <markdown...>  (use \"\" to clear)")
	}
	c := &commonFlags{}
	if err := c.resolveDirs(); err != nil {
		return err
	}
	ac := &agentControl{cfgDir: c.cfgDir}
	status, err := ac.SetSoul(args[0], strings.Join(args[1:], " "))
	if err != nil {
		return err
	}
	fmt.Println(status)
	return nil
}

func cmdAgentsRename(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: tenant agents rename <old> <new>")
	}
	c := &commonFlags{}
	if err := c.resolveDirs(); err != nil {
		return err
	}
	ac := &agentControl{cfgDir: c.cfgDir}
	status, err := ac.Rename(args[0], args[1])
	if err != nil {
		return err
	}
	fmt.Println(status)
	return nil
}

func cmdAgentsModel(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: tenant agents model <name> <provider> [model]")
	}
	c := &commonFlags{}
	if err := c.resolveDirs(); err != nil {
		return err
	}
	ac := &agentControl{cfgDir: c.cfgDir}
	mdl := ""
	if len(args) >= 3 {
		mdl = args[2]
	}
	status, err := ac.SetModel(args[0], args[1], mdl)
	if err != nil {
		return err
	}
	fmt.Println(status)
	return nil
}
