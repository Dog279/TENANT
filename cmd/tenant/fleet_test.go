package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tenant/internal/approval"
	"tenant/internal/control"
	croneng "tenant/internal/cron"
	"tenant/internal/fleet"
	"tenant/internal/model"
)

// fleetLab is a master in temp dirs: the `tenant fleet` commands run against
// it in this process, the hub (startFleetHub) holds its fleet on a loopback
// port, and real children link to it. Nothing here is the installed Tenant's.
type fleetLab struct {
	t         *testing.T
	cfg, data string
	c         *commonFlags
	broker    *approvalBroker
	stg       *settings
	stgMu     sync.Mutex
	answer    string // what the operator types when asked
	asked     []string
	feedMu    sync.Mutex
	feed      []string
	stop      func()
}

func newFleetLab(t *testing.T) *fleetLab {
	t.Helper()
	root := t.TempDir()
	// No default dir of this account is in reach, whatever a command's flags say.
	for _, env := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "XDG_CONFIG_HOME", "XDG_DATA_HOME"} {
		t.Setenv(env, filepath.Join(root, "home"))
	}
	l := &fleetLab{t: t, cfg: filepath.Join(root, "cfg"), data: filepath.Join(root, "data")}
	l.c = &commonFlags{cfgDir: l.cfg, dataDir: l.data, agent: "main"}
	prevWait, prevAsk, prevEvery, prevFleet := fleetInitWait, fleetAsk, fleetWatchInterval, defaultFleet
	fleetInitWait, fleetWatchInterval, defaultFleet = 0, 25*time.Millisecond, approval.NewFleet(categorize)
	fleetAsk = func(prompt string) (string, error) {
		l.asked = append(l.asked, prompt)
		return l.answer, nil
	}
	t.Cleanup(func() {
		if l.stop != nil {
			l.stop()
		}
		fleetRelays.Delete(surfaceDiscord)
		fleetRelays.Delete(surfaceIMessage)
		fleetInitWait, fleetAsk, fleetWatchInterval, defaultFleet = prevWait, prevAsk, prevEvery, prevFleet
	})
	return l
}

// run is `tenant fleet <args>` on the lab's dirs: its standard output, and
// its error.
func (l *fleetLab) run(args ...string) (string, error) {
	l.t.Helper()
	out, err := captureTenantStdout(l.t, func() error {
		return cmdFleet(context.Background(), append(args, "--config", l.cfg, "--data", l.data))
	})
	if os.Getenv("TENANT_FLEETCTL_SHOW") != "" { // for a person reading along: go test -v with this set
		l.t.Logf("$ tenant fleet %s\n%s\n(error: %v)", strings.Join(args, " "), out, err)
	}
	return out, err
}

func (l *fleetLab) must(args ...string) string {
	l.t.Helper()
	out, err := l.run(args...)
	if err != nil {
		l.t.Fatalf("tenant fleet %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// settings writes the master's settings file.
func (l *fleetLab) settings(s settings) {
	l.t.Helper()
	if err := s.Save(l.cfg, "main"); err != nil {
		l.t.Fatal(err)
	}
}

// saved is the settings file as it is on disk.
func (l *fleetLab) saved() *settings {
	l.t.Helper()
	stg, err := loadSettings(l.cfg, "main")
	if err != nil {
		l.t.Fatal(err)
	}
	return stg
}

// schemeOf is one machine's scheme as the settings file holds it.
func (l *fleetLab) schemeOf(id string) approval.Scheme {
	l.t.Helper()
	var s approval.Scheme
	raw, ok := l.saved().FleetSchemes[id]
	if !ok {
		l.t.Fatalf("no scheme for %s in the settings file", id)
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		l.t.Fatal(err)
	}
	return s
}

// master makes the lab a fleet master and starts the hub the way serve does:
// the fleet's schemes loaded from the settings file, the listener on a
// loopback port.
func (l *fleetLab) master() *fleet.Manager {
	l.t.Helper()
	l.must("init", "--listen", "127.0.0.1:0")
	l.stg = l.saved()
	l.broker = newApprovalBroker(slog.Default())
	loadFleetSettings(l.stg, &l.stgMu, l.cfg, "main", l.tell)
	l.stop = startFleetHub(context.Background(), l.c, "serve", l.broker, l.tell, slog.Default())
	mgr := fleetManager()
	if mgr == nil {
		l.t.Fatalf("the hub did not take the fleet: %v", l.told())
	}
	l.must("init", "--address", mgr.Addr().String())
	return mgr
}

// sync does what the settings watcher does when the file changed.
func (l *fleetLab) sync() {
	applySettingsFile(l.cfg, "main", nil, nil, &l.stgMu, l.stg)
}

func (l *fleetLab) tell(s string) {
	l.feedMu.Lock()
	defer l.feedMu.Unlock()
	l.feed = append(l.feed, s)
}

func (l *fleetLab) told() []string {
	l.feedMu.Lock()
	defer l.feedMu.Unlock()
	return append([]string{}, l.feed...)
}

// labChild is a machine enrolled with the lab's master.
type labChild struct {
	dir, id, code string // its config dir, its machine id, the six digits it shows
	enrollCode    string
}

// enroll makes a code with the command line and spends it as a new machine
// would: the machine is then pending.
func (l *fleetLab) enroll(name string, rep fleet.Reported) labChild {
	l.t.Helper()
	code := strings.SplitN(l.must("enroll", name), "\n", 2)[0]
	dir := l.t.TempDir()
	id, err := fleet.MintIdentity(fleet.ChildIdentityPath(dir))
	if err != nil {
		l.t.Fatal(err)
	}
	if rep.OS == "" {
		rep = fleet.Reported{OS: "linux", User: "root", Version: "1.2.3", Sandbox: "host", Ceiling: "full"}
	}
	child, six, err := fleet.Enroll(context.Background(), code, id, rep)
	if err != nil {
		l.t.Fatalf("enroll %s: %v", name, err)
	}
	if err := fleet.SaveChild(fleet.ChildPath(dir), child); err != nil {
		l.t.Fatal(err)
	}
	return labChild{dir: dir, id: child.ID, code: six, enrollCode: code}
}

// confirm confirms a pending machine with the command line, the operator
// typing the machine's six digits.
func (l *fleetLab) confirm(name string, ch labChild, flags ...string) string {
	l.t.Helper()
	l.answer = ch.code
	return l.must(append([]string{"confirm", name}, flags...)...)
}

// labTools is what a lab child serves: one tool that asks its master before
// it runs.
type labTools struct{ calls atomic.Int32 }

func (*labTools) All() []model.ToolSpec {
	return []model.ToolSpec{{Name: "os_exec", Description: "run a command", Parameters: json.RawMessage(`{"type":"object"}`)}}
}

func (t *labTools) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	t.calls.Add(1)
	if !fleet.Confirm(ctx, "os_exec", "uname -a") {
		return "the master said no", true, nil
	}
	return "Linux", false, nil
}

// start runs the child's end of the link until the test ends. done gets
// Run's error.
func (ch labChild) start(t *testing.T, tools fleet.Tools) (client *fleet.Client, done <-chan error) {
	t.Helper()
	client, err := fleet.NewClient(fleet.ClientConfig{Dir: ch.dir, Tools: tools, Version: "1.2.3", User: "root"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ran, stopped := make(chan error, 1), make(chan struct{})
	go func() {
		ran <- client.Run(ctx)
		close(stopped)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			t.Error("the child did not stop")
		}
	})
	return client, ran
}

// online reports whether the master holds a link to the machine.
func online(mgr *fleet.Manager, id string) bool {
	all, _ := mgr.Machines()
	for _, m := range all {
		if m.ID == id {
			return m.Online
		}
	}
	return false
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("after 15 s: %s", what)
		}
	}
}

