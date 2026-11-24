package tui

// Setup commands: /setup, /configure, /cancel.

import (
	"fmt"
	"strings"
	"tenant/internal/cmdreg"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// slashConfigure runs /configure, /config.
func slashConfigure(m *model, line string) tea.Cmd {
	_, _, arg := slashParts(line)
	// TEN-65 follow-up: OpenClaw-style interactive configure.
	// Usage:
	//   /configure              → list configurable skills (no session)
	//   /configure <id>         → start interactive walkthrough
	//   /configure <id> <args>  → one-shot (same as /skill configure)
	//   /configure --no-enable <id> [<args>]  → skip auto-enable
	if m.cfg.SkillConfig == nil && m.cfg.Secrets == nil {
		m.sysChat("/configure is not available in this session")
		return nil
	}
	arg = strings.TrimSpace(arg)
	// Parse --no-enable from anywhere in the arg list.
	fields := strings.Fields(arg)
	noEnable := false
	remaining := make([]string, 0, len(fields))
	for _, f := range fields {
		if f == "--no-enable" {
			noEnable = true
			continue
		}
		remaining = append(remaining, f)
	}
	if len(remaining) == 0 {
		// No arg → openclaw-style arrow-key picker over every keyed service
		// (providers, web search, Discord, …). Pick one → paste its key.
		if m.cfg.Secrets != nil {
			return m.startConfigurePicker()
		}
		// Fallback (no Secrets control wired): legacy skill menu.
		if m.cfg.SkillConfig != nil {
			m.sysChat(renderSkillList(m.cfg.SkillConfig.SkillList(), m.chat.Width))
			m.sysChat("type `/configure <id>` to start an interactive walkthrough — e.g. `/configure gsuite`")
		}
		return nil
	}
	// `/configure peer <name> [cap=on|off …]` is the per-peer share editor —
	// route it to the peer surface, not the skill walkthrough.
	if remaining[0] == "peer" {
		m.handleConfigurePeer(remaining[1:])
		return nil
	}
	if m.cfg.SkillConfig == nil {
		m.sysChat("/configure <id> needs the skill-config surface, which isn't available in this session")
		return nil
	}
	id := remaining[0]
	rest := remaining[1:]
	if len(rest) > 0 {
		// One-shot — delegate to SkillConfigure with assembled args.
		args := append([]string{id}, rest...)
		out, err := m.cfg.SkillConfig.SkillConfigure(args, noEnable)
		if err != nil {
			m.sysChat("✗ " + err.Error())
		} else {
			m.sysChat(out)
		}
		return nil
	}
	// Interactive walkthrough.
	if cmd := m.startConfigure(id, noEnable); cmd != nil {
		return cmd
	}
	return nil
}

// slashSetup runs /setup.
func slashSetup(m *model, line string) tea.Cmd {
	// TEN-150: the external `tenant setup` wizard, in-TUI. Arrow-key menu of
	// settings (provider/model/endpoint/key/tool-format/embeddings/gateway);
	// pick one → edit it (masked for the key) → applied live; back to menu.
	if m.cfg.Setup == nil {
		m.sysChat("/setup is not available in this session")
		return nil
	}
	return m.startSetupMenu()
}

// slashCancel runs /cancel.
func slashCancel(m *model, line string) tea.Cmd {
	// TEN-65 follow-up: aborts an in-flight /configure session.
	// Harmless if no session is active.
	if m.comp != nil { // TEN-518: a form or choices waiting for an answer
		m.comp = nil
		m.picker = nil
		if m.setupEntry != nil {
			m.clearSetupEntry()
		}
		m.sysChat("form: cancelled — nothing sent")
		return nil
	}
	if m.secretEntry != nil {
		m.clearSecretEntry()
		m.sysChat("configuration cancelled")
		return nil
	}
	if m.setupEntry != nil {
		m.clearSetupEntry()
		m.sysChat("setup cancelled")
		return nil
	}
	if m.configureSession != nil {
		m.configureSession = nil
		m.sysChat("configuration cancelled")
		return nil
	}
	m.sysChat("nothing to cancel")
	return nil
}

// secretEntryState is the no-arg `/configure` key-entry: the operator picked a
// service and the next plain input is its key (masked while active).
type secretEntryState struct {
	credID string
	name   string
}

// startConfigurePicker opens the arrow-key picker over every keyed service. On
// select it arms secretEntry + masks the input so the next line is captured as
// that service's key. Reuses the existing listPicker modal.
func (m *model) startConfigurePicker() tea.Cmd {
	items := m.cfg.Secrets.List()
	if len(items) == 0 {
		m.sysChat("configure: no configurable services available")
		return nil
	}
	labels := make([]string, len(items))
	byLabel := make(map[string]secretEntryState, len(items))
	for i, it := range items {
		status := "○ not set"
		if it.Set {
			status = "● set"
		}
		label := fmt.Sprintf("%-26s %-13s %s", it.Name, it.Category, status)
		// Guarantee a unique map key even if two services share Name+Category+
		// status (latent today; future-proofs the label→credID round-trip).
		if _, dup := byLabel[label]; dup {
			label = label + "  " + it.CredID
		}
		labels[i] = label
		byLabel[label] = secretEntryState{credID: it.CredID, name: it.Name}
	}
	m.picker = &listPicker{
		title: "Configure — pick a service to set its API key",
		hint:  "↑/↓ select · enter set key · esc cancel",
		items: labels,
		onSelect: func(choice string) tea.Cmd {
			se := byLabel[choice]
			m.secretEntry = &se
			m.ta.Blur()
			m.input.EchoMode = textinput.EchoPassword
			m.input.Reset()
			m.input.Focus()
			m.sysChat("paste the key/token for " + se.name + ", then Enter (input hidden · /cancel to abort)")
			return nil
		},
		onCancel: func() tea.Cmd {
			m.sysChat("configure cancelled")
			return nil
		},
	}
	return nil
}

// clearSecretEntry disarms a pending /configure key entry and restores the
// input to normal (unmasked, empty). Safe to call when nothing is armed.
func (m *model) clearSecretEntry() {
	m.secretEntry = nil
	m.input.EchoMode = textinput.EchoNormal
	m.input.Reset()
	m.input.Blur()
	m.ta.Focus()
}

// saveSecretEntry persists the just-entered key via the Secrets control (which
// applies it live — TEN-147) and clears the masked-entry state. The value is
// never echoed to chat.
func (m *model) saveSecretEntry(value string) tea.Cmd {
	se := m.secretEntry
	m.clearSecretEntry()
	if se == nil {
		return nil
	}
	value = strings.TrimSpace(value)
	if value == "" {
		m.sysChat("configure: no key entered for " + se.name + " — cancelled")
		return nil
	}
	if m.cfg.Secrets == nil {
		m.sysChat("configure: not available in this session")
		return nil
	}
	if err := m.cfg.Secrets.Set(se.credID, value); err != nil {
		m.sysChat("configure: couldn't save " + se.name + " — " + err.Error())
		return nil
	}
	m.sysChat("✓ " + se.name + " key saved to credentials.json (0600) — applied live, no restart.")
	return nil
}

// configureSessionState tracks an in-flight `/configure <skill>` flow.
// Each user input is consumed as the next field's value until all
// fields are answered, then SkillConfigure runs with the assembled
// key=value args.
type configureSessionState struct {
	skillID  string
	fields   []SkillField      // ordered list from SkillConfigControl.SkillFields
	idx      int               // index of the field currently being answered
	values   map[string]string // collected so far
	noEnable bool              // if /configure --no-enable was used
}

// renderConfigurePrompt renders the next-field prompt that the TUI
// pushes to chat when advancing the session. Idempotent — calling it
// repeatedly shows the same prompt until the user answers.
//
// Format:
//
//	▸ Step N of M · gsuite.composio_api_key
//	  Paste your Composio API key (app.composio.dev → Settings → API Keys)
//	  REQUIRED · Secret (stored at credentials.json, 0600)
//	  Type your answer below, or `/cancel` to abort
//
// Required vs Optional is its own line so the operator can see at a
// glance whether they can press Enter to skip.
func (cs *configureSessionState) renderPrompt() string {
	if cs.idx >= len(cs.fields) {
		return ""
	}
	f := cs.fields[cs.idx]
	// Count visible fields (skipping ShowIf-hidden ones) so the step
	// counter reflects what the operator actually sees, not raw
	// catalog length.
	visibleTotal := 0
	visibleStep := 0
	for i, ff := range cs.fields {
		if ff.ShowIf != nil && !ff.ShowIf(cs.values) {
			continue
		}
		visibleTotal++
		if i == cs.idx {
			visibleStep = visibleTotal
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "▸ Step %d of %d · %s.%s\n  %s", visibleStep, visibleTotal, cs.skillID, f.Key, f.Prompt)
	// Status line: REQUIRED/OPTIONAL + secret + default + picker hint.
	status := []string{}
	if f.Required {
		status = append(status, "REQUIRED")
	} else {
		status = append(status, "OPTIONAL — press Enter to skip")
	}
	if f.Default != "" {
		status = append(status, fmt.Sprintf("Default: %s (Enter to use)", f.Default))
	}
	if f.Secret {
		status = append(status, "Secret (stored at credentials.json, 0600)")
	}
	if len(f.Options) > 0 {
		status = append(status, fmt.Sprintf("Pick one: %s", strings.Join(f.Options, " | ")))
	}
	b.WriteString("\n  " + strings.Join(status, " · "))
	b.WriteString("\n  Type your answer below, or `/cancel` to abort")
	return b.String()
}

// startConfigure begins an interactive `/configure <id>` session. If
// the skill is unknown OR has no fields, returns an error and does
// not enter the session state.
func (m *model) startConfigure(id string, noEnable bool) tea.Cmd {
	if m.cfg.SkillConfig == nil {
		m.sysChat("/configure is not available in this session")
		return nil
	}
	fields, err := m.cfg.SkillConfig.SkillFields(id)
	if err != nil {
		m.sysChat("✗ " + err.Error())
		return nil
	}
	if len(fields) == 0 {
		// Zero fields → run probe+enable immediately.
		out, err := m.cfg.SkillConfig.SkillConfigure([]string{id}, noEnable)
		if err != nil {
			m.sysChat("✗ " + err.Error())
		} else {
			m.sysChat(out)
		}
		return nil
	}
	m.configureSession = &configureSessionState{
		skillID:  id,
		fields:   fields,
		values:   map[string]string{},
		noEnable: noEnable,
	}
	// Friendly intro: name the skill + show its setup hint (if any) so
	// the operator understands what the walkthrough is about to ask.
	// Non-developers especially benefit from upfront context.
	m.sysChat(fmt.Sprintf("Configuring %s — I'll walk you through %d step(s). Type `/cancel` at any time to stop.", id, m.visibleFieldCount(fields)))
	if hint := m.skillSetupHint(id); hint != "" {
		m.sysChat(hint)
	}
	return m.advanceConfigureSession()
}

// visibleFieldCount returns the number of fields the operator will
// actually be prompted for, accounting for ShowIf. Used in the intro
// message so "4 fields" doesn't mislead when 2 are ShowIf-conditional.
func (m *model) visibleFieldCount(fields []SkillField) int {
	n := 0
	for _, f := range fields {
		// Show count is best-effort: ShowIf depends on prior answers
		// we don't have yet. Count fields without a ShowIf gate + 1
		// (the conditional fields might or might not appear).
		if f.ShowIf == nil {
			n++
		}
	}
	return n
}

// skillSetupHint pulls the SetupHint from the skill's catalog entry,
// if any, via the SkillConfigControl. Best-effort — returns empty
// when not available.
func (m *model) skillSetupHint(id string) string {
	if m.cfg.SkillConfig == nil {
		return ""
	}
	for _, s := range m.cfg.SkillConfig.SkillList() {
		if s.ID == id {
			return s.SetupHint
		}
	}
	return ""
}

// advanceConfigureSession picks the next action: if the current field
// has Options, launch a picker; if all fields answered, finalize;
// otherwise show the chat prompt and wait for the next user input.
//
// Skips fields whose ShowIf returns false given the values collected so
// far (TEN-65 follow-up: gsuite sa_json + subject only show when
// auth=sa).
func (m *model) advanceConfigureSession() tea.Cmd {
	cs := m.configureSession
	if cs == nil {
		return nil
	}
	// Skip past any fields hidden by ShowIf.
	for cs.idx < len(cs.fields) {
		f := cs.fields[cs.idx]
		if f.ShowIf == nil || f.ShowIf(cs.values) {
			break
		}
		cs.idx++
	}
	if cs.idx >= len(cs.fields) {
		return m.finalizeConfigureSession()
	}
	f := cs.fields[cs.idx]
	if len(f.Options) > 0 {
		// Use the existing listPicker modal for enum selection.
		// Build display items + reverse map so we store the VALUE
		// (f.Options[i]) but show the human LABEL (f.OptionLabels[i]).
		// If OptionLabels is missing or length-mismatched, fall back to
		// showing the raw values.
		items := append([]string(nil), f.Options...)
		labelToValue := map[string]string{}
		if len(f.OptionLabels) == len(f.Options) {
			items = append([]string(nil), f.OptionLabels...)
			for i, lab := range f.OptionLabels {
				labelToValue[lab] = f.Options[i]
			}
		}
		selectedIdx := 0
		for i, o := range f.Options {
			if o == f.Default {
				selectedIdx = i
				break
			}
		}
		p := &listPicker{
			title:    fmt.Sprintf("%s.%s — %s", cs.skillID, f.Key, f.Prompt),
			items:    items,
			selected: selectedIdx,
			onSelect: func(choice string) tea.Cmd {
				// Map label back to value when labels are in use; otherwise
				// `choice` IS the value (label-less picker).
				value := choice
				if v, ok := labelToValue[choice]; ok {
					value = v
				}
				m.handleConfigureAnswer(value)
				return m.advanceConfigureSession()
			},
			onCancel: func() tea.Cmd {
				m.sysChat("configuration cancelled")
				m.configureSession = nil
				return nil
			},
		}
		return startPickerCmd(p)
	}
	// Free-text field: print the prompt; the next user input will be
	// caught by submit() and routed to handleConfigureAnswer.
	m.input.Reset()
	m.input.Focus()
	m.sysChat(cs.renderPrompt())
	return nil
}

// handleConfigureAnswer accepts the operator's value for the current
// field, stores it (after applying default-on-empty), and bumps the
// index. Does NOT advance the session — the caller does that.
//
// Required + empty (no Default): re-prompts in place with a warning
// rather than silently advancing. Catches the "I forgot to paste"
// case at the field, not at the final validation pass — much better
// UX than walking through 4 fields and getting "missing required
// composio_api_key" at submit time.
func (m *model) handleConfigureAnswer(value string) {
	cs := m.configureSession
	if cs == nil || cs.idx >= len(cs.fields) {
		return
	}
	f := cs.fields[cs.idx]
	value = strings.TrimSpace(value)
	if value == "" && f.Default != "" {
		value = f.Default
	}
	// TEN-72 follow-up: required field + empty answer + no default ⇒
	// warn AND don't advance. Operator hits Enter again or types the
	// value. They can still /cancel to bail entirely.
	if value == "" && f.Required {
		m.sysChat(fmt.Sprintf("⚠ %s is REQUIRED — paste a value (or `/cancel` to abort the whole flow).",
			f.Key))
		return
	}
	// Empty + not Required + no Default ⇒ skip (don't store).
	if value != "" {
		cs.values[f.Key] = value
	}
	// TEN-65 follow-up: surface field-specific guidance + honor abort.
	// gsuite's auth field returns abort=true when the operator picks
	// gcloud but the CLI isn't installed — clean stop instead of
	// pretending to configure a broken setup.
	if value != "" && f.NoteAfter != nil {
		note, abort := f.NoteAfter(value)
		if note != "" {
			m.sysChat(note)
		}
		if abort {
			m.sysChat("configuration cancelled — prerequisite missing")
			m.configureSession = nil
			m.input.Blur()
			m.input.Reset()
			m.ta.Focus()
			return
		}
	}
	cs.idx++
}

// finalizeConfigureSession assembles all collected values into
// key=value args and calls SkillConfigure. Reports the result, clears
// the session.
func (m *model) finalizeConfigureSession() tea.Cmd {
	cs := m.configureSession
	if cs == nil {
		return nil
	}
	args := []string{cs.skillID}
	for k, v := range cs.values {
		// SkillConfigure's key=value parser splits on the first '='.
		args = append(args, k+"="+v)
	}
	noEnable := cs.noEnable
	m.configureSession = nil
	m.input.Blur()
	m.input.Reset()
	m.ta.Focus()

	m.sysChat("⟳ probing…")
	// SkillConfigure may hit the network (probe); run it on a tea.Cmd
	// goroutine so the TUI stays responsive.
	cfg := m.cfg.SkillConfig
	return func() tea.Msg {
		out, err := cfg.SkillConfigure(args, noEnable)
		if err != nil {
			return sysChatMsg{text: "✗ " + err.Error()}
		}
		return sysChatMsg{text: out}
	}
}

// setupCommands are the setup menu, the /configure walkthroughs and /cancel.
func setupCommands() []*slashCommand {
	return []*slashCommand{
		{Names: []string{"/setup"}, Section: "model", Run: slashSetup,
			Summary: "arrow-key setup menu for provider, model, endpoint, key, embeddings, gateway",
			Task:    "pick a model, endpoint and key with the arrow keys",
			Rows:    []cmdreg.Row{{Syntax: "/setup", Desc: "arrow-key setup menu — provider, model, endpoint, key, embeddings, gateway"}}},
		{Names: []string{"/configure", "/config"}, Section: "tools", Run: slashConfigure,
			Summary: "set an API key or walk through an integration's settings",
			Rows: []cmdreg.Row{
				{Syntax: "/configure", Desc: "arrow-key key picker — set an API key for any service (providers, web search, Discord)", Section: "model"},
				{Syntax: "/configure <id>", Desc: "interactive walkthrough — asks for each field one at a time (e.g. /configure gsuite)"},
				{Syntax: "/configure <id> <args>", Desc: "one-shot with key=value (e.g. /configure gsuite auth=gcloud)"},
				{Syntax: "/configure peer <name>", Desc: "share editor — like /permissions: each item allow or deny (set <item> <mode>)", Section: "mcp"},
			}},
		{Names: []string{"/cancel"}, Section: "tools", Run: slashCancel,
			Summary: "abort an in-flight /configure or /setup session",
			Rows:    []cmdreg.Row{{Syntax: "/cancel", Desc: "abort an in-flight /configure session"}}},
	}
}
