package tui

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"tenant/internal/agent"
)

var ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")

func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

// wrap must wrap by VISIBLE width and never split an ANSI escape sequence
// (a split would corrupt the color codes in styled lists/help). Uses
// literal escapes so it's independent of lipgloss's TTY color profile.
func TestWrap_ANSIAwareNeverSplitsEscapes(t *testing.T) {
	const red, reset = "\x1b[31m", "\x1b[0m"
	styled := red + "/command" + reset + "  " + red + "a fairly long description that needs to wrap" + reset
	const w = 12
	got := wrap(styled, w)

	// 1. No escape sequence is broken: an ESC must never be immediately
	//    followed by a newline we inserted, and the SGR codes are intact.
	if strings.Contains(got, "\x1b\n") {
		t.Fatal("wrap split an escape sequence (ESC followed by newline)")
	}
	if c1, c2 := strings.Count(styled, "\x1b"), strings.Count(got, "\x1b"); c1 != c2 {
		t.Fatalf("escape count changed: in=%d out=%d", c1, c2)
	}

	// 2. Every output line fits the visible width.
	for _, line := range strings.Split(got, "\n") {
		if vw := lipgloss.Width(line); vw > w {
			t.Fatalf("line exceeds width %d: visible=%d %q", w, vw, line)
		}
	}

	// 3. Visible content is preserved (only newlines inserted).
	in := stripANSI(styled)
	out := strings.ReplaceAll(stripANSI(got), "\n", "")
	if out != in {
		t.Fatalf("content changed by wrap:\n in=%q\nout=%q", in, out)
	}
}

func TestWrap_PlainWrapsByWidth(t *testing.T) {
	got := wrap("abcdefghij", 4)
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 3 || lines[0] != "abcd" || lines[2] != "ij" {
		t.Fatalf("plain wrap wrong: %q", lines)
	}
}

// renderHelp: the transcript (plain) form must carry NO escape codes, and
// the styled form must render the same visible content (color emission
// itself is TTY-dependent, so we compare stripped visible text).
func TestRenderHelp_PlainCleanStyledSameVisible(t *testing.T) {
	plain, styled := renderHelp()
	if strings.Contains(plain, "\x1b") {
		t.Fatal("plain help must not contain ANSI escapes (it's the copyable form)")
	}
	for _, want := range []string{"Tools", "Memory", "/enable", "/memory soul import"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("help missing %q", want)
		}
	}
	if stripANSI(styled) != plain {
		t.Fatalf("styled visible text != plain:\nplain=%q\nvis=%q", plain, stripANSI(styled))
	}
}

// renderHelpIndex: the cascading default. Lists every visible section as
// `/help <id>` with title + tagline. No command rows — that's the WHOLE
// POINT: keeps the index short enough to fit without scrolling.
func TestRenderHelpIndex(t *testing.T) {
	plain, styled := renderHelpIndex()
	if strings.Contains(plain, "\x1b") {
		t.Fatal("plain help index must not contain ANSI escapes")
	}
	if stripANSI(styled) != plain {
		t.Fatalf("styled visible text != plain:\nplain=%q\nvis=%q", plain, stripANSI(styled))
	}
	// Every non-hidden section's id + title appears.
	for _, sec := range helpSections {
		if sec.hidden {
			continue
		}
		if !strings.Contains(plain, "/help "+sec.id) {
			t.Errorf("index missing /help %s", sec.id)
		}
		if !strings.Contains(plain, sec.title) {
			t.Errorf("index missing title %q", sec.title)
		}
		if !strings.Contains(plain, sec.tagline) {
			t.Errorf("index missing tagline for %s", sec.id)
		}
	}
	// `/help all` hint shown.
	if !strings.Contains(plain, "/help all") {
		t.Errorf("index should mention /help all: %q", plain)
	}
	// Index must NOT include any actual sub-commands (e.g. /memory soul) —
	// that's the WHOLE POINT of the cascade. The user reads the index, picks
	// a category, expands that one. Guard against accidental drift.
	for _, sub := range []string{"/memory soul import", "/permissions set", "/skills enable"} {
		if strings.Contains(plain, sub) {
			t.Errorf("index leaked sub-command %q — index should ONLY show categories", sub)
		}
	}
}

// renderHelpSection renders just one section's commands, with the category's
// tagline as the header line.
func TestRenderHelpSection(t *testing.T) {
	sec := helpSectionByID("agents")
	if sec == nil {
		t.Fatal("agents section missing — drift between dispatcher and registry")
	}
	plain, styled := renderHelpSection(sec)
	if strings.Contains(plain, "\x1b") {
		t.Fatal("plain section must not contain ANSI escapes")
	}
	if stripANSI(styled) != plain {
		t.Fatalf("styled visible text != plain:\nplain=%q\nvis=%q", plain, stripANSI(styled))
	}
	// Every command row appears.
	for _, r := range sec.rows {
		if !strings.Contains(plain, r[0]) {
			t.Errorf("section missing %q", r[0])
		}
	}
	// Title + tagline appear in the header.
	if !strings.Contains(plain, sec.title) {
		t.Errorf("section missing title")
	}
	if !strings.Contains(plain, sec.tagline) {
		t.Errorf("section missing tagline")
	}
}

// helpSectionByID handles aliases (plural/singular and shorthand) so the
// operator's natural typing finds the right section.
func TestHelpSectionByID_Aliases(t *testing.T) {
	for _, alias := range []string{"agents", "agent", "Agents", "  AGENTS "} {
		if helpSectionByID(alias) == nil {
			t.Errorf("alias %q didn't resolve to agents", alias)
		}
	}
	for alias, want := range map[string]string{
		"models":      "model",
		"deep":        "research",
		"mem":         "memory",
		"tool":        "tools",
		"plugins":     "tools",
		"skill":       "skills",
		"perms":       "safety",
		"permissions": "safety",
	} {
		sec := helpSectionByID(alias)
		if sec == nil {
			t.Errorf("alias %q didn't resolve", alias)
			continue
		}
		if sec.id != want {
			t.Errorf("alias %q resolved to %q, want %q", alias, sec.id, want)
		}
	}
	if helpSectionByID("nope") != nil {
		t.Error("unknown alias should return nil")
	}
}

// Dispatcher routing: bare /help → index; /help <id> → section; /help all → full dump.
func TestHelp_DispatcherCascades(t *testing.T) {
	m := newModel(context.Background(), Config{})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	// Bare /help → index.
	m.handleSlash("/help")
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "/help agents") {
		t.Errorf("bare /help didn't show the index (missing /help agents):\n%s", last)
	}
	// Index MUST be short — no command rows leaked.
	if strings.Contains(last, "/memory soul import") {
		t.Error("bare /help leaked sub-commands")
	}

	// /help agents → section.
	m.handleSlash("/help agents")
	last = m.msgs[len(m.msgs)-1].content
	for _, want := range []string{"Sub-agents", "/agents add", "/agents model", "/agents rename"} {
		if !strings.Contains(last, want) {
			t.Errorf("/help agents missing %q:\n%s", want, last)
		}
	}

	// /help all → full dump (every section's rows).
	m.handleSlash("/help all")
	last = m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "/memory soul import") {
		t.Errorf("/help all should dump every command:\n%s", last)
	}

	// Unknown category → useful error.
	m.handleSlash("/help nonsense")
	last = m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "no help category") || !strings.Contains(last, "/help to see") {
		t.Errorf("unknown category message wrong:\n%s", last)
	}
}

// Scrolling up must stop follow (so a long /tools list stays put), and
// sending re-engages it (so you snap back to the reply).
func TestScroll_PgUpStopsFollow_SubmitReengages(t *testing.T) {
	m := newModel(context.Background(), Config{})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	for i := 0; i < 200; i++ { // overflow the chat pane
		m.sysChat(fmt.Sprintf("line %d", i))
	}
	m.refresh()
	if !m.chatFollow || !m.chat.AtBottom() {
		t.Fatal("should start following at the bottom")
	}

	m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	if m.chatFollow {
		t.Fatal("PgUp should stop follow")
	}
	if m.chat.AtBottom() {
		t.Fatal("should be scrolled up, not at bottom")
	}

	// New content arriving must NOT yank the view back down.
	m.sysChat("a new line appears")
	m.refresh()
	if m.chat.AtBottom() {
		t.Fatal("new content yanked the scrolled-up view to the bottom")
	}

	// Typing a message and sending re-engages follow.
	m.ta.SetValue("hello")
	m.submit()
	m.refresh()
	if !m.chatFollow {
		t.Fatal("submit should re-engage follow")
	}
}

// Esc while a turn is in flight cancels the turn's context (the interrupter)
// and marks the turn interrupted — without quitting. Esc while idle quits.
func TestInterrupt_EscCancelsTurnNeverQuits(t *testing.T) {
	m := newModel(context.Background(), Config{})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	// Simulate a turn in flight with a per-turn cancel context.
	turnCtx, cancel := context.WithCancel(context.Background())
	m.busy = true
	m.turnCancel = cancel

	// Esc must interrupt (cancel the ctx), not quit.
	_, quitCmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if isQuit(quitCmd) {
		t.Fatal("Esc during a turn should interrupt, not quit")
	}
	if !m.interrupted {
		t.Fatal("Esc during a turn should set interrupted")
	}
	select {
	case <-turnCtx.Done():
		// good — the in-flight turn was cancelled
	default:
		t.Fatal("Esc did not cancel the turn context")
	}

	// The turn completes (with whatever error cancellation produced); the UI
	// must report "interrupted" and return to idle.
	before := len(m.msgs)
	m.Update(turnDoneMsg{err: context.Canceled})
	if m.busy {
		t.Fatal("turn should be idle after completion")
	}
	if m.interrupted {
		t.Fatal("interrupted flag should reset after the turn ends")
	}
	if len(m.msgs) != before+1 || !strings.Contains(m.msgs[len(m.msgs)-1].content, "interrupted") {
		t.Fatalf("expected an 'interrupted' system message, got: %+v", m.msgs)
	}

	// Esc while idle must NOT quit — only /exit closes the app. It just
	// reminds the user how to leave.
	before = len(m.msgs)
	_, idleCmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if isQuit(idleCmd) {
		t.Fatal("Esc while idle must not quit — only /exit closes the app")
	}
	if len(m.msgs) != before+1 || !strings.Contains(m.msgs[len(m.msgs)-1].content, "/exit") {
		t.Fatalf("idle Esc should hint to use /exit, got: %+v", m.msgs)
	}

	// Ctrl-C while idle must also not quit.
	_, ctrlcCmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if isQuit(ctrlcCmd) {
		t.Fatal("Ctrl-C while idle must not quit — only /exit closes the app")
	}

	// /exit IS the way out.
	if !isQuit(m.handleSlash("/exit")) {
		t.Fatal("/exit should quit")
	}
}

// isQuit reports whether a tea.Cmd resolves to tea.Quit.
func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

// An incoming approval request must queue as pending, and /approve must
// send the right decision back on the request's reply channel + clear it.
func TestApproval_PromptThenApproveAlways(t *testing.T) {
	m := newModel(context.Background(), Config{})
	reply := make(chan ApprovalDecision, 1)
	m.Update(approvalMsg(ApprovalRequest{
		Category: "destructive", Action: "os_exec_dangerous", Detail: "rm -rf /tmp/x", Reply: reply,
	}))
	if len(m.pending) != 1 {
		t.Fatalf("approval should be queued, pending=%d", len(m.pending))
	}

	m.handleSlash("/approve always")
	select {
	case d := <-reply:
		if d != ApproveAlways {
			t.Fatalf("decision = %v, want ApproveAlways", d)
		}
	default:
		t.Fatal("no decision delivered to the requester")
	}
	if len(m.pending) != 0 {
		t.Fatal("pending should clear after a decision")
	}

	// Deny path + nothing-pending guard.
	r2 := make(chan ApprovalDecision, 1)
	m.Update(approvalMsg(ApprovalRequest{Category: "web", Action: "web_interact", Reply: r2}))
	m.handleSlash("/deny")
	if d := <-r2; d != DenyOnce {
		t.Fatalf("deny decision = %v", d)
	}
	m.handleSlash("/approve") // nothing pending now — must not panic/block
	if len(m.pending) != 0 {
		t.Fatal("still no pending")
	}
}

func TestHumanTokens(t *testing.T) {
	cases := map[int]string{0: "0", 999: "999", 1234: "1.2k", 42300: "42.3k", 1_500_000: "1.5M"}
	for n, want := range cases {
		if got := humanTokens(n); got != want {
			t.Errorf("humanTokens(%d)=%q want %q", n, got, want)
		}
	}
}

// The context gauge colors by utilization (green<60, yellow 60–84, red≥85)
// and never overfills.
func TestCtxBar_ColorThresholdsAndClamp(t *testing.T) {
	green := cOK.Render("█")
	yellow := cSys.Render("█")
	red := cErr.Render("█")
	// With no TTY lipgloss strips color, so assert via fill structure +
	// (when colored) the style bytes. Always-true checks: fill count + clamp.
	if got := stripANSI(ctxBar(50, 10)); strings.Count(got, "█") != 5 {
		t.Fatalf("50%% should fill 5/10: %q", got)
	}
	if got := stripANSI(ctxBar(200, 10)); strings.Count(got, "█") != 10 {
		t.Fatalf("over-budget must clamp to full: %q", got)
	}
	if got := stripANSI(ctxBar(0, 10)); strings.Count(got, "█") != 0 {
		t.Fatalf("0%% should be empty: %q", got)
	}
	// When color IS emitted, the right style is used per threshold.
	if strings.Contains(green, "\x1b") {
		if !strings.Contains(ctxBar(30, 10), green) {
			t.Error("low utilization should be green")
		}
		if !strings.Contains(ctxBar(70, 10), yellow) {
			t.Error("mid utilization should be yellow")
		}
		if !strings.Contains(ctxBar(90, 10), red) {
			t.Error("high utilization should be red")
		}
	}
}

type fakeTools struct {
	list             []ToolInfo
	setEnabledCalls  []fakeSetEnabledCall
	setPluginCalls   []fakeSetEnabledCall
	pluginEnabledRet func(string, bool) (int, string, error) // optional override
	pluginsRet       []string
}

type fakeSetEnabledCall struct {
	target string
	on     bool
}

func (f *fakeTools) ToolList() []ToolInfo { return f.list }

func (f *fakeTools) SetEnabled(target string, on bool) (int, string, error) {
	f.setEnabledCalls = append(f.setEnabledCalls, fakeSetEnabledCall{target, on})
	return 0, "", nil
}

func (f *fakeTools) SetPluginEnabled(label string, on bool) (int, string, error) {
	f.setPluginCalls = append(f.setPluginCalls, fakeSetEnabledCall{label, on})
	if f.pluginEnabledRet != nil {
		return f.pluginEnabledRet(label, on)
	}
	return 0, "", nil
}

func (f *fakeTools) Plugins() []string { return f.pluginsRet }

func TestRenderToolList_GroupedReadable(t *testing.T) {
	m := &model{cfg: Config{Tools: &fakeTools{list: []ToolInfo{
		{Name: "sql_query", Plugin: "sql", Enabled: true},
		{Name: "sql_exec", Plugin: "sql", Enabled: false},
		{Name: "os_sysinfo", Plugin: "os", Enabled: true},
	}}}}
	plain, styled := m.renderToolList()

	if strings.Contains(plain, "\x1b") {
		t.Fatal("plain tool list must be ANSI-free")
	}
	// One tool per line, with the on/off glyphs.
	if !strings.Contains(plain, "● sql_query") || !strings.Contains(plain, "○ sql_exec") {
		t.Fatalf("tools not one-per-line with marks:\n%s", plain)
	}
	// Per-plugin enabled count.
	if !strings.Contains(plain, "sql (1/2 on)") || !strings.Contains(plain, "os (1/1 on)") {
		t.Fatalf("missing per-plugin counts:\n%s", plain)
	}
	// Styled form renders the same visible content.
	if stripANSI(styled) != plain {
		t.Fatalf("styled visible text != plain:\nplain=%q\nvis=%q", plain, stripANSI(styled))
	}
}

// --- /skill (singular) integration-config surface — TEN-64 ---

