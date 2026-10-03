package tui

// Model commands: /model, /route, /ceiling, /reasoning, /eval, /judge.

import (
	"fmt"
	"strconv"
	"strings"
	"tenant/internal/cmdreg"

	tea "github.com/charmbracelet/bubbletea"
)

// slashJudge runs /judge.
func slashJudge(m *model, line string) tea.Cmd {
	_, _, arg := slashParts(line)
	// Choose the eval LLM-judge model (TEN-91): default = planner (self-judge),
	// or pin a separate model for `tenant eval` + the nightly gate.
	if m.cfg.Judge == nil {
		m.sysChat("judge control not available in this session")
		return nil
	}
	jf := strings.Fields(arg)
	sub := ""
	if len(jf) > 0 {
		sub = strings.ToLower(jf[0])
	}
	switch {
	case sub == "" || sub == "status":
		m.sysChat(m.cfg.Judge.Current())
	case sub == "clear" || sub == "default" || sub == "reset":
		if err := m.cfg.Judge.Clear(); err != nil {
			m.sysChat("judge: " + err.Error())
		} else {
			m.sysChat("judge: reverted to the planner model (self-judging).")
		}
	case sub == "set":
		// /judge set <kind> <model> [endpoint]
		if len(jf) < 3 {
			m.sysChat("usage: /judge set <kind> <model> [endpoint]   (e.g. /judge set anthropic claude-opus-4-8)")
			return nil
		}
		endpoint := ""
		if len(jf) >= 4 {
			endpoint = jf[3]
		}
		msg, err := m.cfg.Judge.Set(jf[1], jf[2], endpoint)
		if err != nil {
			m.sysChat("judge: " + err.Error())
		} else {
			m.sysChat("🔬 " + msg)
		}
	default:
		m.sysChat("usage: /judge [status | set <kind> <model> [endpoint] | clear]")
	}
	return nil
}

// slashModel runs /model, /models.
func slashModel(m *model, line string) tea.Cmd {
	_, _, arg := slashParts(line)
	if cmd := m.handleModel(arg); cmd != nil {
		return cmd
	}
	return nil
}

// slashEval runs /eval.
func slashEval(m *model, line string) tea.Cmd {
	_, _, arg := slashParts(line)
	m.handleEval(arg)
	return nil
}

// slashRoute runs /route.
func slashRoute(m *model, line string) tea.Cmd {
	_, _, arg := slashParts(line)
	return m.handleRoute(arg)
}

// slashCeiling runs /ceiling, /loops, /loop-ceiling.
func slashCeiling(m *model, line string) tea.Cmd {
	_, _, arg := slashParts(line)
	if m.cfg.Models == nil {
		m.sysChat("loop-ceiling tuning is not available in this session")
		return nil
	}
	if strings.TrimSpace(arg) == "" {
		m.sysChat(fmt.Sprintf("loop ceiling = %d (max planner↔tool iterations per turn, per agent) — set with /ceiling <n>", m.cfg.Models.LoopCeiling()))
		return nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(arg))
	if err != nil {
		m.sysChat("usage: /ceiling <number>   (e.g. /ceiling 30)")
		return nil
	}
	status, err := m.cfg.Models.SetLoopCeiling(n)
	if err != nil {
		m.sysChat("error: " + err.Error())
		return nil
	}
	m.appendFeed(cOK.Render("⟳ " + status))
	m.sysChat(status)
	return nil
}

// slashReasoning runs /reasoning, /effort.
func slashReasoning(m *model, line string) tea.Cmd {
	_, _, arg := slashParts(line)
	if m.cfg.Models == nil {
		m.sysChat("reasoning-effort tuning is not available in this session")
		return nil
	}
	if cmd := m.handleReasoning(strings.TrimSpace(arg)); cmd != nil {
		return cmd
	}
	return nil
}