// --- refusals ---

// A machine is confirmed by the six digits it shows and by nothing else, and
// until then the master sends it nothing.
func TestFleetConfirmTakesTheMachinesOwnDigits(t *testing.T) {
	l := newFleetLab(t)
	mgr := l.master()
	ch := l.enroll("web1", fleet.Reported{})
	tools := &labTools{}
	client, _ := ch.start(t, tools)
	eventually(t, "the pending machine is told to wait", func() bool { return client.Status().State == fleet.LinkWaiting })

	pending := func(why string) {
		t.Helper()
		if m, err := fleet.Open(l.cfg).Machine(ch.id); err != nil || m.State != fleet.StatePending {
			t.Fatalf("%s: the machine is %v (%v), want pending", why, m.State, err)
		}
		if _, ok := l.saved().FleetSchemes[ch.id]; ok {
			t.Fatalf("%s: a scheme was saved for a machine that is not confirmed", why)
		}
		if _, err := mgr.Call(context.Background(), ch.id, fleet.Call{Tool: "os_exec"}); !errors.Is(err, fleet.ErrPending) {
			t.Fatalf("%s: a call to the pending machine: %v, want ErrPending", why, err)
		}
	}
	pending("before any confirmation")

	wrong := "000000"
	if wrong == ch.code {
		wrong = "111111"
	}
	for _, typed := range []string{wrong, ch.code[:5], ch.code + "0", "abcdef", "١٢٣٤٥٦"} {
		l.answer = typed
		if _, err := l.run("confirm", "web1"); !errors.Is(err, fleet.ErrWrongCode) {
			t.Fatalf("confirm with %q: %v, want ErrWrongCode", typed, err)
		}
		pending("after the wrong digits " + typed)
	}
	l.answer = "" // Enter alone
	if _, err := l.run("confirm", "web1"); err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Fatalf("confirm with nothing typed: %v", err)
	}
	pending("after a cancelled confirmation")
	// There is no way to hand the digits over but to type them.
	if _, err := l.run("confirm", "web1", "--code", ch.code); err == nil {
		t.Fatal("confirm took the digits as a flag")
	}
	if _, err := l.run("confirm", "web1", ch.code); err == nil {
		t.Fatal("confirm took the digits as an argument")
	}
	pending("after digits passed as arguments")
	if tools.calls.Load() != 0 || client.Status().State != fleet.LinkWaiting {
		t.Fatalf("the pending machine was sent something: %d calls, link %s", tools.calls.Load(), client.Status().State)
	}

	out := l.confirm("web1", ch)
	if !strings.Contains(out, "confirmed web1") {
		t.Fatalf("confirm said:\n%s", out)
	}
	if len(l.asked) == 0 || !strings.Contains(l.asked[len(l.asked)-1], "six digits") {
		t.Fatalf("the operator was not asked for the digits: %q", l.asked)
	}
	eventually(t, "the confirmed machine links", func() bool { return client.Status().State == fleet.LinkConnected })
	// Confirmed machines are not confirmed twice.
	if _, err := l.run("confirm", "web1"); err == nil || !strings.Contains(err.Error(), "not waiting for confirmation") {
		t.Fatalf("a second confirm: %v", err)
	}
}

// The digits are a person's to type: with no terminal there is nobody.
func TestFleetConfirmNeedsATerminal(t *testing.T) {
	l := newFleetLab(t)
	l.master()
	ch := l.enroll("web1", fleet.Reported{})
	r, w, err := os.Pipe() // what a script, or the agent's shell, hands a command
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.WriteString(ch.code + "\n")
	_ = w.Close()
	prevIn, prevAsk := os.Stdin, fleetAsk
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = prevIn; _ = r.Close() })
	fleetAsk = terminalAsk
	defer func() { fleetAsk = prevAsk }()
	if _, err := l.run("confirm", "web1"); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("confirm with the digits piped in: %v, want a refusal", err)
	}
	if m, _ := fleet.Open(l.cfg).Machine(ch.id); m.State != fleet.StatePending {
		t.Fatalf("the machine is %s after a confirmation nobody typed", m.State)
	}
	// Revoking without a terminal needs the answer as a flag.
	if _, err := l.run("revoke", "web1"); err == nil || !strings.Contains(err.Error(), "--keep-scheme") {
		t.Fatalf("revoke with nobody to ask: %v", err)
	}
}

func TestFleetCommandRefusals(t *testing.T) {
	l := newFleetLab(t)
	l.master()
	ch := l.enroll("web1", fleet.Reported{})
	for _, tc := range []struct {
		why  string
		args []string
		want string
	}{
		{"a name a pending machine holds", []string{"enroll", "web1"}, "taken"},
		{"the same name in another case", []string{"enroll", "WEB1"}, "taken"},
		{"a name that is not one", []string{"enroll", "web 1"}, "not a machine name"},
		{"a name that reads as an id", []string{"enroll", "m0123456789abcdef"}, "not a machine name"},
		{"an escape sequence for a name", []string{"enroll", "\x1b[2Jweb"}, "not a machine name"},
		{"adopting an id nobody had", []string{"enroll", "web2", "--adopt", "m0123456789abcdef"}, "unknown machine"},
		{"adopting a machine that is not revoked", []string{"enroll", "web2", "--adopt", ch.id}, "only a revoked machine"},
		{"a code that never expires", []string{"enroll", "web2", "--ttl", "48h"}, "lasts between"},
		{"revoking a machine that is not there", []string{"revoke", "nobody", "--keep-scheme"}, "no machine"},
		{"keeping and deleting at once", []string{"revoke", "web1", "--keep-scheme", "--delete-scheme"}, "usage"},
		{"showing a machine that is not there", []string{"show", "nobody"}, "no machine"},
		{"renaming to a name that is not one", []string{"rename", "web1", "a b"}, "not a machine name"},
		{"renaming a machine that is not there", []string{"rename", "nobody", "x"}, "no machine"},
		{"confirming a machine that is not there", []string{"confirm", "nobody"}, "no machine"},
		{"a category that is not one", []string{"confirm", "web1", "--set", "root=allow"}, "not category=mode"},
		{"a mode that is not one", []string{"confirm", "web1", "--set", "exec=sometimes"}, "not category=mode"},
		{"permissions of a machine that is not there", []string{"permissions", "nobody"}, "no machine"},
		{"an edit of a machine with no scheme yet", []string{"permissions", "web1", "set", "exec", "allow"}, "no approval scheme"},
		{"a relay that is not one", []string{"relay", "slack", "on"}, "usage"},
		{"a listen address that is not one", []string{"init", "--listen", "nowhere"}, "not a host:port"},
		{"an address that is not one", []string{"init", "--address", "http://x/"}, "not a host:port"},
		{"an unknown command", []string{"exec", "web1", "id"}, "usage"},
	} {
		if out, err := l.run(tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s (tenant fleet %s): %v, want an error with %q\n%s", tc.why, strings.Join(tc.args, " "), err, tc.want, out)
		}
	}
	if m, err := fleet.Open(l.cfg).Machine(ch.id); err != nil || m.State != fleet.StatePending || m.Name != "web1" {
		t.Fatalf("the refused commands changed the machine: %+v %v", m, err)
	}
}

