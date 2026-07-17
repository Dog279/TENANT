package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// --- /route configure: three-step arrow-key wizard ---
//
//	① big leg — pick a configured CLOUD provider, then one of its live models
//	   (persists route.big_role + the provider's model, binds planner-big)
//	② small leg — pick a configured LOCAL provider, then one of its live models
//	   (switches the active planner through the same UseModel path as /model pick)
//	③ policy — a menu of knobs (mode, cloud offload, min complexity, sticky
//	   session); each knob opens a value picker and returns to the refreshed
//	   menu; done prints live status + test hints.
//
// Every selection applies immediately, so Esc at any point keeps the steps
// already taken (it skips forward, never rolls back). Control calls that touch
// disk or network run on tea.Cmd goroutines; steps chain through
// routeWizardMsg so a step's status and the next picker land in one Update.

// RouteConfigureInfo is the provider inventory + current route settings the
// /route configure wizard walks through. Cloud and Local reuse ModelInfo;
// Model carries the effective model (config value, or the kind's default for
// cloud providers).
type RouteConfigureInfo struct {
	Cloud   []ModelInfo // configured cloud providers — big-leg candidates
	Local   []ModelInfo // configured local providers — small-leg candidates
	BigRole string      // current route.big_role provider name ("" = unset)
	// Mode is the operator-facing rollout-ladder word:
	// off | shadow | assisted | on | everywhere.
	Mode                string
	NeverCloud          bool
	MinComplexityForBig int
	StickySession       bool
}

// routeWizardMsg chains wizard steps: append text to the chat pane (when
// non-empty) and open the next picker (when non-nil) in the same Update pass.
type routeWizardMsg struct {
	text string
	next *listPicker
}

// startRouteConfigure powers /route configure. Runs on the UI goroutine; the
// returned cmd loads the inventory (and, with a single cloud provider, its
// live model list) off it.
func (m *model) startRouteConfigure() tea.Cmd {
	ctl := m.cfg.Route
	if m.cfg.Models == nil {
		m.sysChat("model control unavailable — /route configure needs it to list and switch models")
		return nil
	}
	m.appendFeed(cDim.Render("⟳ loading route inventory…"))
	return func() tea.Msg {
		info, err := ctl.ConfigureInfo()
		if err != nil {
			return sysChatMsg{text: "route configure: " + safeErr(err)}
		}
		if len(info.Cloud) == 0 {
			return sysChatMsg{text: "no cloud providers configured — add one with `/model add-cloud <kind> <api-key>` (zai/openai/grok/anthropic/sakana), then re-run /route configure"}
		}
		intro := "Route configure — 3 steps: ① big cloud model → ② small local model → ③ policy. Picks apply immediately; esc skips ahead."
		if len(info.Cloud) == 1 {
			msg := m.routeBigModelStep(info, info.Cloud[0], false)
			if w, ok := msg.(routeWizardMsg); ok {
				w.text = strings.TrimSpace(intro + "\n" + w.text)
				return w
			}
			return msg
		}
		return routeWizardMsg{text: intro, next: m.routeBigProviderPicker(info)}
	}
}

// routeProviderLabels renders "name  ·  kind  ·  model" picker rows with the
// same uniqueness backstop as startModelPicker; markName selects the row that
// gets the "(current)" marker.
func routeProviderLabels(infos []ModelInfo, markName string) (labels []string, byLabel map[string]ModelInfo, mark string) {
	byLabel = make(map[string]ModelInfo, len(infos))
	for _, info := range infos {
		label := info.Name + "  ·  " + info.Kind
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
		if markName != "" && mark == "" && info.Name == markName {
			mark = label
		}
	}
	return labels, byLabel, mark
}

// routeBigProviderPicker builds step ①'s provider picker. Pure builder — no
// I/O, no mutable model state.
func (m *model) routeBigProviderPicker(info RouteConfigureInfo) *listPicker {
	labels, byLabel, mark := routeProviderLabels(info.Cloud, info.BigRole)
	return &listPicker{
		title:       "① big (cloud) model — pick a provider",
		hint:        "↑/↓ select · enter fetch models · esc skip to small model",
		items:       labels,
		currentMark: mark,
		selected:    pickerIndexOf(labels, mark),
		onSelect: func(choice string) tea.Cmd {
			p := byLabel[choice]
			return func() tea.Msg { return m.routeBigModelStep(info, p, true) }
		},
		onCancel: func() tea.Cmd {
			return func() tea.Msg { return m.routeSmallStep(info, "big leg unchanged") }
		},
	}
}

