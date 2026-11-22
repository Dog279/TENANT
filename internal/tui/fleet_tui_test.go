package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// fakeFleet records what /fleet asked for.
type fakeFleet struct {
	calls    []string
	perms    map[string]*fakeFleetPerms
	defaults *fakeFleetPerms
}

type fakeFleetPerms struct {
	modes map[string]string
	rules map[string]string
}

func (p *fakeFleetPerms) Permissions() []PermissionInfo {
	return []PermissionInfo{{Category: "exec", Mode: p.modes["exec"], Desc: "run commands"}}
}

func (p *fakeFleetPerms) SetPermission(cat, mode string) (bool, error) {
	if cat != "exec" {
		return false, nil
	}
	p.modes[cat] = mode
	return true, nil
}

func (p *fakeFleetPerms) Rules() []ToolRuleInfo {
	var out []ToolRuleInfo
	for pat, mode := range p.rules {
		out = append(out, ToolRuleInfo{Pattern: pat, Mode: mode, Kind: "command"})
	}
	return out
}

func (p *fakeFleetPerms) SetRule(pattern, mode string) error { p.rules[pattern] = mode; return nil }

func (p *fakeFleetPerms) RemoveRule(pattern string) (bool, error) {
	_, had := p.rules[pattern]
	delete(p.rules, pattern)
	return had, nil
}

func newFakeFleet() *fakeFleet {
	mk := func() *fakeFleetPerms {
		return &fakeFleetPerms{modes: map[string]string{"exec": "ask"}, rules: map[string]string{}}
	}
	return &fakeFleet{perms: map[string]*fakeFleetPerms{"web1": mk()}, defaults: mk()}
}

func (f *fakeFleet) note(parts ...string) string {
	f.calls = append(f.calls, strings.Join(parts, " "))
	return f.calls[len(f.calls)-1]
}

func (f *fakeFleet) Status() string { return f.note("status") }
func (f *fakeFleet) Show(name string) (string, error) {
	if name != "web1" {
		return "", errors.New("no machine " + name)
	}
	return f.note("show", name), nil
}
func (f *fakeFleet) Confirm(name, code string, set []string) (string, error) {
	return f.note(append([]string{"confirm", name, "code=" + code}, set...)...), nil
}
func (f *fakeFleet) Enroll(name string) (string, error) { return f.note("enroll", name), nil }
func (f *fakeFleet) Revoke(name, scheme string) (string, error) {
	return f.note("revoke", name, "scheme="+scheme), nil
}
func (f *fakeFleet) Perms(name string) (PermissionControl, error) {
	if p := f.perms[name]; p != nil {
		return p, nil
	}
	return nil, errors.New("no machine " + name)
}
func (f *fakeFleet) Defaults() ToolRuleControl { return f.defaults }
func (f *fakeFleet) Relay(args []string) (string, error) {
	return f.note(append([]string{"relay"}, args...)...), nil
}

func newFleetModel(f FleetControl) *model {
	m := newModel(context.Background(), Config{Fleet: f, Perms: &fakeFleetPerms{modes: map[string]string{"exec": "allow"}, rules: map[string]string{}}})
	m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	return m
}

// slash runs a line as the app would submit it itself, and delivers what
// the command answers.
func slash(m *model, line string) {
	if cmd := m.handleSlash(line); cmd != nil {
		if msg := cmd(); msg != nil {
			m.Update(msg)
		}
	}
}

// typeLine enters a line at the keyboard: the text, then Enter.
func typeLine(m *model, line string) {
	m.ta.SetValue(line)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	for _, msg := range drain(cmd) {
		if _, ok := msg.(sysChatMsg); ok {
			m.Update(msg)
		}
	}
}

// drain runs a command and whatever it batches, and returns the messages
// that come at once. A command that waits (the cursor's timer, a listener)
// is left waiting: it is not the answer to a typed line.
func drain(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	got := make(chan tea.Msg, 1)
	go func() { got <- cmd() }()
	var first tea.Msg
	select {
	case first = <-got:
	case <-time.After(300 * time.Millisecond):
		return nil
	}
	switch msg := first.(type) {
	case tea.BatchMsg:
		var out []tea.Msg
		for _, c := range msg {
			out = append(out, drain(c)...)
		}
		return out
	case nil:
		return nil
	default:
		return []tea.Msg{msg}
	}
}

