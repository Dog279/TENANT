package main

// childrehome_test.go: `tenant child rehome` (TEN-533; gap6-fleet-plan.md
// section 6, "Losing the master"). Refusals first: every one leaves the
// machine exactly as it was, its old master's. Then what a rehome changes
// and what it keeps, and that the old master is refused afterwards.

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tenant/internal/fleet"
)

// rehome runs `tenant child rehome` on dir with code on standard input.
func rehome(t *testing.T, dir, code string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := childRehome(context.Background(), strings.NewReader(code+"\n"), &out, dir)
	return out.String(), err
}

// childSnapshot is every file of a child's own directory, by path, and the
// working directory beside it: what a refused rehome must leave as it was.
func childSnapshot(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, root := range []string{dir, childDataDir(dir)} {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || strings.HasSuffix(path, ".lock") {
				return nil
			}
			out[path] = mustRead(t, path)
			return nil
		})
	}
	return out
}

func sameSnapshot(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !bytes.Equal(v, b[k]) {
			return false
		}
	}
	return true
}

// codeTo is code with its master's addresses replaced: a code that names one
// master's certificate and leads to another address.
func codeTo(t *testing.T, code string, addrs ...string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(code, "tntfleet1_"))
	if err != nil {
		t.Fatal(err)
	}
	var c map[string]any
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	c["a"] = addrs
	raw, _ = json.Marshal(c)
	return "tntfleet1_" + base64.RawURLEncoding.EncodeToString(raw)
}

// impostor is a TLS listener on loopback with a certificate of its own, that
// takes any client certificate: a server at a master's address that is not
// that master. It counts the handshakes that completed.
type impostor struct {
	addr  string
	shook atomic.Int32
}

func newImpostor(t *testing.T) *impostor {
	t.Helper()
	id, err := fleet.MintIdentity(filepath.Join(t.TempDir(), "impostor.pem"))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{id.Certificate},
		ClientAuth: tls.RequireAnyClientCert, MinVersion: tls.VersionTLS13, NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	im := &impostor{addr: ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				if c.(*tls.Conn).Handshake() == nil {
					im.shook.Add(1)
				}
			}(c)
		}
	}()
	return im
}

// --- refusals ---

// The code is never an argument: rehome refuses one before it reads
// anything, as install does.
func TestChildRehomeRefusesTheCodeAsAnArgument(t *testing.T) {
	a := newFleetMaster(t)
	dir, _, _ := enrollChild(t, a, "box", childInstallOpts{})
	before := childSnapshot(t, dir)
	err := cmdChild(context.Background(), []string{"rehome", "--dir", dir, a.code("box2")})
	if exitCode(err) != 2 || !strings.Contains(err.Error(), "never from an argument") {
		t.Fatalf("rehome with the code as an argument: %v", err)
	}
	if !sameSnapshot(before, childSnapshot(t, dir)) {
		t.Fatal("a refused rehome changed the child's files")
	}
}