type fakeSkillCfg struct {
	listRet     []SkillConfigInfo
	showRet     map[string]string
	configureFn func(args []string, noEnable bool) (string, error)
	probeFn     func(id string) (string, error)
	clearFn     func(id, field string) (string, error)
	fieldsFn    func(id string) ([]SkillField, error)
	listCalls   int
	configCalls []fakeSkillConfigCall
}
type fakeSkillConfigCall struct {
	args     []string
	noEnable bool
}

func (f *fakeSkillCfg) SkillList() []SkillConfigInfo { f.listCalls++; return f.listRet }
func (f *fakeSkillCfg) SkillShow(id string) (string, error) {
	if v, ok := f.showRet[id]; ok {
		return v, nil
	}
	return "", fmt.Errorf("no skill named %q", id)
}
func (f *fakeSkillCfg) SkillConfigure(args []string, noEnable bool) (string, error) {
	f.configCalls = append(f.configCalls, fakeSkillConfigCall{args, noEnable})
	if f.configureFn != nil {
		return f.configureFn(args, noEnable)
	}
	return "configured " + args[0], nil
}
func (f *fakeSkillCfg) SkillProbe(id string) (string, error) {
	if f.probeFn != nil {
		return f.probeFn(id)
	}
	return "✓ " + id + " probe OK", nil
}
func (f *fakeSkillCfg) SkillClear(id, field string) (string, error) {
	if f.clearFn != nil {
		return f.clearFn(id, field)
	}
	return "cleared " + id + "." + field, nil
}
func (f *fakeSkillCfg) SkillFields(id string) ([]SkillField, error) {
	if f.fieldsFn != nil {
		return f.fieldsFn(id)
	}
	return nil, nil
}

// `/skill` (bare) and `/skill list` both route to SkillList and render
// the list. Empty list shows actionable message.
func TestSlash_SkillList(t *testing.T) {
	ft := &fakeSkillCfg{listRet: []SkillConfigInfo{
		{ID: "fake", Label: "Fake (test)", Configured: true, Enabled: true},
		{ID: "discord", Label: "Discord", Legacy: true},
	}}
	m := newModel(context.Background(), Config{SkillConfig: ft})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	m.handleSlash("/skill list")
	if ft.listCalls != 1 {
		t.Errorf("expected SkillList to be called once; got %d", ft.listCalls)
	}
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "fake") || !strings.Contains(last, "discord") {
		t.Errorf("list output should include both ids:\n%s", last)
	}
	if !strings.Contains(last, "legacy") {
		t.Errorf("legacy marker missing for discord:\n%s", last)
	}
}

// `/skill configure <id> <value>` routes to SkillConfigure with the
// positional value preserved. Default: noEnable=false.
func TestSlash_SkillConfigurePositional(t *testing.T) {
	ft := &fakeSkillCfg{}
	m := newModel(context.Background(), Config{SkillConfig: ft})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	m.handleSlash("/skill configure fake mytokenvalue")
	if len(ft.configCalls) != 1 {
		t.Fatalf("expected one configure call; got %d", len(ft.configCalls))
	}
	got := ft.configCalls[0]
	if len(got.args) != 2 || got.args[0] != "fake" || got.args[1] != "mytokenvalue" {
		t.Errorf("args wrong: %v", got.args)
	}
	if got.noEnable {
		t.Error("default --no-enable should be false")
	}
}

// `/skill configure <id> --no-enable <value>` parses the flag out of
// the arg list before passing to SkillConfigure.
func TestSlash_SkillConfigureNoEnableFlag(t *testing.T) {
	ft := &fakeSkillCfg{}
	m := newModel(context.Background(), Config{SkillConfig: ft})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	m.handleSlash("/skill configure fake --no-enable mytokenvalue")
	if len(ft.configCalls) != 1 {
		t.Fatalf("expected one configure call; got %d", len(ft.configCalls))
	}
	got := ft.configCalls[0]
	if !got.noEnable {
		t.Errorf("--no-enable flag should set noEnable=true; got %v", got)
	}
	if len(got.args) != 2 || got.args[0] != "fake" || got.args[1] != "mytokenvalue" {
		t.Errorf("--no-enable should be parsed out of the args; got %v", got.args)
	}
}

// `/skill probe <id>` routes to SkillProbe.
func TestSlash_SkillProbe(t *testing.T) {
	called := ""
	ft := &fakeSkillCfg{probeFn: func(id string) (string, error) {
		called = id
		return "✓ probe ok", nil
	}}
	m := newModel(context.Background(), Config{SkillConfig: ft})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/skill probe fake")
	if called != "fake" {
		t.Errorf("probe should receive id; got %q", called)
	}
}

// `/skill clear <id> <field>` requires exactly two args; show usage otherwise.
func TestSlash_SkillClearRequiresBothArgs(t *testing.T) {
	cleared := []string{}
	ft := &fakeSkillCfg{clearFn: func(id, field string) (string, error) {
		cleared = append(cleared, id+"."+field)
		return "ok", nil
	}}
	m := newModel(context.Background(), Config{SkillConfig: ft})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	m.handleSlash("/skill clear fake")
	if len(cleared) != 0 {
		t.Errorf("partial args should NOT call SkillClear; got %v", cleared)
	}
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "usage:") {
		t.Errorf("partial args should show usage; got:\n%s", last)
	}

	m.handleSlash("/skill clear fake token")
	if len(cleared) != 1 || cleared[0] != "fake.token" {
		t.Errorf("full args should route correctly; got %v", cleared)
	}
}

// Unknown subcommand → discoverable error.
func TestSlash_SkillUnknownSubcommand(t *testing.T) {
	ft := &fakeSkillCfg{}
	m := newModel(context.Background(), Config{SkillConfig: ft})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/skill foobar")
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "unknown /skill subcommand") {
		t.Errorf("unknown subcommand should be surfaced; got:\n%s", last)
	}
}

// /skill without SkillConfig wired → clean error, not panic.
func TestSlash_SkillUnconfigured(t *testing.T) {
	m := newModel(context.Background(), Config{}) // no SkillConfig
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/skill list")
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "not available") {
		t.Errorf("missing SkillConfig should give clean error; got:\n%s", last)
	}
}

// --- /configure interactive flow — TEN-65 follow-up ---

// `/configure <id>` with no args starts an interactive session and
// prints the first field's prompt. The picker is launched for the
// first field (auth) because it has Options.
func TestSlash_ConfigureLaunchesPicker(t *testing.T) {
	ft := &fakeSkillCfg{
		fieldsFn: func(id string) ([]SkillField, error) {
			if id != "gsuite" {
				return nil, fmt.Errorf("unknown")
			}
			return []SkillField{
				{Key: "auth", Prompt: "auth mode", Required: true, Default: "gcloud",
					Options: []string{"gcloud", "sa"}},
				{Key: "sa_json", Prompt: "service-account JSON path"},
				{Key: "subject", Prompt: "impersonated user email"},
			}, nil
		},
	}
	m := newModel(context.Background(), Config{SkillConfig: ft})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	cmd := m.handleSlash("/configure gsuite")
	if cmd == nil {
		t.Fatal("/configure gsuite should return a tea.Cmd to start the picker")
	}
	if m.configureSession == nil {
		t.Fatal("/configure gsuite should set configureSession")
	}
	if m.configureSession.skillID != "gsuite" {
		t.Errorf("session skillID wrong; got %q", m.configureSession.skillID)
	}
	if len(m.configureSession.fields) != 3 {
		t.Errorf("expected 3 fields; got %d", len(m.configureSession.fields))
	}
	if m.configureSession.idx != 0 {
		t.Errorf("session should start at idx 0; got %d", m.configureSession.idx)
	}
	// The cmd dispatches a pickerStartMsg — exercise it.
	msg := cmd()
	psm, ok := msg.(pickerStartMsg)
	if !ok {
		t.Fatalf("expected pickerStartMsg; got %T", msg)
	}
	if psm.picker == nil {
		t.Fatal("pickerStartMsg has no picker")
	}
	if len(psm.picker.items) != 2 || psm.picker.items[0] != "gcloud" {
		t.Errorf("picker items wrong: %v", psm.picker.items)
	}
}

// `/configure <id>` with positional/kv args bypasses the interactive
// session and delegates to SkillConfigure directly (one-shot).
func TestSlash_ConfigureOneShotSkipsSession(t *testing.T) {
	ft := &fakeSkillCfg{}
	m := newModel(context.Background(), Config{SkillConfig: ft})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	m.handleSlash("/configure gsuite auth=gcloud")
	if m.configureSession != nil {
		t.Errorf("one-shot args should NOT start a session; got %+v", m.configureSession)
	}
	if len(ft.configCalls) != 1 {
		t.Fatalf("expected one configure call; got %d", len(ft.configCalls))
	}
	if ft.configCalls[0].args[1] != "auth=gcloud" {
		t.Errorf("one-shot args not passed through: %v", ft.configCalls[0].args)
	}
}

// Required field + empty Enter → warns and DOESN'T advance.
// Operator catches the missing field at the prompt, not at the final
// validation pass.
func TestConfigure_RequiredFieldEmptyEnterWarns(t *testing.T) {
	configureCalled := false
	ft := &fakeSkillCfg{
		fieldsFn: func(id string) ([]SkillField, error) {
			return []SkillField{
				{Key: "must_paste", Prompt: "paste a thing", Required: true},
			}, nil
		},
		configureFn: func(args []string, noEnable bool) (string, error) {
			configureCalled = true
			return "ok", nil
		},
	}
	m := newModel(context.Background(), Config{SkillConfig: ft})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/configure fake")

	// Submit an empty answer to the required field.
	m.input.SetValue("")
	_ = m.submit()
	// Should still be in the session (idx unchanged), not finalized.
	if m.configureSession == nil {
		t.Fatal("required empty should NOT finalize — session is gone")
	}
	if m.configureSession.idx != 0 {
		t.Errorf("required empty should NOT advance; idx=%d", m.configureSession.idx)
	}
	if configureCalled {
		t.Error("SkillConfigure shouldn't fire when required field missing")
	}
	// The warning should be in chat.
	hasWarning := false
	for _, msg := range m.msgs {
		if strings.Contains(msg.content, "REQUIRED") {
			hasWarning = true
		}
	}
	if !hasWarning {
		t.Error("expected REQUIRED warning in chat")
	}
}

