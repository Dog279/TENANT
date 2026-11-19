package tui

// The slash-command registry (ARC-001 / TEN-303, DOC-003 / TEN-320). Each
// feature file declares its commands, with their help rows, next to their
// handlers (cmd_*.go), and lists them in its xxxCommands function. The
// registry is built from those lists, handleSlash dispatches through it, and
// /help renders from it, so a command cannot run without being in the help.
// Adding a command means adding it to its feature's list; adding a feature
// means one line in slashFeatures.

import (
	"strings"
	"sync"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"

	"tenant/internal/cmdreg"
)

// slashRun handles one slash command line.
type slashRun func(m *model, line string) tea.Cmd

// slashCommand is one registered slash command.
type slashCommand = cmdreg.Command[slashRun]

// slashFeatures lists every feature's commands, in help order. It is a
// function, not a variable, because the handlers reach back to the registry.
func slashFeatures() []func() []*slashCommand {
	return []func() []*slashCommand{
		modelCommands,
		setupCommands,
		agentCommands,
		researchCommands,
		memoryCommands,
		skillCommands,
		peerCommands,
		fleetCommands,
		approvalCommands,
		cronCommands,
		logsCommands,
		sessionCommands,
	}
}

// slashSections are the /help categories, in index order.
var slashSections = []cmdreg.Section{
	{ID: "model", Title: "Models", Aliases: []string{"models"},
		Tagline: "switch the primary LLM backend (vLLM / Z.ai / OpenAI / Anthropic …)"},
	{ID: "agents", Title: "Sub-agents", Aliases: []string{"agent"},
		Tagline: "named sub-agent profiles the orchestrator spawns (own model + identity)"},
	{ID: "research", Title: "Deep research", Aliases: []string{"deep"},
		Tagline: "plan → parallel researchers → cited markdown report"},
	{ID: "memory", Title: "Memory", Aliases: []string{"mem"},
		Tagline: "inspect tiers, search, edit operator soul/rules, compact"},
	{ID: "tools", Title: "Tools & plugins", Aliases: []string{"tool", "plugins", "plugin"},
		Tagline: "see what tools are available, toggle them on/off (live)"},
	{ID: "mcp", Title: "MCP connectors",
		Tagline: "connect remote MCP servers (browser OAuth, no app setup) and bring their tools live"},
	{ID: "fleet", Title: "Fleet",
		Tagline: "machines this Tenant is the master of: who is online, confirm, revoke, each machine's permissions"},
	{ID: "skills", Title: "Skills (T4)", Aliases: []string{"skill"},
		Tagline: "reusable recipes the agent retrieves into its prompt"},
	{ID: "eval", Title: "Eval & quality",
		Tagline: "the nightly regression gate: schedule it, fire it, read the trend"},
	{ID: "safety", Title: "Safety & approvals", Aliases: []string{"perms", "permissions", "approve", "deny"},
		Tagline: "per-category permission modes; approve/deny paused actions"},
	{ID: "automation", Title: "Automation (cron)", Aliases: []string{"cron"},
		Tagline: "recurring agent-prompt jobs on a crontab/@every schedule (read/comms-safe)"},
	{ID: "logs", Title: "Logs", Aliases: []string{"log"},
		Tagline: "the log book: find events, tail them live, open one's detail and correlated timeline",
		Keys:    []cmdreg.Row{{Syntax: "enter / c / esc, q", Desc: "in /logs: an event's detail / its correlated timeline / back"}}},
	{ID: "goal", Title: "Goal (autonomous loop)",
		Tagline: "set a stop condition; judge LLM auto-continues turns until met",
		Keys:    []cmdreg.Row{{Syntax: "Esc / Ctrl-C", Desc: "interrupt the in-flight turn and CLEAR the goal"}}},
	{ID: "review", Title: "Plan review (GStack)",
		Tagline: "cascading CEO / Engineer / Designer review appended to a plan.md"},
	{ID: "session", Title: "Session",
		Tagline: "help, exit, transcript copy, interrupt keys",
		Keys: []cmdreg.Row{
			{Syntax: "Ctrl+S", Desc: "select mode: plain drag highlights text for copy (any terminal); Ctrl+S/Esc returns"},
			{Syntax: "↑ / ↓", Desc: "recall previous prompts/commands (cursor on first/last input line)"},
			{Syntax: "(type mid-turn)", Desc: "send a message while busy to steer the agent — it addresses it, then resumes"},
			{Syntax: "Esc / Ctrl-C", Desc: "hard-stop the running turn (a stuck/looping agent); never closes the app"},
			{Syntax: "Ctrl-Y", Desc: "copy the transcript to clipboard + file"},
			{Syntax: "PgUp/PgDn, Shift+↑/↓", Desc: "scroll the chat pane (works with or without mouse capture)"},
			{Syntax: "Alt+PgUp/PgDn, Alt+↑/↓", Desc: "scroll the activity feed (Ctrl+PgUp/PgDn too; with or without mouse capture)"},
		}},
}

