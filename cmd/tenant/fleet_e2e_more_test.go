package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"tenant/internal/control"
	"tenant/internal/fleet"
)

// The rest of the acceptance list (TEN-531), each in a test of its own
// because it needs something TestFleetEndToEnd does not have: a second
// operating system, a relay's turn, a browser, the real 30 s ping, minutes
// of waiting, or a hundred processes. TENANT_FLEET_E2E names the ones a
// plain `go test` leaves out: browser, long, spike.

// e2eWants reports whether TENANT_FLEET_E2E asks for the tests of this kind.
func e2eWants(kind string) bool {
	return slices.Contains(strings.FieldsFunc(os.Getenv("TENANT_FLEET_E2E"), func(r rune) bool { return r == ',' || r == ' ' }), kind)
}

// wslHost is the Linux inside WSL, for a test that runs on Windows:
// TENANT_FLEET_E2E_LINUX is this test binary built for Linux, as WSL names
// it (a path under /tmp). Nothing is installed there and nothing is left:
// the test's directory is removed when it ends.
func wslHost(t *testing.T) *e2eHost {
	t.Helper()
	bin := os.Getenv("TENANT_FLEET_E2E_LINUX")
	if runtime.GOOS != "windows" || bin == "" {
		t.Skip("needs Windows with WSL, and TENANT_FLEET_E2E_LINUX set to this test binary built for Linux (as WSL names it)")
	}
	var id [6]byte
	_, _ = rand.Read(id[:])
	h := &e2eHost{t: t, wsl: true, bin: bin, root: "/tmp/tenant-fleet-e2e-" + hex.EncodeToString(id[:]), os: "linux"}
	sh := func(args ...string) {
		cmd := exec.Command("wsl.exe", append([]string{"-d", e2eDistro, "-e"}, args...)...)
		e2eQuiet(cmd)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("wsl %v: %v\n%s", args, err, out)
		}
	}
	sh("mkdir", "-p", h.join("home"))
	t.Cleanup(func() { sh("rm", "-rf", h.root) })
	return h
}