// Step counter shows the operator how far through the wizard they are.
func TestConfigure_PromptShowsStepCounter(t *testing.T) {
	ft := &fakeSkillCfg{
		fieldsFn: func(id string) ([]SkillField, error) {
			return []SkillField{
				{Key: "a", Prompt: "A", Required: true},
				{Key: "b", Prompt: "B", Required: false},
				{Key: "c", Prompt: "C", Required: true},
			}, nil
		},
	}
	m := newModel(context.Background(), Config{SkillConfig: ft})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/configure fake")
	prompt := m.configureSession.renderPrompt()
	if !strings.Contains(prompt, "Step 1 of 3") {
		t.Errorf("expected 'Step 1 of 3'; got prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "REQUIRED") {
		t.Errorf("expected REQUIRED marker; got prompt:\n%s", prompt)
	}
}

// Optional field renders 'OPTIONAL — press Enter to skip'.
func TestConfigure_OptionalFieldShowsSkipHint(t *testing.T) {
	ft := &fakeSkillCfg{
		fieldsFn: func(id string) ([]SkillField, error) {
			return []SkillField{
				{Key: "maybe", Prompt: "maybe", Required: false},
			}, nil
		},
	}
	m := newModel(context.Background(), Config{SkillConfig: ft})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/configure fake")
	prompt := m.configureSession.renderPrompt()
	if !strings.Contains(prompt, "OPTIONAL") {
		t.Errorf("expected OPTIONAL marker; got prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "press Enter to skip") {
		t.Errorf("expected skip hint; got prompt:\n%s", prompt)
	}
}

// OptionLabels: picker displays labels but onSelect passes the
// underlying value. Critical for TEN-72: gsuite's "oauth" value gets
// shown as "Sign in with your Google account" but stored as "oauth".
func TestConfigure_PickerShowsLabelsStoresValues(t *testing.T) {
	storedValue := ""
	ft := &fakeSkillCfg{
		fieldsFn: func(id string) ([]SkillField, error) {
			return []SkillField{
				{
					Key: "mode", Prompt: "pick", Required: true,
					Default: "tech-id-2",
					Options: []string{"tech-id-1", "tech-id-2", "tech-id-3"},
					OptionLabels: []string{
						"Friendly label one",
						"Friendly label two (recommended)",
						"Friendly label three",
					},
				},
			}, nil
		},
		configureFn: func(args []string, noEnable bool) (string, error) {
			// args[0] = id, args[1+] = key=value
			for _, a := range args[1:] {
				if strings.HasPrefix(a, "mode=") {
					storedValue = strings.TrimPrefix(a, "mode=")
				}
			}
			return "ok", nil
		},
	}
	m := newModel(context.Background(), Config{SkillConfig: ft})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	cmd := m.handleSlash("/configure fake")
	if cmd == nil || m.configureSession == nil {
		t.Fatal("session should start")
	}
	msg := cmd()
	psm, ok := msg.(pickerStartMsg)
	if !ok {
		t.Fatalf("expected pickerStartMsg; got %T", msg)
	}
	// items should be the LABELS, not the values.
	if psm.picker.items[0] != "Friendly label one" {
		t.Errorf("picker should show labels; got items[0]=%q", psm.picker.items[0])
	}
	// Default "tech-id-2" should highlight index 1.
	if psm.picker.selected != 1 {
		t.Errorf("Default 'tech-id-2' should select index 1; got %d", psm.picker.selected)
	}
	// Selecting the label should store the underlying value.
	m.picker = psm.picker
	next := m.picker.onSelect("Friendly label two (recommended)")
	if next != nil {
		_ = next()
	}
	if storedValue != "tech-id-2" {
		t.Errorf("expected stored value 'tech-id-2'; got %q (label leaked through)", storedValue)
	}
}

// Backwards compat: when OptionLabels is nil, picker shows raw values
// (existing behavior pre-TEN-72).
func TestConfigure_PickerFallbackWithoutLabels(t *testing.T) {
	ft := &fakeSkillCfg{
		fieldsFn: func(id string) ([]SkillField, error) {
			return []SkillField{
				{
					Key: "mode", Prompt: "pick", Required: true,
					Default: "b",
					Options: []string{"a", "b"},
					// No OptionLabels — should show "a"/"b" directly.
				},
			}, nil
		},
	}
	m := newModel(context.Background(), Config{SkillConfig: ft})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	cmd := m.handleSlash("/configure fake")
	psm := cmd().(pickerStartMsg)
	if psm.picker.items[0] != "a" || psm.picker.items[1] != "b" {
		t.Errorf("without labels, items should be raw values; got %v", psm.picker.items)
	}
}

// NoteAfter returning abort=true halts the configure session — no
// further fields prompted, no probe runs, no config persists. Used by
// gsuite to short-circuit when gcloud CLI isn't installed instead of
// pretending to configure a broken setup.
func TestConfigure_NoteAfterAbort(t *testing.T) {
	configureCalled := false
	ft := &fakeSkillCfg{
		fieldsFn: func(id string) ([]SkillField, error) {
			return []SkillField{
				{
					Key: "mode", Prompt: "mode", Required: true,
					Options: []string{"good", "bad"},
					NoteAfter: func(v string) (string, bool) {
						if v == "bad" {
							return "✗ prereq missing — bailing", true
						}
						return "", false
					},
				},
				{Key: "follow_up", Prompt: "follow-up", Required: true},
			}, nil
		},
		configureFn: func(args []string, noEnable bool) (string, error) {
			configureCalled = true
			return "ok", nil
		},
	}
	m := newModel(context.Background(), Config{SkillConfig: ft})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	cmd := m.handleSlash("/configure fake")
	if cmd == nil || m.configureSession == nil {
		t.Fatal("session should start")
	}
	// Trigger the picker callback with the BAD value (NoteAfter aborts).
	msg := cmd()
	psm, ok := msg.(pickerStartMsg)
	if !ok {
		t.Fatalf("expected pickerStartMsg; got %T", msg)
	}
	m.picker = psm.picker
	_ = m.picker.onSelect("bad")
	if m.configureSession != nil {
		t.Errorf("session must be cleared after abort; still: %+v", m.configureSession)
	}
	if configureCalled {
		t.Error("SkillConfigure should NOT run after abort")
	}
	// The chat should contain BOTH the NoteAfter message AND the
	// generic cancel message.
	hasAbortMsg := false
	hasCancelMsg := false
	for _, msg := range m.msgs {
		if strings.Contains(msg.content, "prereq missing") {
			hasAbortMsg = true
		}
		if strings.Contains(msg.content, "cancelled") {
			hasCancelMsg = true
		}
	}
	if !hasAbortMsg {
		t.Error("NoteAfter abort message missing from chat")
	}
	if !hasCancelMsg {
		t.Error("generic 'cancelled' message missing")
	}
}

// `/cancel` during a session clears state and prints a message.
func TestSlash_ConfigureCancel(t *testing.T) {
	ft := &fakeSkillCfg{
		fieldsFn: func(id string) ([]SkillField, error) {
			return []SkillField{{Key: "auth", Prompt: "auth", Options: []string{"a"}}}, nil
		},
	}
	m := newModel(context.Background(), Config{SkillConfig: ft})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	m.handleSlash("/configure gsuite")
	if m.configureSession == nil {
		t.Fatal("session should have started")
	}
	m.handleSlash("/cancel")
	if m.configureSession != nil {
		t.Error("/cancel should clear configureSession")
	}
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "cancelled") {
		t.Errorf("cancel should announce; got %q", last)
	}
}

// `/configure` (no id) prints the skill list + a hint.
func TestSlash_ConfigureNoArgsListsAndHints(t *testing.T) {
	ft := &fakeSkillCfg{listRet: []SkillConfigInfo{
		{ID: "gsuite", Label: "Google Workspace"},
	}}
	m := newModel(context.Background(), Config{SkillConfig: ft})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	m.handleSlash("/configure")
	if m.configureSession != nil {
		t.Error("/configure with no args should NOT start a session")
	}
	// Last two messages should be: list + hint
	if len(m.msgs) < 2 {
		t.Fatalf("expected ≥2 messages (list + hint); got %d", len(m.msgs))
	}
	hint := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(hint, "/configure <id>") {
		t.Errorf("hint missing usage; got %q", hint)
	}
}

// During an active session, plain chat input is consumed as the field
// answer rather than sent to the agent.
func TestSubmit_ConfigureSessionEatsPlainInput(t *testing.T) {
	configureCalled := false
	ft := &fakeSkillCfg{
		fieldsFn: func(id string) ([]SkillField, error) {
			return []SkillField{
				// One free-text field — no Options means no picker, chat-input intercept fires.
				{Key: "token", Prompt: "the token", Required: true},
			}, nil
		},
		configureFn: func(args []string, noEnable bool) (string, error) {
			configureCalled = true
			if len(args) != 2 || args[0] != "fake" || args[1] != "token=mytoken" {
				return "", fmt.Errorf("bad args: %v", args)
			}
			return "configured fake", nil
		},
	}
	m := newModel(context.Background(), Config{SkillConfig: ft})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	m.handleSlash("/configure fake")
	if m.configureSession == nil {
		t.Fatal("session should have started")
	}
	// Simulate the user typing the answer in chat input.
	m.input.SetValue("mytoken")
	cmd := m.submit()
	if m.configureSession != nil {
		t.Errorf("session should have advanced + finalized; still active: %+v", m.configureSession)
	}
	if cmd == nil {
		t.Fatal("submit should return a tea.Cmd to run probe asynchronously")
	}
	// Execute the deferred SkillConfigure.
	msg := cmd()
	if !configureCalled {
		t.Error("SkillConfigure was not called")
	}
	if sc, ok := msg.(sysChatMsg); !ok || !strings.Contains(sc.text, "configured fake") {
		t.Errorf("expected sysChatMsg with success; got %T %+v", msg, msg)
	}
}

// `/configure` empty-input fix: pressing Enter on an empty field in
// configure-session mode MUST advance the session (apply Default or
// skip optional), NOT early-return. Before the fix the user was
// locked out because submit() returned nil on empty input.
func TestSubmit_ConfigureSessionEmptyInputAdvances(t *testing.T) {
	configureCalled := false
	ft := &fakeSkillCfg{
		fieldsFn: func(id string) ([]SkillField, error) {
			return []SkillField{
				{Key: "thing", Prompt: "thing", Required: true, Default: "default-value"},
			}, nil
		},
		configureFn: func(args []string, noEnable bool) (string, error) {
			configureCalled = true
			// Default should have been applied — args should carry thing=default-value
			if len(args) < 2 || !strings.Contains(args[1], "default-value") {
				return "", fmt.Errorf("default not applied: %v", args)
			}
			return "configured", nil
		},
	}
	m := newModel(context.Background(), Config{SkillConfig: ft})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/configure fake")

	// Simulate empty Enter — input value is empty.
	m.input.SetValue("")
	cmd := m.submit()
	if m.configureSession != nil {
		t.Errorf("session should have advanced + finalized; still active: %+v", m.configureSession)
	}
	if cmd == nil {
		t.Fatal("submit should return a tea.Cmd (probe runs async)")
	}
	_ = cmd()
	if !configureCalled {
		t.Error("SkillConfigure not called after empty-Enter advance")
	}
}

// ShowIf hides conditional fields during the interactive walkthrough.
// gsuite-shape: pick auth=other, sa_json + subject (which have ShowIf:
// auth=="sa") must be skipped, flow proceeds straight to finalize.
func TestConfigure_ShowIfHidesConditionalFields(t *testing.T) {
	configureArgs := []string{}
	ft := &fakeSkillCfg{
		fieldsFn: func(id string) ([]SkillField, error) {
			return []SkillField{
				{Key: "auth", Prompt: "auth", Required: true, Default: "gcloud",
					Options: []string{"gcloud", "sa"}},
				{Key: "sa_json", Prompt: "sa json", Required: true,
					ShowIf: func(v map[string]string) bool { return v["auth"] == "sa" }},
				{Key: "subject", Prompt: "subject", Required: true,
					ShowIf: func(v map[string]string) bool { return v["auth"] == "sa" }},
			}, nil
		},
		configureFn: func(args []string, noEnable bool) (string, error) {
			configureArgs = append([]string(nil), args...)
			return "ok", nil
		},
	}
	m := newModel(context.Background(), Config{SkillConfig: ft})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	cmd := m.handleSlash("/configure fake")
	// Cmd should start the picker for auth.
	if cmd == nil || m.configureSession == nil {
		t.Fatal("session should start")
	}
	// Simulate picking gcloud via the picker callback.
	msg := cmd()
	psm, ok := msg.(pickerStartMsg)
	if !ok {
		t.Fatalf("expected pickerStartMsg; got %T", msg)
	}
	m.picker = psm.picker
	// Trigger onSelect with "gcloud".
	next := m.picker.onSelect("gcloud")
	if m.configureSession != nil {
		t.Errorf("after gcloud pick + skipped sa_json/subject, session should finalize; still active: %+v", m.configureSession)
	}
	if next == nil {
		t.Fatal("expected a tea.Cmd to finalize (probe)")
	}
	_ = next()
	if len(configureArgs) < 2 || configureArgs[0] != "fake" {
		t.Errorf("expected args to start with [fake, ...]; got %v", configureArgs)
	}
	for _, a := range configureArgs[1:] {
		if strings.HasPrefix(a, "sa_json") || strings.HasPrefix(a, "subject") {
			t.Errorf("hidden field leaked into configure args: %v", configureArgs)
		}
	}
}

// --- /enable skill <plugin>: categorical toggle ---

// `/enable skill gsuite` must route through SetPluginEnabled (forced
// plugin scope) and report "enabled N tool(s) in skill \"gsuite\"".
// Validates the explicit categorical form added per the goal.
func TestSlash_EnableSkillCategorical(t *testing.T) {
	ft := &fakeTools{
		pluginEnabledRet: func(label string, on bool) (int, string, error) {
			if label == "gsuite" {
				return 4, "plugin", nil
			}
			return 0, "", nil
		},
		pluginsRet: []string{"gsuite", "os", "sql", "web"},
	}
	m := newModel(context.Background(), Config{Tools: ft})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	m.handleSlash("/enable skill gsuite")

	// Must have routed to SetPluginEnabled, NOT SetEnabled.
	if len(ft.setPluginCalls) != 1 || ft.setPluginCalls[0].target != "gsuite" || !ft.setPluginCalls[0].on {
		t.Errorf("expected SetPluginEnabled(\"gsuite\", true); got setPluginCalls=%v", ft.setPluginCalls)
	}
	if len(ft.setEnabledCalls) != 0 {
		t.Errorf("`/enable skill gsuite` must NOT fall through to SetEnabled (smart-match); got %v", ft.setEnabledCalls)
	}
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "enabled 4 tool(s) in skill \"gsuite\"") {
		t.Errorf("missing success message:\n%s", last)
	}
}

// `/enable skill <typo>` must emit a "no skill named X — try: a, b, c"
// hint. Without the typo guidance, the user has no way to discover
// available skill labels from the error.
func TestSlash_EnableSkillUnknownGivesHint(t *testing.T) {
	ft := &fakeTools{pluginsRet: []string{"gsuite", "os", "sql"}}
	m := newModel(context.Background(), Config{Tools: ft})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	m.handleSlash("/enable skill nosuchskill")

	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "no skill named nosuchskill") {
		t.Errorf("expected 'no skill named' error:\n%s", last)
	}
	for _, want := range []string{"gsuite", "os", "sql"} {
		if !strings.Contains(last, want) {
			t.Errorf("hint should list available skill %q:\n%s", want, last)
		}
	}
}

// `/disable skill gsuite` mirrors the enable path with on=false.
func TestSlash_DisableSkillMirrorsEnable(t *testing.T) {
	ft := &fakeTools{
		pluginEnabledRet: func(string, bool) (int, string, error) { return 4, "plugin", nil },
	}
	m := newModel(context.Background(), Config{Tools: ft})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	m.handleSlash("/disable skill gsuite")

	if len(ft.setPluginCalls) != 1 || ft.setPluginCalls[0].on {
		t.Errorf("expected SetPluginEnabled(\"gsuite\", false); got %v", ft.setPluginCalls)
	}
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "disabled 4 tool(s) in skill \"gsuite\"") {
		t.Errorf("missing disable success message:\n%s", last)
	}
}

// `/enable gmail_send` (no `skill` keyword) must keep the existing
// smart-match path — calls SetEnabled, NOT SetPluginEnabled. Validates
// back-compat with the muscle-memory case.
func TestSlash_EnableBareNameUsesSmartMatch(t *testing.T) {
	ft := &fakeTools{}
	m := newModel(context.Background(), Config{Tools: ft})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	m.handleSlash("/enable gmail_send")

	if len(ft.setEnabledCalls) != 1 || ft.setEnabledCalls[0].target != "gmail_send" {
		t.Errorf("bare /enable must route to SetEnabled; got setEnabledCalls=%v setPluginCalls=%v",
			ft.setEnabledCalls, ft.setPluginCalls)
	}
	if len(ft.setPluginCalls) != 0 {
		t.Errorf("bare /enable must NOT call SetPluginEnabled; got %v", ft.setPluginCalls)
	}
}

// --- /dashboard (TEN-86) ---

// fakeDash records Enable/Disable/Status calls and reports a canned state,
// mirroring the fakeTools pattern. running is toggled by Enable/Disable so a
// status query after a toggle reflects the change.
type fakeDash struct {
	addr        string
	running     bool
	enableErr   error
	disableErr  error
	enableCalls int
	disableC    int
	statusCalls int
}

func (f *fakeDash) Enable() (string, error) {
	f.enableCalls++
	if f.enableErr == nil {
		f.running = true
	}
	return f.addr, f.enableErr
}
func (f *fakeDash) Disable() error {
	f.disableC++
	if f.disableErr == nil {
		f.running = false
	}
	return f.disableErr
}
func (f *fakeDash) Status() (bool, string) {
	f.statusCalls++
	return f.running, f.addr
}

// `/dashboard on` enables + reports the URL; `/dashboard off` disables +
// reports stopped; bare/`status` reflect the running state.
func TestSlash_Dashboard(t *testing.T) {
	fd := &fakeDash{addr: "127.0.0.1:8770"}
	m := newModel(context.Background(), Config{Dash: fd})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	// on → Enable + URL.
	m.handleSlash("/dashboard on")
	if fd.enableCalls != 1 {
		t.Fatalf("`/dashboard on` should call Enable once; got %d", fd.enableCalls)
	}
	if last := m.msgs[len(m.msgs)-1].content; !strings.Contains(last, "http://127.0.0.1:8770") {
		t.Errorf("`/dashboard on` should report the URL: %q", last)
	}

	// status → running.
	m.handleSlash("/dashboard status")
	if last := m.msgs[len(m.msgs)-1].content; !strings.Contains(last, "running") {
		t.Errorf("`/dashboard status` should report running: %q", last)
	}

	// off → Disable + stopped.
	m.handleSlash("/dashboard off")
	if fd.disableC != 1 {
		t.Fatalf("`/dashboard off` should call Disable once; got %d", fd.disableC)
	}
	if last := m.msgs[len(m.msgs)-1].content; !strings.Contains(last, "stopped") {
		t.Errorf("`/dashboard off` should report stopped: %q", last)
	}

	// bare → now stopped.
	m.handleSlash("/dashboard")
	if last := m.msgs[len(m.msgs)-1].content; !strings.Contains(last, "stopped") {
		t.Errorf("bare `/dashboard` should reflect stopped state: %q", last)
	}
}

// An Enable error surfaces to the feed instead of a bogus URL.
func TestSlash_DashboardEnableError(t *testing.T) {
	fd := &fakeDash{addr: "127.0.0.1:8770", enableErr: errors.New("bind failed")}
	m := newModel(context.Background(), Config{Dash: fd})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	m.handleSlash("/dashboard on")
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "bind failed") {
		t.Errorf("Enable error should surface to the feed: %q", last)
	}
}

// With no DashboardControl wired, /dashboard degrades gracefully (and never
// touches a nil control).
func TestSlash_DashboardNil(t *testing.T) {
	m := newModel(context.Background(), Config{})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	m.handleSlash("/dashboard on")
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "not available") {
		t.Errorf("nil Dash should report unavailable: %q", last)
	}
}

// --- C3: /research history / show / replay / delete ---

// fakeResearchCtl is a deterministic stand-in for the real researchControl —
// records calls + returns canned data so we can exercise the TUI dispatcher
// without spinning up agents, stores, or models.
type fakeResearchCtl struct {
	rows           []ResearchHistoryRow
	showFn         func(id string) (string, error)
	replayFn       func(id string) (string, error)
	deleteFn       func(id string) error
	calls          []string // appended on every method invocation, for assertions
	researchFn     func(question string) (string, error)
	afterClarifyFn func(question string) (string, error)
}

func (f *fakeResearchCtl) Research(_ context.Context, q string) (string, error) {
	f.calls = append(f.calls, "Research:"+q)
	if f.researchFn != nil {
		return f.researchFn(q)
	}
	return "report for " + q, nil
}
func (f *fakeResearchCtl) ResearchAfterClarify(_ context.Context, q string) (string, error) {
	f.calls = append(f.calls, "ResearchAfterClarify:"+q)
	if f.afterClarifyFn != nil {
		return f.afterClarifyFn(q)
	}
	return "report (after clarify) for " + q, nil
}
func (f *fakeResearchCtl) ResearchHistory(limit int) ([]ResearchHistoryRow, error) {
	f.calls = append(f.calls, fmt.Sprintf("History:%d", limit))
	if limit > 0 && limit < len(f.rows) {
		return f.rows[:limit], nil
	}
	return f.rows, nil
}
func (f *fakeResearchCtl) ResearchShow(id string) (string, error) {
	f.calls = append(f.calls, "Show:"+id)
	if f.showFn != nil {
		return f.showFn(id)
	}
	return "shown " + id, nil
}
func (f *fakeResearchCtl) ResearchReplay(_ context.Context, id string) (string, error) {
	f.calls = append(f.calls, "Replay:"+id)
	if f.replayFn != nil {
		return f.replayFn(id)
	}
	return "replayed " + id, nil
}
func (f *fakeResearchCtl) ResearchDelete(id string) error {
	f.calls = append(f.calls, "Delete:"+id)
	if f.deleteFn != nil {
		return f.deleteFn(id)
	}
	return nil
}