// handleEval powers /eval: view + live-tune the nightly eval schedule
// (TEN-196). Mutations persist via the control and re-arm the scheduler in
// place; "now" rides the improve scheduler's own goroutine, so the run lands
// in the feed and trend log exactly like a scheduled one.
func (m *model) handleEval(arg string) {
	if m.cfg.Eval == nil {
		m.sysChat("eval schedule not available in this session")
		return
	}
	say := func(msg string, err error) {
		if err != nil {
			m.sysChat("eval: " + err.Error())
			return
		}
		m.sysChat(msg)
	}
	f := strings.Fields(arg)
	switch {
	case len(f) == 0:
		m.sysChat(m.cfg.Eval.Status())
	case f[0] == "every" && len(f) == 2:
		say(m.cfg.Eval.SetEvery(f[1]))
	case f[0] == "at" && len(f) == 2:
		say(m.cfg.Eval.SetAt(f[1]))
	case f[0] == "off" && len(f) == 1:
		say(m.cfg.Eval.Off())
	case f[0] == "now" && len(f) == 1:
		say(m.cfg.Eval.RunNow())
	case f[0] == "trend":
		n := 10
		if len(f) == 2 {
			if v, err := strconv.Atoi(f[1]); err == nil && v > 0 {
				n = v
			}
		}
		m.sysChat(m.cfg.Eval.Trend(n))
	case f[0] == "diff" && len(f) == 1:
		say(m.cfg.Eval.Diff())
	default:
		m.sysChat("usage: /eval | /eval every <dur> | /eval at <HH:MM> | /eval off | /eval now | /eval trend [n] | /eval diff")
	}
}

// handleRoute powers /route. Probe/stat reads run off the UI goroutine; config
// mutations are short atomic writes and apply to the next turn.
func (m *model) handleRoute(arg string) tea.Cmd {
	if m.cfg.Route == nil {
		m.sysChat("prompt routing not available in this session")
		return nil
	}
	f := strings.Fields(arg)
	sub := ""
	if len(f) > 0 {
		sub = strings.ToLower(f[0])
	}
	switch {
	case len(f) == 0 || (sub == "status" && len(f) == 1):
		ctl, ctx := m.cfg.Route, m.ctx
		return func() tea.Msg {
			return sysChatMsg{text: ctl.Status(ctx)}
		}
	case sub == "stats" && len(f) == 1:
		ctl := m.cfg.Route
		return func() tea.Msg { return sysChatMsg{text: ctl.Stats()} }
	case sub == "configure" && len(f) == 1:
		return m.startRouteConfigure()
	case (sub == "on" || sub == "off" || sub == "shadow" || sub == "assisted" || sub == "everywhere") && len(f) == 1:
		status, err := m.cfg.Route.SetMode(sub)
		if err != nil {
			m.sysChat("route: " + err.Error())
		} else {
			m.sysChat(status)
		}
		return nil
	case sub == "force" && len(f) == 2:
		status, err := m.cfg.Route.Force(f[1])
		if err != nil {
			m.sysChat("route: " + err.Error())
		} else {
			m.sysChat(status)
		}
		return nil
	case sub == "why" && len(f) <= 2:
		n := 1
		if len(f) == 2 {
			v, err := strconv.Atoi(f[1])
			if err != nil || v <= 0 {
				m.sysChat("usage: /route <status|configure|on|off|shadow|assisted|everywhere|stats|force <profileID>|why [n]>")
				return nil
			}
			n = v
		}
		m.sysChat(m.cfg.Route.Why(n))
		return nil
	default:
		m.sysChat("usage: /route <status|configure|on|off|shadow|assisted|everywhere|stats|force <profileID>|why [n]>")
		return nil
	}
}