// Looking is open to any line; changing the fleet takes a line typed at this
// keyboard. A line the app submits itself (a goal's next prompt is the one
// kind there is) confirms nothing, revokes nothing and opens no relay.
func TestSlashFleetChangesNeedTheKeyboard(t *testing.T) {
	f := newFakeFleet()
	m := newFleetModel(f)
	for _, line := range []string{
		"/fleet enroll web2", "/fleet confirm web1 123456", "/fleet confirm web1 123 456 exec=allow", "/fleet revoke web1 delete", "/fleet revoke web1 keep", "/fleet relay discord on",
	} {
		slash(m, line)
		if len(f.calls) != 0 {
			t.Fatalf("%q, not typed, reached the fleet: %v", line, f.calls)
		}
		if !strings.Contains(lastMsg(m), "type this command here yourself") {
			t.Fatalf("%q, not typed, was answered with %q", line, lastMsg(m))
		}
	}
	// What only looks is answered either way.
	for line, want := range map[string]string{
		"/fleet": "status", "/fleet show web1": "show web1", "/fleet confirm web1": "confirm web1 code=",
		"/fleet revoke web1": "revoke web1 scheme=", "/fleet relay": "relay", "/fleet relay discord": "relay discord",
	} {
		f.calls = nil
		slash(m, line)
		if len(f.calls) != 1 || f.calls[0] != want {
			t.Errorf("%q asked the fleet for %v, want %q", line, f.calls, want)
		}
	}

	// Typed, the same lines act, and the digits may carry a space.
	for line, want := range map[string]string{
		"/fleet confirm web1 123 456 exec=allow send=ask": "confirm web1 code=123456 exec=allow send=ask",
		"/fleet revoke web1 DELETE":                       "revoke web1 scheme=delete",
		"/fleet enroll web2":                              "enroll web2",
		"/fleet relay imessage on":                        "relay imessage on",
	} {
		f.calls = nil
		typeLine(m, line)
		if len(f.calls) != 1 || f.calls[0] != want {
			t.Errorf("%q, typed, asked the fleet for %v, want %q", line, f.calls, want)
		}
		if m.typed {
			t.Fatal("the keyboard mark outlived the line it was set for")
		}
	}
	for _, line := range []string{"/fleet bogus", "/fleet show", "/fleet enroll", "/fleet enroll a b", "/fleet confirm", "/fleet revoke", "/fleet revoke a b c", "/fleet relay a b c"} {
		f.calls = nil
		typeLine(m, line)
		if len(f.calls) != 0 || !strings.Contains(lastMsg(m), "usage: /fleet") {
			t.Errorf("%q: calls %v, answer %q", line, f.calls, lastMsg(m))
		}
	}
	none := newModel(context.Background(), Config{})
	none.handleSlash("/fleet")
	if !strings.Contains(lastMsg(none), "not available") {
		t.Errorf("/fleet with no fleet control: %q", lastMsg(none))
	}
	none.handleSlash("/permissions fleet web1")
	if !strings.Contains(lastMsg(none), "not available") {
		t.Errorf("/permissions fleet with no control at all: %q", lastMsg(none))
	}
}