// Every way a rehome can be refused leaves the child exactly as it was, and
// a refusal before the code is sent leaves the code unspent.
func TestChildRehomeRefusals(t *testing.T) {
	a := newFleetMaster(t)
	dir, digits, _ := enrollChild(t, a, "box", childInstallOpts{})
	mach := a.confirm("box", digits)
	b := newFleetMaster(t)
	// The machine has run: its audit log and its status are there to keep.
	stop := startChild(t, dir)
	a.awaitOnline(mach.ID, true)
	stop()
	a.awaitOnline(mach.ID, false)
	before := childSnapshot(t, dir)
	unchanged := func(what string) {
		t.Helper()
		if !sameSnapshot(before, childSnapshot(t, dir)) {
			t.Fatalf("%s changed the child's files", what)
		}
		child, err := fleet.LoadChild(fleet.ChildPath(dir))
		if err != nil || child.MasterFingerprint != a.id.Fingerprint || child.ID != mach.ID {
			t.Fatalf("%s: the child trusts %+v, %v", what, child, err)
		}
	}

	// Not a code, a peer invite, a code b never made, an expired one.
	for name, code := range map[string]string{"not a code": "hunter2", "a peer invite": "tnt1_abc",
		"a code the master never made": forgedCode(t, b), "nothing": ""} {
		if _, err := rehome(t, dir, code); err == nil {
			t.Fatalf("%s was taken", name)
		}
		unchanged(name)
	}
	expired, err := b.store.CreateEnrollment(b.id.Fingerprint, "late", time.Second, "")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2100 * time.Millisecond)
	if _, err := rehome(t, dir, expired); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("an expired code: %v", err)
	}
	unchanged("an expired code")

	// A code another machine spent first.
	used := b.code("box")
	if _, _, err := enrollWith(t, used, childInstallOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, err := rehome(t, dir, used); err == nil || !strings.Contains(err.Error(), "still trusts its master") {
		t.Fatalf("a used code: %v", err)
	}
	unchanged("a used code")

	// A code for b that leads to another server: the code's certificate is
	// the only one taken, so the secret never leaves this machine.
	im := newImpostor(t)
	stolen := b.code("box2")
	if _, err := rehome(t, dir, codeTo(t, stolen, im.addr)); err == nil {
		t.Fatal("a rehome to a server that is not the code's master went through")
	}
	unchanged("a server that is not the code's master")
	if im.shook.Load() != 0 {
		t.Fatal("this machine finished a handshake with a server that is not the code's master")
	}

	// A child process on the directory: refused before the code is read, so
	// the code still works afterwards.
	stop = startChild(t, dir)
	a.awaitOnline(mach.ID, true)
	kept := b.code("box3")
	if _, err := rehome(t, dir, kept); exitCode(err) != 2 || !strings.Contains(err.Error(), "a child process is running on this directory") {
		t.Fatalf("rehome beside a running child: %v", err)
	}
	stop()
	a.awaitOnline(mach.ID, false)
	before = childSnapshot(t, dir) // the run wrote its audit records and status

	// The machine's own child, from a terminal that is not elevated.
	prev := childSystemDir
	childSystemDir = func() string { return dir }
	fakeElevated(t, false)
	if _, err := rehome(t, dir, kept); exitCode(err) != 2 || !strings.Contains(err.Error(), "needs an elevated terminal") {
		t.Fatalf("rehome of the machine's child without elevation: %v", err)
	}
	childSystemDir = prev
	unchanged("a terminal that is not elevated")

	// The code that was refused on the machine was never sent: it still works.
	if _, err := rehome(t, dir, kept); err != nil {
		t.Fatalf("the code a refusal left unspent: %v", err)
	}
	if child, _ := fleet.LoadChild(fleet.ChildPath(dir)); child.MasterFingerprint != b.id.Fingerprint {
		t.Fatalf("after the rehome the child trusts %s", child.MasterFingerprint)
	}

	// Not a child at all.
	if _, err := rehome(t, t.TempDir(), b.code("none")); err == nil || !strings.Contains(err.Error(), "no child is enrolled here") {
		t.Fatalf("rehome where no child is: %v", err)
	}
}

// --- what changes, what stays ---