// routeBigModelStep fetches the chosen cloud provider's live models and
// returns step ①'s model picker. picked reports whether the operator
// explicitly chose this provider (multi-provider picker) rather than the
// wizard fast-pathing the only candidate: config is only ever written on an
// operator decision, so an unreachable catalog keeps the saved model when
// picked and changes nothing at all when not. A SetBig failure skips forward
// to step ② — an unbuildable provider must not strand the wizard.
// Blocking — tea.Cmd goroutine only.
func (m *model) routeBigModelStep(info RouteConfigureInfo, p ModelInfo, picked bool) tea.Msg {
	ctl := m.cfg.Route
	list, err := m.cfg.Models.ListProviderModels(p.Name)
	if err != nil || len(list) == 0 {
		note := p.Name + " reported no models"
		if err != nil {
			note = "could not fetch live models from " + p.Name + " (" + safeErr(err) + ")"
		}
		if !picked {
			return m.routeSmallStep(info, note+" — big leg unchanged (re-run /route configure when the endpoint is reachable)")
		}
		status, aerr := ctl.SetBig(p.Name, "")
		if aerr != nil {
			return m.routeSmallStep(info, "✗ set big leg: "+safeErr(aerr)+" — big leg unchanged")
		}
		return m.routeSmallStep(info, note+" — kept its saved model.\n"+status)
	}
	return routeWizardMsg{next: &listPicker{
		title:       fmt.Sprintf("① big (cloud) model — %s (%d available)", p.Name, len(list)),
		hint:        "↑/↓ select · enter set big model · esc skip to small model",
		items:       list,
		currentMark: p.Model,
		selected:    pickerIndexOf(list, p.Model),
		onSelect: func(choice string) tea.Cmd {
			return func() tea.Msg {
				status, err := ctl.SetBig(p.Name, choice)
				if err != nil {
					return m.routeSmallStep(info, "✗ set big leg: "+safeErr(err)+" — big leg unchanged")
				}
				return m.routeSmallStep(info, status)
			}
		},
		onCancel: func() tea.Cmd {
			return func() tea.Msg { return m.routeSmallStep(info, "big leg unchanged") }
		},
	}}
}

// routeSmallStep opens step ② (or skips to ③ when no local provider exists).
// May fetch models on the single-provider fast path — tea.Cmd goroutine only.
func (m *model) routeSmallStep(info RouteConfigureInfo, status string) tea.Msg {
	if len(info.Local) == 0 {
		return m.routePolicyStep(strings.TrimSpace(status + "\nno local providers configured — skipping the small-model step"))
	}
	if len(info.Local) == 1 {
		return m.routeSmallModelStep(info.Local[0], status, false)
	}
	active := ""
	for _, p := range info.Local {
		if p.Active {
			active = p.Name
			break
		}
	}
	labels, byLabel, mark := routeProviderLabels(info.Local, active)
	return routeWizardMsg{text: status, next: &listPicker{
		title:       "② small (local) model — pick a provider",
		hint:        "↑/↓ select · enter fetch models · esc skip to policy",
		items:       labels,
		currentMark: mark,
		selected:    pickerIndexOf(labels, mark),
		onSelect: func(choice string) tea.Cmd {
			p := byLabel[choice]
			return func() tea.Msg { return m.routeSmallModelStep(p, "", true) }
		},
		onCancel: func() tea.Cmd {
			return func() tea.Msg { return m.routePolicyStep("small leg unchanged") }
		},
	}}
}

// routeSmallModelStep fetches the local provider's served models and applies
// the choice through UseModel — the same switch-probe-rebind path as /model
// pick, which also re-asserts the planner-big binding from step ①. picked
// mirrors routeBigModelStep: the empty-list auto-switch to the provider's
// saved default only happens after an explicit provider pick, never on the
// single-provider fast path (an esc-only walkthrough must not rewrite the
// active provider). Blocking — tea.Cmd goroutine only.
func (m *model) routeSmallModelStep(p ModelInfo, status string, picked bool) tea.Msg {
	models := m.cfg.Models
	list, err := models.ListProviderModels(p.Name)
	if err != nil {
		return m.routePolicyStep(strings.TrimSpace(status +
			"\ncould not fetch models from " + p.Name + ": " + safeErr(err) +
			" — small leg unchanged (try `/model use " + p.Name + "` later)"))
	}
	if len(list) == 0 {
		if !picked {
			return m.routePolicyStep(strings.TrimSpace(status + "\n" + p.Name + " reported no models — small leg unchanged"))
		}
		s, _, uerr := models.UseModel(p.Name, "")
		if uerr != nil {
			return m.routePolicyStep(strings.TrimSpace(status + "\n" + p.Name + " reported no models, and switching failed: " + safeErr(uerr)))
		}
		return m.routePolicyStep(strings.TrimSpace(status + "\n" + p.Name + " reported no models — switched to its saved default:\n" + s))
	}
	return routeWizardMsg{text: status, next: &listPicker{
		title:       fmt.Sprintf("② small (local) model — %s (%d available)", p.Name, len(list)),
		hint:        "↑/↓ select · enter switch planner · esc skip to policy",
		items:       list,
		currentMark: p.Model,
		selected:    pickerIndexOf(list, p.Model),
		onSelect: func(choice string) tea.Cmd {
			return func() tea.Msg {
				s, _, uerr := models.UseModel(p.Name, choice)
				if uerr != nil {
					return m.routePolicyStep("✗ switch to " + choice + " failed: " + safeErr(uerr))
				}
				return m.routePolicyStep(s)
			}
		},
		onCancel: func() tea.Cmd {
			return func() tea.Msg { return m.routePolicyStep("small leg unchanged") }
		},
	}}
}

