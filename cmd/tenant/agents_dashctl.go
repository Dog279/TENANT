package main

// agents_dashctl.go: the dashboard's Helpers page over the terminal app's
// agentControl (TEN-209). Every change passes straight through, so a web edit
// saves config.json and republishes the live registry (publishRuntime, which
// flushes the per-helper model cache) exactly as /agents does. The adapter
// adds only what the page needs on top: who a helper's work is sent to, the
// dashboard's stricter name rules, and plain wording for the errors.

import (
	"fmt"
	"runtime"
	"strings"

	"tenant/internal/dashboard"
)

// dashAgents adapts agentControl to dashboard.AgentControl. service marks a
// Windows service install, where restarting Tenant needs directions.
type dashAgents struct {
	ac      *agentControl
	service bool
}

// newDashAgents adapts ac; nil (an untyped nil interface) leaves the page
// showing "isn't available".
func newDashAgents(ac *agentControl) dashboard.AgentControl {
	if ac == nil {
		return nil
	}
	return dashAgents{ac: ac, service: runtime.GOOS == "windows" && runAsService()}
}

func (d dashAgents) Agents() ([]dashboard.AgentProfileView, error) {
	rows, err := d.ac.List()
	if err != nil {
		return nil, err
	}
	lc, err := loadLaunchConfig(d.ac.cfgDir)
	if err != nil {
		return nil, err
	}
	builtins := builtinAgentProfiles()
	out := make([]dashboard.AgentProfileView, 0, len(rows))
	for _, r := range rows {
		v := dashboard.AgentProfileView{
			Name: r.Name, Provider: r.Provider, Description: r.Description,
			HasSoul: r.HasSoul, Builtin: r.Builtin, Valid: r.Valid,
			// An override of a shipped name: Reset (Remove) restores it.
			Customized: !r.Builtin && builtins[r.Name] != nil,
		}
		if r.Provider != "" { // "" = the main model; List labels it "(your model)"
			v.Model = r.Model
			if pc := lc.Providers[r.Provider]; pc != nil {
				v.SendsTo = sendsTo(pc.Kind)
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// sendsTo names who a hosted-API provider kind sends a helper's work to. A
// self-hosted kind (Local in the catalog: vLLM, llama.cpp, Ollama, even on
// another machine) and a kind the catalog doesn't know say nothing.
func sendsTo(kind string) string {
	pk, ok := providerKinds[kind]
	if !ok || pk.Local {
		return ""
	}
	if kind == "claudecode" { // the claude CLI's requests go to Anthropic
		return "Anthropic"
	}
	name, _, _ := strings.Cut(pk.Label, " (") // "Grok (xAI)" → "Grok"
	return name
}

func (d dashAgents) Soul(name string) (string, error) {
	det, err := d.ac.Show(name)
	return det.Soul, err
}

func (d dashAgents) Add(name, provider, model, description string) error {
	// agentControl.Add replaces a profile of the same name, which would wipe
	// a built-in's instructions: the page's Add only ever adds.
	if err := d.nameFree(name, ""); err != nil {
		return err
	}
	_, err := d.ac.Add(name, provider, model, description, "")
	return plainAgentErr(err)
}

func (d dashAgents) SetModel(name, provider, model string) error {
	_, err := d.ac.SetModel(name, provider, model)
	return plainAgentErr(err)
}

func (d dashAgents) SetSoul(name, soul string) error {
	_, err := d.ac.SetSoul(name, soul)
	return plainAgentErr(err)
}

func (d dashAgents) Rename(oldName, newName string) error {
	// A customized built-in lives in config like any helper, and moving it
	// would bring the shipped one back under the old name.
	for n := range builtinAgentProfiles() {
		if strings.EqualFold(n, strings.TrimSpace(oldName)) {
			return dashboard.PlainError("Built-in helpers can't be renamed.")
		}
	}
	if err := d.nameFree(newName, oldName); err != nil {
		return err
	}
	_, err := d.ac.Rename(oldName, newName)
	return plainAgentErr(err)
}

func (d dashAgents) Remove(name string) error {
	_, err := d.ac.Remove(name)
	return plainAgentErr(err)
}

// RestartNote is how to restart Tenant, where the page's "when Tenant
// restarts" needs it: a Windows service has no window to close.
func (d dashAgents) RestartNote() string {
	if d.service {
		return "Restart the Tenant service (open Services, find Tenant, click Restart)."
	}
	return ""
}

// nameFree refuses a name another helper has, built-ins included, in any
// letter case: spawns match names that way (routerForProfile). except is the
// helper being renamed, so a change of case alone goes through.
func (d dashAgents) nameFree(name, except string) error {
	rows, err := d.ac.List()
	if err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	for _, r := range rows {
		if strings.EqualFold(r.Name, name) && r.Name != strings.TrimSpace(except) {
			return dashboard.PlainError(fmt.Sprintf("A helper named %s already exists.", r.Name))
		}
	}
	return nil
}

// plainAgentErrs phrase agentControl's validation errors for the page (D21),
// matched on its own wording (TestDashAgents_PlainErrors pins each one).
// Anything else stays off the page.
var plainAgentErrs = []struct{ has, say string }{
	{"unsafe characters", "Helper names can use only letters, numbers, dashes and underscores."},
	{"is not configured", "That model provider isn't set up yet. Add it on the Model page."},
	{"has no Model", "That model provider has no default model. Type the model's name."},
	{"built-in specialist", "Built-in helpers can't be removed."},
	{"already exists", "A helper with that name already exists."},
}

func plainAgentErr(err error) error {
	if err == nil {
		return nil
	}
	for _, p := range plainAgentErrs {
		if strings.Contains(err.Error(), p.has) {
			return dashboard.PlainError(p.say)
		}
	}
	return err
}