// /research with no args prints a multi-form usage hint (not just the legacy
// "needs a question") so users can discover the new sub-commands.
func TestResearch_BareUsageMentionsSubcommands(t *testing.T) {
	rc := &fakeResearchCtl{}
	m := newModel(context.Background(), Config{Research: rc})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.handleSlash("/research")
	last := m.msgs[len(m.msgs)-1].content
	for _, want := range []string{"history", "show", "replay", "delete"} {
		if !strings.Contains(last, want) {
			t.Errorf("usage missing %q:\n%s", want, last)
		}
	}
}

// /research history → calls History, renders the table to the chat pane.
func TestResearch_History_RendersTable(t *testing.T) {
	rc := &fakeResearchCtl{rows: []ResearchHistoryRow{
		{ID: "20260523-110000-x", Question: "what is x?", Status: "done", NumFinds: 3, NumRefs: 2,
			Model: "aeon-ultimate", Cycles: 2},
		{ID: "20260523-100000-y", Question: "what is y?", Status: "error", NumFinds: 0, NumRefs: 0,
			Model: "aeon-ultimate", Cycles: 1, ReplayOf: "20260522-foo"},
	}}
	m := newModel(context.Background(), Config{Research: rc})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/research history")
	if len(rc.calls) != 1 || !strings.HasPrefix(rc.calls[0], "History:") {
		t.Fatalf("History not called: %v", rc.calls)
	}
	rendered := m.msgs[len(m.msgs)-1].content
	for _, want := range []string{"2 run(s):", "what is x?", "done", "what is y?", "error", "(replay of 20260522-foo)"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("history render missing %q:\n%s", want, rendered)
		}
	}
}

// /research history N — bare numeric limit override.
func TestResearch_History_NumericLimit(t *testing.T) {
	rc := &fakeResearchCtl{rows: make([]ResearchHistoryRow, 5)}
	for i := range rc.rows {
		rc.rows[i] = ResearchHistoryRow{ID: fmt.Sprintf("id-%d", i), Question: "q", Status: "done"}
	}
	m := newModel(context.Background(), Config{Research: rc})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/research history 3")
	if len(rc.calls) != 1 || rc.calls[0] != "History:3" {
		t.Errorf("wrong limit forwarded: %v", rc.calls)
	}
}

// /research show <id> — invokes Show and posts the result as an assistant message.
func TestResearch_Show(t *testing.T) {
	rc := &fakeResearchCtl{showFn: func(id string) (string, error) {
		return "── " + id + " ──\nQuestion: x\n\nfull report body", nil
	}}
	m := newModel(context.Background(), Config{Research: rc})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/research show 20260523-110000-x")
	if len(rc.calls) != 1 || rc.calls[0] != "Show:20260523-110000-x" {
		t.Fatalf("Show not called correctly: %v", rc.calls)
	}
	last := m.msgs[len(m.msgs)-1]
	if last.role != "assistant" {
		t.Errorf("show output should land as assistant msg, got role=%q", last.role)
	}
	if !strings.Contains(last.content, "full report body") {
		t.Errorf("body missing: %q", last.content)
	}
}

// /research show with no id → usage hint, no call.
func TestResearch_Show_RequiresID(t *testing.T) {
	rc := &fakeResearchCtl{}
	m := newModel(context.Background(), Config{Research: rc})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.handleSlash("/research show")
	if len(rc.calls) != 0 {
		t.Errorf("Show should not be called without id, got %v", rc.calls)
	}
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "usage:") || !strings.Contains(last, "<id>") {
		t.Errorf("missing usage hint: %q", last)
	}
}

// /research delete <id> — invokes Delete and acknowledges in chat.
func TestResearch_Delete(t *testing.T) {
	rc := &fakeResearchCtl{}
	m := newModel(context.Background(), Config{Research: rc})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.handleSlash("/research delete some-id")
	if len(rc.calls) != 1 || rc.calls[0] != "Delete:some-id" {
		t.Fatalf("Delete not called: %v", rc.calls)
	}
	if !strings.Contains(m.msgs[len(m.msgs)-1].content, "some-id") {
		t.Errorf("ack missing id: %q", m.msgs[len(m.msgs)-1].content)
	}
}

// /research <question> still routes to the question path even when the first
// word LOOKS like it could be ambiguous. Guard against the sub-command sniff
// being too greedy.
func TestResearch_QuestionStillWorks(t *testing.T) {
	rc := &fakeResearchCtl{}
	m := newModel(context.Background(), Config{Research: rc})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	cmd := m.handleSlash("/research nvidia stock")
	if cmd == nil {
		t.Fatal("/research <q> should return a tea.Cmd (the async runner)")
	}
	// Run the cmd to trigger Research call.
	cmd()
	found := false
	for _, c := range rc.calls {
		if c == "Research:nvidia stock" {
			found = true
		}
	}
	if !found {
		t.Errorf("Research not called with question, calls: %v", rc.calls)
	}
}

// /research replay <id> — guarded by busy state same as /research <q>; when
// idle, schedules a tea.Cmd that invokes ResearchReplay.
func TestResearch_Replay_Schedules(t *testing.T) {
	rc := &fakeResearchCtl{}
	m := newModel(context.Background(), Config{Research: rc})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	cmd := m.handleSlash("/research replay 20260523-x")
	if cmd == nil {
		t.Fatal("replay should return a tea.Cmd")
	}
	if !m.busy {
		t.Error("replay should set busy=true")
	}
	cmd() // run the async function
	if len(rc.calls) != 1 || rc.calls[0] != "Replay:20260523-x" {
		t.Errorf("Replay not invoked: %v", rc.calls)
	}
}

// Replay refuses while busy (same guard as the question path).
func TestResearch_Replay_RefusesWhenBusy(t *testing.T) {
	rc := &fakeResearchCtl{}
	m := newModel(context.Background(), Config{Research: rc})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.busy = true
	if cmd := m.handleSlash("/research replay 20260523-x"); cmd != nil {
		t.Error("replay while busy should NOT return a cmd")
	}
	if len(rc.calls) != 0 {
		t.Errorf("replay should not call control while busy, got %v", rc.calls)
	}
}

// Unknown /research sub-command — useful error rather than silent ignore.
func TestResearch_UnknownSubcommand(t *testing.T) {
	rc := &fakeResearchCtl{}
	m := newModel(context.Background(), Config{Research: rc})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	// `nuke` isn't a real sub, but it ALSO doesn't look like a question
	// (single word) — so per the dispatcher rules, it's treated as a question
	// (the original behavior). Verify the EXPLICIT sub names get the special
	// branch and an unknown word treated as a question doesn't crash.
	cmd := m.handleSlash("/research nuke")
	if cmd == nil {
		t.Error("single-word question should still kick off /research <q>")
	}
}

// splitFirstWord pure helper — handles common shapes the dispatcher relies on.
func TestSplitFirstWord(t *testing.T) {
	cases := []struct {
		in, first, rest string
		ok              bool
	}{
		{"", "", "", false},
		{"  ", "", "", false},
		{"history", "history", "", true},
		{"history 10", "history", "10", true},
		{"  show   some-id  ", "show", "some-id", true},
		{"replay\t20260523-x", "replay", "20260523-x", true},
	}
	for _, c := range cases {
		f, r, ok := splitFirstWord(c.in)
		if f != c.first || r != c.rest || ok != c.ok {
			t.Errorf("splitFirstWord(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.in, f, r, ok, c.first, c.rest, c.ok)
		}
	}
}

// fakeModelCtl is a deterministic stand-in for the real modelControl —
// records calls + returns canned data so we can exercise the TUI dispatcher
// without spinning up an agent + config files.
type fakeModelCtl struct {
	addCloudFn func(kind, key string) (string, error)
	addFn      func(name, endpoint, fmt string) (string, error)
	infos      []ModelInfo // canned ModelList() result (nil = none configured)
	calls      []string
}

func (f *fakeModelCtl) ModelList() []ModelInfo { return f.infos }
func (f *fakeModelCtl) UseModel(name, modelOverride string) (string, string, error) {
	f.calls = append(f.calls, fmt.Sprintf("Use:%s/%s", name, modelOverride))
	return "✓ now using " + name, name, nil
}
func (f *fakeModelCtl) ListProviderModels(name string) ([]string, error) {
	f.calls = append(f.calls, "ListModels:"+name)
	return []string{"glm-4.6", "glm-5.1"}, nil
}
func (f *fakeModelCtl) RemoveModel(name string) (string, error) { return "", nil }
func (f *fakeModelCtl) ReloadKeys() (string, error) {
	f.calls = append(f.calls, "ReloadKeys")
	return "✓ reloaded", nil
}
func (f *fakeModelCtl) LoopCeiling() int                          { return 16 }
func (f *fakeModelCtl) SetLoopCeiling(n int) (string, error)      { return "", nil }
func (f *fakeModelCtl) ReasoningSupported() bool                  { return false }
func (f *fakeModelCtl) ReasoningEffort() string                   { return "" }
func (f *fakeModelCtl) SetReasoningEffort(string) (string, error) { return "", nil }
func (f *fakeModelCtl) Fallback() []string                        { return nil }
func (f *fakeModelCtl) SetFallback([]string) (string, error)      { return "", nil }
func (f *fakeModelCtl) AddModel(name, endpoint, format string) (string, error) {
	f.calls = append(f.calls, fmt.Sprintf("Add:%s,%s,%s", name, endpoint, format))
	if f.addFn != nil {
		return f.addFn(name, endpoint, format)
	}
	return "added " + name, nil
}
func (f *fakeModelCtl) AddCloudModel(kind, apiKey string) (string, error) {
	f.calls = append(f.calls, fmt.Sprintf("AddCloud:%s,%s", kind, apiKey))
	if f.addCloudFn != nil {
		return f.addCloudFn(kind, apiKey)
	}
	return "added cloud " + kind, nil
}

// /model reload re-resolves the active provider's key live (no restart).
func TestModel_Reload_Dispatches(t *testing.T) {
	mc := &fakeModelCtl{}
	m := newModel(context.Background(), Config{Models: mc})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	m.handleSlash("/model reload")
	found := false
	for _, c := range mc.calls {
		if c == "ReloadKeys" {
			found = true
		}
	}
	if !found {
		t.Fatalf("/model reload should call ReloadKeys: %v", mc.calls)
	}
	if last := m.msgs[len(m.msgs)-1].content; !strings.Contains(last, "reloaded") {
		t.Errorf("status missing: %q", last)
	}
}

// /model add-cloud <kind> <key> routes to AddCloudModel and shows confirmation.
func TestModel_AddCloud_Dispatches(t *testing.T) {
	mc := &fakeModelCtl{}
	m := newModel(context.Background(), Config{Models: mc})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	m.handleSlash("/model add-cloud zai sk-test-key")
	if len(mc.calls) != 1 || mc.calls[0] != "AddCloud:zai,sk-test-key" {
		t.Fatalf("AddCloudModel not invoked correctly: %v", mc.calls)
	}
	// Success message lands in the system chat.
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "zai") {
		t.Errorf("status message missing kind: %q", last)
	}
}

// /model add-cloud with missing args shows usage.
func TestModel_AddCloud_Usage(t *testing.T) {
	mc := &fakeModelCtl{}
	m := newModel(context.Background(), Config{Models: mc})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	m.handleSlash("/model add-cloud zai") // missing key
	if len(mc.calls) != 0 {
		t.Errorf("should not dispatch with missing arg: %v", mc.calls)
	}
	last := m.msgs[len(m.msgs)-1].content
	for _, want := range []string{"usage:", "<kind>", "<api-key>", "zai"} {
		if !strings.Contains(last, want) {
			t.Errorf("usage hint missing %q:\n%s", want, last)
		}
	}
}

// /model add-cloud errors are surfaced (not silently swallowed).
func TestModel_AddCloud_ErrorSurfaced(t *testing.T) {
	mc := &fakeModelCtl{
		addCloudFn: func(kind, key string) (string, error) {
			return "", fmt.Errorf("unknown provider kind %q", kind)
		},
	}
	m := newModel(context.Background(), Config{Models: mc})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	m.handleSlash("/model add-cloud nope sk-test")
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "unknown provider kind") {
		t.Errorf("error not shown to user: %q", last)
	}
}

// --- C4: research timeline (live status pane) ---

// applyResearchTimeline must initialize state on `started`, replace plan +
// seed pending rows on `plan`, then transition phases through reflect/synth/
// done. Each step exercised in order to match a real /research lifecycle.
func TestResearchTimeline_LifecycleStateMachine(t *testing.T) {
	m := newModel(context.Background(), Config{})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	// Idle → no pane.
	if m.researchTimeline != nil {
		t.Fatal("timeline should be nil before any update")
	}

	// started → snapshot created, phase=planning.
	m.applyResearchTimeline(ResearchTimelineUpdate{
		Kind: "started", Total: 2,
		Started: &ResearchStartedData{Question: "nvidia stock"},
	})
	if m.researchTimeline == nil {
		t.Fatal("started should initialize the timeline state")
	}
	if m.researchTimeline.Question != "nvidia stock" {
		t.Errorf("question lost: %q", m.researchTimeline.Question)
	}
	if m.researchTimeline.Phase != ResearchPhasePlanning {
		t.Errorf("phase = %q, want planning", m.researchTimeline.Phase)
	}
	if m.researchTimeline.Total != 2 {
		t.Errorf("total = %d, want 2", m.researchTimeline.Total)
	}

	// plan → cycle stamped, sub-questions seeded as pending rows.
	m.applyResearchTimeline(ResearchTimelineUpdate{
		Kind: "plan", Cycle: 1, Total: 2,
		Plan: &ResearchPlanData{SubQuestions: []string{"What is NVDA's current price?", "Q3 2026 earnings?"}},
	})
	if m.researchTimeline.Cycle != 1 {
		t.Errorf("cycle = %d, want 1", m.researchTimeline.Cycle)
	}
	if len(m.researchTimeline.Agents) != 2 {
		t.Fatalf("want 2 pending rows, got %d", len(m.researchTimeline.Agents))
	}
	for _, r := range m.researchTimeline.Agents {
		if r.Status != "pending" {
			t.Errorf("pre-spawn row should be 'pending', got %q", r.Status)
		}
		if r.SubQ == "" {
			t.Error("pre-spawn row should have a SubQ")
		}
	}
	if m.researchTimeline.Phase != ResearchPhaseDispatch {
		t.Errorf("phase after plan should be dispatch, got %q", m.researchTimeline.Phase)
	}

	// agent_status (spawn): pairs an id to the first pending row.
	m.applyResearchTimeline(ResearchTimelineUpdate{
		Kind:  "agent_status",
		Agent: &ResearchAgentRow{ID: "main-r-1", SubQ: "What is NVDA's current price?", Status: "running"},
	})
	if got := m.researchTimeline.Agents[0]; got.ID != "main-r-1" || got.Status != "running" {
		t.Errorf("first row not bound: %+v", got)
	}
	if _, ok := m.researchTimeline.agentByID["main-r-1"]; !ok {
		t.Error("agentByID not indexed")
	}

	// Tool calls accumulate via tallyTimelineToolEvent.
	m.tallyTimelineToolEvent(TeamEvent{AgentID: "main-r-1", Event: agent.Event{Kind: agent.EventToolCall, Tool: "web_search"}})
	m.tallyTimelineToolEvent(TeamEvent{AgentID: "main-r-1", Event: agent.Event{Kind: agent.EventToolResult, Tool: "web_search"}})
	m.tallyTimelineToolEvent(TeamEvent{AgentID: "main-r-1", Event: agent.Event{Kind: agent.EventToolResult, IsErr: true}})
	row := m.researchTimeline.agentByID["main-r-1"]
	if row.NumTools != 1 || row.NumOK != 1 || row.NumErr != 1 {
		t.Errorf("tool tally wrong: %+v", row)
	}

	// agent_status (done): merges status + result length without clobbering tools.
	m.applyResearchTimeline(ResearchTimelineUpdate{
		Kind:  "agent_status",
		Agent: &ResearchAgentRow{ID: "main-r-1", Status: "done", ResultLen: 2048},
	})
	row = m.researchTimeline.agentByID["main-r-1"]
	if row.Status != "done" || row.ResultLen != 2048 {
		t.Errorf("done merge wrong: %+v", row)
	}
	if row.NumTools != 1 {
		t.Errorf("tool count clobbered on status merge: %d", row.NumTools)
	}

	// reflect_done → phase + gaps recorded.
	m.applyResearchTimeline(ResearchTimelineUpdate{
		Kind: "reflect_done", Cycle: 1, Total: 2,
		Reflect: &ResearchReflectData{Gaps: []string{"What about competitors?"}},
	})
	if m.researchTimeline.Phase != ResearchPhaseReflect {
		t.Errorf("phase after reflect = %q", m.researchTimeline.Phase)
	}
	if len(m.researchTimeline.LastReflectGaps) != 1 {
		t.Errorf("gaps lost: %+v", m.researchTimeline.LastReflectGaps)
	}

	// synth → phase = synth.
	m.applyResearchTimeline(ResearchTimelineUpdate{
		Kind: "synth", Synth: &ResearchSynthData{Starting: true},
	})
	if m.researchTimeline.Phase != ResearchPhaseSynth {
		t.Errorf("phase after synth = %q", m.researchTimeline.Phase)
	}

	// done → phase = done + numbers stamped (snapshot remains until cleared).
	m.applyResearchTimeline(ResearchTimelineUpdate{
		Kind: "done",
		Done: &ResearchDoneData{Status: "done", NumRefs: 3, NumFinds: 2, Duration: 90 * time.Second},
	})
	if m.researchTimeline.Phase != ResearchPhaseDone {
		t.Errorf("phase after done = %q", m.researchTimeline.Phase)
	}
	if m.researchTimeline.DoneRefs != 3 || m.researchTimeline.DoneFinds != 2 {
		t.Errorf("done numbers wrong: refs=%d finds=%d",
			m.researchTimeline.DoneRefs, m.researchTimeline.DoneFinds)
	}
}