// A rehome swaps which master the machine trusts, and nothing else: the key,
// the ceiling, the tools it serves, the sandbox, the secret list, the pause
// mark, the audit log and the memory copies stay. The new master holds it as
// pending until the operator types its digits; the old master is refused
// from then on, by address and by certificate.
func TestChildRehome(t *testing.T) {
	a := newFleetMaster(t)
	dir, digits, _ := enrollChild(t, a, "box", childInstallOpts{ceiling: "no-exec"})
	old := a.confirm("box", digits)
	if err := fleet.UpdateChild(fleet.ChildPath(dir), func(c *fleet.Child) error {
		c.Tools = []string{"os_sysinfo", "os_read_file"}
		c.SecretPaths = []string{filepath.Join(dir, "operator-secret")}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stop := startChild(t, dir)
	a.awaitOnline(old.ID, true)
	a.ok(old.ID, "os_sysinfo", nil) // a call: the audit log has records
	stop()
	a.awaitOnline(old.ID, false)
	if err := os.MkdirAll(childMemoryDir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	copyFile := filepath.Join(childMemoryDir(dir), "copy-of-an-old-generation")
	if err := os.WriteFile(copyFile, []byte("sealed for the old fleet"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fleet.SetPaused(dir, true); err != nil {
		t.Fatal(err)
	}
	was, _ := fleet.LoadChild(fleet.ChildPath(dir))
	key := mustRead(t, fleet.ChildIdentityPath(dir))
	auditBefore, err := fleet.VerifyAudit(dir)
	if err != nil {
		t.Fatal(err)
	}

	b := newFleetMaster(t)
	said, err := rehome(t, dir, b.code("web1"))
	if err != nil {
		t.Fatalf("rehome: %v\n%s", err, said)
	}
	now, err := fleet.LoadChild(fleet.ChildPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	pending, err := b.store.ByName("web1")
	if err != nil || pending.State != fleet.StatePending || now.ID != pending.ID || now.Name != "web1" ||
		now.MasterFingerprint != b.id.Fingerprint || !slices.Equal(now.MasterAddresses, []string{b.mgr.Addr().String()}) {
		t.Fatalf("after the rehome: child %+v, on the new master %+v (%v)", now, pending, err)
	}
	if now.Ceiling != was.Ceiling || now.Sandbox != was.Sandbox || !slices.Equal(now.Tools, was.Tools) || !slices.Equal(now.SecretPaths, was.SecretPaths) {
		t.Fatalf("the machine's own settings changed: %+v, were %+v", now, was)
	}
	if !bytes.Equal(key, mustRead(t, fleet.ChildIdentityPath(dir))) || pending.Fingerprint == "" {
		t.Fatal("the machine's key changed")
	}
	if string(mustRead(t, copyFile)) != "sealed for the old fleet" || !fleet.Paused(dir) {
		t.Fatal("a memory copy or the pause mark did not stay")
	}
	if pending.Reported.Ceiling != "no-exec" || pending.Reported.Sandbox != "host" {
		t.Fatalf("what the machine told the new master: %+v", pending.Reported)
	}
	want := confirmationCode.FindStringSubmatch(said)
	if want == nil || !strings.Contains(said, "it was "+old.ID+" under its old master") || !strings.Contains(said, "tenant fleet recover web1 --was "+old.ID) ||
		!strings.Contains(said, "paused on this machine") {
		t.Fatalf("rehome said:\n%s", said)
	}
	// The audit log goes on, with a record of who did it and from which
	// master to which.
	sum, err := fleet.VerifyAudit(dir)
	if err != nil || sum.Records != auditBefore.Records+1 {
		t.Fatalf("the audit log after the rehome: %+v, %v", sum, err)
	}
	last := lastAuditRecord(t, dir)
	if last.Event != fleet.AuditRehome || last.From != a.id.Fingerprint || last.To != b.id.Fingerprint || last.By == "" {
		t.Fatalf("the audit record of the rehome: %+v", last)
	}
	if lines := childOSLog.all(); len(lines) == 0 || !strings.Contains(lines[len(lines)-1], "event=rehome") {
		t.Errorf("the system log did not get the record: %q", lines)
	}

	// Confirmation on the new master is as for a new machine: until it, the
	// new master sends it nothing.
	if err := fleet.SetPaused(dir, false); err != nil {
		t.Fatal(err)
	}
	startChild(t, dir)
	awaitStatus(t, dir, "waiting for confirmation", 3)
	if _, err := b.mgr.Call(context.Background(), pending.ID, fleet.Call{Tool: "os_sysinfo", Arguments: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("the new master called a machine it has not confirmed")
	}
	b.confirm("web1", want[1])
	b.awaitOnline(pending.ID, true)
	b.ok(pending.ID, "os_sysinfo", nil)
	if online(a.mgr, old.ID) {
		t.Fatal("the old master has the machine online")
	}
	if _, err := a.mgr.Call(context.Background(), old.ID, fleet.Call{Tool: "os_sysinfo", Arguments: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("the old master reached the machine after the rehome")
	}

	// Sent to the old master's address, it refuses that master's certificate:
	// what it trusts is the new master's, not an address.
	if err := fleet.UpdateChild(fleet.ChildPath(dir), func(c *fleet.Child) error {
		c.MasterAddresses = []string{a.mgr.Addr().String()}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := fleet.SetPaused(dir, true); err != nil { // the link drops, and is made again from child.json
		t.Fatal(err)
	}
	b.awaitOnline(pending.ID, false)
	if err := fleet.SetPaused(dir, false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	if online(a.mgr, old.ID) || online(b.mgr, pending.ID) {
		t.Fatal("the machine linked to its old master")
	}
}

// lastAuditRecord is the last record of the child's audit log.
func lastAuditRecord(t *testing.T, dir string) fleet.AuditRecord {
	t.Helper()
	raw := bytes.TrimSpace(mustRead(t, filepath.Join(fleet.AuditDir(dir), "audit.jsonl")))
	var rec fleet.AuditRecord
	if err := json.Unmarshal(raw[bytes.LastIndexByte(raw, '\n')+1:], &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

// --- only on the machine ---

// Nothing but the command on the machine makes a child rehome: childRehome
// is called from `tenant child` alone; no method of the link names it or
// writes which master a child trusts; and the fleet's tools did not grow
// one (TestOnlyTheOperatorConfirmsAMachine pins their names).
func TestChildRehomeIsStartedOnlyOnTheMachine(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var callers, methods, writers []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator)))
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CallExpr:
					name := ""
					switch f := n.Fun.(type) {
					case *ast.Ident:
						name = f.Name
					case *ast.SelectorExpr:
						name = f.Sel.Name
					}
					switch name {
					case "childRehome":
						callers = append(callers, rel+": "+fn.Name.Name)
					case "UpdateChild", "SaveChild":
						writers = append(writers, rel+": "+fn.Name.Name)
					}
				case *ast.BasicLit:
					if s, err := strconv.Unquote(n.Value); err == nil && n.Kind == token.STRING &&
						strings.HasPrefix(s, "fleet/") && !strings.Contains(s, " ") && strings.HasPrefix(rel, "internal/fleet/") {
						methods = append(methods, s)
					}
				}
				return true
			})
		}
		// The link's method names are constants, outside any function.
		if strings.HasPrefix(rel, "internal/fleet/") {
			ast.Inspect(file, func(n ast.Node) bool {
				if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if s, err := strconv.Unquote(lit.Value); err == nil && strings.HasPrefix(s, "fleet/") && !strings.ContainsAny(s, " .") {
						methods = append(methods, s)
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(callers, []string{"cmd/tenant/childctl.go: cmdChild"}) {
		t.Errorf("childRehome is called from %q; only `tenant child rehome` may", callers)
	}
	slices.Sort(writers)
	if want := []string{"cmd/tenant/childctl.go: childInstall", "cmd/tenant/childctl.go: childRehome", "internal/fleet/child.go: UpdateChild",
		"internal/fleet/client.go: addresses"}; !slices.Equal(writers, want) {
		t.Errorf("child.json is written from %q, want %q", writers, want)
	}
	slices.Sort(methods)
	methods = slices.Compact(methods)
	link := []string{"fleet/addresses", "fleet/confirm", "fleet/hello", "fleet/memory/get", "fleet/memory/list",
		"fleet/memory/purge", "fleet/memory/put", "fleet/result", "fleet/revoked", "fleet/update"}
	if !slices.Equal(methods, link) {
		t.Errorf("the link's methods are %q, want %q: a new one is reviewed here, and none may change which master a child trusts", methods, link)
	}
}