// handleModel runs /model subcommands: bare list, `use <name>` (live swap of
// the primary), and `add <name> <endpoint> [tool-format]` (register a new
// vLLM backend mid-session).
// handleModel runs a /model sub-command and returns an optional tea.Cmd
// (currently only add-cloud uses this — to dispatch the variant picker
// after the live model-list fetch finishes asynchronously). Named
// return lets the existing bare `return` statements stay as no-cmd
// short-circuits without churning every case.
func (m *model) handleModel(arg string) (cmd tea.Cmd) {
	if m.cfg.Models == nil {
		m.sysChat("model switching is not available in this session")
		return
	}
	f := strings.Fields(arg)
	if len(f) == 0 {
		m.sysChatStyled(m.renderModelList())
		return
	}
	switch strings.ToLower(f[0]) {
	case "reload", "refresh":
		// Re-resolve the active provider's key (env/credentials.json) + hot-swap
		// it live — for picking up a rotated key without a restart.
		m.appendFeed(cDim.Render("⟳ reloading active provider key…"))
		status, err := m.cfg.Models.ReloadKeys()
		if err != nil {
			m.sysChat("model: reload failed: " + err.Error())
		} else if status == "" {
			m.sysChat("model: no active provider to reload")
		} else {
			m.sysChat(status)
		}
		return
	case "fallback":
		// `/model fallback` shows the chain; `/model fallback <name...>` sets it;
		// `/model fallback off` clears it. (TEN-246)
		if len(f) == 1 {
			chain := m.cfg.Models.Fallback()
			if len(chain) == 0 {
				m.sysChat("no model fallback configured. Set one with `/model fallback <provider...>` so a rate-limited/unavailable model auto-routes to the next.")
			} else {
				m.sysChat("model fallback: " + strings.Join(chain, " → ") + "  (tried after the active model on 429 / out-of-credits / unreachable)\n" +
					"health gating: " + m.cfg.Models.HealthGating())
			}
			return
		}
		names := f[1:]
		if len(names) == 1 && strings.EqualFold(names[0], "off") {
			names = nil
		}
		status, err := m.cfg.Models.SetFallback(names)
		if err != nil {
			m.sysChat("model: fallback failed: " + err.Error())
		} else {
			m.sysChat("✓ " + status)
		}
		return
	case "use", "switch":
		if len(f) < 2 {
			// No provider named → open the arrow-key picker: pick a provider →
			// fetch its live cloud models → pick → swap. (TEN-173)
			cmd = m.startModelPicker()
			return
		}
		// Optional third arg: a model variant to pin on this provider.
		// Useful for multi-variant providers (Z.ai's glm-*, OpenAI's
		// gpt-*). Empty preserves the saved model. Use `/model models
		// <name>` to discover what's served.
		modelOverride := ""
		if len(f) >= 3 {
			modelOverride = f[2]
		}
		feedMsg := "⇄ switching model → " + f[1]
		if modelOverride != "" {
			feedMsg += " (" + modelOverride + ")"
		}
		feedMsg += "…"
		m.appendFeed(cDim.Render(feedMsg))
		status, active, err := m.cfg.Models.UseModel(f[1], modelOverride)
		if err != nil {
			m.appendFeed(cErr.Render("✗ model switch failed: " + clip(err.Error(), 70)))
			m.sysChat("could not switch: " + err.Error())
			return
		}
		if active != "" {
			m.cfg.Model = active // status bar reflects the new primary
		}
		// The status leads with ✓ (connected) or ⚠ (switched but degraded);
		// color the feed line accordingly.
		feedStyle := cOK
		if !strings.HasPrefix(status, "✓") {
			feedStyle = cErr
		}
		m.appendFeed(feedStyle.Render(status))
		m.sysChat(status)
		// If the swapped-to endpoint is unreachable, start auto-reconnect now
		// (don't make the user send a doomed message first).
		if strings.Contains(status, "UNREACHABLE") && m.cfg.Reconnect != nil {
			m.cfg.Reconnect.OnGenerationDown()
		}
	case "add":
		// /model add <name> <endpoint> [tool-format]
		// Keyless catalog kinds need neither an endpoint nor an API key:
		// `/model add claudecode` registers the Claude Code subscription
		// backend (claude -p) straight from the provider catalog.
		if len(f) == 2 {
			status, err := m.cfg.Models.AddModel(f[1], "", "")
			if err != nil {
				m.appendFeed(cErr.Render("✗ add backend failed: " + clip(err.Error(), 70)))
				m.sysChat("could not add backend: " + err.Error())
				return
			}
			m.appendFeed(cOK.Render("＋ backend added: " + f[1]))
			m.sysChat(status)
			return
		}
		if len(f) < 3 {
			m.sysChat("usage: /model add <name> <endpoint> [tool-format]   e.g. /model add gpu http://localhost:8000 qwen   |   keyless kinds: /model add claudecode")
			return
		}
		name, endpoint := f[1], f[2]
		doAdd := func(toolFmt string) {
			status, err := m.cfg.Models.AddModel(name, endpoint, toolFmt)
			if err != nil {
				m.appendFeed(cErr.Render("✗ add backend failed: " + clip(err.Error(), 70)))
				m.sysChat("could not add backend: " + err.Error())
				return
			}
			m.appendFeed(cOK.Render("＋ backend added: " + name + " (" + toolFmt + ")"))
			m.sysChat(status)
		}
		if len(f) >= 4 {
			doAdd(f[3]) // explicit format
			return
		}
		// No tool format given → arrow-key picker instead of silently defaulting
		// to gemma (a wrong format makes the model emit tool calls Tenant can't
		// parse). TEN-138. doAdd runs in the Update goroutine via onSelect.
		cmd = startPickerCmd(&listPicker{
			title:    "Tool format for " + name + " — how the model emits tool calls",
			hint:     "↑/↓ select · enter add · esc cancel  (match your model family; qwen for Qwen, etc.)",
			items:    modelToolFormats,
			selected: pickerIndexOf(modelToolFormats, defaultToolFormat),
			onSelect: func(choice string) tea.Cmd {
				return func() tea.Msg {
					doAdd(choice)
					return nil
				}
			},
			onCancel: func() tea.Cmd {
				return func() tea.Msg {
					return sysChatMsg{text: "model add cancelled — pick a tool format, or pass it explicitly: /model add " + name + " " + endpoint + " <qwen|gemma|llama|mistral|openai>"}
				}
			},
		})
		return
	case "add-cloud", "addcloud":
		// /model add-cloud <kind> <api-key>
		// One-shot setup for keyed cloud providers (zai, openai, grok, anthropic)
		// — pulls endpoint + default model + tool format from the catalog,
		// stores the key in credentials.json (0600). After the add succeeds,
		// fetches the live model catalog and shows an arrow-key picker so
		// the operator can pin a variant (glm-4.6 vs glm-5.1 etc.) without
		// dropping to the CLI or editing config.json.
		if len(f) < 3 {
			m.sysChat("usage: /model add-cloud <kind> <api-key>   e.g. /model add-cloud zai sk-xxxx   (kinds: zai, openai, grok, anthropic)")
			return
		}
		kind, key := f[1], f[2]
		status, err := m.cfg.Models.AddCloudModel(kind, key)
		if err != nil {
			m.appendFeed(cErr.Render("✗ add-cloud failed: " + clip(err.Error(), 70)))
			m.sysChat("could not add cloud backend: " + err.Error())
			return
		}
		m.appendFeed(cOK.Render("＋ cloud backend added: " + kind))
		m.sysChat(status)
		// Fetch live model variants in a tea.Cmd so the UI stays
		// responsive. On return, dispatch pickerStartMsg with the
		// onSelect wired to pin the choice.
		ctl := m.cfg.Models
		providerName := kind
		cmd = func() tea.Msg {
			models, err := ctl.ListProviderModels(providerName)
			if err != nil || len(models) == 0 {
				return sysChatMsg{
					text: "added but could not list model variants for picker: " + safeErr(err) +
						"\n(use `/model models " + providerName + "` later to see options)",
				}
			}
			return pickerStartMsg{picker: &listPicker{
				title: "Pick a model variant for " + providerName + " (" + fmt.Sprintf("%d available", len(models)) + ")",
				hint:  "↑/↓ select · enter pin & switch · esc keep catalog default",
				items: models,
				onSelect: func(choice string) tea.Cmd {
					return func() tea.Msg {
						s, _, perr := ctl.UseModel(providerName, choice)
						if perr != nil {
							return sysChatMsg{text: "pinned model but switch failed: " + perr.Error()}
						}
						return sysChatMsg{text: "✓ pinned " + choice + " on " + providerName + ":\n" + s}
					}
				},
				onCancel: func() tea.Cmd {
					return func() tea.Msg {
						return sysChatMsg{text: "kept catalog default for " + providerName + " (change later with `/model use " + providerName + " <model>`)"}
					}
				},
			}}
		}
	case "remove", "rm", "delete":
		if len(f) < 2 {
			m.sysChat("usage: /model remove <name>")
			return
		}
		status, err := m.cfg.Models.RemoveModel(f[1])
		if err != nil {
			m.sysChat("could not remove: " + err.Error())
			return
		}
		m.appendFeed(cSys.Render("－ backend removed: " + f[1]))
		m.sysChat(status)
	case "models", "model-list", "list-models":
		// /model models [<name>] — discover available model variants from
		// the provider's endpoint (live fetch). Without a name, queries
		// the currently-active provider.
		target := ""
		if len(f) >= 2 {
			target = f[1]
		}
		ids, err := m.cfg.Models.ListProviderModels(target)
		if err != nil {
			m.sysChat("could not list models: " + err.Error())
			return
		}
		if len(ids) == 0 {
			m.sysChat("(no models reported by the endpoint)")
			return
		}
		name := target
		if name == "" {
			name = "active"
		}
		var b strings.Builder
		fmt.Fprintf(&b, "models served by %q (%d):\n", name, len(ids))
		for _, id := range ids {
			fmt.Fprintf(&b, "  · %s\n", id)
		}
		b.WriteString("\nswitch + pin a variant with: /model use " + name + " <model>")
		m.sysChat(strings.TrimRight(b.String(), "\n"))
	case "pick", "picker", "choose":
		// Arrow-key picker: pick provider → live cloud models → swap. (TEN-173)
		cmd = m.startModelPicker()
		return
	default:
		m.sysChat("usage: /model   |   /model pick   |   /model use <name> [<model>]   |   /model models [<name>]   |   /model add <name> <endpoint> [fmt]   |   /model add-cloud <kind> <api-key>   |   /model remove <name>")
	}
	return cmd
}

