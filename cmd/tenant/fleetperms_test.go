package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"tenant/internal/approval"
	"tenant/internal/tui"
)

// fleetSettings is a fresh fleet over a settings file in a temp dir, loaded
// and saving the way serve and the TUI wire it. The process-wide fleet is put
// back when the test ends.
type fleetSettings struct {
	t      *testing.T
	cfg    string
	stg    *settings
	mu     sync.Mutex
	broker *approvalBroker
	told   []string
}

func newFleetSettings(t *testing.T, file string) *fleetSettings {
	t.Helper()
	old := defaultFleet
	defaultFleet = approval.NewFleet(categorize)
	t.Cleanup(func() { defaultFleet = old })
	f := &fleetSettings{t: t, cfg: t.TempDir(), broker: newApprovalBroker(nil)}
	f.broker.SetRules(newToolRules())
	if file != "" {
		f.write(file)
	}
	stg, err := loadSettings(f.cfg, "main")
	if err != nil {
		t.Fatal(err)
	}
	f.stg = stg
	loadFleetSettings(f.stg, &f.mu, f.cfg, "main", func(s string) { f.told = append(f.told, s) })
	return f
}

func (f *fleetSettings) path() string { return filepath.Join(f.cfg, "settings.main.json") }

func (f *fleetSettings) write(body string) {
	f.t.Helper()
	if err := os.WriteFile(f.path(), []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// file is the settings file as it stands, parsed.
func (f *fleetSettings) file() *settings {
	f.t.Helper()
	s, err := loadSettings(f.cfg, "main")
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *fleetSettings) apply() string {
	return applySettingsFile(f.cfg, "main", f.broker, nil, &f.mu, f.stg)
}

// confirm asks about an action on machine id from this computer and answers
// the card with d.
func (f *fleetSettings) confirm(id, action, detail string, d tui.ApprovalDecision) bool {
	f.t.Helper()
	go func() {
		req := <-f.broker.Requests()
		f.broker.Resolve(req.ID, d)
	}()
	return defaultFleet.For(approval.LocalOrigin(f.broker), approval.Machine{ID: id, Name: "box"}).Confirm(context.Background(), action, detail)
}

func schemeOf(t *testing.T, raw json.RawMessage) approval.Scheme {
	t.Helper()
	var s approval.Scheme
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return s
}

// A machine's scheme goes through the settings file and back: made at
// confirm from the master's saved scheme, changed by "always" on its card
// under its own id and nowhere else, and read again by the next process.
func TestFleetSettings_RoundTrip(t *testing.T) {
	f := newFleetSettings(t, `{"tools":{"os_exec":true},"permissions":{"exec":"ask","write":"allow","web":"allow"},"tool_rules":{"os_exec(rm *)":"deny"}}`)
	if len(f.told) != 0 {
		t.Fatalf("a file without the fleet's keys: told %q", f.told)
	}
	// Confirm: the master's saved scheme as it is, an allow included.
	local := approval.Scheme{Permissions: f.stg.Permissions, ToolRules: f.stg.ToolRules}
	for _, id := range []string{"m1", "m2"} {
		s, err := defaultFleet.Inherit(local)
		if err != nil {
			t.Fatal(err)
		}
		if err := defaultFleet.Put(id, s); err != nil {
			t.Fatal(err)
		}
	}
	m1 := schemeOf(t, f.file().FleetSchemes["m1"])
	if m1.Permissions["exec"] != "ask" || m1.Permissions["write"] != "allow" || m1.Permissions["web"] != "allow" || m1.ToolRules["os_exec(rm *)"] != "deny" {
		t.Fatalf("m1 as saved at confirm: %+v", m1)
	}

	// "Always" on m1's card.
	if !f.confirm("m1", "os_exec", "systemctl status nginx", tui.ApproveAlways) {
		t.Fatal("always must approve")
	}
	file := f.file()
	if got := schemeOf(t, file.FleetSchemes["m1"]).ToolRules["os_exec(systemctl status *)"]; got != "allow" {
		t.Fatalf("m1 in the file after always: %s", file.FleetSchemes["m1"])
	}
	if got := schemeOf(t, file.FleetSchemes["m2"]).ToolRules; len(got) != 1 {
		t.Fatalf("m2 gained a rule: %v", got)
	}
	if len(file.ToolRules) != 1 || file.Permissions["exec"] != "ask" || !file.Tools["os_exec"] {
		t.Fatalf("the master's own keys moved: rules %v, permissions %v, tools %v", file.ToolRules, file.Permissions, file.Tools)
	}
	// The process's own save is not an edit.
	if got := f.apply(); got != "" {
		t.Fatalf("the fleet's own save read as an edit: %q", got)
	}

	// The next process reads the same schemes.
	next := approval.NewFleet(categorize)
	if n, problems := next.Load(file.FleetSchemes, file.FleetRules); n != 2 || len(problems) != 0 {
		t.Fatalf("the next start: %d change(s), problems %v", n, problems)
	}
	for _, id := range []string{"m1", "m2"} {
		a, _ := defaultFleet.Scheme(id)
		b, err := next.Scheme(id)
		if ja, _ := json.Marshal(a); err != nil || string(ja) != string(mustJSON(t, b)) {
			t.Fatalf("%s reloaded as %+v (%v), want %+v", id, b, err, a)
		}
	}

	// Revoked, its scheme deleted: the key goes; with no machine left, both keys go.
	defaultFleet.Delete("m1")
	if _, ok := f.file().FleetSchemes["m1"]; ok {
		t.Fatal("a deleted scheme is still in the file")
	}
	defaultFleet.Delete("m2")
	if b, _ := os.ReadFile(f.path()); strings.Contains(string(b), "fleet") {
		t.Fatalf("with no machine and no fleet rule, the file still has the fleet's keys:\n%s", b)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A file from before the fleet: nothing is loaded, nothing is said, and a
// save of something else adds none of the fleet's keys.
func TestFleetSettings_OldFile(t *testing.T) {
	f := newFleetSettings(t, `{"tools":{},"permissions":{"exec":"ask"},"tool_rules":{"delete_*":"deny"}}`)
	if got := f.apply(); got != "" || len(f.told) != 0 {
		t.Fatalf("an old file: applied %q, told %q", got, f.told)
	}
	f.mu.Lock()
	f.stg.Tools["os_exec"] = false
	saveSettingsOrTell(f.stg, f.cfg, "main", func(s string) { t.Error(s) })
	f.mu.Unlock()
	if b, _ := os.ReadFile(f.path()); strings.Contains(string(b), "fleet") {
		t.Fatalf("a save added the fleet's keys to an old file:\n%s", b)
	}
	if err := defaultFleet.For(approval.LocalOrigin(f.broker), approval.Machine{ID: "m1"}).Reach(); err == nil {
		t.Fatal("a machine with no scheme was reachable")
	}
}

// Hand edits to the fleet's keys reach the running process as edits to
// tool_rules do: a scheme tightened, a machine added, a machine taken out.
// An entry that can't be read fails closed for that machine alone, is told
// to the operator, and survives the process's later saves as written.
func TestFleetSettings_HandEdits(t *testing.T) {
	f := newFleetSettings(t, `{"tools":{},"fleet_schemes":{"m1":{"permissions":{"exec":"allow"}},"m2":{"permissions":{"exec":"allow"}}},"fleet_rules":{"os_exec(df *)":"allow"}}`)
	local := approval.LocalOrigin(f.broker)
	run := func(id string) bool {
		return defaultFleet.For(local, approval.Machine{ID: id}).Confirm(context.Background(), "os_exec", "uptime")
	}
	if !run("m1") || !run("m2") {
		t.Fatal("both machines allow exec as loaded")
	}

	f.write(`{"tools":{},"fleet_schemes":{
		"m1":{"permissions":{"exec":"deny"}},
		"m2":{"permissions":{"exec":"alow"}},
		"m3":{"tool_rules":{"os_exec(uptime)":"allow"}}},
		"fleet_rules":{"os_exec(df *)":"allow","x_post":"deny"}}`)
	got := f.apply()
	if !strings.HasPrefix(got, "4 fleet scheme change(s) (") || !strings.Contains(got, `machine "m2"`) || !strings.Contains(got, "alow") {
		t.Fatalf("summary = %q", got)
	}
	if run("m1") {
		t.Error("m1 was tightened to deny in the file")
	}
	if run("m2") {
		t.Error("m2's entry can't be read: it must be refused, not left as it was")
	}
	if err := defaultFleet.For(local, approval.Machine{ID: "m2"}).Reach(); err == nil {
		t.Error("m2 is reachable with an entry that can't be read")
	}
	if !run("m3") {
		t.Error("m3 was added with a rule that allows uptime")
	}
	if defaultFleet.Rules().Mode("x_post") != "deny" {
		t.Error("the fleet's new rule was not loaded")
	}
	if got := f.apply(); got != "" {
		t.Errorf("the same file again: %q", got)
	}

	// A later save of anything keeps m2's text for the operator to fix.
	if !f.confirm("m3", "os_exec", "df -h", tui.ApproveAlways) {
		t.Fatal("always must approve")
	}
	file := f.file()
	if string(compactJSON(t, file.FleetSchemes["m2"])) != `{"permissions":{"exec":"alow"}}` {
		t.Errorf("m2 in the file after a save: %s", file.FleetSchemes["m2"])
	}
	if schemeOf(t, file.FleetSchemes["m3"]).ToolRules["os_exec(df *)"] != "allow" || schemeOf(t, file.FleetSchemes["m1"]).Permissions["exec"] != "deny" {
		t.Errorf("after a save: m3 %s, m1 %s", file.FleetSchemes["m3"], file.FleetSchemes["m1"])
	}
	if got := f.apply(); got != "" {
		t.Errorf("the process's own save read as an edit: %q", got)
	}

	// Fixed, and m1 taken out.
	f.write(`{"tools":{},"fleet_schemes":{"m2":{"permissions":{"exec":"allow"}},"m3":{"tool_rules":{"os_exec(uptime)":"allow"}}}}`)
	if got := f.apply(); got != "4 fleet scheme change(s)" { // m2 fixed, m3's saved rule gone, m1 out, the fleet's rules gone
		t.Fatalf("summary = %q", got)
	}
	if !run("m2") || run("m1") {
		t.Error("m2 was fixed and m1 taken out")
	}

	// A file that is not JSON at all is not applied: the fleet stays as it was.
	f.write(`{"fleet_schemes":`)
	if got := f.apply(); !strings.HasPrefix(got, "not applied") || !run("m2") || run("m1") {
		t.Errorf("a corrupt file: %q", got)
	}
}

func compactJSON(t *testing.T, raw json.RawMessage) []byte {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return mustJSON(t, v)
}

// Entries that can't be read at start are told to the operator, and those
// machines are refused; a malformed fleet_rules stops new machines only.
func TestFleetSettings_MalformedAtStart(t *testing.T) {
	f := newFleetSettings(t, `{"tools":{},"permissions":{"exec":"allow"},
		"fleet_schemes":{"good":{"permissions":{"exec":"allow"}},"bad":{"tool_rule":{"os_exec(rm *)":"deny"}},"worse":7},
		"fleet_rules":{"two words":"allow"}}`)
	if len(f.told) != 3 || !strings.Contains(strings.Join(f.told, "\n"), `machine "bad"`) || !strings.Contains(strings.Join(f.told, "\n"), "fleet_rules") {
		t.Fatalf("told %q", f.told)
	}
	// The master's own settings loaded: the fleet's keys spoil nothing else.
	if f.stg.Permissions["exec"] != "allow" {
		t.Fatalf("the master's own keys did not load: %+v", f.stg)
	}
	local := approval.LocalOrigin(f.broker)
	for id, want := range map[string]bool{"good": true, "bad": false, "worse": false, "never": false} {
		if got := defaultFleet.For(local, approval.Machine{ID: id}).Confirm(context.Background(), "os_exec", "uptime"); got != want {
			t.Errorf("%s: Confirm = %v, want %v", id, got, want)
		}
	}
	if _, err := defaultFleet.Inherit(approval.Scheme{}); err == nil {
		t.Error("a new machine was given a scheme from fleet rules that can't be read")
	}
}

// A change the fleet has made and not yet written is not undone by a watcher
// tick that reads the older file (TEN-428, for the fleet's keys).
func TestFleetSettings_InFlightChangeSurvives(t *testing.T) {
	f := newFleetSettings(t, `{"tools":{},"fleet_schemes":{"m1":{}}}`)
	// Live, but not saved: the hook is the last step of a change, and the tick
	// lands before it.
	defaultFleet.SetPersist(nil)
	child, err := defaultFleet.Child("m1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := child.SetPermissionFrom("tui", "exec", "deny"); err != nil {
		t.Fatal(err)
	}
	if got := f.apply(); got != "" {
		t.Fatalf("the fleet's own unsaved change read as an edit: %q", got)
	}
	if s, _ := defaultFleet.Scheme("m1"); s.Permissions["exec"] != "deny" {
		t.Fatal("the in-flight change was undone")
	}
	// A real edit after it still applies.
	f.write(`{"tools":{},"fleet_schemes":{"m1":{"permissions":{"web":"deny"}}}}`)
	if got := f.apply(); got != "1 fleet scheme change(s)" {
		t.Fatalf("hand edit summary = %q", got)
	}
	if s, _ := defaultFleet.Scheme("m1"); s.Permissions["web"] != "deny" || s.Permissions["exec"] != "ask" {
		t.Fatalf("after the hand edit: %+v", s)
	}
}

// Saves and watcher ticks race: every "always" ends up in the file and in
// the running fleet, whichever order they land in.
func TestFleetSettings_SavesAndTicksRace(t *testing.T) {
	f := newFleetSettings(t, `{"tools":{},"fleet_schemes":{"m1":{},"m2":{}}}`)
	stop, watched := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watched)
		for {
			select {
			case <-stop:
				return
			default:
				f.apply()
			}
		}
	}()
	var wg sync.WaitGroup
	cmds := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel"}
	for _, id := range []string{"m1", "m2"} {
		child, err := defaultFleet.Child(id)
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, c := range cmds {
				if err := child.SetRule("os_exec("+c+" *)", "allow"); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	<-watched
	f.apply()
	file := f.file()
	for _, id := range []string{"m1", "m2"} {
		live, _ := defaultFleet.Scheme(id)
		if saved := schemeOf(t, file.FleetSchemes[id]); len(live.ToolRules) != len(cmds) || len(saved.ToolRules) != len(cmds) {
			t.Fatalf("%s: %d rule(s) live, %d in the file, want %d of each", id, len(live.ToolRules), len(saved.ToolRules), len(cmds))
		}
	}
}