// What a machine says about itself is shown as its own words, in quotes it
// cannot close, and can neither draw on the screen nor pass for a line of it.
func TestFleetScreensEscapeWhatAMachineSays(t *testing.T) {
	hostile := []string{
		"\x1b[2J\x1b[H\x1b]0;owned\x07",                                     // clear the screen, retitle the window
		"root\r  state       confirmed",                                     // overwrite the line
		"linux\n  ceiling   \"read-only\"\n  id          m0000000000000000", // add lines of its own
		"web1\x00\x08\x7f",                                                  // another machine's name, and what erases
		"rооt",                                                              // Cyrillic o, twice
		"a\u202eb\u200bc\ufeff",                                             // reorder and hide
		`"quoted" \ and back`,                                               // close the quotes
		strings.Repeat("A", 4000),                                           // push the rest out of view
		"\xff\xfe not UTF-8",
		"Reported by the machine (its own words; the master has not checked them):", // the screen's own heading
	}
	for _, evil := range hostile {
		m := fleet.Machine{ID: "m0123456789abcdef", Name: evil, Fingerprint: strings.Repeat("ab", 32), State: fleet.StatePending,
			Reported: fleet.Reported{OS: evil, User: evil, Version: evil, Sandbox: evil, Ceiling: evil}, EnrolledAt: time.Now()}
		clean := fleet.Machine{ID: m.ID, Name: "web1", Fingerprint: m.Fingerprint, State: fleet.StatePending,
			Reported: fleet.Reported{OS: "linux", User: "root", Version: "1", Sandbox: "host", Ceiling: "full"}, EnrolledAt: m.EnrolledAt}
		scheme := approval.Scheme{Permissions: map[string]string{"exec": "ask"}, ToolRules: map[string]string{evil: "allow"}}
		link := fleet.MachineStatus{Machine: m, LastError: evil, Remote: evil}
		for screen, pair := range map[string][2]string{
			"confirm": {confirmScreen(m, fleetPreview{Scheme: scheme}), confirmScreen(clean, fleetPreview{Scheme: approval.Scheme{Permissions: scheme.Permissions, ToolRules: map[string]string{"x": "allow"}}})},
			"list":    {listText([]fleet.Machine{m}, map[string]fleet.MachineStatus{m.ID: link}), listText([]fleet.Machine{clean}, map[string]fleet.MachineStatus{m.ID: {Machine: clean}})},
			"show":    {showText(m, &link, &scheme, nil), showText(clean, &fleet.MachineStatus{Machine: clean, LastError: "x", Remote: "y"}, &approval.Scheme{Permissions: scheme.Permissions, ToolRules: map[string]string{"x": "allow"}}, nil)},
			"alert":   {alertLine(t, evil), alertLine(t, "web1")},
		} {
			got, want := pair[0], pair[1]
			for i := 0; i < len(got); i++ {
				if c := got[i]; c != '\n' && (c < 0x20 || c > 0x7e) {
					t.Errorf("%s screen for %q carries byte %#x:\n%q", screen, evil, c, got)
					break
				}
			}
			// The machine's words add no line: the screen has the lines it has
			// for a machine that says nothing remarkable.
			if g, w := strings.Count(got, "\n"), strings.Count(want, "\n"); g != w {
				t.Errorf("%s screen for %q has %d lines, a plain machine's has %d:\n%s", screen, evil, g, w, got)
			}
		}
		if q := fleetQuote(evil); !strings.HasPrefix(q, `"`) || len(q) > 6*64+5 {
			t.Errorf("fleetQuote(%q) = %q", evil, q)
		}
	}
	// A plain name stays readable; anything else is quoted.
	if fleetName("web-1.lan_a") != "web-1.lan_a" || fleetName("web 1") != `"web 1"` || fleetName("") != `""` || fleetName("-x") != `"-x"` {
		t.Errorf("fleetName: %q %q %q %q", fleetName("web-1.lan_a"), fleetName("web 1"), fleetName(""), fleetName("-x"))
	}
}

// alertLine is the replay alert as the operator's feed gets it.
func alertLine(t *testing.T, text string) string {
	t.Helper()
	var said []string
	h := &fleetHub{tell: func(s string) { said = append(said, s) }}
	h.alert(fleet.Refusal{Reason: fleet.RefusedReplay, Name: text, Source: text, Machine: "m0123456789abcdef"})
	if len(said) != 1 {
		t.Fatalf("the alert was told %d times", len(said))
	}
	return said[0]
}

// The store lets a machine report printable text only. What it does let
// through still reaches every screen as the machine's quoted words.
func TestFleetScreensMarkWhatTheMachineReported(t *testing.T) {
	l := newFleetLab(t)
	l.master()
	rep := fleet.Reported{OS: "linux", User: "root  state confirmed  id m0000000000000000", Version: "1.0 (reported by you)", Sandbox: "host", Ceiling: "read-only"}
	ch := l.enroll("web1", rep)
	l.answer = "" // look at the screen, confirm nothing
	screen, _ := l.run("confirm", "web1")
	list := l.must("list")
	show := l.must("show", "web1")
	for name, out := range map[string]string{"confirm": screen, "show": show} {
		if !strings.Contains(out, "Reported by the machine (its own words") {
			t.Errorf("the %s screen does not mark what the machine reported:\n%s", name, out)
		}
		for _, v := range []string{rep.User, rep.Version, rep.Ceiling, rep.OS, rep.Sandbox} {
			if !strings.Contains(out, `"`+v+`"`) {
				t.Errorf("the %s screen does not show %q in quotes:\n%s", name, v, out)
			}
		}
		if !strings.Contains(out, ch.id) {
			t.Errorf("the %s screen does not name the machine's id:\n%s", name, out)
		}
	}
	if !strings.Contains(list, `"`+rep.User+`"`) || !strings.Contains(list, "reported by the machine") {
		t.Errorf("list does not mark the reported user:\n%s", list)
	}
	if strings.Count(list, "m0000000000000000") != 1 || !strings.Contains(list, `id m0000000000000000"`) {
		t.Errorf("the id the machine made up is not inside its quotes:\n%s", list)
	}
}

// --- inheritance, on the real settings file ---