// renderModelList builds the /model listing as (plain, styled).
func (m *model) renderModelList() (string, string) {
	models := m.cfg.Models.ModelList()
	if len(models) == 0 {
		return "no model backends configured — `/model add <name> <endpoint>` or run `tenant setup`", ""
	}
	var p, s strings.Builder
	p.WriteString("Model backends  ● active  ○ idle   ·   /model use <name>\n")
	s.WriteString(feedTitleStyle.Render("Model backends") + cDim.Render("   ● active  ○ idle   ·   /model use <name>") + "\n")
	for _, mi := range models {
		mark, marker := "○", cDim.Render("○")
		if mi.Active {
			mark, marker = "●", cOK.Render("●")
		}
		model := mi.Model
		if model == "" {
			model = "(auto)"
		}
		degr := ""
		if mi.Degraded {
			degr = "  (degraded — echo fallback; /model use to recover)"
		}
		line := fmt.Sprintf("  %s %-14s %-10s %s  %s%s", mark, mi.Name, mi.Kind, model, mi.Endpoint, degr)
		p.WriteString(line + "\n")
		styled := fmt.Sprintf("  %s %s %s %s  %s%s", marker,
			cUser.Render(fmt.Sprintf("%-14s", mi.Name)), fmt.Sprintf("%-10s", mi.Kind),
			model, cDim.Render(mi.Endpoint), cErr.Render(degr))
		s.WriteString(styled + "\n")
	}
	return strings.TrimRight(p.String(), "\n"), strings.TrimRight(s.String(), "\n")
}

