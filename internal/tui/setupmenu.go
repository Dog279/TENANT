package tui

// setupmenu.go is the in-TUI `/setup` manager (TEN-150): the external
// `tenant setup` wizard brought into the running TUI as an arrow-key menu,
// working like `/configure` — pick a setting, edit it (masked for the API key),
// applied live where possible, then back to the menu. It reuses the existing
// listPicker modal + a small setupEntry capture (mirrors secretEntry).

import (
	"fmt"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// SetupControl is the settings surface `/setup` drives. The cmd/tenant adapter
// reads/writes launchConfig + credentials.json and applies changes live where
// possible (provider/model/endpoint/tool-format/key via TEN-147). Set* return a
// short human status (✓/⚠) for the feed.
type SetupControl interface {
	Snapshot() SetupView
	ProviderOptions() []SetupOption
	ToolFormatOptions() []string
	SetProvider(kind string) (status string, err error)
	SetModel(model string) (status string, err error)
	SetEndpoint(url string) (status string, err error)
	SetToolFormat(format string) (status string, err error)
	SetKey(value string) (status string, err error)
	SetEmbeddings(endpoint, model string) (status string, err error)
	SetGateway(mode, addr string) (status string, err error)
}

// SetupView is the current configuration, rendered into the menu rows.
type SetupView struct {
	Provider      string
	ProviderLabel string
	Model         string
	Endpoint      string
	ToolFormat    string
	KeySet        bool
	KeySource     string
	EmbedEndpoint string
	EmbedModel    string
	Gateway       string
	NeedsKey      bool
	NeedsEndpoint bool
	IsVLLM        bool
}

// SetupOption is a pickable provider/value pair.
type SetupOption struct {
	Label string
	Value string
}

// setupEntryState captures one `/setup` step's text/secret value.
type setupEntryState struct {
	label  string
	masked bool
	apply  func(value string) tea.Cmd
}

func (m *model) clearSetupEntry() {
	m.setupEntry = nil
	m.input.EchoMode = textinput.EchoNormal
	m.input.Reset()
	m.input.Blur()
	m.ta.Focus()
}

func setupOrNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// startSetupMenu (re)opens the arrow-key settings menu reflecting current state.
func (m *model) startSetupMenu() tea.Cmd {
	if m.cfg.Setup == nil {
		m.sysChat("/setup is not available in this session")
		return nil
	}
	v := m.cfg.Setup.Snapshot()

	type row struct {
		label string
		act   func() tea.Cmd
	}
	var rows []row
	add := func(label string, act func() tea.Cmd) { rows = append(rows, row{label, act}) }

	add(fmt.Sprintf("%-15s %s", "Provider", setupOrNone(v.Provider)), m.setupPickProvider)
	if v.NeedsKey {
		ks := "not set"
		if v.KeySet {
			ks = "set · " + v.KeySource
		}
		add(fmt.Sprintf("%-15s %s", "API key", ks), m.setupEnterKey)
	}
	if v.NeedsEndpoint {
		mdl := v.Model
		if mdl == "" {
			mdl = "(auto-detect)"
		}
		add(fmt.Sprintf("%-15s %s", "Model", mdl), m.setupEnterModel)
		add(fmt.Sprintf("%-15s %s", "Endpoint", setupOrNone(v.Endpoint)), m.setupEnterEndpoint)
	}
	if v.IsVLLM {
		add(fmt.Sprintf("%-15s %s", "Tool format", setupOrNone(v.ToolFormat)), m.setupPickToolFormat)
	}
	add(fmt.Sprintf("%-15s %s @ %s", "Embeddings", setupOrNone(v.EmbedModel), setupOrNone(v.EmbedEndpoint)), m.setupEnterEmbeddings)
	add(fmt.Sprintf("%-15s %s", "MCP gateway", setupOrNone(v.Gateway)), m.setupPickGateway)

	labels := make([]string, len(rows))
	actByLabel := make(map[string]func() tea.Cmd, len(rows))
	for i, r := range rows {
		labels[i] = r.label
		actByLabel[r.label] = r.act
	}
	m.picker = &listPicker{
		title: "Setup — pick a setting to edit",
		hint:  "↑/↓ select · enter edit · esc close",
		items: labels,
		onSelect: func(choice string) tea.Cmd {
			if act := actByLabel[choice]; act != nil {
				return act()
			}
			return nil
		},
		onCancel: func() tea.Cmd { m.sysChat("setup closed"); return nil },
	}
	return nil
}

// setupStatus reports a setter's result to the feed.
func (m *model) setupStatus(status string, err error) {
	if err != nil {
		m.sysChat("setup: " + err.Error())
		return
	}
	if status != "" {
		m.sysChat("setup: " + status)
	}
}

func (m *model) setupPickProvider() tea.Cmd {
	opts := m.cfg.Setup.ProviderOptions()
	labels := make([]string, len(opts))
	byLabel := make(map[string]string, len(opts))
	for i, o := range opts {
		labels[i] = o.Label
		byLabel[o.Label] = o.Value
	}
	m.picker = &listPicker{
		title: "Select model provider",
		hint:  "↑/↓ select · enter switch · esc back",
		items: labels,
		onSelect: func(choice string) tea.Cmd {
			st, err := m.cfg.Setup.SetProvider(byLabel[choice])
			m.setupStatus(st, err)
			return m.startSetupMenu()
		},
		onCancel: func() tea.Cmd { return m.startSetupMenu() },
	}
	return nil
}

func (m *model) setupPickToolFormat() tea.Cmd {
	m.picker = &listPicker{
		title: "Tool format",
		hint:  "↑/↓ select · enter set · esc back",
		items: m.cfg.Setup.ToolFormatOptions(),
		onSelect: func(choice string) tea.Cmd {
			st, err := m.cfg.Setup.SetToolFormat(choice)
			m.setupStatus(st, err)
			return m.startSetupMenu()
		},
		onCancel: func() tea.Cmd { return m.startSetupMenu() },
	}
	return nil
}

func (m *model) setupPickGateway() tea.Cmd {
	m.picker = &listPicker{
		title: "MCP gateway transport",
		hint:  "↑/↓ select · enter set · esc back",
		items: []string{"local", "sse", "both"},
		onSelect: func(choice string) tea.Cmd {
			if choice == "local" {
				st, err := m.cfg.Setup.SetGateway("local", "")
				m.setupStatus(st, err)
				return m.startSetupMenu()
			}
			mode := choice
			m.setupEntry = &setupEntryState{label: "SSE bind address", apply: func(addr string) tea.Cmd {
				st, err := m.cfg.Setup.SetGateway(mode, addr)
				m.setupStatus(st, err)
				return m.startSetupMenu()
			}}
			m.sysChat("enter the SSE bind address (e.g. 127.0.0.1:8765), then Enter (/cancel to abort)")
			return nil
		},
		onCancel: func() tea.Cmd { return m.startSetupMenu() },
	}
	return nil
}

func (m *model) setupEnterKey() tea.Cmd {
	m.setupEntry = &setupEntryState{label: "API key", masked: true, apply: func(v string) tea.Cmd {
		st, err := m.cfg.Setup.SetKey(v)
		m.setupStatus(st, err)
		return m.startSetupMenu()
	}}
	m.input.EchoMode = textinput.EchoPassword
	m.sysChat("paste the API key for the active provider, then Enter (input hidden · /cancel to abort)")
	return nil
}

func (m *model) setupEnterModel() tea.Cmd {
	m.setupEntry = &setupEntryState{label: "model", apply: func(v string) tea.Cmd {
		st, err := m.cfg.Setup.SetModel(v)
		m.setupStatus(st, err)
		return m.startSetupMenu()
	}}
	m.sysChat("enter the model id (blank = auto-detect at launch), then Enter (/cancel to abort)")
	return nil
}

func (m *model) setupEnterEndpoint() tea.Cmd {
	m.setupEntry = &setupEntryState{label: "endpoint", apply: func(v string) tea.Cmd {
		st, err := m.cfg.Setup.SetEndpoint(v)
		m.setupStatus(st, err)
		return m.startSetupMenu()
	}}
	m.sysChat("enter the endpoint base URL, then Enter (/cancel to abort)")
	return nil
}

func (m *model) setupEnterEmbeddings() tea.Cmd {
	m.setupEntry = &setupEntryState{label: "embeddings endpoint", apply: func(ep string) tea.Cmd {
		m.setupEntry = &setupEntryState{label: "embeddings model", apply: func(model string) tea.Cmd {
			st, err := m.cfg.Setup.SetEmbeddings(ep, model)
			m.setupStatus(st, err)
			return m.startSetupMenu()
		}}
		m.sysChat("enter the embeddings model (blank = keep current), then Enter")
		return nil
	}}
	m.sysChat("enter the embeddings endpoint (blank = keep current), then Enter (/cancel to abort)")
	return nil
}