func TestFleetSchemeAtConfirm(t *testing.T) {
	l := newFleetLab(t)
	l.settings(settings{
		Permissions: map[string]string{"exec": "allow", "write": "allow", "destructive": "allow", "send": "allow", "sandbox": "allow", "web": "allow", "mcp": "deny"},
		ToolRules:   map[string]string{"os_exec(git status *)": "allow", "os_exec(rm *)": "deny", "os_exec(systemctl status *)": "ask"},
	})
	l.master()
	// The fleet's rules, set before any machine exists.
	l.must("permissions", "--defaults", "add", "os_exec(systemctl status *)", "allow")
	if out := l.must("permissions", "--defaults"); !strings.Contains(out, "os_exec(systemctl status *)") {
		t.Fatalf("the fleet rules do not list the rule:\n%s", out)
	}

	web1 := l.enroll("web1", fleet.Reported{})
	l.answer = ""
	screen, _ := l.run("confirm", "web1", "--set", "send=allow")
	for _, want := range []string{
		"exec         ask    (this master: allow)", "write        ask    (this master: allow)",
		"send         allow  set by you, for this machine only", "web          allow", "mcp          deny",
		"os_exec(systemctl status *)", "fleet rule", "os_exec(rm *)",
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("the confirm screen lacks %q:\n%s", want, screen)
		}
	}
	l.confirm("web1", web1, "--set", "send=allow")
	db1 := l.enroll("db1", fleet.Reported{})
	l.confirm("db1", db1)

	s := l.schemeOf(web1.id)
	for cat, want := range map[string]string{"exec": "ask", "write": "ask", "destructive": "ask", "sandbox": "ask", "send": "allow", "web": "allow", "mcp": "deny"} {
		if s.Permissions[cat] != want {
			t.Errorf("web1 %s = %q, want %q", cat, s.Permissions[cat], want)
		}
	}
	// A fleet rule wins over the inherited one; an inherited allow that
	// reaches a command is no looser than ask; a deny is kept.
	for rule, want := range map[string]string{"os_exec(systemctl status *)": "allow", "os_exec(git status *)": "ask", "os_exec(rm *)": "deny"} {
		if s.ToolRules[rule] != want {
			t.Errorf("web1 rule %s = %q, want %q", rule, s.ToolRules[rule], want)
		}
	}
	// Loosening was for web1 alone: not db1, not the master.
	if got := l.schemeOf(db1.id).Permissions["send"]; got != "ask" {
		t.Errorf("db1 send = %q: web1's loosening carried over", got)
	}
	if got := l.saved().Permissions; got["send"] != "allow" || got["exec"] != "allow" {
		t.Errorf("the master's own scheme changed: %v", got)
	}

	// A later change on the master moves no machine.
	stg := l.saved()
	stg.Permissions["web"], stg.ToolRules["os_exec(git status *)"] = "deny", "deny"
	l.settings(*stg)
	if s := l.schemeOf(web1.id); s.Permissions["web"] != "allow" || s.ToolRules["os_exec(git status *)"] != "ask" {
		t.Errorf("web1 followed the master: %v %v", s.Permissions, s.ToolRules)
	}
	// Nor does a change of the fleet's rules, until it is applied again.
	l.must("permissions", "--defaults", "add", "os_exec(journalctl *)", "allow")
	l.must("permissions", "web1", "rm", "os_exec(systemctl status *)")
	if s := l.schemeOf(web1.id); s.ToolRules["os_exec(journalctl *)"] != "" || s.ToolRules["os_exec(systemctl status *)"] != "" {
		t.Errorf("web1's rules after the edits: %v", s.ToolRules)
	}
	l.must("permissions", "web1", "--from", "defaults")
	if s := l.schemeOf(web1.id); s.ToolRules["os_exec(journalctl *)"] != "allow" || s.ToolRules["os_exec(systemctl status *)"] != "allow" || s.Permissions["send"] != "allow" {
		t.Errorf("web1 after --from defaults: %v %v", s.Permissions, s.ToolRules)
	}
	if s := l.schemeOf(db1.id); s.ToolRules["os_exec(journalctl *)"] != "" {
		t.Errorf("db1 got a fleet rule it was not given: %v", s.ToolRules)
	}
	// One machine's edits are its own.
	l.must("permissions", "web1", "set", "exec", "allow")
	l.must("permissions", "web1", "add", "os_exec(uptime *)", "allow")
	if s := l.schemeOf(web1.id); s.Permissions["exec"] != "allow" || s.ToolRules["os_exec(uptime *)"] != "allow" {
		t.Errorf("web1 after its own edits: %v %v", s.Permissions, s.ToolRules)
	}
	if s := l.schemeOf(db1.id); s.Permissions["exec"] != "ask" || s.ToolRules["os_exec(uptime *)"] != "" {
		t.Errorf("db1 changed with web1: %v %v", s.Permissions, s.ToolRules)
	}
	if out := l.must("permissions", "web1"); !strings.Contains(out, "exec         allow") || !strings.Contains(out, "os_exec(uptime *)") {
		t.Errorf("permissions web1 shows:\n%s", out)
	}
	// A fresh copy takes the master as it is now, clamped again.
	l.must("permissions", "web1", "--from", "master")
	if s := l.schemeOf(web1.id); s.Permissions["exec"] != "ask" || s.Permissions["web"] != "deny" || s.Permissions["send"] != "ask" || s.ToolRules["os_exec(uptime *)"] != "" {
		t.Errorf("web1 after --from master: %v %v", s.Permissions, s.ToolRules)
	}
	for _, bad := range [][]string{
		{"permissions", "web1", "set", "exec", "sometimes"}, {"permissions", "web1", "set", "root", "allow"},
		{"permissions", "web1", "add", "os_exec(*)", "allow"}, {"permissions", "web1", "add", "*", "allow"}, {"permissions", "web1", "rm", "no_such_rule"},
		{"permissions", "web1", "--from", "elsewhere"}, {"permissions", "--defaults", "set", "exec", "allow"},
		{"permissions", "--defaults", "add", "x", "maybe"},
	} {
		before := l.saved()
		if _, err := l.run(bad...); err == nil {
			t.Errorf("tenant fleet %s was accepted", strings.Join(bad, " "))
		}
		if after := l.saved(); string(after.FleetRules) != string(before.FleetRules) || string(after.FleetSchemes[web1.id]) != string(before.FleetSchemes[web1.id]) {
			t.Errorf("tenant fleet %s changed the file", strings.Join(bad, " "))
		}
	}
}

// Revoking keeps or deletes the scheme, and a machine enrolled under the old
// name starts clean unless it adopts the old id.
func TestFleetRevokeAndAdopt(t *testing.T) {
	l := newFleetLab(t)
	l.settings(settings{Permissions: map[string]string{"web": "allow"}})
	l.master()
	old := l.enroll("web1", fleet.Reported{})
	l.confirm("web1", old)
	l.must("permissions", "web1", "add", "os_exec(uptime *)", "allow")
	l.must("rename", "web1", "edge1")
	if m, _ := fleet.Open(l.cfg).Machine(old.id); m.Name != "edge1" {
		t.Fatalf("renamed to %q", m.Name)
	}
	if l.schemeOf(old.id).ToolRules["os_exec(uptime *)"] != "allow" {
		t.Fatal("the rename lost the scheme: it is kept under the id")
	}
	l.must("rename", "edge1", "web1")

	l.answer = "keep" // asked at the terminal
	if out := l.must("revoke", "web1"); !strings.Contains(out, "kept under its id") {
		t.Fatalf("revoke said:\n%s", out)
	}
	if m, _ := fleet.Open(l.cfg).Machine(old.id); m.State != fleet.StateRevoked {
		t.Fatalf("the machine is %s", m.State)
	}
	if l.schemeOf(old.id).ToolRules["os_exec(uptime *)"] != "allow" {
		t.Fatal("a kept scheme was not kept")
	}

	// Under the old name, clean: a new id and a new scheme.
	fresh := l.enroll("web1", fleet.Reported{})
	if fresh.id == old.id {
		t.Fatal("a machine enrolled under an old name took the old id without being asked to")
	}
	l.confirm("web1", fresh)
	if l.schemeOf(fresh.id).ToolRules["os_exec(uptime *)"] != "" {
		t.Fatal("a clean machine started with the old machine's rules")
	}
	l.must("revoke", "web1", "--delete-scheme")
	if _, ok := l.saved().FleetSchemes[fresh.id]; ok {
		t.Fatal("a deleted scheme is still in the settings file")
	}
	if _, err := fleet.Open(l.cfg).Machine(fresh.id); !errors.Is(err, fleet.ErrUnknown) {
		t.Fatalf("a removed machine's record: %v", err)
	}

	// Adopting the old id: the kept scheme stands, and the screen says so.
	l.must("enroll", "web1", "--adopt", old.id) // exercised below through labChild
	adopted := l.enrollAdopting("web1", old.id)
	if adopted.id != old.id {
		t.Fatalf("adopted id %s, want %s", adopted.id, old.id)
	}
	l.answer = ""
	screen, _ := l.run("confirm", "web1")
	if !strings.Contains(screen, "kept under this id") || !strings.Contains(screen, "os_exec(uptime *)") {
		t.Fatalf("the confirm screen of an adopted id:\n%s", screen)
	}
	l.confirm("web1", adopted)
	if l.schemeOf(old.id).ToolRules["os_exec(uptime *)"] != "allow" {
		t.Fatal("adoption lost the kept scheme")
	}
	// A revoked machine is revoked by its id; with the scheme deleted, its record goes.
	l.must("revoke", "web1", "--keep-scheme")
	l.must("revoke", old.id, "--delete-scheme")
	if _, ok := l.saved().FleetSchemes[old.id]; ok {
		t.Fatal("the scheme of a removed machine is still there")
	}
}