// Acceptance: enroll a Linux and a Windows machine with one master, run an
// approved command on each, and reboot each. The master is a Linux `tenant
// serve` inside WSL; its machines are a Linux one beside it and this Windows
// computer, which reaches the master's loopback port through WSL's
// forwarding. (The other direction, a Linux machine dialing a Windows
// master, would need the master on a non-loopback address.)
func TestFleetLinuxMasterWindowsChild(t *testing.T) {
	lin := wslHost(t)
	win := thisHost(t)
	m := lin.master(true, "--os")
	m.serve()
	m.init("")
	machines := []*e2eChild{m.join(lin, "lin1"), m.join(win, "win1")}

	turn := m.turn(e2eScript(e2eCall("fleet_list", nil)), nil)
	for _, c := range machines {
		if want := "- " + c.name + " (" + fleetOS(c.host.os) + "): online\n"; !strings.Contains(turn.Fleet, want) {
			t.Errorf("the Linux master's agent is not told %q:\n%s", want, turn.Fleet)
		}
	}
	run := func(word string) {
		t.Helper()
		for _, c := range machines {
			w := word + "-" + c.name
			res, cards := m.one("fleet_exec", map[string]any{"host": c.name, "command": c.host.say(w)}, "approve")
			if len(cards) != 1 || cards[0].HostName != c.name || cards[0].Host != c.id {
				t.Errorf("the card for a command on %s: %+v", c.name, cards)
			}
			if !strings.HasPrefix(res, "[fleet "+c.name+" ("+c.id+", "+fleetOS(c.host.os)+"): os_exec.") || !strings.Contains(res, "\n"+w) {
				t.Errorf("a command on %s:\n%s", c.name, res)
			}
		}
	}
	run("first")
	// What each machine says it is, in its own words, on the master.
	for _, c := range machines {
		if out := m.must(nil, "fleet", "show", c.name); !strings.Contains(out, `os        "`+c.host.os+`"`) || !strings.Contains(out, "confirmed") {
			t.Errorf("fleet show %s:\n%s", c.name, out)
		}
	}
	// A reboot: each machine's child ends and is started again, as its unit
	// would start it at boot. It connects with no code and no confirmation.
	for _, c := range machines {
		c.proc.stop()
		c.state("not running")
		c.run()
		c.state("connected to the master")
	}
	run("after-reboot")
	// A file on the Windows machine, read by the Linux master's agent.
	note := filepath.Join(win.root, "note.txt")
	if err := os.WriteFile(note, []byte("read across two operating systems\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if res, _ := m.one("fleet_read", map[string]any{"host": "win1", "path": note}, ""); !strings.Contains(res, "read across two operating systems") {
		t.Errorf("fleet_read on the Windows machine:\n%s", res)
	}
}

// Acceptance: a relay turn cannot read a file on a child, and can once that
// relay is opened for the fleet. The machine is a real `tenant child`
// process; the master's hub, the fleet tools and the relay's broker are the
// real ones, in this process. What is not real is the relay: no Discord
// gateway runs offline, so the turn's origin is stamped on the context the
// way the relay's runner stamps it (withExecSurface), and the tool is
// dispatched directly, with no model.
func TestFleetARelayTurnCannotReadAFileOnAChild(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a machine as a process")
	}
	l := wiredLab(t)
	h := thisHost(t)
	dir, scratch := h.join("web1", "child"), h.join("web1")
	code := strings.SplitN(l.must("enroll", "web1"), "\n", 2)[0]
	out, errOut, exit := h.run(e2eSpec{Argv: []string{"child", "install", "--dir", dir}, Env: map[string]string{childCodeEnv: code}, Scratch: scratch})
	digits := confirmationCode.FindStringSubmatch(out)
	if exit != 0 || digits == nil {
		t.Fatalf("child install: exit %d\n%s\n%s", exit, out, errOut)
	}
	l.answer = digits[1]
	l.must("confirm", "web1")
	l.sync()
	h.start("child web1", e2eSpec{Argv: []string{"child", "run", "--dir", dir}, Scratch: scratch})
	mach, err := fleet.Open(l.cfg).ByName("web1")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the machine links", func() bool { return online(fleetManager(), mach.ID) })
	note := filepath.Join(h.root, "note.txt")
	if err := os.WriteFile(note, []byte("only for an opened relay\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := func() int {
		sum, err := fleet.VerifyAudit(dir)
		if err != nil {
			t.Fatal(err)
		}
		return sum.Records
	}

	fleetRelayBroker(surfaceDiscord, newDiscordApprovalBroker(quietLog))
	relay := func() (string, bool) {
		return fleetTool(t, withExecSurface(fleetTurnCtx(), surfaceDiscord), "fleet_read", map[string]any{"host": "web1", "path": note})
	}
	res, isErr := relay()
	if !isErr || !strings.Contains(res, "discord is not opened for the fleet") || !strings.Contains(res, "reads included") || strings.Contains(res, "only for an opened relay") {
		t.Fatalf("a Discord turn read a file on a machine, or was not told why it may not: %q", res)
	}
	if list, _ := fleetTool(t, withExecSurface(fleetTurnCtx(), surfaceDiscord), "fleet_list", nil); strings.Contains(list, "web1") {
		t.Errorf("a relay that is not opened learns the fleet's machines: %q", list)
	}
	if n := calls(); n != 0 {
		t.Fatalf("the machine's audit log has %d records: a refused relay's call reached it", n)
	}
	// The operator's own turn reads it: the refusal above is the relay's.
	if res, isErr := fleetTool(t, fleetTurnCtx(), "fleet_read", map[string]any{"host": "web1", "path": note}); isErr || !strings.Contains(res, "only for an opened relay") {
		t.Fatalf("a local turn's read: %q", res)
	}

	l.must("relay", "discord", "on")
	eventually(t, "the hub sees the relay opened", func() bool { return fleetRelayRoster(surfaceDiscord) != "" })
	before := calls()
	res, isErr = relay()
	if isErr || !strings.HasPrefix(res, "[fleet web1 ("+mach.ID) || !strings.Contains(res, "only for an opened relay") {
		t.Fatalf("an opened relay's read: %q", res)
	}
	if n := calls(); n != before+2 {
		t.Errorf("the machine's audit log grew by %d records for the relay's read, want 2 (call, done)", n-before)
	}
	// Opening Discord opened nothing else.
	if res, isErr := fleetTool(t, withExecSurface(fleetTurnCtx(), surfaceIMessage), "fleet_read", map[string]any{"host": "web1", "path": note}); !isErr || strings.Contains(res, "only for an opened relay") {
		t.Errorf("an iMessage turn read the file after Discord was opened: %q", res)
	}
}

// Acceptance: a dead link is reported within a minute, with the ping the
// shipped link sends (30 s, give up after two): nothing here shortens it.
// The wire between the machine and its master goes dead without a word, and
// each end is asked what it sees until it says the other is gone.
func TestFleetADeadLinkIsReportedWithinAMinute(t *testing.T) {
	if testing.Short() {
		t.Skip("waits a minute for the real ping")
	}
	t.Parallel()
	h := thisHost(t)
	wire := newE2EWire(t)
	m := h.master(true)
	m.serve()
	m.init(wire.addr)
	wire.to(m.fleet)
	c := m.join(h, "alpha")
	list := e2eScript(e2eCall("fleet_list", nil))
	if turn := m.turn(list, nil); !strings.Contains(turn.Results[0], "alpha ("+c.id+"): "+fleetOS(h.os)+", online") {
		t.Fatalf("before the wire died: %q", turn.Results)
	}

	wire.kill()
	died := time.Now()
	var master, child time.Duration
	for master == 0 || child == 0 {
		if master == 0 {
			if turn := m.turn(list, nil); strings.Contains(turn.Results[0], "alpha ("+c.id+"): "+fleetOS(h.os)+", offline") {
				master = time.Since(died)
			}
		}
		if child == 0 {
			if out, _ := c.tenant("status"); !strings.Contains(out, "connected to the master") {
				child = time.Since(died)
			}
		}
		if time.Since(died) > 90*time.Second {
			t.Fatalf("90 s after the wire died: the master reported it after %s, the machine after %s (0 = not yet)", master, child)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("the wire died; the master's agent was told alpha is offline %s later, and `tenant child status` on the machine stopped saying connected %s later",
		master.Round(time.Second), child.Round(time.Second))
	// Two silent pings are a minute; what is over is the time to ask.
	if limit := 60*time.Second + 5*time.Second; master > limit || child > limit {
		t.Errorf("a dead link was reported after %s (master) and %s (machine), want within a minute", master, child)
	}
	// The wire carries again: the machine finds its master by itself.
	wire.mend()
	c.state("connected to the master")
	if turn := m.turn(list, nil); !strings.Contains(turn.Results[0], ", online") {
		t.Errorf("after the wire was mended: %q", turn.Results)
	}
}

// A machine whose master is unreachable for longer than its backoff ceiling
// (one minute) keeps trying, and is connected again within that ceiling of
// the master's return.
func TestFleetAnUnreachableMasterIsFoundAgain(t *testing.T) {
	if !e2eWants("long") {
		t.Skip("takes five minutes: TENANT_FLEET_E2E=long")
	}
	h := thisHost(t)
	wire := newE2EWire(t)
	m := h.master(false)
	m.serve()
	m.init(wire.addr)
	wire.to(m.fleet)
	c := m.join(h, "alpha")

	wire.away() // nothing listens where the machine dials
	c.state("trying again")
	const gone = 3*time.Minute + 30*time.Second // the waits double from 1 s to the ceiling within about a minute
	time.Sleep(gone)
	if out, _ := c.tenant("status"); !strings.Contains(out, "trying again") || c.proc.exited() {
		t.Fatalf("after %s without its master the machine's child gave up:\n%s", gone, out)
	}
	wire.back()
	back := time.Now()
	c.state("connected to the master")
	took := time.Since(back)
	t.Logf("unreachable for %s; connected again %s after the master was back", gone, took.Round(time.Second))
	if took > 65*time.Second {
		t.Errorf("the machine took %s to find its master again, over its one-minute backoff ceiling", took)
	}
}

// Acceptance: a browser tool on a child. One headless browser is started by
// the machine's child for one screenshot, and ends with that child. Nothing
// is loaded from a network: the page is the blank one a new browser shows,
// and the one address tried is this computer's own, which the web tools
// refuse on a child as they do anywhere.
func TestFleetBrowserOnAChild(t *testing.T) {
	if !e2eWants("browser") {
		t.Skip("starts a real browser: TENANT_FLEET_E2E=browser")
	}
	h := thisHost(t)
	// The machine's child runs in this account's own environment, not in the
	// test's bare home folder like the other helper processes: on Windows,
	// Edge does not come up (chromedp waits 20 s and gives up) when
	// %USERPROFILE%\AppData\Local is not there. Seen here, one start each:
	// the account's own environment starts it; USERPROFILE alone pointed at
	// an empty folder does not; the same folder with AppData\Local and
	// AppData\Roaming made in it does. TENANT_FLEET_E2E_BROWSER_HOME repeats
	// that: the variables to point at the test's home ("all": every one of
	// e2eHomeVars), and "+layout" to make a profile's folders there. The
	// master keeps the test's home either way.
	kid := *h
	kid.rehome = []string{}
	v, layout := strings.CutSuffix(os.Getenv("TENANT_FLEET_E2E_BROWSER_HOME"), "+layout")
	if v == "all" {
		kid.rehome = nil
	} else if v != "" {
		kid.rehome = strings.Split(v, ",")
	}
	if layout { // the folders a Windows profile has, in the test's home
		for _, d := range []string{"Local", "Roaming"} {
			if err := os.MkdirAll(filepath.Join(h.join("home"), "AppData", d), 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Logf("the child's environment: %v pointed at the test's home folder (nil: all of %v); a profile's folders made there: %v", kid.rehome, e2eHomeVars, layout)
	var c *e2eChild
	// What the machine's child and its browser said, before the test's
	// directory goes: a child has no log book of its own, so its browser's
	// lines are on its standard error. Registered before the child starts,
	// so it runs after the child has stopped.
	t.Cleanup(func() {
		if c == nil || c.proc == nil {
			return
		}
		t.Logf("%s", c.proc.said())
		data := c.dir + "-data"
		_ = filepath.WalkDir(data, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				t.Logf("in the child's working folder: %s", strings.TrimPrefix(p, data))
			}
			return nil
		})
	})
	m := h.master(true)
	m.serve()
	m.init("")
	c = m.enroll(&kid, "alpha")
	c.confirm()
	c.env = map[string]string{"TENANT_LOG": "info"}
	c.run()
	c.state("connected to the master")

	turn := m.turn(e2eScript(
		e2eCall("fleet_tools", map[string]any{"host": "alpha"}),
		e2eCall("fleet_call", map[string]any{"host": "alpha", "tool": "web_screenshot", "arguments": map[string]any{}}),
		e2eCall("fleet_call", map[string]any{"host": "alpha", "tool": "web_navigate", "arguments": map[string]any{"url": "http://127.0.0.1:9/"}}),
	), func(control.PendingApproval) string { return "approve" })
	if len(turn.Results) != 3 {
		t.Fatalf("the model was handed %d results: %q", len(turn.Results), turn.Results)
	}
	t.Logf("fleet_call web_screenshot, as the master's model reads it:\n%s", turn.Results[1])
	if !strings.Contains(turn.Results[0], "- web_screenshot") || !strings.Contains(turn.Results[0], "- web_navigate") {
		t.Errorf("fleet_tools does not list the machine's browser:\n%s", turn.Results[0])
	}
	t.Logf("fleet_call web_navigate to this computer's own address:\n%s", turn.Results[2])
	shot := regexp.MustCompile(`(?m)^screenshot of \S+ saved to (.+)$`).FindStringSubmatch(turn.Results[1])
	if shot == nil || !strings.HasPrefix(turn.Results[1], "[fleet alpha (") {
		t.Fatalf("web_screenshot on the machine:\n%s", turn.Results[1])
	}
	// A real picture is on the machine's disk: a PNG or a JPEG.
	if raw, err := os.ReadFile(strings.TrimSpace(shot[1])); err != nil || !(strings.HasPrefix(string(raw), "\x89PNG") || strings.HasPrefix(string(raw), "\xff\xd8")) {
		t.Errorf("the screenshot the machine says it saved: %v (%d bytes)", err, len(raw))
	} else {
		t.Logf("the screenshot is %d bytes on the machine's disk", len(raw))
	}
	// The picture stays on the machine, and the result says so.
	if !strings.Contains(turn.Results[1], "1 picture(s) this tool made stayed on this machine") {
		t.Errorf("the result does not say the picture was not sent:\n%s", turn.Results[1])
	}
	if !strings.Contains(turn.Results[2], "blocked") {
		t.Errorf("the machine's browser was not refused this computer's own address:\n%s", turn.Results[2])
	}
	if len(turn.Cards) != 0 {
		t.Errorf("a screenshot and a refused address raised cards: %+v", turn.Cards)
	}
}

// --- the idle budget ---

// e2eIdle is what an idle fleet cost over one window.
type e2eIdle struct {
	Children int           `json:"children"`
	Window   time.Duration `json:"window_ns"`
	// The master's process.
	MasterGoroutines int           `json:"master_goroutines"`
	MasterCPU        time.Duration `json:"master_cpu_ns"` // over the window
	MasterRSS        int64         `json:"master_rss"`    // at its end
	// One machine's child process: the mean over all of them, and the worst.
	ChildCPU, ChildCPUMax time.Duration // over the window
	ChildRSS, ChildRSSMax int64         // at its end
	ChildGoroutines       int           // the most any child had
	// The wire, both directions together, per machine per minute.
	BytesPerMinute float64 `json:"bytes_per_minute"`
}

func (i e2eIdle) String() string {
	mb := func(b int64) float64 { return float64(b) / (1 << 20) }
	return fmt.Sprintf("%3d machine(s), %s idle: master %d goroutines, %.3f s CPU, %.1f MB resident | per child: %.4f s CPU (worst %.4f), %.1f MB resident (worst %.1f), %d goroutines at most, %.0f bytes/min on the wire",
		i.Children, i.Window.Round(time.Second), i.MasterGoroutines, i.MasterCPU.Seconds(), mb(i.MasterRSS),
		i.ChildCPU.Seconds(), i.ChildCPUMax.Seconds(), mb(i.ChildRSS), mb(i.ChildRSSMax), i.ChildGoroutines, i.BytesPerMinute)
}

// idleFleet is a master with machines that only hold their links: real
// `tenant serve` and `tenant child run` processes on the intervals a shipped
// Tenant runs with, the offline echo backend, and a wire that counts.
type idleFleet struct {
	t    *testing.T
	h    *e2eHost
	m    *e2eMaster
	wire *e2eWire
	kids []*e2eChild
}

// newIdleFleet makes the master a fleet master before it starts. Its
// machines are added by grow.
func newIdleFleet(t *testing.T) *idleFleet {
	t.Helper()
	f := &idleFleet{t: t, h: thisHost(t), wire: newE2EWire(t)}
	f.m = f.h.master(false)
	f.m.shipped = true
	// With no Tenant running: the bind address is on disk before the identity.
	f.m.must(nil, "fleet", "init", "--listen", "127.0.0.1:0", "--address", f.wire.addr, "--wait", "0s")
	f.m.master = true
	f.m.serve()
	f.wire.to(f.m.fleet)
	return f
}

// grow adds machines until the fleet has n, each a `tenant child run`
// process of its own, and waits until every one is connected. A machine is
// written into the master's fleet.json as confirmed, with the certificate
// its child will present: the enroll endpoint takes five machines a minute,
// and how a machine joins is not what an idle fleet measures
// (TestFleetEndToEnd enrolls through the endpoint).
func (f *idleFleet) grow(n int) {
	f.t.Helper()
	master, err := fleet.LoadIdentity(fleet.MasterIdentityPath(f.m.cfg))
	if err != nil {
		f.t.Fatal(err)
	}
	path := fleet.StorePath(f.m.cfg)
	raw, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatal(err)
	}
	var file map[string]any
	if err := json.Unmarshal(raw, &file); err != nil {
		f.t.Fatal(err)
	}
	machines, _ := file["machines"].(map[string]any)
	if machines == nil {
		machines = map[string]any{}
		file["machines"] = machines
	}
	var fresh []*e2eChild
	for i := len(f.kids); i < n; i++ {
		c := &e2eChild{t: f.t, host: f.h, m: f.m, name: fmt.Sprintf("node%03d", i), id: fmt.Sprintf("m%016x", 0xf1ee70000+i), shipped: true}
		c.scratch = f.h.join(c.name)
		c.dir = filepath.Join(c.scratch, "child")
		if err := os.MkdirAll(c.dir, 0o700); err != nil {
			f.t.Fatal(err)
		}
		id, err := fleet.MintIdentity(fleet.ChildIdentityPath(c.dir))
		if err != nil {
			f.t.Fatal(err)
		}
		if err := fleet.SaveChild(fleet.ChildPath(c.dir), fleet.Child{ID: c.id, Name: c.name, MasterAddresses: []string{f.wire.addr},
			MasterFingerprint: master.Fingerprint, Sandbox: "host", Ceiling: fleet.CeilingFull}); err != nil {
			f.t.Fatal(err)
		}
		now := time.Now().UTC().Format(time.RFC3339)
		machines[c.id] = map[string]any{"name": c.name, "fingerprint": id.Fingerprint, "state": "confirmed",
			"reported": map[string]any{"os": runtime.GOOS, "ceiling": "full"}, "enrolled_at": now, "confirmed_at": now}
		fresh = append(fresh, c)
	}
	out, err := json.Marshal(file)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		f.t.Fatal(err)
	}
	if _, err := fleet.Open(f.m.cfg).Machines(); err != nil {
		f.t.Fatalf("the fleet.json with %d machines does not load: %v", n, err)
	}
	// In batches: a hundred processes are not started in one instant.
	for len(fresh) > 0 {
		batch := fresh[:min(10, len(fresh))]
		fresh = fresh[len(batch):]
		for _, c := range batch {
			c.run()
		}
		for _, c := range batch {
			for deadline := time.Now().Add(e2eWait); ; time.Sleep(50 * time.Millisecond) {
				if st, err := fleet.ReadChildStatus(c.dir); err == nil && st.State == fleet.LinkConnected {
					break
				}
				if time.Now().After(deadline) {
					f.t.Fatalf("%s did not connect\n%s", c.name, c.proc.said())
				}
			}
		}
		f.kids = append(f.kids, batch...)
	}
}

// measure lets the fleet sit idle for window and returns what that cost.
func (f *idleFleet) measure(window time.Duration) e2eIdle {
	f.t.Helper()
	type sample struct {
		cpu time.Duration
		rss int64
		g   int
	}
	take := func() (master sample, kids []sample, bytes int64) {
		master.g, master.cpu, master.rss = f.m.proc.usage()
		for _, c := range f.kids {
			var s sample
			s.g, s.cpu, s.rss = c.proc.usage()
			kids = append(kids, s)
		}
		return master, kids, f.wire.up.Load() + f.wire.down.Load()
	}
	m0, k0, b0 := take()
	start := time.Now()
	time.Sleep(window)
	m1, k1, b1 := take()
	took := time.Since(start)
	idle := e2eIdle{Children: len(f.kids), Window: took, MasterGoroutines: m1.g, MasterCPU: m1.cpu - m0.cpu, MasterRSS: m1.rss}
	for i := range k1 {
		cpu := k1[i].cpu - k0[i].cpu
		idle.ChildCPU += cpu
		idle.ChildCPUMax = max(idle.ChildCPUMax, cpu)
		idle.ChildRSS += k1[i].rss
		idle.ChildRSSMax = max(idle.ChildRSSMax, k1[i].rss)
		idle.ChildGoroutines = max(idle.ChildGoroutines, k1[i].g)
	}
	if n := len(k1); n > 0 {
		idle.ChildCPU /= time.Duration(n)
		idle.ChildRSS /= int64(n)
		idle.BytesPerMinute = float64(b1-b0) / float64(n) / took.Minutes()
	}
	return idle
}

// The spike the spec's first work item asks for: what an idle child costs,
// and what the master costs with 1, 10 and 100 of them, measured on real
// processes. It prints the numbers TestFleetIdleBudget's limits come from.
func TestFleetIdleSpike(t *testing.T) {
	if !e2eWants("spike") {
		t.Skip("starts a hundred processes and takes twenty minutes: TENANT_FLEET_E2E=spike")
	}
	f := newIdleFleet(t)
	settle := 45 * time.Second // past the connect, the hello and the first address push
	base := f.measure(30 * time.Second)
	t.Logf("SPIKE %s", base)
	for _, step := range []struct {
		n      int
		window time.Duration
	}{{1, 3 * time.Minute}, {10, 3 * time.Minute}, {100, 10 * time.Minute}} {
		f.grow(step.n)
		time.Sleep(settle)
		t.Logf("SPIKE %s", f.measure(step.window))
	}
}

// Acceptance: the idle budget. Three machines hold their links to a master
// for a little over a minute (two pings), as real processes on the shipped
// intervals, and what that costs stays inside limits set from the spike's
// numbers (TestFleetIdleSpike; docs/ARCHITECTURE.md has the table).
//
// What is counted is held to about three times what was measured: bytes on
// the wire, goroutines, resident memory. A link that started to poll, a child
// that grew a second ping or a goroutine per call left behind fails it. CPU
// seconds depend on the runner, so that limit is looser: it catches a child
// that spins, not one that got a little slower.
func TestFleetIdleBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a master and three machines as processes, and waits")
	}
	t.Parallel()
	f := newIdleFleet(t)
	base, _, _ := f.m.proc.usage()
	f.grow(3)
	time.Sleep(15 * time.Second) // past the connect, the hello and the first address push
	idle := f.measure(65 * time.Second)
	t.Logf("%s (the master alone: %d goroutines)", idle, base)

	if got := idle.BytesPerMinute; got > idleBytesPerMinute {
		t.Errorf("an idle machine's link carried %.0f bytes a minute, over the budget of %d", got, idleBytesPerMinute)
	}
	if got := idle.BytesPerMinute; got == 0 {
		t.Error("nothing crossed an idle link in two ping intervals: the link is not kept alive, or the wire did not count")
	}
	if got := idle.ChildGoroutines; got > idleChildGoroutines {
		t.Errorf("an idle child runs %d goroutines, over the budget of %d", got, idleChildGoroutines)
	}
	if got := float64(idle.MasterGoroutines-base) / float64(idle.Children); got > idleMasterGoroutinesPerChild {
		t.Errorf("the master runs %.1f goroutines per idle machine, over the budget of %.1f", got, idleMasterGoroutinesPerChild)
	}
	limits, measured := idleBudget[runtime.GOOS]
	if !measured {
		t.Logf("no CPU or memory budget was measured on %s: only the wire and the goroutines are held to one", runtime.GOOS)
		return
	}
	if got := idle.ChildCPUMax; got > limits.childCPU {
		t.Errorf("an idle child used %s of CPU in %s, over the budget of %s", got, idle.Window.Round(time.Second), limits.childCPU)
	}
	if got := idle.ChildRSSMax; got > limits.childRSS {
		t.Errorf("an idle child holds %d MB resident, over the budget of %d MB", got>>20, limits.childRSS>>20)
	}
}

// The idle budget, from the spike (real processes, the test binary, the
// shipped intervals). Measured on Windows 11 and on Linux (WSL2), with 1, 10
// and 100 machines:
//
//   - the wire: 104 bytes a minute per machine, both directions together (one
//     ping and its pong every 30 s), the same at 1, 10 and 100;
//   - a child: 7 goroutines in the test's helper process;
//   - the master: 1 goroutine per connected machine.
const (
	idleBytesPerMinute           = 300 // measured 104
	idleChildGoroutines          = 10  // measured 7
	idleMasterGoroutinesPerChild = 2.5 // measured 1
)

// idleLimits is what depends on the operating system.
type idleLimits struct {
	childCPU time.Duration // the worst child, over the budget test's 65 s
	childRSS int64         // the worst child, resident
}

// idleBudget has an entry for each system the spike ran on.
var idleBudget = map[string]idleLimits{
	// Windows 11: a child used 0.13 s of CPU in ten minutes on average (the
	// worst of 100: 0.36 s), which is 0.014 s (0.04 s) in 65 s, and held 34 MB.
	"windows": {childCPU: 250 * time.Millisecond, childRSS: 100 << 20},
}
