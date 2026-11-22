package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"tenant/internal/approval"
	"tenant/internal/control"
	"tenant/internal/fleet"
)

// The spec's acceptance list for the fleet's first pull request (TEN-531),
// walked with real processes on this computer: one `tenant serve` as the
// master, two machines enrolled and run with `tenant child`, the operator at
// `tenant fleet` and at the dashboard. Each step is one item of the list, or
// one of the things nobody had run together before this test.
//
// What it cannot do here: a reboot is the child's process stopped and started
// again (no unit is registered); a relay's turn and a browser are in tests of
// their own (TestFleetARelayTurnCannotReadAFileOnAChild, TestFleetBrowserOnAChild);
// the real 30 s ping is TestFleetADeadLinkIsReportedWithinAMinute.
func TestFleetEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a master and its machines as processes")
	}
	h := thisHost(t)
	wire := newE2EWire(t)
	wire.mu.Lock()
	wire.record = true
	wire.mu.Unlock()
	m := h.master(true, "--os")
	m.serve()
	step := func(name string, fn func(t *testing.T)) {
		t.Helper()
		t.Run(name, fn)
		if t.Failed() {
			t.FailNow()
		}
	}
	fleetTools := func(turn e2eTurn) (names []string) {
		for _, n := range turn.Tools {
			if strings.HasPrefix(n, "fleet_") {
				names = append(names, n)
			}
		}
		return names
	}
	list := e2eScript(e2eCall("fleet_list", nil))
	approve := func(control.PendingApproval) string { return "approve" }
	var alpha, beta *e2eChild

	step("no fleet, no fleet tools", func(t *testing.T) {
		turn := m.turn(list, nil)
		if got := fleetTools(turn); len(got) != 0 || turn.Fleet != "" {
			t.Fatalf("a master with no fleet offers its agent %v and tells it %q", got, turn.Fleet)
		}
	})

	step("enroll: pending until the operator types the six digits", func(t *testing.T) {
		m.init(wire.addr)
		wire.to(m.fleet)
		alpha = m.enroll(h, "alpha")
		alpha.run()
		if st := alpha.state("waiting for confirmation"); !strings.Contains(st, alpha.digits) {
			t.Errorf("the pending machine's status does not show its six digits (%s):\n%s", alpha.digits, st)
		}
		if out := m.must(nil, "fleet", "list"); !regexp.MustCompile(`alpha\s+pending`).MatchString(out) {
			t.Errorf("fleet list:\n%s", out)
		}
		// Until it is confirmed the master sends it nothing, and the agent has
		// no fleet tool to try with.
		if turn := m.turn(list, nil); len(fleetTools(turn)) != 0 || turn.Fleet != "" {
			t.Errorf("with one pending machine the agent is offered %v and told %q", fleetTools(turn), turn.Fleet)
		}
		// Nobody at a terminal, nobody confirms; the wrong digits confirm nothing.
		if _, errOut, exit := m.cli(nil, "fleet", "confirm", "alpha"); exit == 0 || !strings.Contains(errOut, "terminal") {
			t.Errorf("confirm with nobody at a terminal: exit %d\n%s", exit, errOut)
		}
		wrong := "000000"
		if wrong == alpha.digits {
			wrong = "111111"
		}
		if _, errOut, exit := m.cli(&wrong, "fleet", "confirm", "alpha"); exit == 0 || !strings.Contains(errOut, "not the code") {
			t.Errorf("confirm with the wrong digits: exit %d\n%s", exit, errOut)
		}
		screen := alpha.confirm()
		for _, want := range []string{"Reported by the machine", "confirmed alpha", `"` + h.os + `"`} {
			if !strings.Contains(screen, want) {
				t.Errorf("the confirm screen lacks %q:\n%s", want, screen)
			}
		}
		alpha.state("connected to the master")
		beta = m.join(h, "beta")

		turn := m.turn(list, nil)
		if got := fleetTools(turn); len(got) != len(fleetToolSpecs()) {
			t.Errorf("with two confirmed machines the agent is offered %v, want the %d fleet tools", got, len(fleetToolSpecs()))
		}
		for _, name := range []string{"alpha", "beta"} {
			if want := "- " + name + " (" + fleetOS(h.os) + "): online\n"; !strings.Contains(turn.Fleet, want) {
				t.Errorf("the agent's context lacks %q:\n%s", want, turn.Fleet)
			}
		}
		if len(turn.Results) != 1 || !strings.Contains(turn.Results[0], "alpha ("+alpha.id+")") || !strings.Contains(turn.Results[0], "beta ("+beta.id+")") {
			t.Errorf("fleet_list: %q", turn.Results)
		}
	})

	// Markers a capture of the wire must not show (the step after next).
	const cmdMark, outA, outB, fileMark = "CMDLINE-7c1e9a", "OUTPUT-", "5b2f8d", "FILEBODY-93aa41"
	noteFile := filepath.Join(h.root, "note.txt")
	if err := os.WriteFile(noteFile, []byte(fileMark+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	step("an approved command runs on each machine; its card names the host", func(t *testing.T) {
		for _, c := range []*e2eChild{alpha, beta} {
			word := "ran-on-" + c.name
			res, cards := m.one("fleet_exec", map[string]any{"host": c.name, "command": h.say(word)}, "approve")
			if len(cards) != 1 || cards[0].Host != c.id || cards[0].HostName != c.name || cards[0].Action != "os_exec" ||
				!strings.HasPrefix(cards[0].Detail, `[host `+c.id+` "`+c.name+`"] `) || !strings.Contains(cards[0].Detail, word) {
				t.Errorf("the card for a command on %s: %+v", c.name, cards)
			}
			if !strings.HasPrefix(res, "[fleet "+c.name+" ("+c.id+", "+fleetOS(h.os)+"): os_exec.") || !strings.Contains(res, "\n"+word) {
				t.Errorf("the command's result as the model reads it:\n%s", res)
			}
		}
		// A file is read with no card: a read asks nobody.
		res, cards := m.one("fleet_read", map[string]any{"host": "alpha", "path": noteFile}, "")
		if len(cards) != 0 || !strings.Contains(res, fileMark) {
			t.Errorf("fleet_read: %d cards\n%s", len(cards), res)
		}
		// And a command with the markers, for the capture.
		res, _ = m.one("fleet_exec", map[string]any{"host": "alpha", "command": h.sayJoined(outA, outB) + "; " + h.say(cmdMark)}, "approve")
		if !strings.Contains(res, outA+outB) || !strings.Contains(res, cmdMark) {
			t.Errorf("the marked command:\n%s", res)
		}
	})

	step("a destructive command raises a card on the master that names the host", func(t *testing.T) {
		res, cards := m.one("fleet_exec", map[string]any{"host": "beta", "command": destructiveOn(h)}, "deny")
		if len(cards) != 1 || cards[0].Action != "os_exec_dangerous" || cards[0].Category != approval.CatDestructive ||
			cards[0].Host != beta.id || cards[0].HostName != "beta" || !strings.HasPrefix(cards[0].Detail, `[host `+beta.id+` "beta"] `) {
			t.Errorf("the card for a destructive command: %+v", cards)
		}
		if !strings.Contains(res, "looks destructive") || !strings.Contains(res, "was not approved") || strings.Contains(res, "\ndiskpart") {
			t.Errorf("a denied destructive command:\n%s", res)
		}
	})

	step("always on one machine carries to no other machine and not to the master", func(t *testing.T) {
		probe := filepath.Join(h.root, "probe.txt")
		touch := map[string]any{"command": h.touch(probe)}
		on := func(host string) map[string]any { return map[string]any{"host": host, "command": touch["command"]} }
		if res, cards := m.one("fleet_exec", on("alpha"), "approve_always"); len(cards) != 1 {
			t.Fatalf("the first run on alpha raised %d cards: %+v\n%s", len(cards), cards, res)
		}
		if _, err := os.Stat(probe); err != nil {
			t.Errorf("the command approved with always did not run: %v", err)
		}
		if res, cards := m.one("fleet_exec", on("alpha"), "deny"); len(cards) != 0 || strings.Contains(res, "wasn't approved") {
			t.Errorf("after always, the same command on alpha asked again (%d cards):\n%s", len(cards), res)
		}
		if res, cards := m.one("fleet_exec", on("beta"), "deny"); len(cards) != 1 || cards[0].Host != beta.id || !strings.Contains(res, "wasn't approved") {
			t.Errorf("always on alpha answered for beta (%d cards):\n%s", len(cards), res)
		}
		if res, cards := m.one("os_exec", touch, "deny"); len(cards) != 1 || cards[0].Host != "" || !strings.Contains(res, "wasn't approved") {
			t.Errorf("always on alpha answered for the master itself (%+v):\n%s", cards, res)
		}
		// Where the rule was saved: under alpha's id, and nowhere else.
		stg, err := loadSettings(m.cfg, "main")
		if err != nil {
			t.Fatal(err)
		}
		var a, b approval.Scheme
		_ = json.Unmarshal(stg.FleetSchemes[alpha.id], &a)
		_ = json.Unmarshal(stg.FleetSchemes[beta.id], &b)
		if len(a.ToolRules) != 1 || len(b.ToolRules) != 0 || len(stg.ToolRules) != 0 || len(stg.FleetRules) > 2 {
			t.Errorf("the rule always saved: alpha %v, beta %v, the master %v, the fleet's rules %s", a.ToolRules, b.ToolRules, stg.ToolRules, stg.FleetRules)
		}
		if out := m.must(nil, "fleet", "permissions", "alpha"); !strings.Contains(out, "allow") || !strings.Contains(out, "os_exec(") {
			t.Errorf("fleet permissions alpha does not show the rule:\n%s", out)
		}
	})

	step("a capture of the wire shows nothing readable, and the session is TLS 1.3", func(t *testing.T) {
		conns := wire.recorded()
		if len(conns) < 4 { // two enrollments, two links at least
			t.Fatalf("the wire recorded %d connections", len(conns))
		}
		secrets := []string{cmdMark, outA + outB, fileMark, "alpha", "beta", alpha.id, beta.id, "tntfleet1_",
			"os_exec", "os_read_file", "fleet_", "tools/call", "tools/list", "fleet/confirm", "fleet/hello", "jsonrpc",
			"/fleet/enroll", "/fleet/link", "HTTP/1.1", "Upgrade", "websocket", "Sec-WebSocket", "POST ", "GET ", "Write-Output", "echo ", "note.txt"}
		var total int
		for i, c := range conns {
			for dir, stream := range c {
				total += len(stream)
				for _, s := range secrets {
					if bytes.Contains(stream, []byte(s)) {
						t.Errorf("connection %d, direction %d: the wire shows %q in the clear", i, dir, s)
					}
				}
				if err := onlyTLS13Records(stream, dir == 1); err != nil {
					t.Errorf("connection %d, direction %d: %v", i, dir, err)
				}
			}
		}
		t.Logf("the wire carried %d connections and %d bytes: enrollment, the links, commands, a file", len(conns), total)
	})

	step("a certificate the master did not pin is dropped", func(t *testing.T) {
		child, err := fleet.LoadChild(fleet.ChildPath(alpha.dir))
		if err != nil {
			t.Fatal(err)
		}
		stranger, err := fleet.MintIdentity(fleet.ChildIdentityPath(t.TempDir()))
		if err != nil {
			t.Fatal(err)
		}
		if got := linkAnswer(t, m.fleet, stranger, child.MasterFingerprint); got != "" {
			t.Errorf("a stranger's certificate got an answer from the fleet port: %q", got)
		}
		// The same request with a pinned certificate is answered: the test
		// above is not passing because the request is wrong. alpha's own
		// link is up, so a second one with its certificate is answered, not
		// dropped.
		own, err := fleet.LoadIdentity(fleet.ChildIdentityPath(alpha.dir))
		if err != nil {
			t.Fatal(err)
		}
		if got := linkAnswer(t, m.fleet, own, child.MasterFingerprint); !strings.HasPrefix(got, "HTTP/1.1 ") {
			t.Errorf("alpha's own certificate got no answer from the fleet port: %q", got)
		}
	})

	step("a reboot: the child's process ends and starts again, and connects on its own", func(t *testing.T) {
		alpha.proc.stop() // a service stop
		alpha.state("not running")
		if res, _ := m.one("fleet_exec", map[string]any{"host": "alpha", "command": h.say("x")}, "approve"); !strings.Contains(res, "alpha is offline") {
			t.Errorf("a call to a machine that is down:\n%s", res)
		}
		alpha.run()
		alpha.state("connected to the master")
		beta.proc.kill() // a power cut
		beta.state("not running")
		beta.run()
		beta.state("connected to the master")
		for _, c := range []*e2eChild{alpha, beta} {
			if res, _ := m.one("fleet_exec", map[string]any{"host": c.name, "command": h.say("back-" + c.name)}, "approve"); !strings.Contains(res, "\nback-"+c.name) {
				t.Errorf("a command on %s after its restart:\n%s", c.name, res)
			}
		}
	})

	step("the ceiling lowered on the machine's disk mid-session", func(t *testing.T) {
		if err := fleet.UpdateChild(fleet.ChildPath(alpha.dir), func(c *fleet.Child) error { c.Ceiling = fleet.CeilingReadOnly; return nil }); err != nil {
			t.Fatal(err)
		}
		res, cards := m.one("fleet_exec", map[string]any{"host": "alpha", "command": h.say("under-a-ceiling")}, "approve")
		lines := strings.Split(res, "\n")
		if len(cards) != 0 || len(lines) < 3 || lines[len(lines)-2] != "[fleet: this machine refused, without asking its master: os_exec (ceiling read-only)]" ||
			!strings.HasPrefix(lines[len(lines)-1], "[/fleet ") || strings.Contains(res, "\nunder-a-ceiling") {
			t.Errorf("a command under a read-only ceiling: %d cards, and the model reads:\n%s", len(cards), res)
		}
		if res, _ := m.one("fleet_read", map[string]any{"host": "alpha", "path": noteFile}, ""); !strings.Contains(res, fileMark) {
			t.Errorf("a read under a read-only ceiling:\n%s", res)
		}
		if err := fleet.UpdateChild(fleet.ChildPath(alpha.dir), func(c *fleet.Child) error { c.Ceiling = fleet.CeilingFull; return nil }); err != nil {
			t.Fatal(err)
		}
		if res, _ := m.one("fleet_exec", map[string]any{"host": "alpha", "command": h.say("ceiling-lifted")}, "approve"); !strings.Contains(res, "\nceiling-lifted") {
			t.Errorf("a command after the ceiling was put back:\n%s", res)
		}
	})

	step("the master's record of a call carries the hash of the child's audit record", func(t *testing.T) {
		m.one("fleet_exec", map[string]any{"host": "beta", "command": h.say("audited")}, "approve")
		out, exit := beta.tenant("audit")
		last := regexp.MustCompile(`Last line: ([0-9a-f]{64})`).FindStringSubmatch(out)
		if exit != 0 || last == nil || !strings.Contains(out, "chain holds") {
			t.Fatalf("tenant child audit on beta: exit %d\n%s", exit, out)
		}
		var found string
		for deadline := time.Now().Add(e2eWait); found == "" && time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			days, _ := filepath.Glob(filepath.Join(m.data, "logs", "fleet", "*.log"))
			for _, day := range days {
				raw, _ := os.ReadFile(day)
				for _, line := range strings.Split(string(raw), "\n") {
					if strings.Contains(line, "fleet call") && strings.Contains(line, "audit="+last[1]) && strings.Contains(line, "host="+beta.id) {
						found = line
					}
				}
			}
		}
		if found == "" || !strings.Contains(found, "trace=") || !strings.Contains(found, "tool=os_exec") {
			t.Errorf("no record in the master's fleet log names beta, its turn and the audit hash %s (found %q)", last[1], found)
		}
	})

	// file is a path in the test's directory; exists reports a file there.
	file := func(name string) string { return filepath.Join(h.root, name) }
	there := func(name string) bool { return exists(file(name)) }
	// running waits until the command that writes name on its first line runs.
	running := func(t *testing.T, name string) time.Time {
		t.Helper()
		for deadline := time.Now().Add(e2eWait); !there(name); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("the command that writes %s never started\n%s", name, m.proc.said())
			}
		}
		return time.Now()
	}
	// never waits until a command started at since and stopped before its
	// secs were up would have written name, and reports whether it did not.
	never := func(since time.Time, secs int, name string) bool {
		time.Sleep(time.Until(since.Add(time.Duration(secs)*time.Second + 3*time.Second)))
		return !there(name)
	}

	step("pause on the machine while a command runs", func(t *testing.T) {
		m.begin(e2eScript(e2eCall("fleet_exec", map[string]any{"host": "alpha", "command": h.slow(file("pause-start"), 12, file("pause-end"), "never-printed"), "timeout": 120})))
		paused := make(chan time.Time, 1)
		go func() {
			since := running(t, "pause-start")
			if out, exit := alpha.tenant("pause"); exit != 0 {
				t.Errorf("tenant child pause: exit %d\n%s", exit, out)
			}
			paused <- since
		}()
		turn := m.finish(approve)
		since := <-paused
		id := regexp.MustCompile(`call (\S+) was in flight`).FindStringSubmatch(strings.Join(turn.Results, "\n"))
		if id == nil || !strings.Contains(turn.Results[0], "OUTCOME UNKNOWN") || time.Since(since) > 10*time.Second {
			t.Fatalf("a call cut by a pause on the machine ended %s after the command started, and the model reads:\n%s", time.Since(since).Round(time.Second), turn.Results)
		}
		alpha.state("paused on this machine")
		if res, _ := m.one("fleet_exec", map[string]any{"host": "alpha", "command": h.say("x")}, "approve"); !strings.Contains(res, "alpha is offline") {
			t.Errorf("a call to a machine paused on itself:\n%s", res)
		}
		if !never(since, 12, "pause-end") {
			t.Error("the command went on running on a paused machine")
		}
		if out, exit := alpha.tenant("resume"); exit != 0 {
			t.Fatalf("tenant child resume: exit %d\n%s", exit, out)
		}
		alpha.state("connected to the master")
		// What became of the call: the machine dropped its record with the pause.
		res, _ := m.one("fleet_process", map[string]any{"host": "alpha", "action": "result", "id": id[1]}, "")
		if !strings.Contains(res, "has no record of call "+id[1]) {
			t.Errorf("the result of a call a pause cut:\n%s", res)
		}
	})

	step("the operator's stop reaches a command on a machine, starting or running", func(t *testing.T) {
		// Running: the command is on its way when the operator presses Stop.
		m.begin(e2eScript(e2eCall("fleet_exec", map[string]any{"host": "beta", "command": h.slow(file("stop-start"), 8, file("stop-end"), "never-printed"), "timeout": 120})))
		go m.approve(func() bool { return there("stop-start") }) // the card, until the command runs
		since := running(t, "stop-start")
		m.stop()
		if !never(since, 8, "stop-end") {
			t.Error("a command went on running on the machine after the operator stopped the turn")
		}
		// Starting: with commands allowed on alpha, the stop follows the call at
		// once, and a little later, and later still. None runs to its end.
		from := m.proc.out.count()
		m.must(nil, "fleet", "permissions", "alpha", "set", "exec", "allow")
		m.schemes(from)
		delays := []time.Duration{0, 5 * time.Millisecond, 20 * time.Millisecond, 60 * time.Millisecond, 150 * time.Millisecond, 400 * time.Millisecond}
		var last time.Time
		for i, d := range delays {
			m.begin(e2eScript(e2eCall("fleet_exec", map[string]any{"host": "alpha",
				"command": h.slow(file(fmt.Sprintf("race-start-%d", i)), 5, file(fmt.Sprintf("race-end-%d", i)), "never-printed"), "timeout": 120})))
			time.Sleep(d)
			m.stop()
			last = time.Now()
		}
		time.Sleep(time.Until(last.Add(9 * time.Second)))
		started := 0
		for i := range delays {
			if there(fmt.Sprintf("race-start-%d", i)) {
				started++
			}
			if there(fmt.Sprintf("race-end-%d", i)) {
				t.Errorf("the command stopped %s after its turn began ran to its end on the machine", delays[i])
			}
		}
		t.Logf("of %d turns stopped %v after they began, %d had started their command on the machine; none finished", len(delays), delays, started)
		from = m.proc.out.count()
		m.must(nil, "fleet", "permissions", "alpha", "set", "exec", "ask")
		m.schemes(from)
		if res, cards := m.one("fleet_exec", map[string]any{"host": "alpha", "command": h.say("after-the-stops")}, "approve"); len(cards) != 1 || !strings.Contains(res, "\nafter-the-stops") {
			t.Errorf("a command after the stopped turns (%d cards):\n%s", len(cards), res)
		}
	})

	step("revoke from the command line while serve holds the fleet", func(t *testing.T) {
		gamma := m.join(h, "gamma")
		out := m.must(nil, "fleet", "revoke", "gamma", "--delete-scheme")
		if !strings.Contains(out, "revoked gamma") {
			t.Errorf("fleet revoke:\n%s", out)
		}
		gamma.proc.ended("the revoked machine's child stops by itself")
		if code := gamma.proc.cmd.ProcessState.ExitCode(); code != 0 {
			t.Errorf("a revoked child exited %d, want 0: its unit must not start it again\n%s", code, gamma.proc.said())
		}
		if st, exit := gamma.tenant("status"); exit != 3 || !strings.Contains(st, "it is revoked") {
			t.Errorf("status on a revoked machine: exit %d\n%s", exit, st)
		}
		turn := m.turn(e2eScript(e2eCall("fleet_list", nil), e2eCall("fleet_read", map[string]any{"host": "gamma", "path": noteFile})), nil)
		if len(turn.Results) != 2 || strings.Contains(turn.Results[0], "gamma") || strings.Contains(turn.Fleet, "gamma") || !strings.Contains(turn.Results[1], `no machine named "gamma"`) {
			t.Errorf("the agent after gamma was revoked: told %q, and reads %q", turn.Fleet, turn.Results)
		}
		// Run again, as a unit set to restart would: it does not connect.
		gamma.run()
		gamma.proc.ended("a revoked child that is started again stops again")
	})

	// A second master process on the same dirs.
	second := h.master(true, "--os")
	step("two masters on one data dir: one holds the fleet, the other says which", func(t *testing.T) {
		second.serve()
		if second.fleet != "" {
			t.Fatalf("two processes listen for one fleet: %s and %s", m.fleet, second.fleet)
		}
		turn := second.turn(e2eScript(e2eCall("fleet_read", map[string]any{"host": "alpha", "path": noteFile})), nil)
		holder := fmt.Sprintf("tenant serve (process %s) holds the fleet, on %s", m.proc.pid, m.fleet)
		if len(turn.Results) != 1 || !strings.Contains(turn.Results[0], holder) || strings.Contains(turn.Results[0], fileMark) {
			t.Errorf("a fleet call from the process that does not hold the fleet reads %q, want it to name the holder: %s", turn.Results, holder)
		}
		if !strings.Contains(turn.Fleet, "- alpha ("+fleetOS(h.os)+"): unknown\n") || !strings.Contains(turn.Fleet, "does not hold the fleet's links") {
			t.Errorf("the agent of the process that does not hold the fleet is told:\n%s", turn.Fleet)
		}
		// The holder goes; the other takes the fleet, and the machines find it.
		from := second.proc.out.count()
		m.proc.stop()
		second.listening(from)
		second.master = true
		wire.to(second.fleet)
		alpha.state("connected to the master")
		if res, _ := second.one("fleet_read", map[string]any{"host": "alpha", "path": noteFile}, ""); !strings.Contains(res, fileMark) {
			t.Errorf("a read through the master that took the fleet over:\n%s", res)
		}
	})

	step("the master is killed with a call in flight, and comes back", func(t *testing.T) {
		m := second
		m.begin(e2eScript(e2eCall("fleet_exec", map[string]any{"host": "alpha", "command": h.slow(file("kill-start"), 5, file("kill-end"), "finished-without-its-master"), "timeout": 120})))
		go m.approve(func() bool { return there("kill-start") })
		since := running(t, "kill-start")
		time.Sleep(200 * time.Millisecond) // the approving goroutine sees the file and stops asking
		m.proc.kill()
		alpha.state("trying again")
		// The command runs on without a master, as the spec says a call does
		// when its link drops.
		for deadline := since.Add(30 * time.Second); !there("kill-end"); time.Sleep(50 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("the command did not finish on the machine after its master was killed")
			}
		}
		m.serve()
		wire.to(m.fleet)
		alpha.state("connected to the master")
		beta.state("connected to the master")
		id := lastAuditCall(t, alpha.dir, "os_exec")
		res, _ := m.one("fleet_process", map[string]any{"host": "alpha", "action": "result", "id": id}, "")
		if !strings.Contains(res, "\nfinished-without-its-master") || !strings.HasPrefix(res, "[fleet alpha ("+alpha.id) {
			t.Errorf("fleet_process result for call %s, asked of the machine by the master that came back:\n%s", id, res)
		}
		if out, exit := alpha.tenant("audit"); exit != 0 || !strings.Contains(out, "chain holds") {
			t.Errorf("tenant child audit after all of this: exit %d\n%s", exit, out)
		}
	})
}