func (l *fleetLab) enrollAdopting(name, id string) labChild {
	l.t.Helper()
	code := strings.SplitN(l.must("enroll", name, "--adopt", id), "\n", 2)[0]
	dir := l.t.TempDir()
	ident, err := fleet.MintIdentity(fleet.ChildIdentityPath(dir))
	if err != nil {
		l.t.Fatal(err)
	}
	child, six, err := fleet.Enroll(context.Background(), code, ident, fleet.Reported{OS: "linux", Sandbox: "host", Ceiling: "full"})
	if err != nil {
		l.t.Fatal(err)
	}
	if err := fleet.SaveChild(fleet.ChildPath(dir), child); err != nil {
		l.t.Fatal(err)
	}
	return labChild{dir: dir, id: child.ID, code: six}
}

// --- the link, the origins and the live socket ---

// A confirmed machine's call asks the master, which decides by that
// machine's scheme and shows the host on the card; a relay that was not
// opened and a cron job without the machine on its list reach nothing; and a
// revoke by the command line closes the socket the hub holds.
func TestFleetCallsOriginsAndRevoke(t *testing.T) {
	l := newFleetLab(t)
	mgr := l.master()
	ch := l.enroll("web1", fleet.Reported{})
	tools := &labTools{}
	client, ran := ch.start(t, tools)
	l.confirm("web1", ch)
	l.sync() // the settings watcher's tick: the scheme the command line saved
	eventually(t, "the confirmed machine links", func() bool { return client.Status().State == fleet.LinkConnected && online(mgr, ch.id) })
	mach := approval.Machine{ID: ch.id, Name: "web1"}

	var cards []control.ApprovalRequest
	var cardMu sync.Mutex
	l.broker.SetAsk(func(_ context.Context, req control.ApprovalRequest) control.ApprovalDecision {
		cardMu.Lock()
		defer cardMu.Unlock()
		cards = append(cards, req)
		return control.ApproveOnce
	})
	call := func(ctx context.Context) (fleet.Result, error) {
		d := defaultFleet.For(fleetOrigin(ctx), mach)
		if err := d.Reach(); err != nil {
			return fleet.Result{}, err
		}
		return mgr.Call(ctx, ch.id, fleet.Call{Tool: "os_exec", Confirm: d.Confirm})
	}

	// Local: the machine's scheme asks (exec is no looser than ask), at the host.
	res, err := call(context.Background())
	if err != nil || res.IsError || res.Text != "Linux" {
		t.Fatalf("a local call: %+v %v", res, err)
	}
	if len(cards) != 1 || cards[0].Host != ch.id || !strings.HasPrefix(cards[0].Detail, `[host `+ch.id+` "web1"] `) {
		t.Fatalf("the card does not name the host: %+v", cards)
	}

	// A relay that was not opened reaches nothing, a read included.
	discord := newDiscordApprovalBroker(slog.Default())
	fleetRelayBroker(surfaceDiscord, discord)
	relayCtx := withExecSurface(context.Background(), surfaceDiscord)
	before := tools.calls.Load()
	if _, err := call(relayCtx); err == nil || !strings.Contains(err.Error(), "not opened for the fleet") {
		t.Fatalf("a Discord turn with the switch off: %v", err)
	}
	if _, err := call(withExecSurface(context.Background(), surfaceIMessage)); err == nil {
		t.Fatal("an iMessage turn with no broker and no switch reached the machine")
	}
	if tools.calls.Load() != before {
		t.Fatal("a refused origin's call reached the machine")
	}
	// Opened by the command line; the hub sees config.json change.
	l.must("relay", "discord", "on")
	eventually(t, "the hub sees the relay opened", func() bool { return fleetOrigin(relayCtx).Open })
	if o := fleetOrigin(withExecSurface(context.Background(), surfaceIMessage)); o.Open {
		t.Fatal("opening Discord opened iMessage")
	}
	// Open, the relay's own rules still stand: exec mode is off there.
	discord.SetLock(relayExecLocks)
	if res, err := call(relayCtx); err != nil || !res.IsError {
		t.Fatalf("a Discord turn whose relay refuses exec ran the command: %+v %v", res, err)
	}
	l.must("relay", "discord", "off")
	eventually(t, "the hub sees the relay closed", func() bool { return !fleetOrigin(relayCtx).Open })

	// Cron: only the machines on the job's list, and nobody is asked.
	cronCtx := func(ids ...string) context.Context {
		return withFleetMachines(withExecSurface(context.Background(), surfaceCronExec), ids)
	}
	if _, err := call(cronCtx()); err == nil {
		t.Fatal("a cron job with no machine list reached a machine")
	}
	if _, err := call(cronCtx("m0123456789abcdef")); err == nil || !strings.Contains(err.Error(), "not one this cron job may reach") {
		t.Fatalf("a cron job reached a machine that is not on its list: %v", err)
	}
	asked := len(cards)
	if res, err := call(cronCtx(ch.id)); err != nil || !res.IsError {
		t.Fatalf("an unattended job's action was approved: %+v %v", res, err)
	}
	if len(cards) != asked {
		t.Fatal("an unattended origin asked somebody")
	}
	// Both checks must pass. With the machine's scheme allowing exec, a job
	// that was opted into exec runs the command (the cron-exec approver's own
	// rules allow it), and a job that was not still does not.
	l.must("permissions", "web1", "set", "exec", "allow")
	l.sync()
	if res, err := call(withOffsiteConfirm(cronCtx(ch.id), cronExecApprover(l.cfg, l.data))); err != nil || res.IsError {
		t.Fatalf("an exec job on a machine that allows exec: %+v %v", res, err)
	}
	if res, err := call(cronCtx(ch.id)); err != nil || !res.IsError {
		t.Fatalf("a job that was not opted into exec ran a command: %+v %v", res, err)
	}
	if len(cards) != asked {
		t.Fatal("an unattended origin asked somebody")
	}
	// An unattended turn with no surface of its own (an alert's investigation)
	// is stamped with no machines, and reaches none.
	if o := fleetOrigin(withFleetMachines(context.Background(), nil)); o.Open || o.Name != "cron" || o.Asker != nil {
		t.Fatalf("an unattended turn with no machine list: %+v", o)
	}
	if o := fleetOrigin(withExecSurface(context.Background(), "somewhere-new")); o.Open {
		t.Fatal("an origin nobody knows is open")
	}

	// Revoked by the command line: the hub closes the socket it holds.
	l.must("revoke", "web1", "--keep-scheme")
	select {
	case err := <-ran:
		if !errors.Is(err, fleet.ErrRevoked) {
			t.Fatalf("the revoked child stopped with %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the revoked machine's link was not closed")
	}
	// A cron job that names the revoked machine simply fails.
	if _, err := call(cronCtx(ch.id)); !errors.Is(err, fleet.ErrRevoked) {
		t.Fatalf("a cron job naming a revoked machine: %v, want ErrRevoked", err)
	}
	if _, err := call(context.Background()); !errors.Is(err, fleet.ErrRevoked) {
		t.Fatalf("a local call to a revoked machine: %v", err)
	}
}

// A spent code presented again is refused, and the operator is told.
func TestFleetReplayedCodeRaisesTheAlert(t *testing.T) {
	l := newFleetLab(t)
	l.master()
	ch := l.enroll("web1", fleet.Reported{})
	thief, err := fleet.MintIdentity(fleet.ChildIdentityPath(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fleet.Enroll(context.Background(), ch.enrollCode, thief, fleet.Reported{OS: "linux", Sandbox: "host", Ceiling: "full"}); err == nil {
		t.Fatal("a spent code enrolled a second machine")
	}
	eventually(t, "the alert reaches the feed", func() bool {
		for _, s := range l.told() {
			if strings.Contains(s, "ALERT") && strings.Contains(s, "web1") && strings.Contains(s, ch.id) {
				return true
			}
		}
		return false
	})
	// A code nobody was given is refused without an alert.
	alerts := func() (n int) {
		for _, s := range l.told() {
			if strings.Contains(s, "ALERT") {
				n++
			}
		}
		return n
	}
	had := alerts()
	// The same code with another secret: well formed, and never given out.
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(ch.enrollCode, "tntfleet1_"))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	fields["s"] = strings.Repeat("A", 43)
	raw, _ = json.Marshal(fields)
	forged := "tntfleet1_" + base64.RawURLEncoding.EncodeToString(raw)
	if _, _, err := fleet.Enroll(context.Background(), forged, thief, fleet.Reported{OS: "linux", Sandbox: "host", Ceiling: "full"}); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a forged code: %v, want the master's refusal", err)
	}
	if alerts() != had {
		t.Fatal("an unknown code raised the replay alert")
	}
	if all, _ := fleet.Open(l.cfg).Machines(); len(all) != 1 {
		t.Fatalf("the refused attempts left %d machines, want the one that enrolled", len(all))
	}
}

// --- the listener's life ---

// A running Tenant takes the fleet when init makes it a master, with no
// restart; follows a new bind address; and a second holder is told who holds
// the fleet.
func TestFleetHubTakesTheFleetWhenInitRuns(t *testing.T) {
	l := newFleetLab(t)
	l.stop = startFleetHub(context.Background(), l.c, "tui", newApprovalBroker(slog.Default()), l.tell, slog.Default())
	if fleetManager() != nil || fleetStore() != nil {
		t.Fatal("a Tenant that was never made a master holds a fleet")
	}
	if _, held := fleet.CurrentHolder(l.cfg); held {
		t.Fatal("somebody holds a fleet that does not exist")
	}
	if entries, _ := os.ReadDir(l.cfg); len(entries) != 0 {
		t.Fatalf("looking for a fleet left files behind: %v", entries)
	}
	if o := fleetOrigin(context.Background()); !o.Open || o.Name != "local" {
		t.Fatalf("the local origin: %+v", o)
	}

	out := l.must("init", "--listen", "127.0.0.1:0")
	if !strings.Contains(out, "now a fleet master") {
		t.Fatalf("init said:\n%s", out)
	}
	eventually(t, "the running hub takes the fleet", func() bool { return fleetManager() != nil })
	if fleetStore() == nil {
		t.Fatal("a master has no store")
	}
	first := fleetManager().Addr().String()
	h, held := fleet.CurrentHolder(l.cfg)
	if !held || h.PID != os.Getpid() || h.Role != "tui" || h.Address != first {
		t.Fatalf("the holder on record: %+v %v", h, held)
	}
	// The terminal app holding the fleet says what that means.
	said := strings.Join(l.told(), "\n")
	if !strings.Contains(said, "listening on "+first) || !strings.Contains(said, "tenant serve") {
		t.Fatalf("the operator was told:\n%s", said)
	}
	// init again replaces nothing.
	id1, _ := fleetIdentity(l.cfg)
	if out := l.must("init"); !strings.Contains(out, "already") || !strings.Contains(out, "tenant tui (process") {
		t.Fatalf("a second init said:\n%s", out)
	}
	if id2, _ := fleetIdentity(l.cfg); id2.Fingerprint != id1.Fingerprint {
		t.Fatal("a second init replaced the master's identity")
	}
	if out := l.must("list"); !strings.Contains(out, "tenant tui (process") || !strings.Contains(out, "no machines yet") {
		t.Fatalf("list said:\n%s", out)
	}

	// A second process that wants the fleet is told who holds it.
	_, err := fleet.StartManager(fleet.ManagerConfig{Store: fleet.Open(l.cfg), Identity: id1, Addr: "127.0.0.1:0", Role: "serve"})
	var heldErr *fleet.HeldError
	if !errors.As(err, &heldErr) || heldErr.Holder.PID != os.Getpid() || heldErr.Holder.Role != "tui" {
		t.Fatalf("a second holder: %v", err)
	}

	// A new bind address is followed without a restart.
	free := freeLoopbackAddr(t)
	l.must("init", "--listen", free)
	eventually(t, "the hub moves to the new address", func() bool {
		mgr := fleetManager()
		return mgr != nil && mgr.Addr().String() == free
	})

	l.stop()
	l.stop = nil
	if fleetManager() != nil {
		t.Fatal("a stopped hub still holds the fleet")
	}
	if _, held := fleet.CurrentHolder(l.cfg); held {
		t.Fatal("the fleet is still held after its holder stopped")
	}
	if out := l.must("list"); !strings.Contains(out, "no Tenant process holds the fleet") || !strings.Contains(out, "tenant serve") {
		t.Fatalf("list with nobody holding the fleet said:\n%s", out)
	}
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	id, err := fleet.MintIdentity(fleet.MasterIdentityPath(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	m, err := fleet.StartManager(fleet.ManagerConfig{Store: fleet.Open(dir), Identity: id, Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	addr := m.Addr().String()
	_ = m.Close()
	return addr
}

// enroll runs init first; what it prints is the code alone on standard
// output, and a paste that puts the code on the new machine's standard input.
func TestFleetEnrollRunsInitAndPrintsThePaste(t *testing.T) {
	l := newFleetLab(t)
	// A command-line process holds no fleet, and a turn in a process without
	// one reaches no machine.
	if fleetManager() != nil || fleetStore() != nil || fleetHeldElsewhere() != "" || fleetOrigin(context.Background()).Open {
		t.Fatal("a process that never started a hub has a fleet")
	}
	// No address can be guessed for a port nobody chose yet.
	if err := persistLaunchConfig(l.cfg, nil, func(lc *launchConfig) { lc.Fleet.Listen = "127.0.0.1:0" }); err != nil {
		t.Fatal(err)
	}
	if _, err := l.run("enroll", "web1"); err == nil || !strings.Contains(err.Error(), "no address") {
		t.Fatalf("enroll with no address to give a machine: %v", err)
	}
	if _, err := fleetIdentity(l.cfg); err != nil {
		t.Fatalf("enroll did not run init: %v", err)
	}
	l.must("init", "--address", "master.example:9777", "--address", "10.0.0.1:9777")
	stderr := captureStderr(t, func() {
		out := l.must("enroll", "web1", "--ttl", "5m")
		if strings.Contains(out, "\n") || !strings.HasPrefix(out, "tntfleet1_") {
			t.Errorf("standard output is not the code alone:\n%s", out)
		}
	})
	for _, want := range []string{"| tenant child install", "printf '%s\\n' 'tntfleet1_", "expires in 5m0s", "tenant fleet confirm web1", "tenant serve"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the instructions lack %q:\n%s", want, stderr)
		}
	}
	if regexp.MustCompile(`tenant child install\s+\S*tntfleet1_`).MatchString(stderr) {
		t.Errorf("the paste passes the code as an argument:\n%s", stderr)
	}
	if addrs, _ := fleet.Open(l.cfg).Addresses(); len(addrs) != 2 || addrs[0] != "master.example:9777" {
		t.Errorf("the addresses machines are told: %v", addrs)
	}
	// The default guess names this computer on the listen port, and nothing
	// fleet.json would refuse.
	for _, a := range fleetDefaultAddresses("0.0.0.0:9777") {
		if !strings.HasSuffix(a, ":9777") || strings.HasPrefix(a, "127.") || strings.HasPrefix(a, "0.0.0.0") {
			t.Errorf("a default address %q", a)
		}
	}
	if got := fleetDefaultAddresses("192.0.2.7:9000"); len(got) != 1 || got[0] != "192.0.2.7:9000" {
		t.Errorf("a named bind host: %v", got)
	}
	if got := fleetDefaultAddresses("127.0.0.1:0"); got != nil {
		t.Errorf("an address for port 0: %v", got)
	}
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	var b bytes.Buffer
	prev := fleetNotes
	fleetNotes = &b
	defer func() { fleetNotes = prev }()
	fn()
	return b.String()
}

// The fleet's files are on the list doctor audits, and the file tools refuse
// them: reading where they hold something, writing for the locks.
func TestFleetFilesAreGuarded(t *testing.T) {
	l := newFleetLab(t)
	l.master()
	managed, err := managedSecrets(l.cfg, l.data)
	if err != nil {
		t.Fatal(err)
	}
	roots := resolveOSRoots(&pluginFlags{}, l.cfg, l.data, &launchConfig{})
	for _, name := range []string{"fleet.json", "fleet-identity.pem", "fleet.holder"} {
		path := filepath.Join(l.cfg, name)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		listed := false
		for _, m := range managed {
			listed = listed || m.path == path
		}
		if !listed {
			t.Errorf("%s is not a managed secret", name)
		}
		if roots.Check(path, false) == nil || roots.Check(path, true) == nil {
			t.Errorf("the file tools may read or write %s", name)
		}
	}
	for _, name := range []string{"fleet.json.lock", "fleet.holder.lock"} {
		path := filepath.Join(l.cfg, name)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if roots.Check(path, true) == nil {
			t.Errorf("the file tools may write %s", name)
		}
	}
	if res := checkSecretProtection(context.Background(), &doctorEnv{c: l.c}); res.Status != statusOK {
		t.Errorf("doctor on a master's fleet files (%s): %s", runtime.GOOS, res.Detail)
	}
}

// The terminal's control runs the same operations.
func TestFleetTUIControl(t *testing.T) {
	l := newFleetLab(t)
	l.settings(settings{Permissions: map[string]string{"exec": "allow"}})
	l.master()
	ctl := newFleetTUIControl(l.c, l.stg, &l.stgMu)
	ch := l.enroll("web1", fleet.Reported{})
	tools := &labTools{}
	client, _ := ch.start(t, tools)

	if st := ctl.Status(); !strings.Contains(st, "this process holds the fleet") || !strings.Contains(st, "waits for confirmation") || !strings.Contains(st, `"linux"`) {
		t.Fatalf("/fleet:\n%s", st)
	}
	screen, err := ctl.Confirm("web1", "", []string{"exec=allow"})
	if err != nil || !strings.Contains(screen, "Reported by the machine") || !strings.Contains(screen, "set by you") || !strings.Contains(screen, "/fleet confirm web1 <six digits> exec=allow") {
		t.Fatalf("the confirm screen: %v\n%s", err, screen)
	}
	if m, _ := fleet.Open(l.cfg).Machine(ch.id); m.State != fleet.StatePending {
		t.Fatal("looking at the confirm screen confirmed the machine")
	}
	if _, err := ctl.Confirm("web1", "000000", nil); !errors.Is(err, fleet.ErrWrongCode) && ch.code != "000000" {
		t.Fatalf("the wrong digits: %v", err)
	}
	if _, err := ctl.Confirm("web1", ch.code, []string{"exec=allow", "root=allow"}); err == nil {
		t.Fatal("a pair that is not category=mode was taken")
	}
	// Digits typed with a space in them are the same digits.
	if out, err := ctl.Confirm("web1", ch.code[:3]+" "+ch.code[3:], []string{"exec=allow"}); err != nil || !strings.Contains(out, "confirmed web1") {
		t.Fatalf("confirm: %v %s", err, out)
	}
	// Saved by this process, for that machine: the scheme is live at once.
	if s, err := defaultFleet.Scheme(ch.id); err != nil || s.Permissions["exec"] != "allow" || s.Permissions["write"] != "ask" {
		t.Fatalf("the machine's scheme: %v %v", s, err)
	}
	if l.schemeOf(ch.id).Permissions["exec"] != "allow" {
		t.Fatal("the scheme was not saved to the settings file")
	}
	eventually(t, "the machine shows online", func() bool { return strings.Contains(ctl.Status(), "online since") })
	show, err := ctl.Show("web1")
	if err != nil || !strings.Contains(show, "link version") || !strings.Contains(show, "Approval scheme") {
		t.Fatalf("/fleet show: %v\n%s", err, show)
	}
	if os.Getenv("TENANT_FLEETCTL_SHOW") != "" {
		t.Logf("/fleet\n%s\n\n/fleet show web1\n%s", ctl.Status(), show)
	}

	// The machine's scheme in the permissions editor, and the fleet's rules.
	pc, err := ctl.Perms("web1")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := pc.SetPermission("write", "allow"); !ok || err != nil {
		t.Fatalf("set write: %v %v", ok, err)
	}
	if l.schemeOf(ch.id).Permissions["write"] != "allow" || l.saved().Permissions["write"] == "allow" {
		t.Fatal("the editor's change is not the machine's alone")
	}
	if err := ctl.Defaults().SetRule("os_exec(uptime *)", "allow"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(l.saved().FleetRules), "uptime") || l.schemeOf(ch.id).ToolRules["os_exec(uptime *)"] != "" {
		t.Fatal("a fleet rule was not saved, or moved a confirmed machine")
	}
	if _, err := ctl.Perms("nobody"); err == nil {
		t.Fatal("permissions of a machine that is not there")
	}

	// The relays' switches.
	if out, err := ctl.Relay(nil); err != nil || strings.Count(out, "off:") != 2 {
		t.Fatalf("/fleet relay: %v %s", err, out)
	}
	if _, err := ctl.Relay([]string{"imessage", "on"}); err != nil {
		t.Fatal(err)
	}
	fleetRelayBroker(surfaceIMessage, newOffsiteApprovalBroker(slog.Default(), l.broker))
	if o := fleetOrigin(withExecSurface(context.Background(), surfaceIMessage)); !o.Open || o.Name != "imessage" {
		t.Fatalf("iMessage after /fleet relay imessage on: %+v", o)
	}
	if lc, _ := loadLaunchConfig(l.cfg); !lc.IMessage.AllowFleet || lc.Relay.AllowFleet {
		t.Fatalf("config.json: imessage %v relay %v", lc.IMessage.AllowFleet, lc.Relay.AllowFleet)
	}

	// Revoke asks first, then does.
	if out, err := ctl.Revoke("web1", ""); err != nil || !strings.Contains(out, "/fleet revoke web1 keep") {
		t.Fatalf("/fleet revoke: %v %s", err, out)
	}
	if _, err := ctl.Revoke("web1", "maybe"); err == nil {
		t.Fatal("revoke took an answer that is neither keep nor delete")
	}
	if _, err := ctl.Revoke("nobody", "keep"); err == nil {
		t.Fatal("revoked a machine that is not there")
	}
	if out, err := ctl.Revoke("web1", "delete"); err != nil || !strings.Contains(out, "deleted") {
		t.Fatalf("/fleet revoke web1 delete: %v %s", err, out)
	}
	if _, err := defaultFleet.Scheme(ch.id); err == nil {
		t.Fatal("the deleted scheme is still loaded")
	}
	eventually(t, "the revoked machine's link is closed", func() bool { return client.Status().State == fleet.LinkRevoked })
}

// A listener that cannot start says why, where the operator looks, and is
// tried again: nothing has to be restarted once the cause is gone.
func TestFleetHubSaysWhyItIsNotListening(t *testing.T) {
	l := newFleetLab(t)
	l.must("init", "--listen", "127.0.0.1:0")
	path := fleet.MasterIdentityPath(l.cfg)
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("-----BEGIN CERTIFICATE-----\nnot one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l.stop = startFleetHub(context.Background(), l.c, "serve", newApprovalBroker(slog.Default()), l.tell, slog.Default())
	if fleetManager() != nil {
		t.Fatal("a master with an identity that does not load is listening")
	}
	if said := strings.Join(l.told(), "\n"); !strings.Contains(said, "fleet: listener not started") {
		t.Fatalf("the operator was told:\n%s", said)
	}
	ctl := newFleetTUIControl(l.c, &settings{}, &l.stgMu)
	if st := ctl.Status(); !strings.Contains(st, "the fleet listener is not up") {
		t.Fatalf("/fleet:\n%s", st)
	}
	// A bind address nobody can listen on is the same kind of failure.
	if err := os.WriteFile(path, good, 0o600); err != nil {
		t.Fatal(err)
	}
	l.must("init", "--listen", "192.0.2.1:1") // TEST-NET-1: not an address of this computer
	eventually(t, "the hub reports the address it cannot bind", func() bool {
		return strings.Contains(strings.Join(l.told(), "\n"), "192.0.2.1:1")
	})
	if fleetManager() != nil {
		t.Fatal("listening on an address this computer does not have")
	}
	l.must("init", "--listen", "127.0.0.1:0")
	eventually(t, "the hub comes up once the cause is gone", func() bool { return fleetManager() != nil })
	if st := ctl.Status(); !strings.Contains(st, "this process holds the fleet") {
		t.Fatalf("/fleet after the recovery:\n%s", st)
	}

	// A config.json that stops loading closes the relays, and the listener
	// stays where it is: it does not fall back to the default address.
	l.must("relay", "discord", "on")
	fleetRelayBroker(surfaceDiscord, newDiscordApprovalBroker(slog.Default()))
	relayCtx := withExecSurface(context.Background(), surfaceDiscord)
	eventually(t, "the relay is opened", func() bool { return fleetOrigin(relayCtx).Open })
	addr := fleetManager().Addr().String()
	if err := os.WriteFile(launchConfigPath(l.cfg), []byte(`{"relay": {"allow_fleet": true`), 0o600); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a config.json that does not load closes the relay", func() bool { return !fleetOrigin(relayCtx).Open })
	if mgr := fleetManager(); mgr == nil || mgr.Addr().String() != addr {
		t.Fatal("a config.json that does not load moved the listener")
	}
}

// /cron fleet opens a job for machines by name and keeps their ids: a rename
// does not move the job, a machine that is not there or is revoked is not
// taken, and "none" closes the job again.
func TestCronJobFleetList(t *testing.T) {
	l := newFleetLab(t)
	l.master()
	web1 := l.enroll("web1", fleet.Reported{})
	l.confirm("web1", web1)
	gone := l.enroll("old1", fleet.Reported{})
	l.confirm("old1", gone)
	l.must("revoke", "old1", "--keep-scheme")

	engine := croneng.NewEngine(nil, nil, nil)
	job, err := engine.Add(croneng.AddSpec{Spec: "@every 1h", Prompt: "check the disks", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	ctl := tuiCron{&cronManager{base: context.Background(), engine: engine}}
	if jobs := ctl.Jobs(); len(jobs) != 1 || len(jobs[0].Fleet) != 0 {
		t.Fatalf("a new job reaches fleet machines: %+v", jobs)
	}
	for _, bad := range [][]string{{"nobody"}, {"old1"}, {gone.id}, {"web1", "nobody"}} {
		if _, err := ctl.SetFleet(job.ID, bad); err == nil {
			t.Errorf("the job was opened for %v", bad)
		}
	}
	if _, err := ctl.SetFleet("nope", []string{"web1"}); err == nil {
		t.Error("a job that is not there was opened for the fleet")
	}
	if got := engine.List()[0].Fleet; len(got) != 0 {
		t.Fatalf("the refused lists left %v on the job", got)
	}
	view, err := ctl.SetFleet(job.ID, []string{"WEB1", web1.id})
	if err != nil || len(view.Fleet) != 1 || view.Fleet[0] != "web1" {
		t.Fatalf("SetFleet: %+v %v", view, err)
	}
	if got := engine.List()[0].Fleet; len(got) != 1 || got[0] != web1.id {
		t.Fatalf("the job keeps %v, want the machine's id", got)
	}
	l.must("rename", "web1", "edge1")
	if jobs := ctl.Jobs(); len(jobs[0].Fleet) != 1 || jobs[0].Fleet[0] != "edge1" {
		t.Fatalf("after the rename the job shows %v", jobs[0].Fleet)
	}
	// A machine revoked after the job was opened for it shows as that.
	l.must("revoke", "edge1", "--keep-scheme")
	if jobs := ctl.Jobs(); !strings.Contains(jobs[0].Fleet[0], "revoked") {
		t.Fatalf("a revoked machine on a job shows as %v", jobs[0].Fleet)
	}
	if view, err := ctl.SetFleet(job.ID, nil); err != nil || len(view.Fleet) != 0 || len(engine.List()[0].Fleet) != 0 {
		t.Fatalf("closing the job: %+v %v", view, err)
	}
}