// `plan` arriving for cycle 2 drops the prior cycle's agent rows (each cycle
// spawns fresh ids, so we shouldn't pile them up forever).
func TestResearchTimeline_PlanResetsAgentsPerCycle(t *testing.T) {
	m := newModel(context.Background(), Config{})
	m.applyResearchTimeline(ResearchTimelineUpdate{Kind: "started", Total: 2, Started: &ResearchStartedData{Question: "q"}})
	m.applyResearchTimeline(ResearchTimelineUpdate{
		Kind: "plan", Cycle: 1, Total: 2,
		Plan: &ResearchPlanData{SubQuestions: []string{"a?", "b?"}},
	})
	m.applyResearchTimeline(ResearchTimelineUpdate{
		Kind: "agent_status", Agent: &ResearchAgentRow{ID: "x", Status: "done", ResultLen: 100},
	})
	m.applyResearchTimeline(ResearchTimelineUpdate{
		Kind: "plan", Cycle: 2, Total: 2,
		Plan: &ResearchPlanData{SubQuestions: []string{"c?"}},
	})
	if len(m.researchTimeline.Agents) != 1 {
		t.Errorf("cycle 2 plan should reset to 1 pending row, got %d", len(m.researchTimeline.Agents))
	}
	if m.researchTimeline.Agents[0].SubQ != "c?" {
		t.Errorf("cycle 2 row should be c?, got %q", m.researchTimeline.Agents[0].SubQ)
	}
	if _, ok := m.researchTimeline.agentByID["x"]; ok {
		t.Error("agentByID should clear on cycle 2 plan")
	}
}

// tallyTimelineToolEvent for an unknown agent id creates a placeholder row
// (don't drop events arriving before the orchestrator's agent_status update).
func TestResearchTimeline_TallyPlaceholderRow(t *testing.T) {
	m := newModel(context.Background(), Config{})
	m.applyResearchTimeline(ResearchTimelineUpdate{Kind: "started", Total: 1, Started: &ResearchStartedData{Question: "q"}})
	m.tallyTimelineToolEvent(TeamEvent{AgentID: "race-id", Event: agent.Event{Kind: agent.EventToolCall}})
	row := m.researchTimeline.agentByID["race-id"]
	if row == nil {
		t.Fatal("placeholder row not created")
	}
	if row.NumTools != 1 {
		t.Errorf("tool count wrong: %+v", row)
	}
}

// applyResearchTimeline must be a no-op when researchTimeline is nil (the
// pane is torn down between runs; stray late updates shouldn't panic).
func TestResearchTimeline_NoOpAfterClear(t *testing.T) {
	m := newModel(context.Background(), Config{})
	// No `started` first — timeline is nil.
	for _, kind := range []string{"plan", "agent_status", "wave", "reflect_done", "synth", "done"} {
		m.applyResearchTimeline(ResearchTimelineUpdate{Kind: kind})
	}
	if m.researchTimeline != nil {
		t.Error("no-op path should leave timeline nil")
	}
	// tallyTimelineToolEvent also no-ops cleanly.
	m.tallyTimelineToolEvent(TeamEvent{AgentID: "x", Event: agent.Event{Kind: agent.EventToolCall}})
}

// renderResearchTimeline produces a multi-line block: header + per-row glyphs.
// Strip ANSI to check the visible characters, since lipgloss styles are
// terminal-conditional.
func TestResearchTimeline_Render(t *testing.T) {
	rt := &researchTimelineState{
		Question:  "nvidia stock",
		Phase:     ResearchPhaseDispatch,
		StartedAt: time.Now().Add(-90 * time.Second),
		Cycle:     1, Total: 2,
		Plan:       []string{"What is NVDA price?", "Q3 earnings?"},
		WaveStatus: "dispatched 1–2 of 2",
		Agents: []*ResearchAgentRow{
			{ID: "main-r-1", SubQ: "What is NVDA price?", Status: "done", NumTools: 4, NumOK: 4, ResultLen: 1800},
			{ID: "main-r-2", SubQ: "Q3 earnings?", Status: "running", NumTools: 2, NumOK: 1, NumErr: 1},
		},
		agentByID: map[string]*ResearchAgentRow{},
	}
	out := stripANSI(renderResearchTimeline(rt))
	for _, want := range []string{
		"research",                            // section header
		"dispatched",                          // phase label
		"cycle 1/2",                           // cycle counter
		"nvidia stock",                        // question
		"dispatched 1–2 of 2",                 // wave status
		"What is NVDA price?", "Q3 earnings?", // sub-questions
		"4 tools",      // first row tally
		"2 tools, 1 ✗", // second row tally (with error count)
		"1800ch",       // result length on the done row
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered timeline missing %q:\n%s", want, out)
		}
	}
}

// done phase renders the final summary in the header area (not the wave line).
func TestResearchTimeline_RenderDone(t *testing.T) {
	rt := &researchTimelineState{
		Question:  "x",
		Phase:     ResearchPhaseDone,
		StartedAt: time.Now().Add(-30 * time.Second),
		Cycle:     1, Total: 1,
		DoneStatus:   "done",
		DoneRefs:     5,
		DoneFinds:    3,
		DoneDuration: 30 * time.Second,
	}
	out := stripANSI(renderResearchTimeline(rt))
	if !strings.Contains(out, "done — 3 finding(s), 5 ref(s)") {
		t.Errorf("done summary missing: %q", out)
	}
}

// error phase shows the error message in the header.
func TestResearchTimeline_RenderError(t *testing.T) {
	rt := &researchTimelineState{
		Question:  "x",
		Phase:     ResearchPhaseError,
		StartedAt: time.Now(),
		DoneError: "research: no usable findings",
	}
	out := stripANSI(renderResearchTimeline(rt))
	if !strings.Contains(out, "no usable findings") {
		t.Errorf("error not shown: %q", out)
	}
}

// renderFeed includes the timeline block when one is active, omits it when nil.
func TestRenderFeed_IncludesTimelineWhenActive(t *testing.T) {
	m := newModel(context.Background(), Config{})
	m.appendFeed("some line")
	// No active timeline → just the activity title + the feed line.
	out := stripANSI(m.renderFeed())
	if strings.Contains(out, "research") && strings.Contains(out, "cycle") {
		t.Error("nil timeline shouldn't render the research block")
	}
	// Active timeline → "research" header appears.
	m.applyResearchTimeline(ResearchTimelineUpdate{
		Kind: "started", Total: 1,
		Started: &ResearchStartedData{Question: "test"},
	})
	out = stripANSI(m.renderFeed())
	if !strings.Contains(out, "research") {
		t.Errorf("active timeline missing 'research' header:\n%s", out)
	}
	if !strings.Contains(out, "some line") {
		t.Errorf("feed line gone after timeline insert:\n%s", out)
	}
}

// --- /agents (named sub-agent profiles) ---

type fakeAgentCtl struct {
	rows     []AgentInfo
	addFn    func(name, provider, model, desc, soul string) (string, error)
	soulFn   func(name, soul string) (string, error)
	rmFn     func(name string) (string, error)
	showFn   func(name string) (AgentDetail, error)
	renameFn func(old, new string) (string, error)
	modelFn  func(name, provider, model string) (string, error)
	calls    []string
}

func (f *fakeAgentCtl) List() ([]AgentInfo, error) {
	f.calls = append(f.calls, "List")
	return f.rows, nil
}
func (f *fakeAgentCtl) Add(name, provider, model, desc, soul string) (string, error) {
	f.calls = append(f.calls, fmt.Sprintf("Add:%s/%s/%s/%s/%s", name, provider, model, desc, soul))
	if f.addFn != nil {
		return f.addFn(name, provider, model, desc, soul)
	}
	return "added " + name, nil
}
func (f *fakeAgentCtl) SetSoul(name, soul string) (string, error) {
	f.calls = append(f.calls, fmt.Sprintf("SetSoul:%s/%s", name, soul))
	if f.soulFn != nil {
		return f.soulFn(name, soul)
	}
	return "soul updated", nil
}
func (f *fakeAgentCtl) Remove(name string) (string, error) {
	f.calls = append(f.calls, "Remove:"+name)
	if f.rmFn != nil {
		return f.rmFn(name)
	}
	return "removed", nil
}
func (f *fakeAgentCtl) Show(name string) (AgentDetail, error) {
	f.calls = append(f.calls, "Show:"+name)
	if f.showFn != nil {
		return f.showFn(name)
	}
	return AgentDetail{Name: name, Provider: "zai", Model: "glm-4.6"}, nil
}
func (f *fakeAgentCtl) Rename(oldName, newName string) (string, error) {
	f.calls = append(f.calls, fmt.Sprintf("Rename:%s/%s", oldName, newName))
	if f.renameFn != nil {
		return f.renameFn(oldName, newName)
	}
	return "renamed " + oldName + " → " + newName, nil
}
func (f *fakeAgentCtl) SetModel(name, provider, model string) (string, error) {
	f.calls = append(f.calls, fmt.Sprintf("SetModel:%s/%s/%s", name, provider, model))
	if f.modelFn != nil {
		return f.modelFn(name, provider, model)
	}
	return "set model for " + name, nil
}

// /agents (bare) → calls List, renders the table or empty-state hint.
func TestAgents_BareListsEmpty(t *testing.T) {
	ac := &fakeAgentCtl{rows: nil}
	m := newModel(context.Background(), Config{Agents: ac})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/agents")
	if len(ac.calls) != 1 || ac.calls[0] != "List" {
		t.Fatalf("List not called: %v", ac.calls)
	}
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "no named agents") {
		t.Errorf("empty-state hint missing: %q", last)
	}
	// Hint should also show how to add.
	if !strings.Contains(last, "/agents add") {
		t.Errorf("add hint missing: %q", last)
	}
}

// /agents with rows → renders each one with model + soul indicator.
func TestAgents_ListRenders(t *testing.T) {
	ac := &fakeAgentCtl{rows: []AgentInfo{
		{Name: "researcher", Provider: "zai", Model: "glm-4.6", Description: "web", HasSoul: true, Valid: true},
		{Name: "writer", Provider: "dgx", Model: "aeon-ultimate", Valid: true},
		{Name: "broken", Provider: "ghost", Model: "", Valid: false},
	}}
	m := newModel(context.Background(), Config{Agents: ac})
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	m.handleSlash("/agents")
	last := m.msgs[len(m.msgs)-1].content
	for _, want := range []string{
		"3 named agent(s)",
		"researcher", "zai/glm-4.6", "has soul", "web",
		"writer", "dgx/aeon-ultimate",
		"broken", // appears with ⚠ marker
	} {
		if !strings.Contains(last, want) {
			t.Errorf("listing missing %q:\n%s", want, last)
		}
	}
}

// /agents add <name> <provider> [<model>] [-- <description>] routes correctly.
func TestAgents_Add_BasicAndWithDescription(t *testing.T) {
	ac := &fakeAgentCtl{}
	m := newModel(context.Background(), Config{Agents: ac})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	// Minimal: name + provider.
	m.handleSlash("/agents add researcher zai")
	if len(ac.calls) != 1 || !strings.HasPrefix(ac.calls[0], "Add:researcher/zai//") {
		t.Errorf("Add minimal not routed: %v", ac.calls)
	}

	// With explicit model.
	ac.calls = nil
	m.handleSlash("/agents add writer dgx aeon-ultimate")
	if len(ac.calls) != 1 || !strings.Contains(ac.calls[0], "Add:writer/dgx/aeon-ultimate/") {
		t.Errorf("Add with model not routed: %v", ac.calls)
	}

	// With description after `--`.
	ac.calls = nil
	m.handleSlash("/agents add critic anthropic claude-sonnet-4 -- adversarial reviewer")
	if len(ac.calls) != 1 {
		t.Fatalf("Add with desc not invoked: %v", ac.calls)
	}
	if !strings.Contains(ac.calls[0], "critic") || !strings.Contains(ac.calls[0], "adversarial reviewer") {
		t.Errorf("description not parsed: %q", ac.calls[0])
	}
}

// /agents add with missing args → usage hint, no call.
func TestAgents_Add_Usage(t *testing.T) {
	ac := &fakeAgentCtl{}
	m := newModel(context.Background(), Config{Agents: ac})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/agents add researcher") // missing provider
	if len(ac.calls) != 0 {
		t.Errorf("Add should not call with missing args: %v", ac.calls)
	}
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "usage:") || !strings.Contains(last, "<provider>") {
		t.Errorf("usage hint missing: %q", last)
	}
}

// /agents soul <name> <markdown> joins multi-word body, routes to SetSoul.
func TestAgents_SetSoul(t *testing.T) {
	ac := &fakeAgentCtl{}
	m := newModel(context.Background(), Config{Agents: ac})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	m.handleSlash("/agents soul researcher You are a thorough web researcher who cites every claim")
	if len(ac.calls) != 1 {
		t.Fatalf("SetSoul not invoked: %v", ac.calls)
	}
	if !strings.Contains(ac.calls[0], "thorough web researcher") {
		t.Errorf("soul body not joined: %q", ac.calls[0])
	}
	// Usage hint on missing args.
	ac.calls = nil
	m.handleSlash("/agents soul")
	if len(ac.calls) != 0 {
		t.Errorf("SetSoul called with no args: %v", ac.calls)
	}
}

// /agents show <name> posts an assistant message with the formatted detail.
func TestAgents_Show(t *testing.T) {
	ac := &fakeAgentCtl{
		showFn: func(name string) (AgentDetail, error) {
			return AgentDetail{
				Name: name, Provider: "zai", Model: "glm-4.6",
				Description: "web research", Soul: "Be thorough. Cite everything.",
			}, nil
		},
	}
	m := newModel(context.Background(), Config{Agents: ac})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/agents show researcher")
	last := m.msgs[len(m.msgs)-1]
	if last.role != "assistant" {
		t.Errorf("show output should be assistant msg, got role=%q", last.role)
	}
	for _, want := range []string{"researcher", "zai/glm-4.6", "web research", "--- soul ---", "Be thorough"} {
		if !strings.Contains(last.content, want) {
			t.Errorf("show missing %q:\n%s", want, last.content)
		}
	}
}

// /agents remove <name> routes + acknowledges.
func TestAgents_Remove(t *testing.T) {
	ac := &fakeAgentCtl{}
	m := newModel(context.Background(), Config{Agents: ac})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/agents remove researcher")
	if len(ac.calls) != 1 || ac.calls[0] != "Remove:researcher" {
		t.Errorf("Remove not routed: %v", ac.calls)
	}
}

