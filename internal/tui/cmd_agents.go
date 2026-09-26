package tui

// Sub-agent commands: /agents.

import (
	"fmt"
	"strings"
	"tenant/internal/cmdreg"

	tea "github.com/charmbracelet/bubbletea"
)

// slashAgents runs /agents, /agent.
func slashAgents(m *model, line string) tea.Cmd {
	_, _, arg := slashParts(line)
	// `/agents model` and `/agents model <name>` (no provider) open the
	// arrow-key provider→model picker (TEN-139 follow-up). Everything else —
	// including the direct `/agents model <name> <provider> [model]` form —
	// falls through to the text handler.
	if cmd := m.handleAgentsModelPicker(arg); cmd != nil {
		return cmd
	}
	m.handleAgents(arg)
	return nil
}

// handleAgents routes /agents [sub] [args...] — manages the named sub-agent
// profile registry. Bare `/agents` lists; sub-commands `add / remove / show
// / soul` mutate. Mutations apply LIVE via the AgentControl implementation.
func (m *model) handleAgents(arg string) {
	if m.cfg.Agents == nil {
		m.sysChat("agents control is not available in this session")
		return
	}
	f := strings.Fields(arg)
	if len(f) == 0 {
		// List.
		rows, err := m.cfg.Agents.List()
		if err != nil {
			m.sysChat("agents list: " + err.Error())
			return
		}
		if len(rows) == 0 {
			m.sysChat("no named agents configured. add one with:\n  /agents add <name> <provider> [<model>] [-- <description>]\nexample:\n  /agents add researcher zai glm-4.6 -- web-research specialist on Z.ai")
			return
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d named agent(s):\n", len(rows))
		for _, r := range rows {
			mark := "✓"
			if !r.Valid {
				mark = "⚠"
			}
			soul := ""
			if r.HasSoul {
				soul = " · has soul"
			}
			fmt.Fprintf(&b, "  %s %-20s %s/%s%s", mark, r.Name, r.Provider, r.Model, soul)
			if r.Description != "" {
				fmt.Fprintf(&b, " — %s", r.Description)
			}
			b.WriteString("\n")
		}
		b.WriteString("(use /agents show <name> to see the full identity, /agents soul <name> <md> to edit identity)")
		m.sysChat(strings.TrimRight(b.String(), "\n"))
		return
	}
	sub := strings.ToLower(f[0])
	switch sub {
	case "add", "set":
		// /agents add <name> <provider> [<model>] [-- <description>]
		// The optional `-- <description>` lets the operator write a free-form
		// description that the orchestrator sees ("when to use this agent").
		if len(f) < 3 {
			m.sysChat("usage: /agents add <name> <provider> [<model>] [-- <description>]   e.g. /agents add researcher zai glm-4.6 -- deep-web researcher")
			return
		}
		name := f[1]
		provider := f[2]
		mdl := ""
		desc := ""
		// Find the optional `--` separator for description.
		descAt := -1
		for i, w := range f {
			if w == "--" && i >= 3 {
				descAt = i
				break
			}
		}
		if descAt >= 0 {
			if descAt > 3 {
				mdl = f[3]
			}
			desc = strings.TrimSpace(strings.Join(f[descAt+1:], " "))
		} else if len(f) >= 4 {
			mdl = f[3]
		}
		status, err := m.cfg.Agents.Add(name, provider, mdl, desc, "")
		if err != nil {
			m.appendFeed(cErr.Render("✗ agents add: " + clip(err.Error(), 80)))
			m.sysChat("agents add: " + err.Error())
			return
		}
		m.appendFeed(cOK.Render("＋ agent: " + name))
		m.sysChat(status)
	case "remove", "rm", "delete":
		if len(f) < 2 {
			m.sysChat("usage: /agents remove <name>")
			return
		}
		status, err := m.cfg.Agents.Remove(f[1])
		if err != nil {
			m.sysChat("agents remove: " + err.Error())
			return
		}
		m.sysChat("🗑  " + status)
	case "rename", "mv":
		// /agents rename <old> <new> — move a profile to a new name lossless.
		if len(f) < 3 {
			m.sysChat("usage: /agents rename <old> <new>")
			return
		}
		status, err := m.cfg.Agents.Rename(f[1], f[2])
		if err != nil {
			m.appendFeed(cErr.Render("✗ agents rename: " + clip(err.Error(), 80)))
			m.sysChat("agents rename: " + err.Error())
			return
		}
		m.appendFeed(cOK.Render("✎ renamed: " + f[1] + " → " + f[2]))
		m.sysChat(status)
	case "model":
		// /agents model <name> <provider> [model]
		// Swap just the provider+model pinning. Preserves soul + description.
		// (The picker forms `/agents model` and `/agents model <name>` are
		// intercepted before this handler — see handleAgentsModelPicker.)
		if len(f) < 3 {
			m.sysChat("usage: /agents model <name> <provider> [model]   — or just `/agents model <name>` for an arrow-key picker over your configured providers/models")
			return
		}
		mdl := ""
		if len(f) >= 4 {
			mdl = f[3]
		}
		status, err := m.cfg.Agents.SetModel(f[1], f[2], mdl)
		if err != nil {
			m.appendFeed(cErr.Render("✗ agents model: " + clip(err.Error(), 80)))
			m.sysChat("agents model: " + err.Error())
			return
		}
		m.appendFeed(cOK.Render("⇄ " + f[1] + " → " + f[2]))
		m.sysChat(status)
	case "show":
		if len(f) < 2 {
			m.sysChat("usage: /agents show <name>")
			return
		}
		d, err := m.cfg.Agents.Show(f[1])
		if err != nil {
			m.sysChat("agents show: " + err.Error())
			return
		}
		var b strings.Builder
		fmt.Fprintf(&b, "agent: %s\n", d.Name)
		fmt.Fprintf(&b, "model: %s/%s\n", d.Provider, d.Model)
		if d.Description != "" {
			fmt.Fprintf(&b, "description: %s\n", d.Description)
		}
		if strings.TrimSpace(d.Soul) != "" {
			fmt.Fprintf(&b, "\n--- soul ---\n%s", d.Soul)
		} else {
			b.WriteString("\n(no custom soul — agent uses the role-stamped base soul)")
		}
		m.msgs = append(m.msgs, chatMsg{role: "assistant", content: b.String()})
	case "soul":
		// /agents soul <name> <markdown...>
		// Multi-word soul body is joined back into one string (operators can
		// paste multi-line markdown by escaping newlines as \n — see docs).
		if len(f) < 3 {
			m.sysChat("usage: /agents soul <name> <markdown>   (empty markdown clears)")
			return
		}
		name := f[1]
		body := strings.Join(f[2:], " ")
		status, err := m.cfg.Agents.SetSoul(name, body)
		if err != nil {
			m.sysChat("agents soul: " + err.Error())
			return
		}
		m.sysChat(status)
	default:
		m.sysChat("usage: /agents [list]   |   /agents add <name> <provider> [model] [-- desc]   |   /agents model <name> <provider> [model]   |   /agents rename <old> <new>   |   /agents soul <name> <md>   |   /agents show <name>   |   /agents remove <name>")
	}
}

// handleAgentsModelPicker intercepts the picker forms of `/agents model`
// (TEN-139 follow-up): point an agent at an ALREADY-CONFIGURED provider with an
// arrow-key picker instead of typing `<provider> <model>` from memory. Returns
// nil for every other `/agents …` input (incl. the direct
// `/agents model <name> <provider> [model]` form) so the text handler runs.
//
//	/agents model            → pick an agent, then its provider, then its model
//	/agents model <name>      → pick a provider, then a model, for <name>
func (m *model) handleAgentsModelPicker(arg string) tea.Cmd {
	if m.cfg.Agents == nil || m.cfg.Models == nil {
		return nil // no picker without both controls; text handler reports it
	}
	f := strings.Fields(arg)
	if len(f) == 0 || strings.ToLower(f[0]) != "model" {
		return nil
	}
	switch len(f) {
	case 1: // bare: pick the agent first
		return m.startAgentPickerForModel()
	case 2: // /agents model <name>: provider→model picker for <name>
		return m.startAgentModelPicker(f[1])
	default: // <name> <provider> [model]: direct path in handleAgents
		return nil
	}
}

// startAgentPickerForModel opens an agent picker (built-ins included), then
// chains into startAgentModelPicker for the chosen one.
func (m *model) startAgentPickerForModel() tea.Cmd {
	rows, err := m.cfg.Agents.List()
	if err != nil {
		m.sysChat("agents list: " + err.Error())
		return nil
	}
	if len(rows) == 0 {
		m.sysChat("no agents to pin — add one with `/agents add <name> <provider> [model]`")
		return nil
	}
	labels := make([]string, 0, len(rows))
	byLabel := make(map[string]string, len(rows))
	for _, r := range rows {
		label := r.Name
		if r.Provider != "" {
			label += "  ·  " + r.Provider + "/" + r.Model
		}
		base := label
		for n := 2; ; n++ {
			if _, dup := byLabel[label]; !dup {
				break
			}
			label = fmt.Sprintf("%s (%d)", base, n)
		}
		labels = append(labels, label)
		byLabel[label] = r.Name
	}
	return startPickerCmd(&listPicker{
		title:    "Pin a model — pick an agent",
		hint:     "↑/↓ select · enter choose provider · esc cancel",
		items:    labels,
		selected: 0,
		onSelect: func(choice string) tea.Cmd {
			return m.startAgentModelPicker(byLabel[choice])
		},
		onCancel: func() tea.Cmd {
			return func() tea.Msg { return sysChatMsg{text: "agent model pin cancelled"} }
		},
	})
}

// startAgentModelPicker drives provider→model selection for one agent and pins
// it via Agents.SetModel — the arrow-key sibling of startModelPicker, but the
// terminal action targets a named sub-agent instead of the primary router.
// Reuses listPicker + the pickerStartMsg async pattern; composes ModelList +
// ListProviderModels + Agents.SetModel (no backend changes).
func (m *model) startAgentModelPicker(agentName string) tea.Cmd {
	agentName = strings.TrimSpace(agentName)
	if agentName == "" {
		m.sysChat("usage: /agents model <name>")
		return nil
	}
	infos := m.cfg.Models.ModelList()
	if len(infos) == 0 {
		m.sysChat("no providers configured — add one with `/model add-cloud <kind> <api-key>` (zai/openai/grok/anthropic) or `/setup`, then point " + agentName + " at it")
		return nil
	}
	ctl := m.cfg.Models
	ag := m.cfg.Agents

	// fetchAndPick: fetch a provider's live models off the UI goroutine, then
	// open the model picker. Empty list → pin the provider's saved default;
	// fetch error → fail closed with guidance (no change).
	fetchAndPick := func(providerName string) tea.Cmd {
		m.appendFeed(cDim.Render("⟳ fetching models from " + providerName + "…"))
		return func() tea.Msg {
			models, err := ctl.ListProviderModels(providerName)
			if err != nil {
				return sysChatMsg{text: "could not fetch models from " + providerName + ": " + safeErr(err) +
					"\n(try `/agents model " + agentName + " " + providerName + "` to pin its default without picking)"}
			}
			if len(models) == 0 {
				s, aerr := ag.SetModel(agentName, providerName, "")
				if aerr != nil {
					return sysChatMsg{text: providerName + " reported no models, and pinning failed: " + aerr.Error()}
				}
				return sysChatMsg{text: providerName + " reported no models — pinned " + agentName + " to its saved default:\n" + s}
			}
			return pickerStartMsg{picker: &listPicker{
				title:    "Pin " + agentName + " → pick a model on " + providerName,
				hint:     "↑/↓ select · enter pin · esc cancel",
				items:    models,
				selected: 0,
				onSelect: func(choice string) tea.Cmd {
					return func() tea.Msg {
						s, perr := ag.SetModel(agentName, providerName, choice)
						if perr != nil {
							return sysChatMsg{text: "✗ pin " + agentName + " → " + providerName + "/" + choice + " failed: " + perr.Error()}
						}
						return sysChatMsg{text: "⇄ " + s}
					}
				},
				onCancel: func() tea.Cmd {
					return func() tea.Msg { return sysChatMsg{text: "kept current model for " + agentName} }
				},
			}}
		}
	}

	// Single provider → skip the provider pick.
	if len(infos) == 1 {
		return fetchAndPick(infos[0].Name)
	}

	labels := make([]string, 0, len(infos))
	byLabel := make(map[string]ModelInfo, len(infos))
	for _, info := range infos {
		label := info.Name
		if info.Model != "" {
			label += "  ·  " + info.Model
		}
		base := label
		for n := 2; ; n++ {
			if _, dup := byLabel[label]; !dup {
				break
			}
			label = fmt.Sprintf("%s (%d)", base, n)
		}
		labels = append(labels, label)
		byLabel[label] = info
	}
	return startPickerCmd(&listPicker{
		title:    "Pin " + agentName + " → pick a provider",
		hint:     "↑/↓ select · enter fetch models · esc cancel",
		items:    labels,
		selected: 0,
		onSelect: func(choice string) tea.Cmd {
			return fetchAndPick(byLabel[choice].Name)
		},
		onCancel: func() tea.Cmd {
			return func() tea.Msg { return sysChatMsg{text: "agent model pin cancelled"} }
		},
	})
}

// agentCommands are the named sub-agent commands.
func agentCommands() []*slashCommand {
	return []*slashCommand{
		{Names: []string{"/agents", "/agent"}, Section: "agents", Run: slashAgents,
			Summary: "list, add, re-model, rename and remove named sub-agents",
			Rows: []cmdreg.Row{
				{Syntax: "/agents", Desc: "list named sub-agent profiles"},
				{Syntax: "/agents add <name> <provider> [model] [-- desc]", Desc: "register a sub-agent"},
				{Syntax: "/agents model <name> <provider> [model]", Desc: "swap an agent's model (live; soul preserved)"},
				{Syntax: "/agents rename <old> <new>", Desc: "rename an agent (all fields preserved)"},
				{Syntax: "/agents soul <name> <markdown>", Desc: "set the agent's identity (empty clears)"},
				{Syntax: "/agents show <name>", Desc: "show an agent's full identity + model"},
				{Syntax: "/agents remove <name>", Desc: "delete an agent profile"},
			}},
	}
}