// handleReasoning powers /reasoning (alias /effort): tune the active provider's
// reasoning-effort hint live. Bare → arrow-key picker (off/high/xhigh), mirroring
// /model pick; `/reasoning <level|off>` sets it directly. Gated to providers that
// support it (Sakana Fugu) so the operator can't set an effort that would 400.
func (m *model) handleReasoning(arg string) tea.Cmd {
	ctl := m.cfg.Models
	if !ctl.ReasoningSupported() {
		m.sysChat("the active provider doesn't support reasoning effort — reasoning models (Sakana Fugu) and claudecode do.\nswitch with /model pick, then /reasoning.")
		return nil
	}
	// Direct set: /reasoning <level|off>.
	if arg != "" {
		status, err := ctl.SetReasoningEffort(arg)
		if err != nil {
			m.sysChat("error: " + err.Error())
			return nil
		}
		m.appendFeed(cOK.Render("⟳ " + status))
		m.sysChat(status)
		return nil
	}
	// Bare /reasoning → arrow-key picker over the ACTIVE kind's level set
	// (Fugu: high/xhigh; claudecode: the Claude CLI's low→max ladder).
	items := []string{"off (default)"}
	items = append(items, ctl.ReasoningLevels()...)
	valueOf := func(label string) string {
		if label == "off (default)" {
			return "off"
		}
		return label
	}
	mark := "off (default)"
	if cur := ctl.ReasoningEffort(); cur != "" {
		for _, it := range items {
			if it == cur {
				mark = it
				break
			}
		}
	}
	return startPickerCmd(&listPicker{
		title:       "Reasoning effort — current: " + reasoningLabel(ctl.ReasoningEffort()),
		hint:        "↑/↓ select · enter apply live · esc cancel",
		items:       items,
		currentMark: mark,
		selected:    pickerIndexOf(items, mark),
		onSelect: func(choice string) tea.Cmd {
			return func() tea.Msg {
				status, err := ctl.SetReasoningEffort(valueOf(choice))
				if err != nil {
					return sysChatMsg{text: "✗ " + err.Error()}
				}
				return sysChatMsg{text: status}
			}
		},
		onCancel: func() tea.Cmd {
			return func() tea.Msg { return sysChatMsg{text: "reasoning effort unchanged"} }
		},
	})
}