// /agents rename <old> <new> routes correctly + ack message.
func TestAgents_Rename(t *testing.T) {
	ac := &fakeAgentCtl{}
	m := newModel(context.Background(), Config{Agents: ac})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/agents rename researcher deep-researcher")
	if len(ac.calls) != 1 || ac.calls[0] != "Rename:researcher/deep-researcher" {
		t.Fatalf("Rename not routed: %v", ac.calls)
	}
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "researcher") || !strings.Contains(last, "deep-researcher") {
		t.Errorf("ack missing names: %q", last)
	}
}

// /agents rename with missing args → usage hint, no call.
func TestAgents_Rename_Usage(t *testing.T) {
	ac := &fakeAgentCtl{}
	m := newModel(context.Background(), Config{Agents: ac})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/agents rename researcher") // missing new name
	if len(ac.calls) != 0 {
		t.Errorf("Rename should not call with missing arg: %v", ac.calls)
	}
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "usage:") || !strings.Contains(last, "<new>") {
		t.Errorf("usage hint missing: %q", last)
	}
}

// /agents model <name> <provider> [model] routes correctly.
func TestAgents_SetModel_BasicAndOverride(t *testing.T) {
	ac := &fakeAgentCtl{}
	m := newModel(context.Background(), Config{Agents: ac})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	// 2-arg form: provider only.
	m.handleSlash("/agents model researcher zai")
	if len(ac.calls) != 1 || ac.calls[0] != "SetModel:researcher/zai/" {
		t.Errorf("2-arg SetModel not routed: %v", ac.calls)
	}

	// 3-arg form with model override.
	ac.calls = nil
	m.handleSlash("/agents model researcher zai glm-4.5-air")
	if len(ac.calls) != 1 || ac.calls[0] != "SetModel:researcher/zai/glm-4.5-air" {
		t.Errorf("3-arg SetModel not routed: %v", ac.calls)
	}

	// Missing args → usage hint.
	ac.calls = nil
	m.handleSlash("/agents model researcher") // missing provider
	if len(ac.calls) != 0 {
		t.Errorf("should not call with missing args: %v", ac.calls)
	}
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "usage:") {
		t.Errorf("usage hint missing: %q", last)
	}
}

// `/agents model` (bare) and `/agents model <name>` open the provider→model
// picker; `/agents model <name> <provider>` stays the direct path (TEN-139
// follow-up). The picker only engages when BOTH controls exist.
func TestAgents_ModelPicker_Routing(t *testing.T) {
	ac := &fakeAgentCtl{rows: []AgentInfo{{Name: "Programmer", Provider: "", Model: ""}}}
	mc := &fakeModelCtl{infos: []ModelInfo{{Name: "zai", Model: "glm-4.6"}, {Name: "dgx", Model: "qwen"}}}
	m := newModel(context.Background(), Config{Agents: ac, Models: mc})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	// `/agents model <name>` → picker (non-nil cmd), NOT a direct SetModel.
	cmd := m.handleSlash("/agents model Programmer")
	if cmd == nil {
		t.Fatal("`/agents model <name>` should launch a picker (non-nil cmd)")
	}
	if len(ac.calls) != 0 {
		t.Errorf("picker form must not call SetModel directly: %v", ac.calls)
	}

	// bare `/agents model` → agent picker (non-nil cmd).
	ac.calls = nil
	if cmd := m.handleSlash("/agents model"); cmd == nil {
		t.Fatal("bare `/agents model` should launch an agent picker (non-nil cmd)")
	}

	// `/agents model <name> <provider>` → direct path, real SetModel call.
	ac.calls = nil
	m.handleSlash("/agents model Programmer zai")
	if len(ac.calls) != 1 || ac.calls[0] != "SetModel:Programmer/zai/" {
		t.Errorf("direct form should call SetModel once: %v", ac.calls)
	}

	// With no Models control, the picker is unavailable → falls through to the
	// text handler's usage hint (no panic, no picker).
	ac2 := &fakeAgentCtl{}
	m2 := newModel(context.Background(), Config{Agents: ac2})
	m2.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	if cmd := m2.handleSlash("/agents model Programmer"); cmd != nil {
		t.Error("no Models control → no picker cmd")
	}
}

// SetModel backend errors surface (not silently swallowed).
func TestAgents_SetModel_ErrorSurfaced(t *testing.T) {
	ac := &fakeAgentCtl{
		modelFn: func(name, prov, model string) (string, error) {
			return "", fmt.Errorf("no agent named %q", name)
		},
	}
	m := newModel(context.Background(), Config{Agents: ac})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/agents model ghost zai")
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "no agent named") {
		t.Errorf("error not shown: %q", last)
	}
}

// Backend errors surface (not silently swallowed).
func TestAgents_Add_ErrorSurfaced(t *testing.T) {
	ac := &fakeAgentCtl{
		addFn: func(name, p, m, d, s string) (string, error) {
			return "", fmt.Errorf("provider %q not configured", p)
		},
	}
	m := newModel(context.Background(), Config{Agents: ac})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/agents add x nope")
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "not configured") {
		t.Errorf("error not shown: %q", last)
	}
}

// Without Agents control → useful message.
func TestAgents_NoControl(t *testing.T) {
	m := newModel(context.Background(), Config{})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.handleSlash("/agents")
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "not available") {
		t.Errorf("missing 'not available' msg: %q", last)
	}
}

// --- /goal — autonomous loop ---

type fakeGoalCtl struct {
	active      bool
	loopCeiling int
	setFn       func(condition string) (firstPrompt, status string, err error)
	judgeFn     func(lastResponse string) (met bool, reason string, atCap bool, err error)
	continFn    func(reason string) string
	showFn      func() GoalStatus
	clearFn     func() string
	calls       []string
}

func (f *fakeGoalCtl) LoopCeiling() int { return f.loopCeiling }

func (f *fakeGoalCtl) Set(_ context.Context, c string) (string, string, error) {
	f.calls = append(f.calls, "Set:"+c)
	if f.setFn != nil {
		return f.setFn(c)
	}
	f.active = true
	return "[Goal] " + c, "🎯 goal set: " + c, nil
}
func (f *fakeGoalCtl) Judge(_ context.Context, last string) (bool, string, bool, error) {
	f.calls = append(f.calls, "Judge:"+last)
	if f.judgeFn != nil {
		return f.judgeFn(last)
	}
	return true, "", false, nil
}
func (f *fakeGoalCtl) Continue(reason string) string {
	f.calls = append(f.calls, "Continue:"+reason)
	if f.continFn != nil {
		return f.continFn(reason)
	}
	return "[Goal continue] " + reason
}
func (f *fakeGoalCtl) Show() GoalStatus {
	f.calls = append(f.calls, "Show")
	if f.showFn != nil {
		return f.showFn()
	}
	if !f.active {
		return GoalStatus{}
	}
	return GoalStatus{Active: true, Condition: "fake-cond", Turns: 0, MaxTurns: 20}
}
func (f *fakeGoalCtl) Active() bool { return f.active }
func (f *fakeGoalCtl) Clear() string {
	f.calls = append(f.calls, "Clear")
	f.active = false
	if f.clearFn != nil {
		return f.clearFn()
	}
	return "goal cleared"
}

// /goal with no args + no active goal → usage hint.
func TestGoal_BareUsage(t *testing.T) {
	gc := &fakeGoalCtl{}
	m := newModel(context.Background(), Config{Goals: gc})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/goal")
	last := m.msgs[len(m.msgs)-1].content
	for _, want := range []string{"usage:", "/goal <condition>", "/goal show", "/goal clear"} {
		if !strings.Contains(last, want) {
			t.Errorf("usage hint missing %q:\n%s", want, last)
		}
	}
}

// /goal show with active goal → status block.
func TestGoal_Show(t *testing.T) {
	gc := &fakeGoalCtl{
		showFn: func() GoalStatus {
			return GoalStatus{
				Active: true, Condition: "write a test", Turns: 3, MaxTurns: 20,
				LastJudge: "missing assertion", ElapsedFmt: "45s",
			}
		},
	}
	m := newModel(context.Background(), Config{Goals: gc})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/goal show")
	last := m.msgs[len(m.msgs)-1].content
	for _, want := range []string{"🎯 goal: write a test", "3 / 20", "45s", "missing assertion"} {
		if !strings.Contains(last, want) {
			t.Errorf("status missing %q:\n%s", want, last)
		}
	}
}

// /goal clear → forwards to Clear, posts the returned message.
func TestGoal_Clear(t *testing.T) {
	gc := &fakeGoalCtl{
		clearFn: func() string { return "goal cleared: write a test" },
	}
	m := newModel(context.Background(), Config{Goals: gc})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/goal clear")
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "goal cleared") {
		t.Errorf("Clear ack missing: %q", last)
	}
	// Aliases also work.
	for _, alias := range []string{"stop", "off", "reset", "cancel"} {
		gc.calls = nil
		m.handleSlash("/goal " + alias)
		found := false
		for _, c := range gc.calls {
			if c == "Clear" {
				found = true
			}
		}
		if !found {
			t.Errorf("alias /goal %s didn't route to Clear: %v", alias, gc.calls)
		}
	}
}

// /goal <condition> → Set + submits the firstPrompt as the next turn.
func TestGoal_Set_KicksOffFirstTurn(t *testing.T) {
	gc := &fakeGoalCtl{
		setFn: func(c string) (string, string, error) {
			return "[Goal — autonomous] " + c, "🎯 goal set: " + c, nil
		},
	}
	m := newModel(context.Background(), Config{Goals: gc, Agent: nil}) // Agent nil = cmd nil from submit; OK for state check
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	cmd := m.handleSlash("/goal write a unit test for the new feature")
	// Set was called with the right condition.
	if len(gc.calls) < 1 || gc.calls[0] != "Set:write a unit test for the new feature" {
		t.Fatalf("Set not routed correctly: %v", gc.calls)
	}
	// The goal triggered submit() — m.busy should be true (a real Agent
	// would have generated a tea.Cmd; with Agent=nil submit early-returns
	// so cmd is nil. We mainly care that Set fired + status posted.
	_ = cmd
	// Status message posted.
	last := ""
	for _, msg := range m.msgs {
		if strings.Contains(msg.content, "🎯 goal set") {
			last = msg.content
		}
	}
	if last == "" {
		t.Errorf("status not posted: %v", m.msgs)
	}
}

// /goal <condition> while busy → refuse politely (no Set call).
func TestGoal_Set_RefusesWhileBusy(t *testing.T) {
	gc := &fakeGoalCtl{}
	m := newModel(context.Background(), Config{Goals: gc})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.busy = true
	m.handleSlash("/goal write a test")
	for _, c := range gc.calls {
		if strings.HasPrefix(c, "Set:") {
			t.Errorf("Set called while busy: %v", gc.calls)
		}
	}
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "busy") {
		t.Errorf("busy hint missing: %q", last)
	}
}

// No GoalControl → useful "not available" message.
func TestGoal_NoControl(t *testing.T) {
	m := newModel(context.Background(), Config{})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.handleSlash("/goal write a test")
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "not available") {
		t.Errorf("missing 'not available': %q", last)
	}
}

// isGoalSub catalog must stay aligned with the dispatcher switch.
func TestIsGoalSub(t *testing.T) {
	for _, sub := range []string{"show", "status", "clear", "stop", "off", "reset", "cancel"} {
		if !isGoalSub(sub) {
			t.Errorf("isGoalSub(%q) should be true", sub)
		}
	}
	for _, notsub := range []string{"", "write", "set", "go"} {
		if isGoalSub(notsub) {
			t.Errorf("isGoalSub(%q) should be false", notsub)
		}
	}
}

// renderGoalStatus formats the snapshot. Inactive → hint to set one;
// active → multi-line block with condition + turns + judge.
func TestRenderGoalStatus(t *testing.T) {
	// Inactive.
	if got := renderGoalStatus(GoalStatus{}); !strings.Contains(got, "no goal") {
		t.Errorf("inactive: %q", got)
	}
	// Active without a judge eval yet.
	got := renderGoalStatus(GoalStatus{
		Active: true, Condition: "write a test", Turns: 0, MaxTurns: 20,
		ElapsedFmt: "3s",
	})
	for _, want := range []string{"write a test", "0 / 20", "3s", "(no evaluation yet)"} {
		if !strings.Contains(got, want) {
			t.Errorf("active-no-judge missing %q:\n%s", want, got)
		}
	}
	// Active with judge verdict.
	got = renderGoalStatus(GoalStatus{
		Active: true, Condition: "x", Turns: 5, MaxTurns: 20,
		LastJudge: "missing imports", ElapsedFmt: "1m",
	})
	if !strings.Contains(got, "missing imports") {
		t.Errorf("judge not shown: %q", got)
	}
}

// isResearchSub catalog must stay aligned with the dispatcher switch.
func TestIsResearchSub(t *testing.T) {
	for _, sub := range []string{"history", "list", "show", "replay", "delete", "rm"} {
		if !isResearchSub(sub) {
			t.Errorf("isResearchSub(%q) should be true", sub)
		}
	}
	for _, notsub := range []string{"", "nuke", "what", "search"} {
		if isResearchSub(notsub) {
			t.Errorf("isResearchSub(%q) should be false", notsub)
		}
	}
}

// --- C2: clarify state machine ---

// fakeClarifyError is the bridge type a fake control returns when it wants
// the TUI to enter the clarify-pending state. Satisfies ResearchClarifyError.
type fakeClarifyError struct {
	original string
	qs       []string
}

func (e *fakeClarifyError) Error() string              { return "clarify needed" }
func (e *fakeClarifyError) ClarifyQuestions() []string { return e.qs }
func (e *fakeClarifyError) ClarifyOriginal() string    { return e.original }

// When Research returns a ClarifyNeededError, the TUI must:
//   - Display the questions in chat (so the user sees them)
//   - Set m.pendingClarify so the NEXT plain message becomes the answer
//   - NOT be busy anymore (the research call returned)
//   - NOT post an "error" — it's a pause, not a failure
func TestResearch_ClarifyPausesAndPrompts(t *testing.T) {
	rc := &fakeResearchCtl{
		researchFn: func(q string) (string, error) {
			return "", &fakeClarifyError{
				original: q,
				qs:       []string{"What angle — price or strategy?", "Which timeframe?"},
			}
		},
	}
	m := newModel(context.Background(), Config{Research: rc})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	cmd := m.handleSlash("/research nvidia stock")
	if cmd == nil {
		t.Fatal("/research <q> should return a tea.Cmd (the async runner)")
	}
	// Drain the cmd → it returns the researchDoneMsg. Feed it back via Update.
	msg := cmd()
	m.Update(msg)
	if m.busy {
		t.Error("busy should be false after clarify (it's a pause, not running)")
	}
	if m.pendingClarify == nil {
		t.Fatal("pendingClarify not set")
	}
	if m.pendingClarify.Original != "nvidia stock" {
		t.Errorf("Original lost: %q", m.pendingClarify.Original)
	}
	if len(m.pendingClarify.Questions) != 2 {
		t.Errorf("want 2 questions, got %v", m.pendingClarify.Questions)
	}
	// The assistant message should include the questions verbatim.
	last := m.msgs[len(m.msgs)-1]
	if last.role != "assistant" {
		t.Errorf("clarify prompt should land as assistant msg, got role=%q", last.role)
	}
	if !strings.Contains(last.content, "What angle — price or strategy?") {
		t.Errorf("question 1 missing from clarify prompt: %q", last.content)
	}
	if !strings.Contains(last.content, "Which timeframe?") {
		t.Errorf("question 2 missing: %q", last.content)
	}
	if !strings.Contains(last.content, "/cancel-clarify") {
		t.Errorf("escape hatch (cancel-clarify) missing from prompt: %q", last.content)
	}
}