// routePolicyStep re-reads the live config (steps ① / ② and earlier knob
// picks have changed it) and opens the policy menu. Blocking — tea.Cmd
// goroutine only.
func (m *model) routePolicyStep(status string) tea.Msg {
	info, err := m.cfg.Route.ConfigureInfo()
	if err != nil {
		return sysChatMsg{text: strings.TrimSpace(status + "\nroute configure: " + safeErr(err))}
	}
	return routeWizardMsg{text: status, next: m.routePolicyMenu(info)}
}

// routePolicyDone is the menu row that closes the wizard. A stable sentinel
// (the other rows embed current values, so prefix matching identifies them).
const routePolicyDone = "✓ done — show status + test hints"

// routePolicyMenu builds step ③: one row per knob showing its current value;
// picking a row opens a value picker that applies and reopens the refreshed
// menu. Pure builder — no I/O.
func (m *model) routePolicyMenu(info RouteConfigureInfo) *listPicker {
	offload := "local only (never_cloud)"
	if !info.NeverCloud {
		offload = "cloud allowed"
	}
	sticky := "off"
	if info.StickySession {
		sticky = "on"
	}
	rows := []string{
		"mode  ·  " + info.Mode,
		"cloud offload  ·  " + offload,
		"min complexity for big  ·  " + strconv.Itoa(info.MinComplexityForBig),
		"sticky session  ·  " + sticky,
		routePolicyDone,
	}
	return &listPicker{
		title: "③ policy — pick a knob to tune",
		hint:  "↑/↓ select · enter tune · esc finish",
		items: rows,
		onSelect: func(choice string) tea.Cmd {
			switch {
			case choice == routePolicyDone:
				return m.routeFinishCmd()
			case strings.HasPrefix(choice, "mode"):
				return startPickerCmd(m.routeModePicker(info))
			case strings.HasPrefix(choice, "cloud offload"):
				return startPickerCmd(m.routeOffloadPicker(info))
			case strings.HasPrefix(choice, "min complexity"):
				return startPickerCmd(m.routeComplexityPicker(info))
			case strings.HasPrefix(choice, "sticky session"):
				return startPickerCmd(m.routeStickyPicker(info))
			}
			return nil
		},
		onCancel: func() tea.Cmd { return m.routeFinishCmd() },
	}
}

// routeApplyPolicyCmd applies one knob off the UI goroutine, then reopens the
// refreshed menu. Mode rides SetMode (rollout ladder, graduation gate);
// everything else goes through SetPolicy. Errors reopen the menu too, so a
// gated pick (e.g. everywhere before graduation) never strands the wizard.
func (m *model) routeApplyPolicyCmd(field, value string) tea.Cmd {
	ctl := m.cfg.Route
	return func() tea.Msg {
		var status string
		var err error
		if field == "mode" {
			status, err = ctl.SetMode(value)
		} else {
			status, err = ctl.SetPolicy(field, value)
		}
		if err != nil {
			return m.routePolicyStep("route: " + err.Error())
		}
		return m.routePolicyStep(status)
	}
}

// routeValueMark returns the item whose first word equals word ("" = none).
func routeValueMark(items []string, word string) string {
	for _, it := range items {
		if f := strings.Fields(it); len(f) > 0 && f[0] == word {
			return it
		}
	}
	return ""
}