// lastAuditCall is the id of the last call of tool in the audit log of the
// child in dir: where an operator finds the id of a call whose master is
// gone.
func lastAuditCall(t *testing.T, dir, tool string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fleet.AuditDir(dir), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	id := ""
	for _, line := range strings.Split(string(raw), "\n") {
		var rec fleet.AuditRecord
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Event == fleet.AuditCall && rec.Tool == tool {
			id = rec.Call
		}
	}
	if id == "" {
		t.Fatalf("no call of %s in the audit log of %s", tool, dir)
	}
	return id
}

// destructiveOn is a command the classifier flags as destructive, for the
// word in it, that would only print that word if a gate failed to hold it.
func destructiveOn(h *e2eHost) string { return h.say("diskpart") }

// onlyTLS13Records reads one direction of a recorded connection as TLS
// records. After the hello, which is in the clear in every TLS version,
// nothing but encrypted records may follow (and the one-byte compatibility
// record TLS 1.3 sends for middleboxes); fromServer also checks that the
// server chose TLS 1.3.
func onlyTLS13Records(stream []byte, fromServer bool) error {
	first := true
	for len(stream) > 0 {
		if len(stream) < 5 {
			return fmt.Errorf("%d stray bytes that are no TLS record", len(stream))
		}
		typ, n := stream[0], int(binary.BigEndian.Uint16(stream[3:5]))
		if len(stream) < 5+n {
			return fmt.Errorf("a TLS record of %d bytes is cut short", n)
		}
		body := stream[5 : 5+n]
		switch {
		case first && typ == 0x16: // handshake: the hello
			if fromServer {
				if err := serverChoseTLS13(body); err != nil {
					return err
				}
			}
		case !first && typ == 0x17: // application data: encrypted
		case !first && typ == 0x14 && n == 1: // change cipher spec, for middleboxes
		default:
			return fmt.Errorf("record type %#x of %d bytes where only encrypted records may be (first record: %v)", typ, n, first)
		}
		first = false
		stream = stream[5+n:]
	}
	if first {
		return fmt.Errorf("nothing was sent")
	}
	return nil
}