// After the clarify pause, the NEXT plain user message must:
//   - Be folded into the original question (enriched query)
//   - Trigger ResearchAfterClarify (NOT Research — that would re-clarify)
//   - Clear m.pendingClarify
//   - Set busy=true while the async runs
func TestResearch_ClarifyAnswerResumesResearch(t *testing.T) {
	rc := &fakeResearchCtl{
		researchFn: func(q string) (string, error) {
			return "", &fakeClarifyError{original: q, qs: []string{"What angle?"}}
		},
	}
	m := newModel(context.Background(), Config{Research: rc})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	// Pass 1: kick off /research, drain clarify, land in pending state.
	cmd := m.handleSlash("/research nvidia stock")
	msg := cmd()
	m.Update(msg)
	if m.pendingClarify == nil {
		t.Fatal("setup: expected pendingClarify after first pass")
	}

	// Pass 2: user types the answer (plain message, no slash).
	m.ta.SetValue("latest closing price and Q3 earnings")
	cmd2 := m.submit()
	if cmd2 == nil {
		t.Fatal("answer submit should return a tea.Cmd")
	}
	if m.pendingClarify != nil {
		t.Error("pendingClarify should clear once answered")
	}
	if !m.busy {
		t.Error("busy should be true while research-after-clarify runs")
	}
	// Drain the cmd to trigger ResearchAfterClarify.
	cmd2()
	found := false
	for _, c := range rc.calls {
		if strings.HasPrefix(c, "ResearchAfterClarify:") {
			found = true
			// Must contain BOTH the original question AND the answer.
			if !strings.Contains(c, "nvidia stock") {
				t.Errorf("original lost in enrichment: %q", c)
			}
			if !strings.Contains(c, "latest closing price") {
				t.Errorf("answer lost in enrichment: %q", c)
			}
		}
	}
	if !found {
		t.Errorf("ResearchAfterClarify never called: %v", rc.calls)
	}
}

// /cancel-clarify drops the pending state without answering. The next plain
// message goes through as a normal chat turn.
func TestResearch_CancelClarify(t *testing.T) {
	rc := &fakeResearchCtl{
		researchFn: func(q string) (string, error) {
			return "", &fakeClarifyError{original: q, qs: []string{"What angle?"}}
		},
	}
	m := newModel(context.Background(), Config{Research: rc})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	// Enter pending state.
	cmd := m.handleSlash("/research nvidia stock")
	m.Update(cmd())
	if m.pendingClarify == nil {
		t.Fatal("setup: expected pendingClarify")
	}

	// Cancel.
	m.handleSlash("/cancel-clarify")
	if m.pendingClarify != nil {
		t.Error("pendingClarify should be nil after /cancel-clarify")
	}
	// Bare /cancel-clarify with nothing pending gives a useful message.
	m.handleSlash("/cancel-clarify")
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "no clarification") {
		t.Errorf("missing 'no clarification' message: %q", last)
	}
}

// /research! <q> (shortcut) skips the clarifier path on the OUTBOUND call —
// the TUI dispatches via ResearchAfterClarify with the bare question.
func TestResearch_BangSkipsClarify(t *testing.T) {
	rc := &fakeResearchCtl{}
	m := newModel(context.Background(), Config{Research: rc})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	cmd := m.handleSlash("/research! nvidia stock")
	if cmd == nil {
		t.Fatal("/research! should return a cmd")
	}
	cmd()
	if len(rc.calls) != 1 || !strings.HasPrefix(rc.calls[0], "ResearchAfterClarify:") {
		t.Errorf("/research! should go to ResearchAfterClarify directly: %v", rc.calls)
	}
	if !strings.Contains(rc.calls[0], "nvidia stock") {
		t.Errorf("question lost: %v", rc.calls)
	}
}

// /research <new q> while a clarification is pending drops the pending state
// and starts the new query — operator changed their mind.
func TestResearch_NewQueryDropsPendingClarify(t *testing.T) {
	rc := &fakeResearchCtl{
		researchFn: func(q string) (string, error) {
			if q == "nvidia stock" {
				return "", &fakeClarifyError{original: q, qs: []string{"What angle?"}}
			}
			return "report for " + q, nil
		},
	}
	m := newModel(context.Background(), Config{Research: rc})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.Update(m.handleSlash("/research nvidia stock")())
	if m.pendingClarify == nil {
		t.Fatal("setup: pendingClarify expected")
	}
	cmd := m.handleSlash("/research what is graphiti")
	if cmd == nil {
		t.Fatal("new /research while pending should still dispatch")
	}
	if m.pendingClarify != nil {
		t.Error("starting a new /research should drop the prior pending state")
	}
}

// While clarify is pending, /research history (and other sub-commands) still
// work — pending state only intercepts PLAIN messages, not slash commands.
func TestResearch_PendingClarifyDoesNotBlockSubcommands(t *testing.T) {
	rc := &fakeResearchCtl{
		researchFn: func(q string) (string, error) {
			return "", &fakeClarifyError{original: q, qs: []string{"?"}}
		},
	}
	m := newModel(context.Background(), Config{Research: rc})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.Update(m.handleSlash("/research nvidia stock")())
	if m.pendingClarify == nil {
		t.Fatal("setup: pendingClarify expected")
	}
	// Sub-command should still work. The dispatcher routes by command name.
	m.handleSlash("/research history")
	historyCalled := false
	for _, c := range rc.calls {
		if strings.HasPrefix(c, "History:") {
			historyCalled = true
		}
	}
	if !historyCalled {
		t.Errorf("/research history should work even while clarify pending: %v", rc.calls)
	}
}

// --- /review — GStack Layer 3 cascading review ---

type fakeReviewCtl struct {
	calls  []string
	report string
	err    error
}

func (f *fakeReviewCtl) Review(_ context.Context, planPath string, roles []string) (string, error) {
	f.calls = append(f.calls, "Review:"+planPath+"|"+strings.Join(roles, ","))
	return f.report, f.err
}

// /review with no args → usage hint listing both forms.
func TestReview_BareUsage(t *testing.T) {
	rc := &fakeReviewCtl{}
	m := newModel(context.Background(), Config{Review: rc})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/review")
	last := m.msgs[len(m.msgs)-1].content
	for _, want := range []string{"usage:", "/review <plan.md>", "subset"} {
		if !strings.Contains(last, want) {
			t.Errorf("usage hint missing %q:\n%s", want, last)
		}
	}
	if len(rc.calls) != 0 {
		t.Errorf("bare /review should not call Review; got %v", rc.calls)
	}
}

// /review with a path → calls Review with empty roles + posts the report.
func TestReview_PathOnly_RunsAllReviewers(t *testing.T) {
	rc := &fakeReviewCtl{report: "## GSTACK REVIEW REPORT\n\n### CEO Review\nok"}
	m := newModel(context.Background(), Config{Review: rc})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/review plan.md")
	if len(rc.calls) != 1 || rc.calls[0] != "Review:plan.md|" {
		t.Errorf("expected single Review:plan.md| call, got %v", rc.calls)
	}
	// Last system-chat message must contain the report body so the
	// operator sees it inline (not just in the file).
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "GSTACK REVIEW REPORT") {
		t.Errorf("report not echoed to chat:\n%s", last)
	}
}

// /review with subset CSV → roles are parsed and forwarded in order.
func TestReview_SubsetCSV(t *testing.T) {
	rc := &fakeReviewCtl{report: "done"}
	m := newModel(context.Background(), Config{Review: rc})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/review plan.md ceo,eng")
	if len(rc.calls) != 1 {
		t.Fatalf("expected 1 Review call, got %v", rc.calls)
	}
	if rc.calls[0] != "Review:plan.md|ceo,eng" {
		t.Errorf("subset CSV not forwarded correctly: %q", rc.calls[0])
	}
}

// /review error → message surfaces, no crash.
func TestReview_ErrorSurfaces(t *testing.T) {
	rc := &fakeReviewCtl{err: errors.New("plan file is empty")}
	m := newModel(context.Background(), Config{Review: rc})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/review empty.md")
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "plan file is empty") {
		t.Errorf("error not surfaced: %s", last)
	}
}

// /review without a wired control → friendly "not available" message,
// never panic on nil Control.
func TestReview_NoControlIsFriendly(t *testing.T) {
	m := newModel(context.Background(), Config{})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/review plan.md")
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "not available") {
		t.Errorf("no-control message missing: %s", last)
	}
}

// Help index must list the new review category once /review ships.
// Drift guard so a future help refactor doesn't silently drop it.
func TestHelpIndex_IncludesReviewCategory(t *testing.T) {
	plain, _ := renderHelpIndex()
	if !strings.Contains(plain, "review") {
		t.Errorf("help index missing review category:\n%s", plain)
	}
}

// --- listPicker (arrow-key model variant selector) ---

func newPickerModel(t *testing.T, p *listPicker) *model {
	t.Helper()
	m := newModel(context.Background(), Config{})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.picker = p
	return m
}

// TestListPicker_DownUpNavigation — up/down arrow + vim j/k must
// move the selection with wrap-around. Wrap matters because operators
// land on a long catalog (Z.ai serves 7+ variants) and shouldn't get
// stuck on either edge.
func TestListPicker_DownUpNavigation(t *testing.T) {
	p := &listPicker{
		title: "x",
		items: []string{"a", "b", "c"},
	}
	m := newPickerModel(t, p)

	m.handlePickerKey(tea.KeyMsg{Type: tea.KeyDown})
	if p.selected != 1 {
		t.Errorf("down → selected=%d want 1", p.selected)
	}
	m.handlePickerKey(tea.KeyMsg{Type: tea.KeyDown})
	m.handlePickerKey(tea.KeyMsg{Type: tea.KeyDown}) // wrap
	if p.selected != 0 {
		t.Errorf("down wrap → selected=%d want 0", p.selected)
	}
	m.handlePickerKey(tea.KeyMsg{Type: tea.KeyUp}) // wrap up
	if p.selected != 2 {
		t.Errorf("up wrap → selected=%d want 2", p.selected)
	}
	// vim-style j/k
	m.handlePickerKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if p.selected != 0 {
		t.Errorf("j (down wrap) → selected=%d want 0", p.selected)
	}
	m.handlePickerKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if p.selected != 2 {
		t.Errorf("k (up wrap) → selected=%d want 2", p.selected)
	}
}

// TestListPicker_EnterCallsOnSelect — Enter fires onSelect with the
// currently-highlighted item and clears the picker. The cmd returned
// is the tea.Cmd from onSelect (passed back through Update).
func TestListPicker_EnterCallsOnSelect(t *testing.T) {
	var chose string
	p := &listPicker{
		title:    "x",
		items:    []string{"glm-4.6", "glm-5.1", "glm-5-turbo"},
		selected: 1, // pre-select glm-5.1
		onSelect: func(choice string) tea.Cmd {
			chose = choice
			return func() tea.Msg { return sysChatMsg{text: "picked " + choice} }
		},
	}
	m := newPickerModel(t, p)
	cmd := m.handlePickerKey(tea.KeyMsg{Type: tea.KeyEnter})
	if chose != "glm-5.1" {
		t.Errorf("onSelect should fire with selected item; got %q", chose)
	}
	if m.picker != nil {
		t.Error("picker should be cleared after Enter")
	}
	if cmd == nil {
		t.Error("Enter should return the cmd from onSelect")
	}
}

// TestListPicker_EscCancels — Esc clears the picker without calling
// onSelect. onCancel (if set) fires instead.
func TestListPicker_EscCancels(t *testing.T) {
	cancelFired := false
	p := &listPicker{
		title:    "x",
		items:    []string{"a"},
		onSelect: func(string) tea.Cmd { t.Error("onSelect must NOT fire on Esc"); return nil },
		onCancel: func() tea.Cmd {
			cancelFired = true
			return nil
		},
	}
	m := newPickerModel(t, p)
	m.handlePickerKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.picker != nil {
		t.Error("picker should be cleared after Esc")
	}
	if !cancelFired {
		t.Error("onCancel should have fired on Esc")
	}
}

// TestListPicker_KeysRoutedOnlyWhenActive — drift guard. When picker
// is nil, /commands and chat input must work normally; the handler
// shouldn't accidentally intercept anything.
func TestListPicker_KeysRoutedOnlyWhenActive(t *testing.T) {
	m := newModel(context.Background(), Config{})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	// No picker → handlePickerKey returns nil cmd, leaves picker nil
	// (and doesn't panic on the nil picker check).
	cmd := m.handlePickerKey(tea.KeyMsg{Type: tea.KeyDown})
	if cmd != nil {
		t.Error("handlePickerKey with nil picker should be a no-op")
	}
	if m.picker != nil {
		t.Error("nil picker should stay nil")
	}
}

// TestPickerStartMsg_ActivatesPicker — sending a pickerStartMsg via
// Update sets m.picker, so the very NEXT keypress routes to the
// picker handler. This is the wire that connects /model add-cloud's
// tea.Cmd to the picker UI.
func TestPickerStartMsg_ActivatesPicker(t *testing.T) {
	m := newModel(context.Background(), Config{})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	p := &listPicker{title: "x", items: []string{"a", "b"}}
	m.Update(pickerStartMsg{picker: p})
	if m.picker != p {
		t.Error("pickerStartMsg should set m.picker to the dispatched picker")
	}
}

// TestWhoami_PrintsRuntimeTruth — /whoami answers from runtime config
// directly, no LLM call. The output MUST contain the agent ID, backend,
// and model from Config — that's the whole point: trusted source of
// truth when the operator wants to know what's actually running.
func TestWhoami_PrintsRuntimeTruth(t *testing.T) {
	m := newModel(context.Background(), Config{
		AgentID: "main",
		Backend: "vllm",
		Model:   "glm-5.1",
	})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/whoami")
	last := m.msgs[len(m.msgs)-1].content
	for _, want := range []string{"main", "vllm", "glm-5.1"} {
		if !strings.Contains(last, want) {
			t.Errorf("/whoami output missing %q:\n%s", want, last)
		}
	}
	// Defense-in-depth: even if endpoint somehow got into Config (it
	// doesn't today), /whoami must NEVER render it — endpoints are a
	// secret-adjacent field and the chat scroll is Ctrl-Y-able.
	if strings.Contains(strings.ToLower(last), "endpoint") {
		t.Errorf("/whoami should NOT mention endpoint (secret-adjacent field): %s", last)
	}
}

// TestWhoami_Alias — /who is the short alias.
func TestWhoami_Alias(t *testing.T) {
	m := newModel(context.Background(), Config{AgentID: "main", Backend: "vllm", Model: "glm-4.6"})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/who")
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "glm-4.6") {
		t.Errorf("/who alias broken:\n%s", last)
	}
}

// TestRenderPicker_HighlightsSelected — visual contract: the selected
// row gets the "▶ " marker. Drift guard for the operator's ability
// to SEE which row Enter will pick.
func TestRenderPicker_HighlightsSelected(t *testing.T) {
	p := &listPicker{
		title:    "Pick model",
		items:    []string{"glm-4.6", "glm-5.1"},
		selected: 1,
	}
	m := newPickerModel(t, p)
	out := stripANSI(m.renderPicker())
	if !strings.Contains(out, "▶ glm-5.1") {
		t.Errorf("selected item should be marked with '▶ '; got:\n%s", out)
	}
	if strings.Contains(out, "▶ glm-4.6") {
		t.Errorf("non-selected item should NOT be marked; got:\n%s", out)
	}
	if !strings.Contains(out, "Pick model") {
		t.Errorf("picker should render its title; got:\n%s", out)
	}
}

// --- /imessage drive-allowlist ------------------------------------------

// fakeIMessage is a stub IMessageControl that records calls + lets a test seed
// the list and force errors.
type fakeIMessage struct {
	list        []string
	allowCalls  []string
	denyCalls   []string
	clearCalls  int
	allowErr    error
	responderOn bool
	setCalls    []bool // SetResponder(on) calls recorded
	perms       *fakePerms
}

func (f *fakeIMessage) ResponderOn() bool { return f.responderOn }
func (f *fakeIMessage) SetResponder(on bool) (string, error) {
	f.setCalls = append(f.setCalls, on)
	f.responderOn = on
	if on {
		return "imessage responder ON", nil
	}
	return "imessage responder OFF", nil
}
func (f *fakeIMessage) Perms() PermissionControl {
	if f.perms == nil {
		return nil
	}
	return f.perms
}

// fakePerms is a minimal PermissionControl for the /imessage permissions tests.
type fakePerms struct {
	modes    map[string]string
	setCalls []string // "cat=mode"
}

func (p *fakePerms) Permissions() []PermissionInfo {
	return []PermissionInfo{{Category: "exec", Mode: p.modes["exec"], Desc: "run commands"}}
}
func (p *fakePerms) SetPermission(cat, mode string) (bool, error) {
	if cat != "exec" && cat != "write" && cat != "destructive" && cat != "web" && cat != "send" {
		return false, nil
	}
	if p.modes == nil {
		p.modes = map[string]string{}
	}
	p.modes[cat] = mode
	p.setCalls = append(p.setCalls, cat+"="+mode)
	return true, nil
}

