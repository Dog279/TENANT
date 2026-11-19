package approval

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"tenant/internal/control"
	"tenant/internal/logs"
)

// fleetRig is a master with a fleet: the host's broker, with a read-only
// command judgement installed and rules of its own (standing in for
// DefaultRules), and one machine per id, each asking for everything.
type fleetRig struct {
	t      *testing.T
	host   *Broker
	master *Rules
	fleet  *Fleet

	// asker is an attended surface that answers every prompt with answer and
	// keeps what it was shown: a relay's broker with a backend, used wherever a
	// test must know for certain whether the operator was asked.
	asker  *Broker
	answer control.ApprovalDecision
	shown  []control.ApprovalRequest
}

func newFleetRig(t *testing.T, ids ...string) *fleetRig {
	t.Helper()
	r := &fleetRig{t: t, host: newTestBroker(testLog()), master: NewRules(), fleet: NewFleet(testCategorize), answer: control.DenyOnce}
	r.host.SetRules(r.master)
	r.host.SetReadOnly(func(c string) bool { return strings.HasPrefix(c, "ls") })
	r.asker = newTestDiscordBroker(testLog())
	r.asker.SetRules(NewRules())
	r.asker.SetAsk(func(_ context.Context, req control.ApprovalRequest) control.ApprovalDecision {
		r.shown = append(r.shown, req)
		return r.answer
	})
	for _, id := range ids {
		if err := r.fleet.Put(id, Scheme{}); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

// attended is an origin that checks nothing of its own and asks through the
// rig's asker: the local origin, with a backend a test can count.
func (r *fleetRig) attended() Origin { return Origin{Name: "local", Open: true, Asker: r.asker} }

// confirm asks the decision-maker for (o, id) and reports its answer and how
// many prompts it took.
func (r *fleetRig) confirm(o Origin, id, action, detail string) (ok bool, prompts int) {
	r.t.Helper()
	before := len(r.shown)
	ok = r.fleet.For(o, Machine{ID: id, Name: "box-" + id}).Confirm(context.Background(), action, detail)
	return ok, len(r.shown) - before
}

func (r *fleetRig) scheme(id string) Scheme {
	r.t.Helper()
	s, err := r.fleet.Scheme(id)
	if err != nil {
		r.t.Fatal(err)
	}
	return s
}

// rested gives machine id its whole allowance of prompts back, for a test
// that asks more often than a machine may.
func (r *fleetRig) rested(id string) {
	r.t.Helper()
	b, err := r.fleet.Child(id)
	if err != nil {
		r.t.Fatal(err)
	}
	b.limit.mu.Lock()
	b.limit.spent = 0
	b.limit.mu.Unlock()
}

// schemeJSON is a scheme as the settings file would hold it, for comparing.
func schemeJSON(s Scheme) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// "Always" on a machine's card saves into that machine's scheme and nowhere
// else: not into another machine's, not into the master's rules or modes. And
// a rule the master gains later does not reach a machine confirmed before it.
func TestFleet_AlwaysStaysOnItsMachine(t *testing.T) {
	r := newFleetRig(t, "aaaa", "bbbb")
	bBefore := schemeJSON(r.scheme("bbbb"))

	// A command: its family is saved as a command rule for aaaa.
	r.answer = control.ApproveAlways
	if ok, n := r.confirm(r.attended(), "aaaa", "os_exec", "systemctl status nginx"); !ok || n != 1 {
		t.Fatalf("always on aaaa: ok %v after %d prompt(s), want approved after one", ok, n)
	}
	if got := r.scheme("aaaa").ToolRules["os_exec(systemctl status *)"]; got != "allow" {
		t.Fatalf("aaaa's rules = %v, want the command's family allowed", r.scheme("aaaa").ToolRules)
	}
	// A category: web turns to allow on aaaa.
	if ok, _ := r.confirm(r.attended(), "aaaa", "web_interact", "click Buy"); !ok {
		t.Fatal("always must approve")
	}
	// A prompt a rule forced: that rule turns to allow on aaaa.
	if err := r.fleet.Put("aaaa", Scheme{Permissions: r.scheme("aaaa").Permissions, ToolRules: map[string]string{
		"os_exec(systemctl status *)": "allow", "x_post": "ask"}}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := r.confirm(r.attended(), "aaaa", "x_post", "hello"); !ok {
		t.Fatal("always must approve")
	}
	a := r.scheme("aaaa")
	if a.Permissions[CatWeb] != "allow" || a.ToolRules["x_post"] != "allow" || a.Permissions[CatExec] != "ask" {
		t.Fatalf("aaaa after three always answers: %+v", a)
	}

	// Nothing else moved.
	if got := schemeJSON(r.scheme("bbbb")); got != bBefore {
		t.Fatalf("bbbb's scheme moved:\n%s\nwas\n%s", got, bBefore)
	}
	if rules := r.master.Snapshot(); len(rules) != 0 {
		t.Fatalf("the master gained rules: %v", rules)
	}
	for cat, mode := range r.host.Modes() {
		if mode != "ask" {
			t.Fatalf("the master's %s turned to %s", cat, mode)
		}
	}

	// aaaa no longer asks; bbbb and the master still do.
	r.answer = control.DenyOnce
	for _, tc := range [][2]string{{"os_exec", "systemctl status sshd"}, {"web_interact", "click"}, {"x_post", "again"}} {
		if ok, n := r.confirm(r.attended(), "aaaa", tc[0], tc[1]); !ok || n != 0 {
			t.Errorf("aaaa %s: ok %v, %d prompt(s); its saved answer must stand", tc[0], ok, n)
		}
		if ok, n := r.confirm(r.attended(), "bbbb", tc[0], tc[1]); ok || n != 1 {
			t.Errorf("bbbb %s: ok %v, %d prompt(s); aaaa's answer must not carry", tc[0], ok, n)
		}
		r.host.SetAsk(r.asker.ask) // the master asks through the same counting surface
		before := len(r.shown)
		if ok := r.host.Confirm(context.Background(), tc[0], tc[1]); ok || len(r.shown)-before != 1 {
			t.Errorf("master %s: ok %v, %d prompt(s); aaaa's answer must not carry", tc[0], ok, len(r.shown)-before)
		}
	}

	// A machine confirmed now, then a rule the master gains: the machine asks.
	if err := r.fleet.Put("cccc", mustInherit(t, r.fleet, Scheme{ToolRules: r.master.Snapshot()})); err != nil {
		t.Fatal(err)
	}
	if err := r.master.Set("os_exec(make test *)", "allow"); err != nil {
		t.Fatal(err)
	}
	if !r.host.Confirm(context.Background(), "os_exec", "make test -v") {
		t.Fatal("the master's own rule must allow it on the master")
	}
	if ok, n := r.confirm(r.attended(), "cccc", "os_exec", "make test -v"); ok || n != 1 {
		t.Fatalf("cccc: ok %v, %d prompt(s); a later master rule must not reach it", ok, n)
	}
}

func mustInherit(t *testing.T, f *Fleet, local Scheme) Scheme {
	t.Helper()
	s, err := f.Inherit(local)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The two checks: for every pair of what the origin says and what the
// machine's scheme says, deny beats ask beats allow, and from an origin with
// nobody to ask, ask is deny. No prompt is ever raised for a denial, and
// never from an unattended origin.
func TestFleet_TwoChecks(t *testing.T) {
	modes := []Mode{ModeAllow, ModeAsk, ModeDeny}
	for _, attended := range []bool{true, false} {
		for _, origin := range modes {
			for _, target := range modes {
				for _, action := range [][2]string{{"os_write", "write /etc/motd"}, {"os_exec", "uptime"}} {
					name := fmt.Sprintf("attended=%v/origin=%s/target=%s/%s", attended, origin, target, action[0])
					t.Run(name, func(t *testing.T) {
						r := newFleetRig(t)
						cat := testCategorize(action[0])
						if err := r.fleet.Put("aaaa", Scheme{Permissions: map[string]string{cat: target.String()}}); err != nil {
							t.Fatal(err)
						}
						// The origin is a relay whose own mode for the category is origin.
						relay := r.asker
						if !attended {
							relay = newTestDiscordBroker(testLog()) // no backend, no host channel: nobody to ask
						}
						if _, err := relay.SetPermission(cat, origin.String()); err != nil {
							t.Fatal(err)
						}
						r.answer = control.ApproveOnce
						ok, prompts := r.confirm(RelayOrigin(relay, true), "aaaa", action[0], action[1])

						want := max(severity(origin), severity(target)) // 0 allow, 1 ask, 2 deny
						switch {
						case want == 2 && (ok || prompts != 0):
							t.Fatalf("a deny on either side: ok %v, %d prompt(s)", ok, prompts)
						case want == 1 && attended && (!ok || prompts != 1):
							t.Fatalf("an ask on either side, attended: ok %v, %d prompt(s); want one prompt, then the operator's yes", ok, prompts)
						case want == 1 && !attended && (ok || prompts != 0):
							t.Fatalf("an ask on either side, unattended: ok %v, %d prompt(s); ask must be deny", ok, prompts)
						case want == 0 && (!ok || prompts != 0):
							t.Fatalf("allowed on both sides: ok %v, %d prompt(s)", ok, prompts)
						}
						// The operator's no is a no.
						if want == 1 && attended {
							r.answer = control.DenyOnce
							if ok, _ := r.confirm(RelayOrigin(relay, true), "aaaa", action[0], action[1]); ok {
								t.Fatal("the operator denied the prompt")
							}
						}
					})
				}
			}
		}
	}
}

func severity(m Mode) int { return map[Mode]int{ModeAllow: 0, ModeAsk: 1, ModeDeny: 2}[m] }

// Where the origin asks and the machine allows, the prompt's answer covers
// that one request: "always" and "for this session" save nothing, on the
// machine or on the origin, since neither is what asked for more.
func TestFleet_OriginAskSavesNothing(t *testing.T) {
	r := newFleetRig(t)
	if err := r.fleet.Put("aaaa", Scheme{Permissions: map[string]string{CatWrite: "allow"}, ToolRules: map[string]string{"os_exec(uptime)": "allow"}}); err != nil {
		t.Fatal(err)
	}
	before := schemeJSON(r.scheme("aaaa"))
	for _, d := range []control.ApprovalDecision{control.ApproveAlways, control.ApproveSession} {
		r.answer = d
		for range 2 {
			for _, a := range [][2]string{{"os_write", "write x"}, {"os_exec", "uptime"}} {
				if ok, n := r.confirm(RelayOrigin(r.asker, true), "aaaa", a[0], a[1]); !ok || n != 1 {
					t.Fatalf("%s: ok %v, %d prompt(s); the origin asks every time", a[0], ok, n)
				}
			}
		}
	}
	if got := schemeJSON(r.scheme("aaaa")); got != before {
		t.Fatalf("the machine's scheme moved:\n%s\nwas\n%s", got, before)
	}
	if len(r.asker.Rules()) != 0 || r.asker.Modes()[CatWrite] != "ask" {
		t.Fatalf("the origin's own settings moved: rules %v, modes %v", r.asker.Rules(), r.asker.Modes())
	}
}

// "For this session" on a machine's card covers that one request: a machine
// has no session grants, so the next request asks again, and the card does
// not offer one. The master's own cards keep theirs.
func TestFleet_SessionCoversOneRequest(t *testing.T) {
	r := newFleetRig(t, "aaaa")
	r.answer = control.ApproveSession
	for i := range 3 {
		for _, a := range [][2]string{{"os_exec", "uptime"}, {"os_write", "write x"}, {"web_interact", "click"}} {
			if ok, n := r.confirm(r.attended(), "aaaa", a[0], a[1]); !ok || n != 1 {
				t.Fatalf("round %d, %s: ok %v, %d prompt(s); every request asks", i, a[0], ok, n)
			}
		}
		r.rested("aaaa")
	}
	if s := r.scheme("aaaa"); len(s.ToolRules) != 0 || s.Permissions[CatExec] != "ask" {
		t.Fatalf("a session answer changed the scheme: %+v", s)
	}
	child, err := r.fleet.Child("aaaa")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range child.Permissions() {
		if p.Session {
			t.Fatalf("%s shows a session grant on a machine", p.Category)
		}
	}
	// The card says so: SessionOK is what a surface offers the session step by.
	done := make(chan bool, 1)
	go func() {
		done <- r.fleet.For(LocalOrigin(r.host), Machine{ID: "aaaa"}).Confirm(context.Background(), "os_write", "write x")
	}()
	id := waitPending(t, r.host)
	if p := r.host.Pending()[0]; p.SessionOK {
		t.Fatalf("a machine's card offers a session grant: %+v", p)
	}
	r.host.Resolve(id, control.ApproveSession)
	if !<-done {
		t.Fatal("approved for the session: refused")
	}
	go func() { done <- r.host.Confirm(context.Background(), "os_write", "write x") }()
	id = waitPending(t, r.host)
	if p := r.host.Pending()[0]; !p.SessionOK {
		t.Fatalf("the master's own card lost its session grant: %+v", p)
	}
	r.host.Resolve(id, control.DenyOnce)
	<-done
}

// A relay's lock (the Discord relay's exec mode) refuses on a machine what it
// refuses on the master, whatever the machine's scheme says.
func TestFleet_RelayLockBinds(t *testing.T) {
	r := newFleetRig(t)
	if err := r.fleet.Put("aaaa", Scheme{ToolRules: map[string]string{"os_exec": "allow"}}); err != nil {
		t.Fatal(err)
	}
	r.asker.SetPermission(CatExec, "allow")
	r.asker.SetLock(func(_, cat string) bool { return cat == CatExec })
	if ok, n := r.confirm(RelayOrigin(r.asker, true), "aaaa", "os_exec", "uptime"); ok || n != 0 {
		t.Fatalf("locked on the relay: ok %v, %d prompt(s)", ok, n)
	}
}

// An origin that was not opened for the fleet gets no fleet call of any kind:
// a relay with its switch off, a cron job with no machine list, the zero
// Origin. Reach is the check a read makes; Confirm makes it again.
func TestFleet_NotOpenedRefusesEverything(t *testing.T) {
	r := newFleetRig(t)
	// The machine itself would allow everything: only the origin stands in the way.
	open := Scheme{Permissions: map[string]string{}}
	for _, c := range Categories {
		open.Permissions[c] = "allow"
	}
	for _, id := range []string{"aaaa", "bbbb"} {
		if err := r.fleet.Put(id, open); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range Categories {
		r.asker.SetPermission(c, "allow")
	}
	yes := func(string, string) bool { return true }
	actions := []string{"fleet_read", "fleet_list_dir", "os_read_file", "os_exec", "os_write", "web_interact", "anything_else"}

	closed := map[string]Origin{
		"relay, switch off":         RelayOrigin(r.asker, false),
		"cron job, no machine list": CronOrigin(nil, yes),
		"cron job, empty list":      CronOrigin([]string{}, yes),
		"the zero origin":           {},
		"cron job, another machine": CronOrigin([]string{"bbbb"}, yes),
		"cron job, a revoked id":    CronOrigin([]string{"gone"}, yes),
	}
	for name, o := range closed {
		id := "aaaa"
		if name == "cron job, a revoked id" {
			id = "gone"
		}
		if err := r.fleet.For(o, Machine{ID: id}).Reach(); err == nil {
			t.Errorf("%s: Reach allowed machine %s", name, id)
		}
		for _, action := range actions {
			if ok, n := r.confirm(o, id, action, "x"); ok || n != 0 {
				t.Errorf("%s: %s on %s: ok %v, %d prompt(s)", name, action, id, ok, n)
			}
		}
	}

	// Opened, each passes to the machine's own check.
	opened := map[string]Origin{
		"relay, switch on":     RelayOrigin(r.asker, true),
		"cron job, listed":     CronOrigin([]string{"aaaa"}, yes),
		"cron job, two listed": CronOrigin([]string{"bbbb", "aaaa"}, yes),
		"local":                LocalOrigin(r.host),
	}
	for name, o := range opened {
		if err := r.fleet.For(o, Machine{ID: "aaaa"}).Reach(); err != nil {
			t.Errorf("%s: Reach: %v", name, err)
		}
		if ok, _ := r.confirm(o, "aaaa", "os_exec", "uptime"); !ok {
			t.Errorf("%s: the machine allows exec, and the origin was opened", name)
		}
	}
	// The machine's check is real: tighten it and the opened origins feel it.
	if err := r.fleet.Put("aaaa", Scheme{Permissions: map[string]string{CatExec: "deny"}}); err != nil {
		t.Fatal(err)
	}
	for name, o := range opened {
		if ok, _ := r.confirm(o, "aaaa", "os_exec", "uptime"); ok {
			t.Errorf("%s: the machine denies exec", name)
		}
	}

	// A cron job's own approver still binds (it refuses; the machine would allow),
	// one with no approver allows nothing gated, and a machine that asks is a no.
	if err := r.fleet.Put("aaaa", open); err != nil {
		t.Fatal(err)
	}
	onlyExec := func(action, _ string) bool { return action == "os_exec" }
	if ok, _ := r.confirm(CronOrigin([]string{"aaaa"}, onlyExec), "aaaa", "os_write", "x"); ok {
		t.Error("the cron job's own approver refused os_write")
	}
	if ok, _ := r.confirm(CronOrigin([]string{"aaaa"}, nil), "aaaa", "os_exec", "uptime"); ok {
		t.Error("a cron job with no approver may read, never act")
	}
	if err := r.fleet.Put("aaaa", Scheme{}); err != nil {
		t.Fatal(err)
	}
	if ok, n := r.confirm(CronOrigin([]string{"aaaa"}, yes), "aaaa", "os_exec", "uptime"); ok || n != 0 || len(r.host.Pending()) != 0 {
		t.Errorf("a machine that asks, from cron: ok %v, %d prompt(s), %d pending; ask must be deny", ok, n, len(r.host.Pending()))
	}

	// Revoked with its scheme deleted: the id fails for everyone, local included.
	if !r.fleet.Delete("aaaa") || r.fleet.Delete("aaaa") {
		t.Fatal("Delete must report whether there was a scheme")
	}
	for name, o := range opened {
		if err := r.fleet.For(o, Machine{ID: "aaaa"}).Reach(); err == nil {
			t.Errorf("%s: Reach allowed a machine with no scheme", name)
		}
		if ok, n := r.confirm(o, "aaaa", "os_exec", "uptime"); ok || n != 0 {
			t.Errorf("%s: a machine with no scheme: ok %v, %d prompt(s)", name, ok, n)
		}
	}
	// An id that is not one never reaches a lookup.
	for _, id := range []string{"", "a b", "../aaaa", "aaaa\n", strings.Repeat("a", 65), "\U0000ff41\U0000ff41\U0000ff41\U0000ff41"} {
		if err := r.fleet.For(LocalOrigin(r.host), Machine{ID: id}).Reach(); err == nil {
			t.Errorf("Reach allowed the id %q", id)
		}
		if err := r.fleet.Put(id, Scheme{}); err == nil {
			t.Errorf("Put took the id %q", id)
		}
	}
}

// A new machine's scheme: a copy of the master's saved local scheme, with
// exec, write, destructive, send and sandbox no looser than ask, and the
// fleet's rules over it.
func TestFleet_Inherit(t *testing.T) {
	r := newFleetRig(t)
	local := Scheme{
		Permissions: map[string]string{
			CatExec: "allow", CatWrite: "allow", CatDestructive: "allow", CatSend: "allow", CatSandbox: "allow",
			CatWeb: "allow", CatEgress: "deny", "bogus": "allow", // a junk entry the master skips too
		},
		ToolRules: map[string]string{
			"os_exec(brew install *)": "allow", // a command: exec
			"os_exec(rm *)":           "deny",
			"os_exec(git push)":       "ask",
			"os_write":                "allow", // write
			"x_post":                  "allow", // send
			"web_interact":            "allow", // web: not one of the five
			"mcp:h:1:write_note":      "allow", // an id no table knows: destructive
			"delete_*":                "allow", // a glob could reach anything
			"gsuite_send":             "deny",
			"bad rule":                "allow", // junk
		},
	}
	got := mustInherit(t, r.fleet, local)
	wantModes := map[string]string{
		CatExec: "ask", CatWrite: "ask", CatDestructive: "ask", CatSend: "ask", CatSandbox: "ask",
		CatWeb: "allow", CatEgress: "deny", CatMCP: "ask",
	}
	if !maps.Equal(got.Permissions, wantModes) {
		t.Errorf("modes = %v\nwant    %v", got.Permissions, wantModes)
	}
	wantRules := map[string]string{
		"os_exec(brew install *)": "ask", "os_exec(rm *)": "deny", "os_exec(git push)": "ask",
		"os_write": "ask", "x_post": "ask", "web_interact": "allow", "mcp:h:1:write_note": "ask",
		"delete_*": "ask", "gsuite_send": "deny",
	}
	if !maps.Equal(got.ToolRules, wantRules) {
		t.Errorf("rules = %v\nwant    %v", got.ToolRules, wantRules)
	}
	// A deny stays a deny: the clamp never loosens.
	if s := mustInherit(t, r.fleet, Scheme{Permissions: map[string]string{CatExec: "deny", CatSend: "deny"}}); s.Permissions[CatExec] != "deny" || s.Permissions[CatSend] != "deny" {
		t.Errorf("a category the master denies: %v", s.Permissions)
	}

	// Launch-flag grants and session grants live in the broker, not in the
	// saved scheme Inherit copies: neither reaches a machine.
	r.host.Allow(CatWeb, CatMCP)                                            // --web-allow-interact and the like
	r.host.session[CatEgress] = true                                        // "approve for this session"
	saved := Scheme{Permissions: map[string]string{CatDestructive: "deny"}} // what the settings file holds
	if r.host.Modes()[CatWeb] != "allow" {
		t.Fatal("the flag grant should be live on the master")
	}
	s := mustInherit(t, r.fleet, saved)
	if s.Permissions[CatWeb] != "ask" || s.Permissions[CatMCP] != "ask" || s.Permissions[CatEgress] != "ask" || s.Permissions[CatDestructive] != "deny" {
		t.Errorf("a flag or session grant was copied: %v", s.Permissions)
	}
	if err := r.fleet.Put("aaaa", s); err != nil {
		t.Fatal(err)
	}
	r.answer = control.DenyOnce
	if ok, n := r.confirm(r.attended(), "aaaa", "web_interact", "click"); ok || n != 1 {
		t.Errorf("web on the new machine: ok %v, %d prompt(s); the master's flag grant must not apply", ok, n)
	}
}

// The fleet's rules for new machines are laid over the inherited scheme and
// win where the two disagree. Changing them later moves no machine already
// confirmed; they can be applied again to one, and a machine can be given a
// fresh copy of the master's scheme. Drift says what differs.
func TestFleet_FleetRulesAndCopyAgain(t *testing.T) {
	r := newFleetRig(t)
	for p, m := range map[string]string{
		"os_exec(systemctl status *)": "allow", // pre-approved everywhere
		"os_exec(rm *)":               "deny",
		"x_post":                      "deny",
		"delete_*":                    "deny",
	} {
		if err := r.fleet.Rules().Set(p, m); err != nil {
			t.Fatal(err)
		}
	}
	local := Scheme{
		Permissions: map[string]string{CatExec: "deny"},
		ToolRules: map[string]string{
			"os_exec(systemctl status *)":     "deny",  // the same pattern: the fleet's wins
			"os_exec(systemctl status nginx)": "deny",  // narrower than the fleet's: it would have won; it goes
			"os_exec(rm -i *)":                "allow", // narrower than the fleet's deny: it goes
			"os_exec(systemctl *)":            "ask",   // wider: it stays, and the fleet's rule is the more specific
			"x_post":                          "allow",
			"delete_cache":                    "allow", // a name the fleet's glob covers
			"mcp:h:1:delete_all":              "ask",   // so is this id
			"delete_c*":                       "ask",   // and this narrower glob, which would have won
			"*_cache":                         "deny",  // a glob that only partly overlaps: it stays
			"gsuite_send":                     "deny",  // nothing to do with the fleet's rules
		},
	}
	got := mustInherit(t, r.fleet, local)
	want := map[string]string{
		"os_exec(systemctl status *)": "allow", "os_exec(rm *)": "deny", "x_post": "deny", "delete_*": "deny",
		"os_exec(systemctl *)": "ask", "*_cache": "deny", "gsuite_send": "deny",
	}
	if !maps.Equal(got.ToolRules, want) {
		t.Fatalf("rules = %v\nwant    %v", got.ToolRules, want)
	}
	if err := r.fleet.Put("aaaa", got); err != nil {
		t.Fatal(err)
	}
	// The fleet's allow runs unasked on the new machine, past the inherited
	// category deny and the inherited narrower deny; its deny refuses what the
	// master allowed.
	for _, c := range []string{"systemctl status nginx", "systemctl status sshd"} {
		if ok, n := r.confirm(r.attended(), "aaaa", "os_exec", c); !ok || n != 0 {
			t.Errorf("%q: ok %v, %d prompt(s); the fleet's rule allows it", c, ok, n)
		}
	}
	r.answer = control.ApproveOnce
	for _, a := range [][2]string{{"os_exec", "rm -i x"}, {"x_post", "hi"}, {"mcp:h:1:delete_cache", "x"}, {"mcp:h:1:delete_all", "x"}} {
		if ok, n := r.confirm(r.attended(), "aaaa", a[0], a[1]); ok || n != 0 {
			t.Errorf("%s %q: ok %v, %d prompt(s); the fleet's rule denies it", a[0], a[1], ok, n)
		}
	}

	// The master and the fleet's rules change; aaaa does not.
	before := schemeJSON(r.scheme("aaaa"))
	if err := r.fleet.Rules().Set("os_exec(rm *)", "allow"); err != nil {
		t.Fatal(err)
	}
	r.fleet.Rules().Remove("x_post")
	if err := r.fleet.Rules().Set("os_exec(df *)", "allow"); err != nil {
		t.Fatal(err)
	}
	local.Permissions[CatWeb] = "allow"
	local.ToolRules["gsuite_send"] = "allow"
	if got := schemeJSON(r.scheme("aaaa")); got != before {
		t.Fatalf("a confirmed machine moved:\n%s\nwas\n%s", got, before)
	}
	if ok, _ := r.confirm(r.attended(), "aaaa", "os_exec", "rm -i x"); ok {
		t.Error("aaaa still denies rm: the fleet's rules changed after it was confirmed")
	}

	// Drift: what a fresh copy would be, against what aaaa has.
	fresh := mustInherit(t, r.fleet, local)
	drift := r.scheme("aaaa").Drift(fresh)
	var lines []string
	for _, d := range drift {
		lines = append(lines, fmt.Sprintf("%v %s %q->%q", d.Rule, d.Key, d.Have, d.Want))
	}
	wantDrift := []string{
		`false web "ask"->"allow"`,
		`true gsuite_send "deny"->"ask"`, // the master's allow, clamped
		`true os_exec(df *) ""->"allow"`,
		`true os_exec(rm *) "deny"->"allow"`,
		`true x_post "deny"->"ask"`,
	}
	if !slices.Equal(lines, wantDrift) {
		t.Fatalf("drift =\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(wantDrift, "\n"))
	}
	if d := fresh.Drift(fresh); len(d) != 0 {
		t.Fatalf("a scheme drifts from itself: %v", d)
	}

	// The fleet's rules, applied again to aaaa alone: its own narrower answer
	// to what they cover goes, the rest of its scheme stays.
	if ok, _ := r.confirm(r.attended(), "aaaa", "os_exec", "df -h"); ok {
		t.Fatal("df was never allowed on aaaa")
	}
	if err := r.fleet.Put("aaaa", r.scheme("aaaa").Overlay(r.fleet.Rules().Snapshot())); err != nil {
		t.Fatal(err)
	}
	if ok, n := r.confirm(r.attended(), "aaaa", "os_exec", "df -h"); !ok || n != 0 {
		t.Errorf("after applying the fleet's rules again: df ok %v, %d prompt(s)", ok, n)
	}
	if s := r.scheme("aaaa"); s.ToolRules["x_post"] != "deny" || s.ToolRules["os_exec(rm *)"] != "allow" || s.Permissions[CatWeb] != "ask" {
		t.Errorf("after applying the fleet's rules again: %+v", s)
	}
	// A fresh copy of the master's scheme.
	if err := r.fleet.Put("aaaa", fresh); err != nil {
		t.Fatal(err)
	}
	if d := r.scheme("aaaa").Drift(fresh); len(d) != 0 {
		t.Fatalf("after copying again, drift = %v", d)
	}
}

// The read-only command judgement is the master's: a command it calls
// read-only runs unasked on the master and still asks on a machine, whatever
// is installed where.
func TestFleet_NoReadOnlyOnAMachine(t *testing.T) {
	r := newFleetRig(t, "aaaa")
	if !r.host.Confirm(context.Background(), "os_exec", "ls -la") {
		t.Fatal("the master runs a read-only command unasked")
	}
	// Even installed on the machine's own broker, and on the surface that asks.
	child, err := r.fleet.Child("aaaa")
	if err != nil {
		t.Fatal(err)
	}
	always := func(string) bool { return true }
	child.SetReadOnly(always)
	r.asker.SetReadOnly(always)
	for _, o := range []Origin{r.attended(), RelayOrigin(r.asker, true)} {
		if _, err := r.asker.SetPermission(CatExec, "allow"); err != nil {
			t.Fatal(err)
		}
		if ok, n := r.confirm(o, "aaaa", "os_exec", "ls -la"); ok || n != 1 {
			t.Fatalf("ls on a machine: ok %v, %d prompt(s); every command asks there", ok, n)
		}
	}
	// Until "always" saves its rule for that machine.
	r.answer = control.ApproveAlways
	r.confirm(r.attended(), "aaaa", "os_exec", "ls -la")
	if ok, n := r.confirm(r.attended(), "aaaa", "os_exec", "ls /tmp"); !ok || n != 0 {
		t.Fatalf("after always: ok %v, %d prompt(s)", ok, n)
	}
}

// A machine's own broker is its scheme, for the permissions editor. It
// decides nothing by itself: an action reaches a machine only through For.
func TestFleet_MachineBrokerDecidesNothingAlone(t *testing.T) {
	r := newFleetRig(t)
	if err := r.fleet.Put("aaaa", Scheme{Permissions: map[string]string{CatExec: "allow"}, ToolRules: map[string]string{"os_write": "allow"}}); err != nil {
		t.Fatal(err)
	}
	child, err := r.fleet.Child("aaaa")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"os_exec", "os_write", "web_interact"} {
		if child.Confirm(context.Background(), a, "x") {
			t.Errorf("the machine's broker approved %s outside a fleet call", a)
		}
	}
	// It edits the scheme, and the edit is the machine's alone.
	if ok, err := child.SetPermissionFrom("tui", CatWeb, "deny"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if err := child.SetRule("os_exec(uptime)", "deny"); err != nil {
		t.Fatal(err)
	}
	if s := r.scheme("aaaa"); s.Permissions[CatWeb] != "deny" || s.ToolRules["os_exec(uptime)"] != "deny" {
		t.Fatalf("the edit did not reach the scheme: %+v", s)
	}
	if ok, _ := r.confirm(r.attended(), "aaaa", "os_exec", "uptime"); ok {
		t.Fatal("the edited rule denies uptime")
	}
	if _, err := r.fleet.Child("nope"); err == nil {
		t.Fatal("a machine with no scheme has no broker")
	}
}

// "Always" on an action whose approval narrows to what it asked for (a
// sandbox escalation saves a folder on the master) saves nothing on the
// master when the action is a machine's: it approves the one request.
func TestFleet_ScopedAlwaysSavesNothingOnTheMaster(t *testing.T) {
	scopedSaved = nil
	r := newFleetRig(t, "aaaa")
	r.answer = control.ApproveAlways
	for range 2 {
		ok := r.fleet.For(r.attended(), Machine{ID: "aaaa"}).Confirm(context.WithValue(context.Background(), scopedKey{}, "/work"), scopedAction, "re-run")
		if !ok {
			t.Fatal("always must approve")
		}
	}
	if len(scopedSaved) != 0 {
		t.Fatalf("a machine's always ran the master's save: %q", scopedSaved)
	}
	if len(r.shown) != 2 || r.scheme("aaaa").Permissions[CatDestructive] != "ask" {
		t.Fatalf("%d prompt(s), destructive %s: always must not open the category", len(r.shown), r.scheme("aaaa").Permissions[CatDestructive])
	}
}

// A request names its machine: in the fields a surface reads, at the start
// of the text every surface already shows, and in the host's pending list.
func TestFleet_RequestNamesTheMachine(t *testing.T) {
	r := newFleetRig(t, "aaaa")
	r.confirm(r.attended(), "aaaa", "os_exec", "uptime")
	if len(r.shown) != 1 {
		t.Fatal("no prompt")
	}
	req := r.shown[0]
	if req.Host != "aaaa" || req.HostName != "box-aaaa" || req.Detail != `[host aaaa "box-aaaa"] uptime` {
		t.Fatalf("request = host %q name %q detail %q", req.Host, req.HostName, req.Detail)
	}

	// On the host's surfaces: the pending list (the dashboard) and the request
	// the terminal draws carry it too.
	done := make(chan bool, 1)
	go func() {
		done <- r.fleet.For(LocalOrigin(r.host), Machine{ID: "aaaa", Name: "web-1"}).Confirm(context.Background(), "os_write", "write /etc/motd")
	}()
	id := waitPending(t, r.host)
	p := r.host.Pending()[0]
	if p.Host != "aaaa" || p.HostName != "web-1" || p.Detail != `[host aaaa "web-1"] write /etc/motd` || p.Origin != "local" {
		t.Fatalf("pending = %+v", p)
	}
	b, _ := json.Marshal(p)
	if !strings.Contains(string(b), `"host":"aaaa"`) || !strings.Contains(string(b), `"host_name":"web-1"`) {
		t.Fatalf("the pending item's JSON lacks the machine: %s", b)
	}
	select {
	case tr := <-r.host.Requests():
		if tr.ID != id || tr.Host != "aaaa" || tr.HostName != "web-1" || !strings.HasPrefix(tr.Detail, `[host aaaa "web-1"] `) {
			t.Fatalf("the terminal's request = %+v", tr)
		}
	case <-time.After(time.Second):
		t.Fatal("the terminal was not notified")
	}
	r.host.Resolve(id, control.ApproveOnce)
	if !<-done {
		t.Fatal("approved on the host: refused")
	}
	// The master's own requests carry no machine.
	go func() { done <- r.host.Confirm(context.Background(), "os_write", "write x") }()
	id = waitPending(t, r.host)
	if p := r.host.Pending()[0]; p.Host != "" || p.HostName != "" || p.Detail != "write x" {
		t.Fatalf("a request of the master's own names a machine: %+v", p)
	}
	r.host.Resolve(id, control.DenyOnce)
	<-done
}

// A machine's name may be the machine's own words. Whatever they are, the
// card's first line and the record say which machine it is: the id the master
// gave it, then the name as plain printable ASCII of bounded length, in
// quotes it cannot close.
func TestFleet_HostileMachineNames(t *testing.T) {
	names := map[string]string{
		"control characters":   "web\x00-1\r\n\x7f\tdb",
		"ANSI escapes":         "\x1b[2J\x1b[1;31mprod\x1b[0m\x1b]0;title\x07",
		"very long":            strings.Repeat("A", 1<<20),
		"very long, wide":      strings.Repeat("\U0001F600", 1<<16),
		"a look-alike":         "w\U00000435b-1", // a Cyrillic ie
		"bidi and zero width":  "web\U0000202e1-\U0000200bdb\U00002066",
		"another machine's":    `x"] [host bbbb "prod-db"] rm -rf /`,
		"a second line":        "x\n[host bbbb \"prod-db\"] approve me",
		"brackets and slashes": `]]\\"[[host`,
		"not UTF-8":            "web\xff\xfe-1",
		"empty":                "",
		"the other's id":       "bbbb",
	}
	read := securityLog(t, logs.Options{}, testChainKey)
	r := newFleetRig(t, "aaaa", "bbbb")
	const prefix = `[host aaaa "`
	n := 0
	for what, name := range names {
		r.shown = nil
		r.rested("aaaa")
		r.fleet.For(r.attended(), Machine{ID: "aaaa", Name: name}).Confirm(context.Background(), "os_exec", "uptime")
		if len(r.shown) != 1 {
			t.Fatalf("%s: no prompt", what)
		}
		n++
		req := r.shown[0]
		label, rest, ok := strings.Cut(req.Detail, `"] uptime`)
		if !ok || rest != "" || !strings.HasPrefix(label, prefix) {
			t.Errorf("%s: detail %q is not the machine's label, then the command", what, clip(req.Detail))
			continue
		}
		shown := label[len(prefix):]
		if shown != req.HostName || req.Host != "aaaa" {
			t.Errorf("%s: the request's fields (%q, %q) disagree with its text", what, req.Host, clip(req.HostName))
		}
		if len(shown) > 10*maxNameRunes+3 {
			t.Errorf("%s: the name is %d bytes on the card", what, len(shown))
		}
		for i := 0; i < len(shown); i++ {
			if c := shown[i]; c < 0x20 || c > 0x7e || c == '[' || c == ']' || (c == '"' && (i == 0 || shown[i-1] != '\\')) {
				t.Errorf("%s: the name on the card holds %q at %d: %q", what, c, i, clip(shown))
				break
			}
		}
		// What every surface draws (control.VisibleText) is the same text.
		if v := control.VisibleText(req.Detail); v != req.Detail {
			t.Errorf("%s: the card would draw %q for %q", what, clip(v), clip(req.Detail))
		}
		// Nothing in it reads as the other machine's label.
		if strings.Contains(req.Detail, "[host bbbb") {
			t.Errorf("%s: the card shows another machine's label: %q", what, clip(req.Detail))
		}
	}
	// A look-alike is visibly not the name it imitates.
	if got := (Machine{ID: "aaaa", Name: "w\U00000435b-1"}).safe().Name; got != "w\\"+"u0435b-1" {
		t.Errorf("a Cyrillic look-alike shows as %q", got)
	}

	// The records: each names the machine by id and by the same escaped name,
	// and its detail starts with the label.
	recs := read("security")
	if len(recs) != 2*n {
		t.Fatalf("%d security records for %d prompts, want an ask and an outcome each", len(recs), n)
	}
	for _, e := range recs {
		var host, name, detail string
		for _, f := range e.Data {
			switch f.Key {
			case "host":
				host = f.Text()
			case "host_name":
				name = f.Text()
			case "detail":
				detail = f.Text()
			}
		}
		if host != "aaaa" || !strings.HasPrefix(detail, prefix+name+`"] `) || !utf8.ValidString(name) || strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r > 0x7e }) {
			t.Errorf("record %d: host %q name %q detail %q", e.ID, host, clip(name), clip(detail))
		}
	}
}

func clip(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

// Every decision about a machine's action is a Security record that names
// the machine, whoever made it: the machine's scheme, the origin's own
// rules, a closed origin, or the operator.
func TestFleet_RecordsNameTheMachine(t *testing.T) {
	read := securityLog(t, logs.Options{}, testChainKey)
	r := newFleetRig(t)
	if err := r.fleet.Put("aaaa", Scheme{Permissions: map[string]string{CatWrite: "allow", CatSend: "deny"}}); err != nil {
		t.Fatal(err)
	}
	m := Machine{ID: "aaaa", Name: "web-1"}
	ctx := context.Background()
	r.fleet.For(r.attended(), m).Confirm(ctx, "os_write", "write a")                                                          // the scheme allows
	r.fleet.For(r.attended(), m).Confirm(ctx, "gsuite_send", "mail bob")                                                      // the scheme denies
	r.fleet.For(RelayOrigin(r.asker, false), m).Confirm(ctx, "os_write", "write b")                                           // a relay not opened
	r.asker.SetPermission(CatWrite, "deny")                                                                                   //
	r.fleet.For(RelayOrigin(r.asker, true), m).Confirm(ctx, "os_write", "write c")                                            // the origin's own rules
	r.fleet.For(CronOrigin([]string{"aaaa"}, func(string, string) bool { return true }), m).Confirm(ctx, "os_exec", "uptime") // nobody to ask
	r.answer = control.ApproveOnce
	r.fleet.For(r.attended(), m).Confirm(ctx, "os_exec", "uptime") // the operator

	const tag = `[host aaaa "web-1"] `
	wantRecords(t, read("security"), []string{"host", "host_name", "category", "action", "detail", "origin", "grant", "via"},
		"4102 tenant@local host=aaaa host_name=web-1 category=write action=os_write detail="+tag+"write a origin=local grant=always via=mode",
		"4103 tenant@local host=aaaa host_name=web-1 category=send action=gsuite_send detail="+tag+"mail bob origin=local via=mode",
		"4103 tenant@discord host=aaaa host_name=web-1 category=write action=os_write detail="+tag+"write b origin=discord via=fleet",
		"4106 operator@tui category=write", // the relay's own mode, set to deny for the next line
		"4103 tenant@discord host=aaaa host_name=web-1 category=write action=os_write detail="+tag+"write c origin=discord via=origin",
		"4103 tenant@cron host=aaaa host_name=web-1 category=exec action=os_exec detail="+tag+"uptime origin=cron via=no_surface",
		"4101 agent@local host=aaaa host_name=web-1 category=exec action=os_exec detail="+tag+"uptime origin=local",
		"4102 tenant@local host=aaaa host_name=web-1 category=exec action=os_exec detail="+tag+"uptime origin=local grant=once via=no_surface")
}

// A pending approval for a machine can be ended from outside: the link
// cancels the call's context when the call ends or the link drops. That is a
// denial, and the card goes.
func TestFleet_CancelDeniesAndRemovesTheCard(t *testing.T) {
	r := newFleetRig(t, "aaaa")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		done <- r.fleet.For(LocalOrigin(r.host), Machine{ID: "aaaa", Name: "web-1"}).Confirm(ctx, "os_exec", "uptime")
	}()
	id := waitPending(t, r.host)
	cancel()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("a cancelled call's approval was granted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Confirm did not return when its context was cancelled")
	}
	if p := r.host.Pending(); len(p) != 0 {
		t.Fatalf("the card is still pending: %+v", p)
	}
	if r.host.Resolve(id, control.ApproveAlways) {
		t.Fatal("a late answer resolved a cancelled approval")
	}
	if rules := r.scheme("aaaa").ToolRules; len(rules) != 0 {
		t.Fatalf("a late always saved a rule: %v", rules)
	}
	// A context already cancelled never shows a card that grants.
	if r.fleet.For(LocalOrigin(r.host), Machine{ID: "aaaa"}).Confirm(ctx, "os_exec", "uptime") || len(r.host.Pending()) != 0 {
		t.Fatal("a cancelled context must deny and leave no card")
	}
	// A relay's backend is handed the call's context, so it can stop waiting too.
	var got context.Context
	r.asker.SetAsk(func(c context.Context, _ control.ApprovalRequest) control.ApprovalDecision {
		got = c
		<-c.Done()
		return control.Expired
	})
	if r.fleet.For(r.attended(), Machine{ID: "aaaa"}).Confirm(ctx, "os_exec", "uptime") || got == nil || got.Err() == nil {
		t.Fatal("the backend must see the cancelled context, and its expiry is a denial")
	}
	// Shutdown denies a machine's pending card like any other.
	go func() {
		done <- r.fleet.For(LocalOrigin(r.host), Machine{ID: "aaaa"}).Confirm(context.Background(), "os_exec", "uptime")
	}()
	waitPending(t, r.host)
	r.host.DenyAll()
	if <-done {
		t.Fatal("DenyAll must deny a machine's pending approval")
	}
}

// One machine's calls put only so many prompts in front of the operator:
// past the burst, a prompt is a denial until time has passed. What the scheme
// decides by itself is not counted, and another machine is not held back.
func TestFleet_PromptsAreRateLimited(t *testing.T) {
	r := newFleetRig(t, "aaaa", "bbbb")
	r.answer = control.ApproveOnce
	for i := range askBurst {
		if ok, n := r.confirm(r.attended(), "aaaa", "os_exec", "uptime"); !ok || n != 1 {
			t.Fatalf("prompt %d of the burst: ok %v, %d prompt(s)", i+1, ok, n)
		}
	}
	for range 3 {
		if ok, n := r.confirm(r.attended(), "aaaa", "os_exec", "uptime"); ok || n != 0 {
			t.Fatalf("past the burst: ok %v, %d prompt(s); it must be refused unasked", ok, n)
		}
	}
	if ok, n := r.confirm(r.attended(), "bbbb", "os_exec", "uptime"); !ok || n != 1 {
		t.Fatalf("another machine: ok %v, %d prompt(s)", ok, n)
	}
	if err := r.fleet.Put("aaaa", Scheme{Permissions: map[string]string{CatExec: "allow"}}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := r.confirm(r.attended(), "aaaa", "os_exec", "uptime"); !ok {
		t.Fatal("a standing allow is not a prompt: the limit must not refuse it")
	}
	// Time gives the prompts back, one every askEvery, never more than the burst.
	l := &askLimit{}
	now := time.Now()
	for range askBurst {
		if !l.take(now) {
			t.Fatal("the burst")
		}
	}
	if l.take(now) || l.take(now.Add(askEvery/2)) {
		t.Fatal("no prompt before askEvery has passed")
	}
	if !l.take(now.Add(askEvery+askEvery/2)) || l.take(now.Add(askEvery+askEvery/2)) {
		t.Fatal("one prompt per askEvery")
	}
	later := now.Add(1000 * askEvery)
	for range askBurst {
		if !l.take(later) {
			t.Fatal("a long quiet refills the burst")
		}
	}
	if l.take(later) {
		t.Fatal("never more than the burst")
	}
	// Nobody to ask is not a prompt either: cron's refusals use none up.
	r = newFleetRig(t, "aaaa")
	cron := CronOrigin([]string{"aaaa"}, func(string, string) bool { return true })
	for range 3 * askBurst {
		r.confirm(cron, "aaaa", "os_exec", "uptime")
	}
	r.answer = control.ApproveOnce
	if ok, n := r.confirm(r.attended(), "aaaa", "os_exec", "uptime"); !ok || n != 1 {
		t.Fatalf("after cron's refusals: ok %v, %d prompt(s)", ok, n)
	}
}

// What a machine sends as its action and detail is another machine's text:
// an action id that is not plain text, or a detail too long to read, is
// refused before any rule sees it.
func TestFleet_MalformedRequestsAreRefused(t *testing.T) {
	r := newFleetRig(t)
	open := Scheme{Permissions: map[string]string{}}
	for _, c := range Categories {
		open.Permissions[c] = "allow"
	}
	if err := r.fleet.Put("aaaa", open); err != nil {
		t.Fatal(err)
	}
	if ok, _ := r.confirm(r.attended(), "aaaa", "mcp:local:anything", strings.Repeat("x", maxDetail)); !ok {
		t.Fatal("a detail at the limit, on a machine that allows everything")
	}
	for what, req := range map[string][2]string{
		"an empty action":         {"", "x"},
		"a long action":           {strings.Repeat("a", 201), "x"},
		"a newline in it":         {"os_exec\nos_read", "x"},
		"a tab in it":             {"os_exec\t", "x"},
		"an escape in it":         {"os_exec\x1b[2J", "x"},
		"a zero-width space":      {"os_\U0000200bexec", "x"},
		"bytes that aren't text":  {"os_exec\xff", "x"},
		"a detail past the limit": {"os_exec", strings.Repeat("x", maxDetail+1)},
	} {
		if ok, n := r.confirm(r.attended(), "aaaa", req[0], req[1]); ok || n != 0 {
			t.Errorf("%s: ok %v, %d prompt(s)", what, ok, n)
		}
	}
}

// raws is a settings file's fleet_schemes value, from JSON text per machine.
func raws(kv ...string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = json.RawMessage(kv[i+1])
	}
	return out
}

// The schemes as the settings file holds them: loaded, saved on every
// change, and loaded again as they were.
func TestFleet_LoadAndSave(t *testing.T) {
	r := newFleetRig(t)
	var schemes map[string]json.RawMessage
	var rules json.RawMessage
	saves := 0
	r.fleet.SetPersist(func() { schemes, rules = r.fleet.Snapshot(); saves++ })

	// A file from before the fleet: nothing to load, nothing wrong, nothing saved.
	if n, problems := r.fleet.Load(nil, nil); n != 0 || len(problems) != 0 || saves != 0 {
		t.Fatalf("an old file: %d change(s), problems %v, %d save(s)", n, problems, saves)
	}

	// A hand-written file: synonyms, a category left out, a rule to trim.
	n, problems := r.fleet.Load(raws(
		"aaaa", `{"permissions":{"exec":"Allow","send":"off"},"tool_rules":{" os_exec(rm *) ":"block","web_interact":"yes"}}`,
		"bbbb", `{}`,
	), json.RawMessage(`{"os_exec(systemctl status *)":"allow"}`))
	if n != 3 || len(problems) != 0 || saves != 0 {
		t.Fatalf("a hand-written file: %d change(s), problems %v, %d save(s) (a load never saves)", n, problems, saves)
	}
	a := r.scheme("aaaa")
	if a.Permissions[CatExec] != "allow" || a.Permissions[CatSend] != "deny" || a.Permissions[CatWrite] != "ask" || len(a.Permissions) != len(Categories) ||
		!maps.Equal(a.ToolRules, map[string]string{"os_exec(rm *)": "deny", "web_interact": "allow"}) {
		t.Fatalf("aaaa = %+v", a)
	}
	if ok, _ := r.confirm(r.attended(), "aaaa", "os_exec", "rm x"); ok {
		t.Fatal("the loaded rule denies rm")
	}
	if ok, n := r.confirm(r.attended(), "aaaa", "os_exec", "uptime"); !ok || n != 0 {
		t.Fatal("the loaded mode allows exec")
	}
	if got := r.fleet.Rules().Snapshot(); !maps.Equal(got, map[string]string{"os_exec(systemctl status *)": "allow"}) {
		t.Fatalf("the fleet's rules = %v", got)
	}

	// "Always" on bbbb saves every machine and the fleet's rules, bbbb changed.
	r.answer = control.ApproveAlways
	if ok, _ := r.confirm(r.attended(), "bbbb", "os_exec", "df -h"); !ok || saves != 1 {
		t.Fatalf("always: %d save(s)", saves)
	}
	var b Scheme
	if err := json.Unmarshal(schemes["bbbb"], &b); err != nil || b.ToolRules["os_exec(df *)"] != "allow" || len(schemes) != 2 {
		t.Fatalf("saved bbbb = %s (%v), %d machine(s)", schemes["bbbb"], err, len(schemes))
	}
	if string(rules) != `{"os_exec(systemctl status *)":"allow"}` {
		t.Fatalf("saved fleet rules = %s", rules)
	}
	// So does an edit of the fleet's rules, a Put and a Delete.
	if err := r.fleet.Rules().Set("x_post", "deny"); err != nil || saves != 2 || !strings.Contains(string(rules), `"x_post":"deny"`) {
		t.Fatalf("a fleet rule: err %v, %d save(s), %s", err, saves, rules)
	}
	if err := r.fleet.Put("cccc", Scheme{}); err != nil || saves != 3 || len(schemes) != 3 {
		t.Fatalf("Put: err %v, %d save(s), %d machine(s)", err, saves, len(schemes))
	}
	if !r.fleet.Delete("cccc") || saves != 4 || len(schemes) != 2 {
		t.Fatalf("Delete: %d save(s), %d machine(s)", saves, len(schemes))
	}

	// What was saved loads into a new fleet as the same schemes.
	again := NewFleet(testCategorize)
	if n, problems := again.Load(schemes, rules); n != 3 || len(problems) != 0 {
		t.Fatalf("reload: %d change(s), problems %v", n, problems)
	}
	for _, id := range []string{"aaaa", "bbbb"} {
		got, err := again.Scheme(id)
		if err != nil || schemeJSON(got) != schemeJSON(r.scheme(id)) {
			t.Fatalf("%s reloaded as %s (%v), want %s", id, schemeJSON(got), err, schemeJSON(r.scheme(id)))
		}
	}
	if !maps.Equal(again.Rules().Snapshot(), r.fleet.Rules().Snapshot()) {
		t.Fatal("the fleet's rules did not reload")
	}
	// Loading the same file again is no change.
	if n, _ := again.Load(schemes, rules); n != 0 {
		t.Fatalf("an unchanged file: %d change(s)", n)
	}
}

// A hand edit is applied to the running fleet: a machine's scheme changed,
// added or taken out. A file that only lags behind a change this process has
// not saved yet is not an edit, and must not undo the change.
func TestFleet_ReloadAppliesEditsOnly(t *testing.T) {
	r := newFleetRig(t)
	file := raws("aaaa", `{"permissions":{"exec":"allow"}}`, "bbbb", `{"tool_rules":{"x_post":"deny"}}`)
	r.fleet.Load(file, nil)

	// In flight: "always" on aaaa is live, and nothing has written it to the
	// file yet (no persist hook has taken a snapshot).
	r.answer = control.ApproveAlways
	r.confirm(r.attended(), "aaaa", "web_interact", "click")
	if n, _ := r.fleet.Load(file, nil); n != 0 {
		t.Fatalf("a file that lags a live change read as an edit: %d change(s)", n)
	}
	if r.scheme("aaaa").Permissions[CatWeb] != "allow" {
		t.Fatal("the live change was undone")
	}

	// A real edit: aaaa tightened (a category removed means ask again), bbbb
	// taken out, cccc added. The same file in another key order is no edit.
	edited := raws("aaaa", `{"permissions":{"exec":"deny"}}`, "cccc", `{"permissions":{"web":"allow"}}`)
	if n, problems := r.fleet.Load(edited, json.RawMessage(`{"os_exec(df *)":"allow"}`)); n != 4 || len(problems) != 0 {
		t.Fatalf("an edit: %d change(s), problems %v", n, problems)
	}
	if a := r.scheme("aaaa"); a.Permissions[CatExec] != "deny" || a.Permissions[CatWeb] != "ask" {
		t.Fatalf("aaaa after the edit = %+v", a)
	}
	if _, err := r.fleet.Scheme("bbbb"); err == nil {
		t.Fatal("bbbb was taken out of the file")
	}
	if ok, _ := r.confirm(r.attended(), "bbbb", "os_exec", "uptime"); ok {
		t.Fatal("a machine taken out of the file has no scheme: refused")
	}
	if r.scheme("cccc").Permissions[CatWeb] != "allow" || r.fleet.Rules().Mode("os_exec") != "" || len(r.fleet.Rules().Snapshot()) != 1 {
		t.Fatal("cccc or the fleet's rules were not loaded")
	}
	reordered := raws("cccc", "{ \"permissions\" : {\n\"web\":\"allow\"} }", "aaaa", `{"permissions":{"exec":"deny"},"tool_rules":{}}`)
	if n, _ := r.fleet.Load(reordered, json.RawMessage(` {"os_exec(df *)" : "allow"} `)); n != 0 {
		t.Fatalf("the same schemes, written differently: %d change(s)", n)
	}
}

// A machine's entry that is not a scheme fails closed for that machine: it
// has no scheme, so nothing reaches it, reads included, until the file is
// fixed. The other machines are untouched, the entry is told to the operator,
// and a save writes it back as it was rather than erasing it.
func TestFleet_MalformedEntriesFailClosed(t *testing.T) {
	bad := map[string]string{
		"not an object":         `"allow"`,
		"an array":              `["exec"]`,
		"null":                  `null`,
		"modes not an object":   `{"permissions":"allow"}`,
		"a mode not a string":   `{"permissions":{"exec":true}}`,
		"a misspelt mode":       `{"permissions":{"exec":"dney"}}`,
		"an unknown category":   `{"permissions":{"exce":"deny"}}`,
		"a misspelt key":        `{"permission":{"exec":"deny"}}`,
		"a misspelt rules key":  `{"tool_rule":{"os_exec(rm *)":"deny"}}`,
		"a rule's mode":         `{"tool_rules":{"os_exec(rm *)":"dney"}}`,
		"a rule's pattern":      `{"tool_rules":{"two words":"deny"}}`,
		"a rule for everything": `{"tool_rules":{"*":"allow"}}`,
		"a rule not a string":   `{"tool_rules":{"os_exec":1}}`,
		"rules not an object":   `{"tool_rules":["os_exec"]}`,
		"two values":            `{} {}`,
		"not JSON":              `{"permissions":`,
	}
	for what, raw := range bad {
		t.Run(what, func(t *testing.T) {
			r := newFleetRig(t)
			saved := map[string]json.RawMessage{}
			r.fleet.SetPersist(func() { saved, _ = r.fleet.Snapshot() })
			// aaaa was fine and allowed everything; then its entry was edited badly.
			r.fleet.Load(raws("aaaa", `{"permissions":{"exec":"allow","write":"allow"}}`, "good", `{"permissions":{"exec":"allow"}}`), nil)
			n, problems := r.fleet.Load(raws("aaaa", raw, "good", `{"permissions":{"exec":"allow"}}`), nil)
			if n != 1 || len(problems) != 1 || !strings.Contains(problems[0], `"aaaa"`) {
				t.Fatalf("%d change(s), problems %v; want aaaa's entry refused and said so", n, problems)
			}
			for _, o := range []Origin{r.attended(), LocalOrigin(r.host), CronOrigin([]string{"aaaa"}, func(string, string) bool { return true })} {
				if err := r.fleet.For(o, Machine{ID: "aaaa"}).Reach(); err == nil {
					t.Errorf("%s: Reach allowed a machine whose scheme is malformed", o.Name)
				}
				if ok, n := r.confirm(o, "aaaa", "os_exec", "uptime"); ok || n != 0 {
					t.Errorf("%s: ok %v, %d prompt(s); a malformed scheme must refuse", o.Name, ok, n)
				}
			}
			if _, err := r.fleet.Scheme("aaaa"); err == nil {
				t.Error("Scheme returned a malformed entry")
			}
			if _, err := r.fleet.Child("aaaa"); err == nil {
				t.Error("Child returned a broker for a malformed entry")
			}
			if ok, n := r.confirm(r.attended(), "good", "os_exec", "uptime"); !ok || n != 0 {
				t.Errorf("the other machine: ok %v, %d prompt(s)", ok, n)
			}
			// A save keeps the operator's text for them to fix.
			if err := r.fleet.Put("cccc", Scheme{}); err != nil {
				t.Fatal(err)
			}
			if string(saved["aaaa"]) != raw || len(saved) != 3 {
				t.Errorf("saved aaaa as %q among %d machine(s); want it as written", saved["aaaa"], len(saved))
			}
			// The same bad file again is not news; the fixed one loads.
			if n, problems := r.fleet.Load(raws("aaaa", raw, "good", `{"permissions":{"exec":"allow"}}`, "cccc", `{}`), nil); n != 0 || len(problems) != 0 {
				t.Errorf("the same file again: %d change(s), problems %v", n, problems)
			}
			if n, problems := r.fleet.Load(raws("aaaa", `{"permissions":{"exec":"deny"}}`, "good", `{"permissions":{"exec":"allow"}}`, "cccc", `{}`), nil); n != 1 || len(problems) != 0 {
				t.Fatalf("the fixed file: %d change(s), problems %v", n, problems)
			}
			if r.scheme("aaaa").Permissions[CatExec] != "deny" {
				t.Error("the fixed entry did not load")
			}
		})
	}

	// A key that is not a machine id is refused the same way.
	r := newFleetRig(t)
	if n, problems := r.fleet.Load(raws("not an id", `{}`, "../x", `{}`), nil); n != 2 || len(problems) != 2 {
		t.Fatalf("keys that are not ids: %d change(s), problems %v", n, problems)
	}

	// Malformed fleet rules: no new machine can be given a scheme (so it stays
	// closed) until they are fixed; confirmed machines are untouched; a save
	// keeps the text.
	for what, raw := range map[string]string{
		"not an object":    `["os_exec"]`,
		"null":             `null`,
		"a misspelt mode":  `{"os_exec(rm *)":"dney"}`,
		"a bad pattern":    `{"two words":"deny"}`,
		"a mode not text":  `{"os_exec":false}`,
		"everything":       `{"*":"allow"}`,
		"not JSON at all":  `{`,
		"a number, a rule": `7`,
	} {
		t.Run("fleet rules: "+what, func(t *testing.T) {
			r := newFleetRig(t)
			var saved json.RawMessage
			r.fleet.SetPersist(func() { _, saved = r.fleet.Snapshot() })
			r.fleet.Load(raws("aaaa", `{"permissions":{"exec":"allow"}}`), json.RawMessage(`{"os_exec(df *)":"allow"}`))
			n, problems := r.fleet.Load(raws("aaaa", `{"permissions":{"exec":"allow"}}`), json.RawMessage(raw))
			if n != 1 || len(problems) != 1 || !strings.Contains(problems[0], "fleet_rules") {
				t.Fatalf("%d change(s), problems %v", n, problems)
			}
			if _, err := r.fleet.Inherit(Scheme{}); err == nil {
				t.Error("Inherit made a scheme from malformed fleet rules")
			}
			if len(r.fleet.Rules().Snapshot()) != 0 {
				t.Error("rules from before the bad edit are still live")
			}
			if ok, n := r.confirm(r.attended(), "aaaa", "os_exec", "uptime"); !ok || n != 0 {
				t.Error("a confirmed machine must not be touched by the fleet's rules")
			}
			if err := r.fleet.Put("bbbb", Scheme{}); err != nil {
				t.Fatal(err)
			}
			if string(saved) != raw {
				t.Errorf("saved the fleet's rules as %q; want them as written", saved)
			}
			// Setting one through the editor replaces the malformed text.
			if err := r.fleet.Rules().Set("os_exec(df *)", "allow"); err != nil {
				t.Fatal(err)
			}
			if _, err := r.fleet.Inherit(Scheme{}); err != nil || string(saved) != `{"os_exec(df *)":"allow"}` {
				t.Errorf("after an edit: Inherit %v, saved %s", err, saved)
			}
		})
	}

	// Put is as strict as the file.
	r = newFleetRig(t)
	for _, s := range []Scheme{
		{Permissions: map[string]string{"exce": "deny"}},
		{Permissions: map[string]string{CatExec: "dney"}},
		{ToolRules: map[string]string{"two words": "deny"}},
		{ToolRules: map[string]string{"os_exec": "maybe"}},
	} {
		if err := r.fleet.Put("aaaa", s); err == nil {
			t.Errorf("Put took %+v", s)
		}
	}
	if _, err := r.fleet.Scheme("aaaa"); err == nil {
		t.Fatal("a refused Put left a scheme behind")
	}
}