func (m *model) routeModePicker(info RouteConfigureInfo) *listPicker {
	items := []string{
		"off — routing disabled; the planner serves every turn",
		"shadow — classify + log only; model selection unchanged (safe)",
		"assisted — suggests the big model; accept a suggestion with !big",
		"on — routes TUI turns live; relays/cron stay shadow",
		"everywhere — routes all surfaces incl. relays (graduation gate must PASS)",
	}
	mark := routeValueMark(items, info.Mode)
	return &listPicker{
		title:       "mode — rollout ladder",
		hint:        "↑/↓ select · enter apply · esc back",
		items:       items,
		currentMark: mark,
		selected:    pickerIndexOf(items, mark),
		onSelect: func(choice string) tea.Cmd {
			return m.routeApplyPolicyCmd("mode", strings.Fields(choice)[0])
		},
		onCancel: func() tea.Cmd { return startPickerCmd(m.routePolicyMenu(info)) },
	}
}

func (m *model) routeOffloadPicker(info RouteConfigureInfo) *listPicker {
	allowed := fmt.Sprintf("allowed — complexity ≥ %d turns may route to the big cloud model", info.MinComplexityForBig)
	blocked := "blocked — big-tier selections clamp to the local model (never_cloud)"
	items := []string{allowed, blocked}
	mark := allowed
	if info.NeverCloud {
		mark = blocked
	}
	return &listPicker{
		title:       "cloud offload — may routed turns leave this machine?",
		hint:        "↑/↓ select · enter apply · esc back",
		items:       items,
		currentMark: mark,
		selected:    pickerIndexOf(items, mark),
		onSelect: func(choice string) tea.Cmd {
			return m.routeApplyPolicyCmd("never_cloud", strconv.FormatBool(strings.HasPrefix(choice, "blocked")))
		},
		onCancel: func() tea.Cmd { return startPickerCmd(m.routePolicyMenu(info)) },
	}
}

func (m *model) routeComplexityPicker(info RouteConfigureInfo) *listPicker {
	items := []string{
		"1 — nearly everything routes big (most aggressive)",
		"2 — only trivial prompts stay local",
		"3 — balanced",
		"4 — only hard prompts route big (default)",
		"5 — only the hardest prompts route big",
	}
	mark := routeValueMark(items, strconv.Itoa(info.MinComplexityForBig))
	return &listPicker{
		title:       "min complexity for big — classifier threshold (1–5)",
		hint:        "↑/↓ select · enter apply · esc back",
		items:       items,
		currentMark: mark,
		selected:    pickerIndexOf(items, mark),
		onSelect: func(choice string) tea.Cmd {
			return m.routeApplyPolicyCmd("min_complexity_for_big", strings.Fields(choice)[0])
		},
		onCancel: func() tea.Cmd { return startPickerCmd(m.routePolicyMenu(info)) },
	}
}

func (m *model) routeStickyPicker(info RouteConfigureInfo) *listPicker {
	items := []string{
		"on — a session that went big stays big until a budget-checked downswitch (default)",
		"off — re-decide the tier fresh every turn",
	}
	mark := items[1]
	if info.StickySession {
		mark = items[0]
	}
	return &listPicker{
		title:       "sticky session — keep the tier across turns?",
		hint:        "↑/↓ select · enter apply · esc back",
		items:       items,
		currentMark: mark,
		selected:    pickerIndexOf(items, mark),
		onSelect: func(choice string) tea.Cmd {
			return m.routeApplyPolicyCmd("sticky_session", strconv.FormatBool(strings.HasPrefix(choice, "on")))
		},
		onCancel: func() tea.Cmd { return startPickerCmd(m.routePolicyMenu(info)) },
	}
}

// routeFinishCmd closes the wizard: live status, policy footnotes for the two
// configurations that silently do nothing (routing off; big configured but
// clamped local), and how to test.
func (m *model) routeFinishCmd() tea.Cmd {
	ctl, ctx := m.cfg.Route, m.ctx
	return func() tea.Msg {
		var b strings.Builder
		b.WriteString("Route configure complete.\n\n")
		b.WriteString(ctl.Status(ctx))
		if info, err := ctl.ConfigureInfo(); err == nil {
			if info.Mode == "off" {
				b.WriteString("\n\nnote: routing is OFF — set mode to shadow to start collecting decisions, or on to route live.")
			} else if info.NeverCloud && (info.Mode == "on" || info.Mode == "everywhere") {
				b.WriteString("\n\nnote: cloud offload is 'local only' (never_cloud) — big-tier selections clamp to the local model until you allow cloud in the policy step.")
			}
		}
		b.WriteString("\n\ntest it: send a prompt, then `/route why` (latest decision) · `/route stats` (rates + graduation gate) · prefix !big or !local to override a turn")
		return sysChatMsg{text: b.String()}
	}
}