// TEN-234: the TUI skips cross-agent / bus events on its shared channel (they're
// mirrored there only for the dashboard); sub-agents render via TeamEvents.
func TestApplyEvent_SkipsCrossAgentAndBus(t *testing.T) {
	m := newModel(context.Background(), Config{})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	base := len(m.feedLines)
	m.applyEvent(agent.Event{Kind: agent.EventToolCall, Agent: "writer", Tool: "os_write"}) // sub-agent
	m.applyEvent(agent.Event{Kind: agent.EventBus, Agent: "researcher", Text: "→ writer: hi"})
	if len(m.feedLines) != base {
		t.Fatalf("cross-agent/bus events must not append to the TUI feed: +%d", len(m.feedLines)-base)
	}
	// A primary-agent event still renders.
	m.applyEvent(agent.Event{Kind: agent.EventTurnStart})
	if len(m.feedLines) != base+1 {
		t.Fatalf("a primary-agent event should append one feed line, got +%d", len(m.feedLines)-base)
	}
}

// fakeTailscale implements TailscaleControl for the /tailscale command tests (TEN-233).
type fakeTailscale struct {
	st         TailscaleStatus
	serveErr   error
	serveURL   string
	serveCalls int
	unCalls    int
}

func (f *fakeTailscale) Status() (TailscaleStatus, error) { return f.st, nil }
func (f *fakeTailscale) Serve() (string, error) {
	f.serveCalls++
	if f.serveErr != nil {
		return "", f.serveErr
	}
	return f.serveURL, nil
}
func (f *fakeTailscale) Unserve() error { f.unCalls++; return nil }

// fakeJudge implements JudgeControl for the /judge command tests (TEN-91).
type fakeJudge struct {
	cur      string
	setCalls []string // "kind|model|endpoint"
	clears   int
	setErr   error
}

func (f *fakeJudge) Current() string { return f.cur }
func (f *fakeJudge) Set(kind, model, endpoint string) (string, error) {
	f.setCalls = append(f.setCalls, kind+"|"+model+"|"+endpoint)
	if f.setErr != nil {
		return "", f.setErr
	}
	return "judge set → " + kind + " " + model, nil
}
func (f *fakeJudge) Clear() error { f.clears++; return nil }

// TEN-91: /judge chooses the eval judge model (status / set / clear / unavailable).
func TestSlash_Judge(t *testing.T) {
	fj := &fakeJudge{cur: "judge: default — planner self-judging"}
	m := newModel(context.Background(), Config{Judge: fj})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	m.handleSlash("/judge")
	if last := m.msgs[len(m.msgs)-1].content; !strings.Contains(last, "self-judging") {
		t.Errorf("bare /judge should show Current(); got:\n%s", last)
	}

	m.handleSlash("/judge set anthropic claude-opus-4-8")
	if len(fj.setCalls) != 1 || fj.setCalls[0] != "anthropic|claude-opus-4-8|" {
		t.Fatalf("/judge set should route kind+model: %v", fj.setCalls)
	}

	m.handleSlash("/judge set zai glm-4.6 https://api.z.ai/x")
	if fj.setCalls[len(fj.setCalls)-1] != "zai|glm-4.6|https://api.z.ai/x" {
		t.Fatalf("/judge set should pass the optional endpoint: %v", fj.setCalls)
	}

	m.handleSlash("/judge clear")
	if fj.clears != 1 {
		t.Fatalf("/judge clear should call Clear once: %d", fj.clears)
	}

	// set with too few args → usage, no Set call.
	before := len(fj.setCalls)
	m.handleSlash("/judge set anthropic")
	if len(fj.setCalls) != before {
		t.Error("/judge set with a missing model must not call Set")
	}

	// Unavailable (nil control) → graceful.
	m2 := newModel(context.Background(), Config{})
	m2.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m2.handleSlash("/judge")
	if last := m2.msgs[len(m2.msgs)-1].content; !strings.Contains(last, "not available") {
		t.Errorf("nil judge control should report unavailable, got %q", last)
	}
}

// TEN-233: /tailscale serve publishes the dashboard via tailscale serve; status
// guides the operator when the CLI is missing or disconnected.
func TestSlash_Tailscale(t *testing.T) {
	// serve → calls Serve, echoes the tailnet URL.
	ft := &fakeTailscale{st: TailscaleStatus{Installed: true, LoggedIn: true, DNSName: "host.ts.net", URL: "https://host.ts.net/"}, serveURL: "https://host.ts.net/"}
	m := newModel(context.Background(), Config{Tailscale: ft})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.handleSlash("/tailscale serve")
	if ft.serveCalls != 1 {
		t.Fatalf("/tailscale serve should call Serve once: %d", ft.serveCalls)
	}
	if last := m.msgs[len(m.msgs)-1].content; !strings.Contains(last, "https://host.ts.net/") {
		t.Errorf("serve should echo the tailnet URL; got:\n%s", last)
	}

	// serve off → Unserve.
	m.handleSlash("/tailscale serve off")
	if ft.unCalls != 1 {
		t.Fatalf("/tailscale serve off should call Unserve once: %d", ft.unCalls)
	}

	// status when not connected → guidance, no Serve call.
	ft2 := &fakeTailscale{st: TailscaleStatus{Installed: true, LoggedIn: false, State: "Stopped"}}
	m2 := newModel(context.Background(), Config{Tailscale: ft2})
	m2.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m2.handleSlash("/tailscale status")
	if last := m2.msgs[len(m2.msgs)-1].content; !strings.Contains(last, "not connected") {
		t.Errorf("status should guide when disconnected; got:\n%s", last)
	}

	// Unavailable (nil control) → graceful message.
	m3 := newModel(context.Background(), Config{})
	m3.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m3.handleSlash("/tailscale serve")
	if last := m3.msgs[len(m3.msgs)-1].content; !strings.Contains(last, "not available") {
		t.Errorf("nil tailscale control should report unavailable; got:\n%s", last)
	}
}

// fakeRelay implements RelayControl for the /relay command tests (TEN-231).
type fakeRelay struct {
	perms       *fakePerms
	operatorSet bool
}

func (f *fakeRelay) Enable() error              { return nil }
func (f *fakeRelay) Disable() error             { return nil }
func (f *fakeRelay) Status() (bool, bool, bool) { return false, f.operatorSet, false }
func (f *fakeRelay) SetOperator(string) error   { f.operatorSet = true; return nil }
func (f *fakeRelay) SetExec(bool) error         { return nil }
func (f *fakeRelay) Perms() PermissionControl {
	if f.perms == nil {
		return nil
	}
	return f.perms
}

// TEN-231: /relay permissions mirrors the global /permissions (per-category
// ask|allow|deny) for the Discord agent's tools; "ask" prompts a Discord button.
func TestSlash_RelayPermissions(t *testing.T) {
	fr := &fakeRelay{perms: &fakePerms{modes: map[string]string{"exec": "ask"}}}
	m := newModel(context.Background(), Config{Relay: fr})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	m.handleSlash("/relay permissions set exec allow")
	if len(fr.perms.setCalls) != 1 || fr.perms.setCalls[0] != "exec=allow" {
		t.Fatalf("set should route to SetPermission: %v", fr.perms.setCalls)
	}
	m.handleSlash("/relay permissions")
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "Discord relay permissions") {
		t.Errorf("bare /relay permissions should render the table; got:\n%s", last)
	}

	// Unconfigured (nil perms) → graceful "unavailable", no panic.
	m2 := newModel(context.Background(), Config{Relay: &fakeRelay{}})
	m2.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m2.handleSlash("/relay permissions")
	if last := m2.msgs[len(m2.msgs)-1].content; !strings.Contains(last, "unavailable") {
		t.Errorf("nil perms should report unavailable, got %q", last)
	}
}

func (f *fakeIMessage) AllowList() []string { return f.list }
func (f *fakeIMessage) Allow(h string) (string, bool, error) {
	f.allowCalls = append(f.allowCalls, h)
	if f.allowErr != nil {
		return "", false, f.allowErr
	}
	norm := strings.ToLower(strings.TrimSpace(h))
	f.list = append(f.list, norm)
	return norm, true, nil
}
func (f *fakeIMessage) Deny(h string) (string, bool, error) {
	f.denyCalls = append(f.denyCalls, h)
	return strings.ToLower(strings.TrimSpace(h)), true, nil
}
func (f *fakeIMessage) Clear() (int, error) {
	f.clearCalls++
	n := len(f.list)
	f.list = nil
	return n, nil
}

// Bare /imessage (and /imessage list) renders the allowlist; an EMPTY list must
// show the deny-by-default notice so the operator is never misled.
func TestSlash_IMessageListEmptyShowsDenyByDefault(t *testing.T) {
	fi := &fakeIMessage{}
	m := newModel(context.Background(), Config{IMessage: fi})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	m.handleSlash("/imessage")
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(strings.ToLower(last), "deny-by-default") || !strings.Contains(last, "empty") {
		t.Errorf("empty allowlist must render a deny-by-default notice; got:\n%s", last)
	}
}

// TEN-230 Phase 1c: /imessage on|off drives the live responder toggle.
func TestSlash_IMessageResponderToggle(t *testing.T) {
	fi := &fakeIMessage{}
	m := newModel(context.Background(), Config{IMessage: fi})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	m.handleSlash("/imessage on")
	if len(fi.setCalls) != 1 || fi.setCalls[0] != true || !fi.responderOn {
		t.Fatalf("/imessage on should turn the responder on: calls=%v on=%v", fi.setCalls, fi.responderOn)
	}
	m.handleSlash("/imessage off")
	if len(fi.setCalls) != 2 || fi.setCalls[1] != false || fi.responderOn {
		t.Fatalf("/imessage off should turn it off: calls=%v on=%v", fi.setCalls, fi.responderOn)
	}
	// The list view reflects responder state.
	fi.responderOn = true
	m.handleSlash("/imessage list")
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(strings.ToLower(last), "responder: on") {
		t.Errorf("list should show responder ON state; got:\n%s", last)
	}
}

// TEN-230: /imessage permissions mirrors the global /permissions (per-category
// ask|allow|deny), scoped to the responder.
func TestSlash_IMessagePermissions(t *testing.T) {
	fi := &fakeIMessage{perms: &fakePerms{modes: map[string]string{"exec": "deny"}}}
	m := newModel(context.Background(), Config{IMessage: fi})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	m.handleSlash("/imessage permissions set exec allow")
	if len(fi.perms.setCalls) != 1 || fi.perms.setCalls[0] != "exec=allow" {
		t.Fatalf("set should route to SetPermission: %v", fi.perms.setCalls)
	}
	m.handleSlash("/imessage permissions")
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "iMessage permissions") {
		t.Errorf("bare /imessage permissions should render the table; got:\n%s", last)
	}

	// Unavailable (nil perms) → graceful message, no panic.
	m2 := newModel(context.Background(), Config{IMessage: &fakeIMessage{}})
	m2.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m2.handleSlash("/imessage permissions")
	if last := m2.msgs[len(m2.msgs)-1].content; !strings.Contains(last, "unavailable") {
		t.Errorf("nil perms should report unavailable, got %q", last)
	}
}

// A populated list renders the handles.
func TestSlash_IMessageListShowsHandles(t *testing.T) {
	fi := &fakeIMessage{list: []string{"+15551234567", "boss@work.com"}}
	m := newModel(context.Background(), Config{IMessage: fi})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	m.handleSlash("/imessage list")
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "+15551234567") || !strings.Contains(last, "boss@work.com") {
		t.Errorf("list output should include both handles; got:\n%s", last)
	}
}

// allow / deny / clear route to the control with the raw handle argument.
func TestSlash_IMessageAllowDenyClear(t *testing.T) {
	fi := &fakeIMessage{}
	m := newModel(context.Background(), Config{IMessage: fi})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	m.handleSlash("/imessage allow boss@work.com")
	if len(fi.allowCalls) != 1 || fi.allowCalls[0] != "boss@work.com" {
		t.Fatalf("allow not routed; calls=%v", fi.allowCalls)
	}
	m.handleSlash("/imsg deny boss@work.com") // alias + remove synonym path
	if len(fi.denyCalls) != 1 || fi.denyCalls[0] != "boss@work.com" {
		t.Fatalf("deny not routed; calls=%v", fi.denyCalls)
	}
	m.handleSlash("/imessage clear")
	if fi.clearCalls != 1 {
		t.Fatalf("clear not routed; got %d", fi.clearCalls)
	}
}

// allow with no argument shows usage and does NOT call the control.
func TestSlash_IMessageAllowRequiresArg(t *testing.T) {
	fi := &fakeIMessage{}
	m := newModel(context.Background(), Config{IMessage: fi})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	m.handleSlash("/imessage allow")
	if len(fi.allowCalls) != 0 {
		t.Errorf("bare allow must not call the control; calls=%v", fi.allowCalls)
	}
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "usage") {
		t.Errorf("expected usage hint; got:\n%s", last)
	}
}

// A surfaced control error is shown rather than swallowed.
func TestSlash_IMessageAllowError(t *testing.T) {
	fi := &fakeIMessage{allowErr: errors.New("not a usable handle")}
	m := newModel(context.Background(), Config{IMessage: fi})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	m.handleSlash("/imessage allow garbage")
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "not a usable handle") {
		t.Errorf("control error should surface; got:\n%s", last)
	}
}

// Without an IMessageControl wired, the command degrades gracefully.
func TestSlash_IMessageUnavailable(t *testing.T) {
	m := newModel(context.Background(), Config{}) // no IMessage
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})

	m.handleSlash("/imessage list")
	last := m.msgs[len(m.msgs)-1].content
	if !strings.Contains(last, "not available") {
		t.Errorf("expected unavailable notice; got:\n%s", last)
	}
}

// TEN-217: when turnDoneMsg finalizes the answer BEFORE a trailing EventAssistant
// from the same turn is drained, the trailing event must not append a second
// copy. Reproduces the /goal inline double-post race.
func TestModel_NoDoublePostOnTrailingAssistantEvent(t *testing.T) {
	m := newModel(context.Background(), Config{})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	// Authoritative finish lands first (finalizes the bubble + seals)...
	m.Update(turnDoneMsg{res: &agent.TurnResult{Response: "the answer"}})
	// ...then the trailing live event for the SAME turn is drained.
	m.Update(eventMsg(agent.Event{Kind: agent.EventAssistant, Text: "the answer"}))

	got := countAssistantBubbles(m.msgs)
	if got != 1 {
		t.Errorf("expected exactly 1 assistant bubble, got %d: %+v", got, m.msgs)
	}
}

// TEN-217: a normal streamed turn still renders — events that arrive BEFORE
// turnDoneMsg (unsealed) build the bubble; only the post-finalize trailing event
// is ignored. Guards against the seal over-suppressing live streaming.
func TestModel_StreamingRendersThenSealsNoDouble(t *testing.T) {
	m := newModel(context.Background(), Config{})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	// Live tokens arrive while unsealed → build the streamed bubble.
	m.Update(eventMsg(agent.Event{Kind: agent.EventToken, Text: "hel"}))
	m.Update(eventMsg(agent.Event{Kind: agent.EventToken, Text: "lo"}))
	// turnDone reconciles to the authoritative text and seals.
	m.Update(turnDoneMsg{res: &agent.TurnResult{Response: "hello"}})
	// A trailing buffered EventAssistant for the same turn must be ignored.
	m.Update(eventMsg(agent.Event{Kind: agent.EventAssistant, Text: "hello"}))

	if got := countAssistantBubbles(m.msgs); got != 1 {
		t.Fatalf("expected exactly 1 assistant bubble, got %d: %+v", got, m.msgs)
	}
	last := m.msgs[len(m.msgs)-1]
	if last.role != "assistant" || last.content != "hello" {
		t.Errorf("final bubble = %q (role %q), want %q", last.content, last.role, "hello")
	}

	// A fresh submit unseals so the NEXT turn's live events render again.
	m.assistantSealed = true // simulate still-sealed from the prior turn
	m.ta.SetValue("next question")
	_ = m.submit()
	if m.assistantSealed {
		t.Error("submit() must clear the seal so the next turn can stream")
	}
}

func countAssistantBubbles(msgs []chatMsg) int {
	n := 0
	for _, msg := range msgs {
		if msg.role == "assistant" {
			n++
		}
	}
	return n
}