var (
	slashOnce sync.Once
	slashReg  *cmdreg.Registry[slashRun]
)

// slashRegistry builds the registry on first use. It is built lazily because
// /help's handler reads it: a package-level initializer would be a cycle.
func slashRegistry() *cmdreg.Registry[slashRun] {
	slashOnce.Do(func() {
		var all []*slashCommand
		for _, f := range slashFeatures() {
			all = append(all, f()...)
		}
		slashReg = cmdreg.MustNew(slashSections, all)
	})
	return slashReg
}

// slashCommandNamed finds a registered command by any of its names.
func slashCommandNamed(name string) *slashCommand { return slashRegistry().Lookup(name) }

// slashParts splits a command line into its fields, the lower-cased command,
// and the rest of the line as typed.
func slashParts(line string) (fields []string, cmd, arg string) {
	fields = strings.Fields(line)
	if len(fields) == 0 {
		return nil, "", ""
	}
	cmd = strings.ToLower(fields[0])
	arg = strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
	return fields, cmd, arg
}

// helpSectionByID returns the section an id or alias names, or nil.
func helpSectionByID(id string) *cmdreg.Section {
	s, ok := slashRegistry().Section(id)
	if !ok {
		return nil
	}
	return &s
}

// renderHelpIndex is bare /help: the few commands a new operator needs, then
// one line per category. Compact — fits in about fifteen lines.
func renderHelpIndex() (plain, styled string) {
	var p, s strings.Builder
	p.WriteString("Tenant — command help.\n\nStart here:\n")
	s.WriteString(cHeading.Render("Tenant — command help.") + "\n\n" + cName.Render("Start here:") + "\n")
	for _, c := range slashRegistry().Tasks() {
		writeHelpRow(&p, &s, c.Names[0], c.Task, 14)
	}
	p.WriteString("\nCategories — type /help <category> to expand one:\n")
	s.WriteString("\n" + cName.Render("Categories") + cDim.Render(" — type ") + cKey.Render("/help <category>") + cDim.Render(" to expand one:") + "\n")
	for _, sec := range slashRegistry().Sections() {
		key := "/help " + sec.ID
		writeHelpRow(&p, &s, key, sec.Title+"  —  "+sec.Tagline, 18)
	}
	// A high-value knob people hunt for but wouldn't guess the category of
	// (/ceiling lives under Models but is a turn control).
	p.WriteString("\nHandy: /ceiling <n> — cap tool calls per turn before forced synthesis")
	s.WriteString("\n" + cDim.Render("Handy: ") + cKey.Render("/ceiling <n>") + cDim.Render(" — cap tool calls per turn before forced synthesis"))
	p.WriteString("\nFull reference: /help all")
	s.WriteString("\n" + cDim.Render("Full reference: ") + cKey.Render("/help all"))
	return strings.TrimRight(p.String(), "\n"), strings.TrimRight(s.String(), "\n")
}

// renderHelpSection is /help <category>.
func renderHelpSection(sec *cmdreg.Section) (plain, styled string) {
	var p, s strings.Builder
	p.WriteString(sec.Title + " — " + sec.Tagline + "\n")
	s.WriteString(cHeading.Render(sec.Title) + cDim.Render(" — "+sec.Tagline) + "\n")
	for _, r := range slashRegistry().Rows(sec.ID) {
		writeHelpRow(&p, &s, r.Syntax, r.Desc, 38)
	}
	return strings.TrimRight(p.String(), "\n"), strings.TrimRight(s.String(), "\n")
}

// renderHelp is /help all: every command in every category.
func renderHelp() (plain, styled string) {
	var p, s strings.Builder
	for i, sec := range slashRegistry().Sections() {
		if i > 0 {
			p.WriteString("\n")
			s.WriteString("\n")
		}
		p.WriteString(sec.Title + "\n")
		s.WriteString(cHeading.Render(sec.Title) + "\n")
		for _, r := range slashRegistry().Rows(sec.ID) {
			writeHelpRow(&p, &s, r.Syntax, r.Desc, 30)
		}
	}
	return strings.TrimRight(p.String(), "\n"), strings.TrimRight(s.String(), "\n")
}

// writeHelpRow appends one aligned row in both forms, counting runes so
// "↑ / ↓" lines up with the rest.
func writeHelpRow(p, s *strings.Builder, syntax, desc string, width int) {
	gap := width - utf8.RuneCountInString(syntax)
	if gap < 2 {
		gap = 2
	}
	sp := strings.Repeat(" ", gap)
	p.WriteString("  " + syntax + sp + desc + "\n")
	s.WriteString("  " + cKey.Render(syntax) + sp + cDim.Render(desc) + "\n")
}