// A machine's scheme is edited in the permissions editor, in its words, and
// is not the master's.
func TestSlashPermissionsFleet(t *testing.T) {
	f := newFakeFleet()
	m := newFleetModel(f)
	master := m.cfg.Perms.(*fakeFleetPerms)

	m.handleSlash("/permissions fleet web1")
	if got := lastMsg(m); !strings.Contains(got, "Permissions of fleet machine web1 (this machine only)") ||
		!strings.Contains(got, "/permissions fleet web1 set <category> <mode>") || !strings.Contains(got, "/permissions fleet web1 add") {
		t.Fatalf("/permissions fleet web1:\n%s", got)
	}
	m.handleSlash("/permissions fleet web1 set exec allow")
	m.handleSlash("/permissions fleet web1 add os_exec(systemctl status *) allow")
	if f.perms["web1"].modes["exec"] != "allow" || f.perms["web1"].rules["os_exec(systemctl status *)"] != "allow" {
		t.Fatalf("web1 after its edits: %+v", f.perms["web1"])
	}
	if len(master.rules) != 0 || len(f.defaults.rules) != 0 {
		t.Fatalf("a machine's rule landed elsewhere: master %v, fleet rules %v", master.rules, f.defaults.rules)
	}
	m.handleSlash("/permissions fleet web1 rm os_exec(systemctl status *)")
	if len(f.perms["web1"].rules) != 0 {
		t.Fatalf("rm left %v", f.perms["web1"].rules)
	}
	m.handleSlash("/permissions fleet web1 rm os_exec(nothing *)")
	if !strings.Contains(lastMsg(m), "see /permissions fleet web1") {
		t.Errorf("a missing rule: %q", lastMsg(m))
	}

	// The fleet's rules for new machines: rules only.
	m.handleSlash("/permissions fleet --defaults add os_exec(uptime *) allow")
	if f.defaults.rules["os_exec(uptime *)"] != "allow" || len(f.perms["web1"].rules) != 0 {
		t.Fatalf("fleet rules %v, web1 %v", f.defaults.rules, f.perms["web1"].rules)
	}
	m.handleSlash("/permissions fleet --defaults")
	if got := lastMsg(m); !strings.Contains(got, "Fleet rules") || !strings.Contains(got, "os_exec(uptime *)") || strings.Contains(got, "set <category>") {
		t.Fatalf("/permissions fleet --defaults:\n%s", got)
	}
	m.handleSlash("/permissions fleet --defaults set exec allow")
	if !strings.Contains(lastMsg(m), "no such category") || f.defaults.modes["exec"] != "ask" {
		t.Errorf("the fleet's rules took a category: %q", lastMsg(m))
	}

	m.handleSlash("/permissions fleet nobody set exec allow")
	if !strings.Contains(lastMsg(m), "no machine nobody") {
		t.Errorf("a machine that is not there: %q", lastMsg(m))
	}
	m.handleSlash("/permissions fleet")
	if !strings.Contains(lastMsg(m), "usage: /permissions fleet") {
		t.Errorf("/permissions fleet alone: %q", lastMsg(m))
	}
	// The master's own editor is as it was.
	m.handleSlash("/permissions add delete_* deny")
	if master.rules["delete_*"] != "deny" || !strings.Contains(lastMsg(m), "on every surface") {
		t.Errorf("/permissions add: %v %q", master.rules, lastMsg(m))
	}
	m.handleSlash("/permissions")
	if got := lastMsg(m); !strings.Contains(got, "/permissions add <tool|pattern> <mode>") || strings.Contains(got, "fleet machine") {
		t.Errorf("/permissions:\n%s", got)
	}
}

// fleetCron is a cron control that can open a job for fleet machines.
type fleetCron struct {
	fakeCron
	fleet map[string][]string
}

func (c *fleetCron) SetFleet(id string, machines []string) (CronJobView, error) {
	if id != "j1" {
		return CronJobView{}, errors.New("no job with id " + id)
	}
	c.fleet[id] = machines
	return CronJobView{ID: id, Fleet: machines}, nil
}

func TestSlashCronFleet(t *testing.T) {
	c := &fleetCron{fleet: map[string][]string{}}
	m := newCronModel(c)
	m.handleSlash("/cron fleet j1 web1 db1")
	if got := c.fleet["j1"]; len(got) != 2 || !strings.Contains(lastMsg(m), "may reach web1, db1 and no other") {
		t.Fatalf("/cron fleet: %v %q", got, lastMsg(m))
	}
	m.handleSlash("/cron fleet j1 none")
	if got := c.fleet["j1"]; len(got) != 0 || !strings.Contains(lastMsg(m), "reaches no fleet machine") {
		t.Fatalf("/cron fleet none: %v %q", got, lastMsg(m))
	}
	m.handleSlash("/cron fleet nope web1")
	if !strings.Contains(lastMsg(m), "no job with id nope") {
		t.Errorf("an unknown job: %q", lastMsg(m))
	}
	m.handleSlash("/cron fleet j1")
	if !strings.Contains(lastMsg(m), "usage: /cron fleet") {
		t.Errorf("no machines named: %q", lastMsg(m))
	}
	// A cron control that cannot do it says how the command is used.
	plain := newCronModel(&fakeCron{})
	plain.handleSlash("/cron fleet j1 web1")
	if !strings.Contains(lastMsg(plain), "usage: /cron fleet") {
		t.Errorf("no fleet support: %q", lastMsg(plain))
	}
}