// serverChoseTLS13 reads a ServerHello and reports an error unless its
// supported_versions extension says TLS 1.3.
func serverChoseTLS13(hs []byte) error {
	if len(hs) < 4 || hs[0] != 0x02 {
		return fmt.Errorf("the server's first message is not a ServerHello")
	}
	b := hs[4:]
	if len(b) < 35 {
		return fmt.Errorf("a short ServerHello")
	}
	b = b[34:]       // legacy version, random
	sid := int(b[0]) // legacy session id
	if len(b) < 1+sid+5 {
		return fmt.Errorf("a short ServerHello")
	}
	b = b[1+sid+3:] // cipher suite, compression method
	ext := int(binary.BigEndian.Uint16(b[:2]))
	if b = b[2:]; len(b) < ext {
		return fmt.Errorf("a short ServerHello")
	}
	for b = b[:ext]; len(b) >= 4; {
		typ, n := binary.BigEndian.Uint16(b[:2]), int(binary.BigEndian.Uint16(b[2:4]))
		if len(b) < 4+n {
			break
		}
		if typ == 43 && n == 2 { // supported_versions
			if v := binary.BigEndian.Uint16(b[4:6]); v != tls.VersionTLS13 {
				return fmt.Errorf("the server chose TLS version %#x, not 1.3", v)
			}
			return nil
		}
		b = b[4+n:]
	}
	return fmt.Errorf("the ServerHello names no TLS 1.3")
}

// linkAnswer dials the fleet port with id as the client certificate, asks
// for the link as a child does, and returns the first line of the answer:
// "" when the connection was dropped with none.
func linkAnswer(t *testing.T, addr string, id fleet.Identity, masterFingerprint string) string {
	t.Helper()
	cfg, err := fleet.ChildTLS(id, masterFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(20 * time.Second))
	conn := tls.Client(raw, cfg)
	// In TLS 1.3 the client finishes its handshake before the server has
	// judged its certificate: the verdict comes with the first read.
	if err := conn.Handshake(); err != nil {
		return ""
	}
	if conn.ConnectionState().Version != tls.VersionTLS13 {
		t.Errorf("the fleet port speaks TLS %#x", conn.ConnectionState().Version)
	}
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: fleet\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n", fleet.LinkPath); err != nil {
		return ""
	}
	line, _ := bufio.NewReader(conn).ReadString('\n')
	return strings.TrimSpace(line)
}