// reasoningLabel renders a reasoning-effort value for display ("" → "off").
func reasoningLabel(effort string) string {
	if strings.TrimSpace(effort) == "" {
		return "off"
	}
	return effort
}

// startModelPicker drives the arrow-key model swap (TEN-173): pick a configured
// provider → fetch its live models from the cloud → pick a model → swap live.
// Mirrors the /model add-cloud picker chain (provider pick is the extra first
// stage). Reuses listPicker + the pickerStartMsg async pattern; no backend
// changes — composes ModelList + ListProviderModels + UseModel.
func (m *model) startModelPicker() tea.Cmd {
	infos := m.cfg.Models.ModelList()
	if len(infos) == 0 {
		m.sysChat("no model providers configured — add one with `/model add-cloud <kind> <api-key>` (zai/openai/grok/anthropic) or `/setup`")
		return nil
	}
	ctl := m.cfg.Models

	// fetchAndPick (STATE 2→3): fetch one provider's live models off the UI
	// goroutine, then open the model picker. Empty list → switch to the saved
	// default; fetch error → fail closed with guidance (no swap).
	fetchAndPick := func(providerName, currentModel string) tea.Cmd {
		m.appendFeed(cDim.Render("⟳ fetching models from " + providerName + "…"))
		return func() tea.Msg {
			models, err := ctl.ListProviderModels(providerName)
			if err != nil {
				return sysChatMsg{text: "could not fetch models from " + providerName + ": " + safeErr(err) +
					"\n(try `/model use " + providerName + "` to switch without picking, or `/configure " + providerName + "` to set the key)"}
			}
			if len(models) == 0 {
				s, _, uerr := ctl.UseModel(providerName, "")
				if uerr != nil {
					return sysChatMsg{text: providerName + " reported no models, and switching failed: " + uerr.Error()}
				}
				return sysChatMsg{text: providerName + " reported no models — switched to its saved default:\n" + s}
			}
			return pickerStartMsg{picker: &listPicker{
				title:       "Pick a model for " + providerName + " (" + fmt.Sprintf("%d available", len(models)) + ")",
				hint:        "↑/↓ select · enter swap live · esc cancel",
				items:       models,
				currentMark: currentModel,
				selected:    pickerIndexOf(models, currentModel),
				onSelect: func(choice string) tea.Cmd {
					return func() tea.Msg {
						s, _, perr := ctl.UseModel(providerName, choice)
						if perr != nil {
							return sysChatMsg{text: "✗ switch to " + choice + " failed: " + perr.Error()}
						}
						return sysChatMsg{text: s}
					}
				},
				onCancel: func() tea.Cmd {
					return func() tea.Msg { return sysChatMsg{text: "kept current model for " + providerName} }
				},
			}}
		}
	}

	// STATE 0: a single provider needs no provider pick.
	if len(infos) == 1 {
		return fetchAndPick(infos[0].Name, infos[0].Model)
	}

	// STATE 1: provider picker.
	labels := make([]string, 0, len(infos))
	byLabel := make(map[string]ModelInfo, len(infos))
	activeLabel := ""
	for _, info := range infos {
		label := info.Name
		if info.Model != "" {
			label += "  ·  " + info.Model
		}
		// Guarantee label uniqueness so the label→info round-trip can't mis-route
		// (mirrors the /configure picker). Provider names are unique config keys,
		// so this only fires on pathological input — but it's a one-line backstop.
		base := label
		for n := 2; ; n++ {
			if _, dup := byLabel[label]; !dup {
				break
			}
			label = fmt.Sprintf("%s (%d)", base, n)
		}
		labels = append(labels, label)
		byLabel[label] = info
		if info.Active {
			activeLabel = label
		}
	}
	return startPickerCmd(&listPicker{
		title:       "Switch model — pick a provider",
		hint:        "↑/↓ select · enter fetch models · esc cancel",
		items:       labels,
		currentMark: activeLabel,
		selected:    pickerIndexOf(labels, activeLabel),
		onSelect: func(choice string) tea.Cmd {
			info := byLabel[choice]
			return fetchAndPick(info.Name, info.Model)
		},
		onCancel: func() tea.Cmd {
			return func() tea.Msg { return sysChatMsg{text: "model switch cancelled"} }
		},
	})
}

