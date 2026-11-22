package main

// fleet_e2e_rehome_test.go: losing the master (TEN-533; gap6-fleet-plan.md
// section 6), with every piece a process of its own. Master A has a machine
// with memory; master B is built from nothing; the machine is re-homed to B
// with `tenant child rehome`, confirmed there, and B works on it while A is
// refused; B reads the machine's memory only once the old fleet's recovery
// identity is brought with `tenant fleet recover`.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"tenant/internal/fleet"
	"tenant/internal/secrets"
)

// e2eRecoveryEnv makes a helper's `fleet init` print the recovery identity
// its operator saved, on a line of its own: the test is the operator, who
// keeps it away from the master.
const e2eRecoveryEnv = "TENANT_FLEET_E2E_RECOVERY"

var e2eRecoveryLine = regexp.MustCompile(`E2E-RECOVERY (AGE-SECRET-KEY-1[0-9A-Z]+)`)

// e2eRehomeSeams is what a helper process needs here (TestFleetE2EProcess).
func e2eRehomeSeams() {
	if os.Getenv(e2eRecoveryEnv) == "" {
		return
	}
	openRecoveryTerminal = func() (*recoveryTerminal, error) {
		term, err := operator(nil).open()
		if err != nil {
			return nil, err
		}
		read := term.read
		term.read = func() (*secrets.LockedBuffer, error) {
			b, err := read()
			if b != nil {
				_ = b.With(func(p []byte) error {
					_, err := os.Stdout.Write([]byte("E2E-RECOVERY " + string(p) + "\n"))
					return err
				})
			}
			return b, err
		}
		return term, nil
	}
}

// initKeeping makes the running master a fleet master on a loopback port, as
// init does, and returns the recovery identity its operator saved.
func (m *e2eMaster) initKeeping() string {
	m.t.Helper()
	from := m.proc.out.count()
	out, errOut, exit := m.host.run(e2eSpec{Argv: []string{"fleet", "init", "--listen", "127.0.0.1:0", "--wait", "60s", "--config", m.cfg, "--data", m.data},
		Env: map[string]string{e2eRecoveryEnv: "1"}})
	found := e2eRecoveryLine.FindStringSubmatch(out)
	if exit != 0 || found == nil {
		m.t.Fatalf("fleet init: exit %d\n%s\n%s", exit, out, errOut)
	}
	m.master = true
	m.listening(from)
	m.must(nil, "fleet", "init", "--address", m.fleet)
	return found[1]
}

// recover runs `tenant fleet recover` on the master with identity on its
// standard input.
func (m *e2eMaster) recover(identity string, args ...string) (string, int) {
	m.t.Helper()
	cmd := m.host.command(e2eSpec{Argv: append(append([]string{"fleet", "recover"}, args...), "--hub", m.dash, "--config", m.cfg, "--data", m.data)})
	cmd.Stdin = strings.NewReader(identity + "\n")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil && cmd.ProcessState == nil {
		m.t.Fatal(err)
	}
	return strings.ReplaceAll(out.String(), "\r\n", "\n"), cmd.ProcessState.ExitCode()
}

// online is whether the master's dashboard says the machine is online.
func (m *e2eMaster) online(id string) bool {
	m.t.Helper()
	var v apiFleet
	if code, body := m.api(http.MethodGet, "/api/fleet", nil, &v); code != http.StatusOK {
		m.t.Fatalf("GET /api/fleet: %d %s", code, body)
	}
	for _, am := range v.Machines {
		if am.ID == id {
			return am.Online
		}
	}
	return false
}

var e2eRehomed = regexp.MustCompile(`as (\S+) \((m[0-9a-f]{16})\); it was (m[0-9a-f]{16}) under its old master`)

func TestFleetRehomeEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("starts two masters and a machine as processes")
	}
	h := thisHost(t)
	var marker string
	{
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		marker = "e2e-rehome-" + hex.EncodeToString(b)
	}
	a := h.master(true)
	a.serve()
	recoveryA := a.initKeeping()
	c := a.join(h, "web1")
	oldID := c.id

	step := func(name string, fn func(t *testing.T)) {
		t.Helper()
		t.Run(name, fn)
		if t.Failed() {
			t.FailNow()
		}
	}
	step("master A keeps the machine's memory, sealed, on the machine", func(t *testing.T) {
		a.proc.memory("note " + c.id + " " + marker + " web1 runs nginx")
		a.proc.memory("pass")
		a.memoryState(c.id, memoryInStep, "on both sides")
		if copiesOn(c) == 0 {
			t.Fatal("the machine keeps no copy")
		}
	})

	// Master B, built from nothing: its own config and data, its own keys.
	b := &e2eMaster{t: t, host: h, cfg: h.join("b-cfg"), data: h.join("b-data"), scripted: true, client: &http.Client{Timeout: 30 * time.Second}}
	b.serve()
	recoveryB := b.initKeeping()
	if recoveryB == recoveryA {
		t.Fatal("two fleets share a recovery identity")
	}
	copiesBefore := dirBytes(t, c.dir+string(os.PathSeparator)+"memory")

	step("rehome refuses while the machine's child runs, and the code stays unspent", func(t *testing.T) {
		code := strings.TrimSpace(strings.SplitN(b.must(nil, "fleet", "enroll", "web1"), "\n", 2)[0])
		out, errOut, exit := h.run(e2eSpec{Argv: []string{"child", "rehome", "--dir", c.dir}, Env: map[string]string{childCodeEnv: code}, Scratch: c.scratch})
		if exit != 2 || !strings.Contains(out+errOut, "a child process is running") {
			t.Fatalf("rehome beside the running child: exit %d\n%s\n%s", exit, out, errOut)
		}
		c.proc.stop()
		out, errOut, exit = h.run(e2eSpec{Argv: []string{"child", "rehome", "--dir", c.dir}, Env: map[string]string{childCodeEnv: code}, Scratch: c.scratch})
		digits, moved := confirmationCode.FindStringSubmatch(out), e2eRehomed.FindStringSubmatch(out)
		if exit != 0 || digits == nil || moved == nil || moved[1] != "web1" || moved[3] != oldID {
			t.Fatalf("rehome: exit %d\n%s\n%s", exit, out, errOut)
		}
		if strings.Contains(out+errOut, code) {
			t.Error("rehome printed the enrollment code")
		}
		c.m, c.id, c.digits = b, moved[2], digits[1]
	})

	step("the new master holds it as pending until the operator confirms it", func(t *testing.T) {
		c.run()
		c.state("waiting for confirmation")
		out := b.must(nil, "fleet", "show", "web1")
		if !strings.Contains(out, "pending") {
			t.Fatalf("fleet show on the new master:\n%s", out)
		}
		c.confirm()
		c.state("connected to the master at " + b.fleet)
		res, _ := b.one("fleet_list_dir", map[string]any{"host": "web1", "path": h.root}, "")
		if !strings.Contains(res, "b-cfg") {
			t.Fatalf("a call from the new master: %q", res)
		}
	})

	step("the old master is refused", func(t *testing.T) {
		// Even with the old master's address first in its list: what the
		// machine trusts is the new master's certificate, not an address.
		c.proc.stop()
		if err := fleet.UpdateChild(fleet.ChildPath(c.dir), func(ch *fleet.Child) error {
			ch.MasterAddresses = []string{a.fleet, b.fleet}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		c.run()
		c.state("connected to the master at " + b.fleet)
		time.Sleep(3 * time.Second) // time for a link the machine would make
		if a.online(oldID) {
			t.Fatal("the old master has the machine online")
		}
		if !b.online(c.id) {
			t.Fatal("the new master lost the machine")
		}
		sum, err := fleet.VerifyAudit(c.dir)
		if err != nil || sum.Records == 0 {
			t.Fatalf("the machine's audit log: %+v %v", sum, err)
		}
		if !strings.Contains(string(readRaw(t, c.dir+string(os.PathSeparator)+"audit"+string(os.PathSeparator)+"audit.jsonl")), `"event":"rehome"`) {
			t.Fatal("the audit log has no record of the rehome")
		}
	})

	step("without the old fleet's recovery identity the new master reads nothing", func(t *testing.T) {
		b.memoryState(c.id, memoryNeedsKey, "another fleet key")
		if text := b.proc.memory("memory " + c.id)["text"].(string); strings.Contains(text, marker) {
			t.Fatalf("the new master reads the machine's memory without the recovery identity: %q", text)
		}
		for name, try := range map[string][]string{
			"its own recovery identity":            {recoveryB, "web1", "--was", oldID},
			"the old identity, without the old id": {recoveryA, "web1"},
			"not an identity":                      {"hunter2", "web1", "--was", oldID},
		} {
			out, exit := b.recover(try[0], try[1:]...)
			if exit == 0 {
				t.Fatalf("%s was taken:\n%s", name, out)
			}
			if strings.Contains(out, try[0]) && strings.HasPrefix(try[0], "AGE-") {
				t.Fatalf("%s: the identity was printed back", name)
			}
		}
		b.memoryState(c.id, memoryNeedsKey, "another fleet key")
		if !sameFiles(dirBytes(t, c.dir+string(os.PathSeparator)+"memory"), copiesBefore) {
			t.Fatal("the machine's copies changed")
		}
	})

	step("with it, the new master reads the machine's memory, and the old copies stay", func(t *testing.T) {
		out, exit := b.recover(recoveryA, "web1", "--was", oldID)
		if exit != 0 || !strings.Contains(out, "read with the recovery identity") {
			t.Fatalf("fleet recover: exit %d\n%s", exit, out)
		}
		if strings.Contains(out, recoveryA) {
			t.Fatal("fleet recover printed the identity")
		}
		b.memoryState(c.id, memoryInStep, "on both sides")
		if text := b.proc.memory("memory " + c.id)["text"].(string); !strings.Contains(text, marker+" web1 runs nginx") {
			t.Fatalf("the recovered memory: %q", text)
		}
		now := dirBytes(t, c.dir+string(os.PathSeparator)+"memory")
		for name, data := range copiesBefore {
			if !bytes.Equal(now[name], data) {
				t.Fatalf("the old copy %s changed or went", name)
			}
		}
		if len(now) != len(copiesBefore)+1 {
			t.Fatalf("the machine keeps %d copies, had %d", len(now), len(copiesBefore))
		}
		if found := filesWith(marker, c.dir, childDataDir(c.dir), b.data, b.cfg); len(found) > 0 {
			t.Fatalf("the marker is readable in %v", found)
		}
	})
}
