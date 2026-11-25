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
	// init is what Init answers with; recovered counts its recovery step's runs.
	init      FleetInit
	recovered int
}

func (f *fakeFleet) Init(listen string, addresses []string) (FleetInit, error) {
	if listen == "bad" {
		return FleetInit{}, errors.New("--listen bad is not a host:port address")
	}
	f.note(append([]string{"init", "listen=" + listen}, addresses...)...)
	return f.init, nil
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
	return &fakeFleet{perms: map[string]*fakeFleetPerms{"web1": mk()}, defaults: &fakeFleetPerms{modes: map[string]string{}, rules: map[string]string{}}}
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
func (f *fakeFleet) Defaults() FleetDefaults { return fakeDefaults{f.defaults} }
func (f *fakeFleet) Group(args []string) (string, error) {
	return f.note(append([]string{"group"}, args...)...), nil
}

// fakeDefaults are the defaults for new machines: only the categories set.
type fakeDefaults struct{ *fakeFleetPerms }

func (d fakeDefaults) Permissions() []PermissionInfo {
	if d.modes["exec"] == "" {
		return nil
	}
	return d.fakeFleetPerms.Permissions()
}

func (d fakeDefaults) UnsetPermission(cat string) (bool, error) {
	had := d.modes[cat] != ""
	delete(d.modes, cat)
	return had, nil
}
func (f *fakeFleet) Relay(args []string) (string, error) {
	return f.note(append([]string{"relay"}, args...)...), nil
}
func (f *fakeFleet) RemoteConfirm(args []string) (string, error) {
	return f.note(append([]string{"remote-confirm"}, args...)...), nil
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
		"/fleet enroll web2", "/fleet confirm web1 123456", "/fleet confirm web1 123 456 exec=allow", "/fleet revoke web1 delete", "/fleet revoke web1 keep", "/fleet relay discord on", "/fleet remote-confirm on",
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
		"/fleet revoke web1": "revoke web1 scheme=", "/fleet relay": "relay", "/fleet relay discord": "relay discord", "/fleet remote-confirm": "remote-confirm",
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
		"/fleet remote-confirm off":                       "remote-confirm off",
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
	for _, line := range []string{"/fleet bogus", "/fleet show", "/fleet enroll", "/fleet enroll a b", "/fleet confirm", "/fleet revoke", "/fleet revoke a b c", "/fleet relay a b c", "/fleet remote-confirm on now"} {
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

// /fleet group: looking is open to any line; a change to a group, and
// applying one, takes a line typed at this keyboard.
func TestSlashFleetGroup(t *testing.T) {
	f := newFakeFleet()
	m := newFleetModel(f)
	for line, want := range map[string]string{
		"/fleet group": "group", "/fleet group list": "group list", "/fleet group show web": "group show web", "/fleet group rules web": "group rules web",
	} {
		f.calls = nil
		slash(m, line)
		if len(f.calls) != 1 || f.calls[0] != want {
			t.Errorf("%q asked the fleet for %v, want %q", line, f.calls, want)
		}
	}
	for line, want := range map[string]string{
		"/fleet group create web":                     "group create web",
		"/fleet group delete web":                     "group delete web",
		"/fleet group add web web1 web2":              "group add web web1 web2",
		"/fleet group remove web web1":                "group remove web web1",
		"/fleet group rules web add x_post deny":      "group rules web add x_post deny",
		"/fleet group rules web rm os_exec(uptime *)": "group rules web rm os_exec(uptime *)",
		"/fleet group apply web":                      "group apply web",
	} {
		f.calls = nil
		slash(m, line)
		if len(f.calls) != 0 || !strings.Contains(lastMsg(m), "type this command here yourself") {
			t.Fatalf("%q, not typed: calls %v, answered %q", line, f.calls, lastMsg(m))
		}
		typeLine(m, line)
		if len(f.calls) != 1 || f.calls[0] != want {
			t.Errorf("%q, typed, asked the fleet for %v, want %q", line, f.calls, want)
		}
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

	// The fleet's defaults for new machines: the rules, and only the
	// categories set (TEN-546), each of which can be given back.
	m.handleSlash("/permissions fleet --defaults add os_exec(uptime *) allow")
	if f.defaults.rules["os_exec(uptime *)"] != "allow" || len(f.perms["web1"].rules) != 0 {
		t.Fatalf("fleet rules %v, web1 %v", f.defaults.rules, f.perms["web1"].rules)
	}
	m.handleSlash("/permissions fleet --defaults")
	if got := lastMsg(m); !strings.Contains(got, "Fleet defaults") || !strings.Contains(got, "os_exec(uptime *)") || strings.Contains(got, "exec ") {
		t.Fatalf("/permissions fleet --defaults, no category set:\n%s", got)
	}
	m.handleSlash("/permissions fleet --defaults set exec allow")
	if f.defaults.modes["exec"] != "allow" {
		t.Fatalf("a default category: %q %v", lastMsg(m), f.defaults.modes)
	}
	m.handleSlash("/permissions fleet --defaults")
	if got := lastMsg(m); !strings.Contains(got, "exec ") || !strings.Contains(got, "allow") {
		t.Fatalf("/permissions fleet --defaults lists the category set:\n%s", got)
	}
	m.handleSlash("/permissions fleet --defaults set nope allow")
	if !strings.Contains(lastMsg(m), "no such category") {
		t.Errorf("a default for no category: %q", lastMsg(m))
	}
	m.handleSlash("/permissions fleet --defaults unset exec")
	if _, set := f.defaults.modes["exec"]; set || !strings.Contains(lastMsg(m), "master's copy") {
		t.Errorf("unset: %q %v", lastMsg(m), f.defaults.modes)
	}
	m.handleSlash("/permissions fleet --defaults unset exec")
	if !strings.Contains(lastMsg(m), "is not set") {
		t.Errorf("unset twice: %q", lastMsg(m))
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

// /fleet init is the operator's act at this keyboard, takes the command
// line's flags, and says what the fleet's init says. A fleet whose recovery
// key is still to be made gets that step on the terminal itself, with the app
// out of the way, before anything is said: the identity never reaches the
// chat.
func TestSlashFleetInit(t *testing.T) {
	f := newFakeFleet()
	f.init = FleetInit{Done: func(err error) string {
		if err != nil {
			return "master, recovery: " + err.Error()
		}
		return "this Tenant is now a fleet master"
	}}
	m := newFleetModel(f)

	slash(m, "/fleet init")
	if len(f.calls) != 0 || !strings.Contains(lastMsg(m), "type this command here yourself") {
		t.Fatalf("/fleet init, not typed: calls %v, answered %q", f.calls, lastMsg(m))
	}
	for _, line := range []string{"/fleet init web1", "/fleet init --listen", "/fleet init --port 9777"} {
		typeLine(m, line)
		if len(f.calls) != 0 || !strings.Contains(lastMsg(m), "usage: /fleet") {
			t.Fatalf("%q: calls %v, answered %q", line, f.calls, lastMsg(m))
		}
	}

	// run types a line and follows /fleet init to its end, but for the step
	// that takes the terminal: handed says the app was about to give it away.
	run := func(line string) (handed bool) {
		t.Helper()
		m.ta.SetValue(line)
		_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		for _, msg := range drain(cmd) {
			in, ok := msg.(fleetInitMsg)
			if !ok {
				m.Update(msg)
				continue
			}
			_, next := m.Update(in)
			if in.Recovery != nil {
				return next != nil
			}
			for _, said := range drain(next) {
				m.Update(said)
			}
		}
		return false
	}

	// The recovery key is proven already: nothing takes the terminal.
	if run("/fleet init --listen 127.0.0.1:9777 --address a.example:9777 --address=10.0.0.1:9777") {
		t.Fatal("an init with nothing to prove took the terminal")
	}
	if want := "init listen=127.0.0.1:9777 a.example:9777 10.0.0.1:9777"; len(f.calls) != 1 || f.calls[0] != want {
		t.Fatalf("the fleet was asked for %v, want %q", f.calls, want)
	}
	if !strings.Contains(lastMsg(m), "now a fleet master") {
		t.Fatalf("init said %q", lastMsg(m))
	}
	run("/fleet init --listen bad")
	if !strings.Contains(lastMsg(m), "fleet: --listen bad is not a host:port address") {
		t.Fatalf("a refused init said %q", lastMsg(m))
	}

	// A recovery key to make: the terminal is handed over first, and what init
	// says waits for it.
	f.init.Recovery = func() error { f.recovered++; return errors.New("cancelled") }
	said := lastMsg(m)
	if !run("/fleet init") || lastMsg(m) != said || f.recovered != 0 {
		t.Fatalf("an init with a recovery key to make did not wait for the terminal step: said %q, ran %d", lastMsg(m), f.recovered)
	}
	// The step itself is the control's function, run as it stands.
	if err := terminalStep(f.init.Recovery).Run(); err == nil || f.recovered != 1 {
		t.Fatalf("the terminal step: %v, ran %d", err, f.recovered)
	}
	// Back in the app: what init says, the step's outcome with it.
	_, next := m.Update(fleetInitMsg{FleetInit: f.init, ran: true, err: errors.New("cancelled")})
	for _, msg := range drain(next) {
		m.Update(msg)
	}
	if !strings.Contains(lastMsg(m), "master, recovery: cancelled") || f.recovered != 1 {
		t.Fatalf("after the terminal step init said %q (step ran %d)", lastMsg(m), f.recovered)
	}
}