// modelToolFormats are the supported OpenAI-compatible tool-call formats,
// mirrored from cmd/tenant setup.go's toolFormatOpts. Surfaced by the /model add
// picker (TEN-138) so a self-hosted backend never silently defaults to gemma.
var modelToolFormats = []string{"qwen", "gemma", "llama", "mistral", "glm", "openai"}

// defaultToolFormat is the picker's initial highlight — the historical fallback,
// shown first so the operator sees what they'd have gotten and can change it.
const defaultToolFormat = "gemma"

// modelCommands are the model, routing, loop-ceiling, reasoning, eval and
// judge commands.
func modelCommands() []*slashCommand {
	return []*slashCommand{
		{Names: []string{"/model", "/models"}, Section: "model", Run: slashModel,
			Summary: "list, switch, add and remove model backends",
			Task:    "see or switch the model you are talking to",
			Rows: []cmdreg.Row{
				{Syntax: "/model", Desc: "list configured model backends + the active one"},
				{Syntax: "/model fallback <name...>", Desc: "auto-route to these providers when the active one 429s/out-of-credits/unreachable (off to clear)"},
				{Syntax: "/model pick", Desc: "arrow-key picker: pick a provider → its live cloud models → swap (or bare /model use)"},
				{Syntax: "/model use <name> [<model>]", Desc: "switch primary; optional model variant (e.g. /model use zai glm-5.1)"},
				{Syntax: "/model models [<name>]", Desc: "list model variants served by the provider's endpoint (live)"},
				{Syntax: "/model add-cloud <kind> <key>", Desc: "register a keyed cloud provider (zai/openai/grok/anthropic)"},
				{Syntax: "/model add claudecode", Desc: "register the Claude Code subscription backend (claude -p; no key)"},
				{Syntax: "/model add <name> <endpoint> [fmt]", Desc: "register a self-hosted vLLM backend mid-session"},
				{Syntax: "/model remove <name>", Desc: "delete a configured backend (not the active one)"},
				{Syntax: "/model reload", Desc: "re-resolve the active provider's key live (after a rotation)"},
			}},
		{Names: []string{"/ceiling", "/loops", "/loop-ceiling"}, Section: "model", Run: slashCeiling,
			Summary: "view or live-tune the loop ceiling (max tool calls per turn)",
			Rows:    []cmdreg.Row{{Syntax: "/ceiling [n]", Desc: "view / live-tune the loop ceiling (max tool calls per turn)"}}},
		{Names: []string{"/reasoning", "/effort"}, Section: "model", Run: slashReasoning,
			Summary: "pick or live-tune reasoning effort",
			Rows:    []cmdreg.Row{{Syntax: "/reasoning [level]", Desc: "arrow-key picker / live-tune reasoning effort (Fugu: high/xhigh · claudecode: low→max)"}}},
		{Names: []string{"/route"}, Section: "model", Run: slashRoute,
			Summary: "prompt-router status, rollout stages, stats and explanations",
			Rows: []cmdreg.Row{
				{Syntax: "/route", Desc: "show prompt-router status + run a parseable classifier health probe"},
				{Syntax: "/route configure", Desc: "arrow-key wizard: big cloud model → small local model → policy"},
				{Syntax: "/route on|off|shadow|assisted|everywhere", Desc: "persist one explicit rollout stage"},
				{Syntax: "/route force <profileID>", Desc: "force an explicitly named profile (privacy override; logged)"},
				{Syntax: "/route stats", Desc: "show big/disagreement/failure rates, latency, and breaker trips"},
				{Syntax: "/route why [n]", Desc: "explain the nth most recent routing decision (default: latest)"},
			}},
		{Names: []string{"/eval"}, Section: "eval", Run: slashEval,
			Summary: "the nightly eval: schedule, run now, trend, per-task diff",
			Rows: []cmdreg.Row{
				{Syntax: "/eval", Desc: "show the nightly-eval schedule + last recorded run"},
				{Syntax: "/eval every <dur> | at <HH:MM> | off", Desc: "re-tune the schedule live (persists; one run per period, relaunch-proof)"},
				{Syntax: "/eval now", Desc: "queue one eval run on the improve scheduler (fires within a minute)"},
				{Syntax: "/eval trend [n]", Desc: "recent eval scores + regression verdicts (trend.jsonl)"},
				{Syntax: "/eval diff", Desc: "per-task movers vs the baseline — what improved, what declined and why"},
			}},
		{Names: []string{"/judge"}, Section: "session", Run: slashJudge,
			Summary: "choose the eval LLM-judge model",
			Rows:    []cmdreg.Row{{Syntax: "/judge [set <kind> <model>|clear|status]", Desc: "choose the eval LLM-judge model (default: planner/self-judge); persists to tenant eval + the nightly gate"}}},
	}
}
