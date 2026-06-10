package main

// setuppick.go is the openclaw-style arrow-key selector for the `tenant setup`
// wizard (TEN-148). The wizard runs BEFORE the main TUI, on stdin/stderr, so
// each enumerable choice runs as a transient inline bubbletea program: ↑/↓ to
// move, Enter to pick, Esc to keep the current value. Non-interactive / non-TTY
// callers never reach here (setup gates choice steps on `interactive`), but
// selectOne still degrades safely to the current value if a terminal isn't
// available. Free-text steps (endpoint, model, pasted key) keep using ask().

import (
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-isatty"
)

// pickOption is one selectable row: Label is shown, Value is returned, Desc is
// an optional dim hint after the label.
type pickOption struct {
	Label string
	Value string
	Desc  string
}

var (
	setupTitleStyle = lipgloss.NewStyle().Bold(true)
	setupSelStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true) // green
	setupDimStyle   = lipgloss.NewStyle().Faint(true)
)

// selectOne shows an arrow-key single-select menu (rendered on stderr, to match
// the wizard's other prompts) and returns the chosen Value. `cur` is the
// current/default value: the menu opens with it highlighted, and Esc (or any
// terminal/program error, or a non-TTY) returns `cur` unchanged.
func selectOne(title string, opts []pickOption, cur string) string {
	if len(opts) == 0 {
		return cur
	}
	initial := 0
	for i, o := range opts {
		if o.Value == cur {
			initial = i
			break
		}
	}
	// bubbletea needs a real terminal on both ends; otherwise keep the default.
	if !isatty.IsTerminal(os.Stdin.Fd()) || !isatty.IsTerminal(os.Stderr.Fd()) {
		return cur
	}
	out, err := tea.NewProgram(
		pickModel{title: title, opts: opts, cursor: initial, chosen: -1},
		tea.WithOutput(os.Stderr),
	).Run()
	if err != nil {
		return cur
	}
	res, ok := out.(pickModel)
	if !ok || res.chosen < 0 {
		return cur
	}
	return opts[res.chosen].Value
}

type pickModel struct {
	title  string
	opts   []pickOption
	cursor int
	chosen int // -1 until Enter
}

func (m pickModel) Init() tea.Cmd { return nil }

func (m pickModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch k.String() {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		} else {
			m.cursor = len(m.opts) - 1
		}
	case "down", "j":
		if m.cursor < len(m.opts)-1 {
			m.cursor++
		} else {
			m.cursor = 0
		}
	case "home", "g":
		m.cursor = 0
	case "end", "G":
		m.cursor = len(m.opts) - 1
	case "enter":
		m.chosen = m.cursor
		return m, tea.Quit
	case "ctrl+c", "esc", "q":
		m.chosen = -1
		return m, tea.Quit
	}
	return m, nil
}

func (m pickModel) View() string {
	var b strings.Builder
	b.WriteString(setupTitleStyle.Render(m.title) + "\n")
	for i, o := range m.opts {
		line := o.Label
		if o.Desc != "" {
			line += "  " + setupDimStyle.Render(o.Desc)
		}
		if i == m.cursor {
			b.WriteString(setupSelStyle.Render("▶ "+line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	b.WriteString(setupDimStyle.Render("↑/↓ move · enter select · esc keep current"))
	return b.String()
}
