package fleet

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"tenant/internal/cnsa"
	"tenant/internal/mcp"
	"tenant/internal/model"
)

// oldIdentityAt writes an identity as Tenant made them before the cnsa
// profile, an ECDSA P-256 certificate, and loads it.
func oldIdentityAt(t *testing.T, path string) Identity {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "tenant-fleet"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	b := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})...)
	if err := writePrivate(path, b); err != nil {
		t.Fatal(err)
	}
	id, err := LoadIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if !id.Legacy || len(id.Fingerprint) != legacyFingerprintLen {
		t.Fatalf("an ECDSA identity loaded as legacy=%v with fingerprint %q", id.Legacy, id.Fingerprint)
	}
	return id
}

type noTools struct{}

func (noTools) All() []model.ToolSpec { return []model.ToolSpec{{Name: "echo"}} }
func (noTools) Dispatch(context.Context, model.ToolCall) (string, bool, error) {
	return "ok", false, nil
}

// oldFleet is a master and one confirmed machine as a Tenant from before the
// cnsa profile left them on disk, with the master started by this Tenant.
type oldFleet struct {
	dir, childDir string
	id            Identity  // the master's new identity
	legacy        *Identity // and the one it had
	store         *Store
	mgr           *Manager
	client        *Client
}

const oldMachine = "m00000000000000a1"

func newOldFleet(t *testing.T, hello func() *UpdateReport) *oldFleet {
	t.Helper()
	f := &oldFleet{dir: t.TempDir(), childDir: t.TempDir()}
	oldIdentityAt(t, MasterIdentityPath(f.dir))
	child := oldIdentityAt(t, ChildIdentityPath(f.childDir))
	f.store = Open(f.dir)
	now := time.Now().UTC().Truncate(time.Second)
	err := f.store.mutate(func(d *fileData) error {
		d.Machines[oldMachine] = &Machine{Name: "old-box", Fingerprint: child.Fingerprint, State: StateConfirmed,
			Reported: linuxBox, EnrolledAt: now, ConfirmedAt: now}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.id, f.legacy, err = LoadMasterIdentity(f.dir); err != nil {
		t.Fatal(err)
	}
	if f.id.Legacy || f.legacy == nil {
		t.Fatalf("LoadMasterIdentity left the master on legacy=%v, legacy identity %v", f.id.Legacy, f.legacy)
	}
	if f.mgr, err = StartManager(ManagerConfig{Store: f.store, Identity: f.id, Legacy: f.legacy, Addr: "127.0.0.1:0", Role: "test"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.mgr.Close() })
	addr := f.mgr.Addr().String()
	if err := f.store.SetAddresses([]string{addr}); err != nil {
		t.Fatal(err)
	}
	err = SaveChild(ChildPath(f.childDir), Child{ID: oldMachine, Name: "old-box", MasterAddresses: []string{addr},
		MasterFingerprint: f.legacy.Fingerprint, Sandbox: "host", Ceiling: CeilingFull})
	if err != nil {
		t.Fatal(err)
	}
	audit, err := OpenAudit(f.childDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = audit.Close() })
	if f.client, err = NewClient(ClientConfig{Dir: f.childDir, Tools: noTools{}, Version: "test", User: "tester", Hello: hello, Rekeyed: audit.Rekey}); err != nil {
		t.Fatal(err)
	}
	f.client.backoffMin = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = f.client.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return f
}

// eventually waits for ok, and fails with what when it does not come.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestLoadMasterIdentityMovesAMasterOnce(t *testing.T) {
	dir := t.TempDir()
	old := oldIdentityAt(t, MasterIdentityPath(dir))
	// One machine still pinned the old way: the reason the old identity is kept.
	store := Open(dir)
	machine := oldIdentityAt(t, ChildIdentityPath(t.TempDir()))
	now := time.Now().UTC().Truncate(time.Second)
	err := store.mutate(func(d *fileData) error {
		d.Machines[oldMachine] = &Machine{Name: "old-box", Fingerprint: machine.Fingerprint, State: StateConfirmed, Reported: linuxBox, EnrolledAt: now}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	id, legacy, err := LoadMasterIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if id.Legacy || len(id.Fingerprint) != cnsa.FingerprintLen || cnsa.CheckLeaf(id.Certificate.Leaf) != nil {
		t.Fatalf("the master's new identity: legacy=%v fingerprint=%q", id.Legacy, id.Fingerprint)
	}
	if legacy == nil || legacy.Fingerprint != old.Fingerprint {
		t.Fatalf("the legacy identity was not kept: %+v", legacy)
	}
	again, legacy2, err := LoadMasterIdentity(dir)
	if err != nil || again.Fingerprint != id.Fingerprint || legacy2 == nil || legacy2.Fingerprint != old.Fingerprint {
		t.Fatalf("a second load: %v, %q (was %q), legacy %v", err, again.Fingerprint, id.Fingerprint, legacy2)
	}
	if !slices.Contains(AtRestPaths(dir), MasterLegacyIdentityPath(dir)) {
		t.Error("the legacy identity, a private key, is not among the fleet's protected files")
	}
	// Once no machine that may still connect pins it, the old key is deleted:
	// a revoked machine does not count.
	if err := store.Revoke(oldMachine); err != nil {
		t.Fatal(err)
	}
	last, legacy3, err := LoadMasterIdentity(dir)
	if err != nil || legacy3 != nil || last.Fingerprint != id.Fingerprint {
		t.Fatalf("with no legacy machine left: %v, legacy %v", err, legacy3)
	}
	if _, err := os.Lstat(MasterLegacyIdentityPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the legacy identity is still on disk with no machine to use it: %v", err)
	}
	// A master that never had another identity has no legacy one.
	fresh := t.TempDir()
	made := must(MintIdentity(MasterIdentityPath(fresh)))(t)
	if id, legacy, err := LoadMasterIdentity(fresh); err != nil || legacy != nil || id.Fingerprint != made.Fingerprint {
		t.Fatalf("a new master: %v, legacy %v", err, legacy)
	}
}

// A machine from before the profile connects with its old certificate, is
// told the master's new one, sends its own, and comes back on the profile.
func TestLegacyMachineMovesToANewCertificate(t *testing.T) {
	f := newOldFleet(t, nil)
	eventually(t, "the machine to move to its new certificate", func() bool {
		m, err := f.store.Machine(oldMachine)
		return err == nil && !m.Legacy() && m.Next == ""
	})
	m := must(f.store.Machine(oldMachine))(t)
	child := must(LoadIdentity(ChildIdentityPath(f.childDir)))(t)
	if child.Legacy || child.Fingerprint != m.Fingerprint || len(m.Fingerprint) != cnsa.FingerprintLen {
		t.Fatalf("the machine's identity %q (legacy=%v) and the master's pin %q", child.Fingerprint, child.Legacy, m.Fingerprint)
	}
	if got := must(LoadChild(ChildPath(f.childDir)))(t).MasterFingerprint; got != f.id.Fingerprint {
		t.Fatalf("child.json pins %q, not the master's new certificate %q", got, f.id.Fingerprint)
	}
	if _, err := os.Lstat(ChildNextIdentityPath(f.childDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the identity the machine moved to is still kept beside its identity: %v", err)
	}
	// Back on the profile, it is called like any machine.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eventually(t, "the machine's tools over its new link", func() bool {
		tools, err := f.mgr.Tools(ctx, oldMachine)
		return err == nil && len(tools) == 1
	})
	if st := f.client.Status(); st.State != LinkConnected || st.Certificate != m.Fingerprint {
		t.Errorf("the child's status: %+v", st)
	}
	// The machine's own audit log has the change of the master it pins, from
	// the old fingerprint to the new, and its chain holds.
	sum, err := VerifyAudit(f.childDir)
	if err != nil {
		t.Fatalf("the audit log after the move: %v", err)
	}
	var moved []AuditRecord
	for _, name := range must(os.ReadDir(AuditDir(f.childDir)))(t) {
		if filepath.Ext(name.Name()) != ".jsonl" {
			continue // the lock beside the log
		}
		file := must(os.Open(filepath.Join(AuditDir(f.childDir), name.Name())))(t)
		_, err := auditLines(file, func(line []byte, _ string) error {
			if rec, perr := parseAuditRecord(line); perr == nil && rec.Event == AuditRekey {
				moved = append(moved, rec)
			}
			return nil
		})
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(moved) != 1 || moved[0].From != f.legacy.Fingerprint || moved[0].To != f.id.Fingerprint {
		t.Errorf("the audit log's record of the move: %+v (summary %+v)", moved, sum)
	}
	// And its old certificate opens nothing any more.
	if _, pins, err := f.store.view(); err != nil || len(pins) != 1 || len(pins[0].sum) != cnsa.FingerprintLen/2 {
		t.Errorf("the pins after the move: %d, %v", len(pins), err)
	}
}

// In the trial run of an update the machine does not move: the older Tenant
// could come back. On the legacy link it is pinged and updated, and nothing
// else, by either end.
func TestLegacyLinkCarriesNoCalls(t *testing.T) {
	trial := func() *UpdateReport {
		return &UpdateReport{From: "0.2.0", To: "0.3.0", State: UpdateTrial, At: time.Now()}
	}
	f := newOldFleet(t, trial)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eventually(t, "the legacy link", func() bool {
		_, err := f.mgr.Ping(ctx, oldMachine)
		return err == nil
	})
	if _, err := f.mgr.Tools(ctx, oldMachine); !errors.Is(err, ErrLegacy) || !errors.Is(err, ErrIncompatible) {
		t.Fatalf("tools over a legacy link: %v", err)
	}
	if _, err := f.mgr.Call(ctx, oldMachine, Call{Tool: "echo"}); !errors.Is(err, ErrLegacy) {
		t.Fatalf("a call over a legacy link: %v", err)
	}
	if m := must(f.store.Machine(oldMachine))(t); !m.Legacy() || m.Next != "" {
		t.Fatalf("the machine moved during a trial: %+v", m)
	}
	if id := must(LoadIdentity(ChildIdentityPath(f.childDir)))(t); !id.Legacy {
		t.Fatal("the child replaced its identity during a trial")
	}
	// The child's own refusal, whoever its master is.
	l := &childLink{c: f.client, legacy: true}
	for _, method := range []string{methodToolsList, methodToolsCall, methodResult} {
		var refused *mcp.Error
		if _, err := l.HandleRequest(ctx, method, nil); !errors.As(err, &refused) || refused.Code != codeIncompatible {
			t.Errorf("%s on a legacy link, at the child: %v", method, err)
		}
	}
}

func TestSetNext(t *testing.T) {
	f := &oldFleet{dir: t.TempDir()}
	f.store = Open(f.dir)
	old := oldIdentityAt(t, ChildIdentityPath(t.TempDir()))
	other := must(MintIdentity(ChildIdentityPath(t.TempDir())))(t)
	now := time.Now().UTC().Truncate(time.Second)
	err := f.store.mutate(func(d *fileData) error {
		d.Machines[oldMachine] = &Machine{Name: "old-box", Fingerprint: old.Fingerprint, State: StateConfirmed, Reported: linuxBox, EnrolledAt: now}
		d.Machines["m00000000000000b2"] = &Machine{Name: "new-box", Fingerprint: other.Fingerprint, State: StateConfirmed, Reported: linuxBox, EnrolledAt: now}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	next := must(MintIdentity(ChildNextIdentityPath(t.TempDir())))(t)
	der := func(id Identity) []byte { return id.Certificate.Certificate[0] }

	if _, err := f.store.setNext(oldMachine, der(old)); err == nil {
		t.Error("an ECDSA certificate was taken as a machine's next one")
	}
	if _, err := f.store.setNext(oldMachine, der(other)); err == nil {
		t.Error("another machine's certificate was taken")
	}
	if _, err := f.store.setNext("m00000000000000b2", der(next)); err == nil {
		t.Error("a machine that is not on a legacy certificate was given a next one")
	}
	fp, err := f.store.setNext(oldMachine, der(next))
	if err != nil || fp != next.Fingerprint {
		t.Fatalf("setNext: %q, %v", fp, err)
	}
	if _, err := f.store.setNext(oldMachine, der(next)); err != nil {
		t.Errorf("the same certificate again: %v", err)
	}
	if m := must(f.store.Machine(oldMachine))(t); m.Next != next.Fingerprint || !m.Legacy() {
		t.Fatalf("after setNext: %+v", m)
	}
	// Both are pinned until the machine connects with the new one.
	if _, pins, _ := f.store.view(); len(pins) != 3 {
		t.Errorf("%d pins for two machines, one of them moving", len(pins))
	}
	// A certificate a machine is moving to is that machine's: nobody enrolls
	// with it, and fleet.json stays readable.
	if err := f.store.SetAddresses([]string{"127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	code := must(f.store.CreateEnrollment(next.Fingerprint, "third", 0, ""))(t)
	key := must(parseEnrollCode(code))(t).key()
	var why *Refusal
	if _, err := f.store.enroll(hashKey(key.PublicKey().Bytes()), next.Fingerprint, linuxBox); !errors.As(err, &why) || why.Reason != RefusedConflict {
		t.Errorf("an enrollment with a certificate another machine is moving to: %v", err)
	}
	if err := f.store.Err(); err != nil {
		t.Fatalf("fleet.json after that: %v", err)
	}
	// And no change is saved that the next reader could not read.
	err = f.store.mutate(func(d *fileData) error {
		d.Machines["m00000000000000b2"].Next = next.Fingerprint // not a legacy machine, and a certificate twice
		return nil
	})
	if err == nil || f.store.Err() != nil {
		t.Errorf("a change that breaks fleet.json: saved? %v, readable: %v", err, f.store.Err())
	}
	if err := f.store.promote(oldMachine, other.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if m := must(f.store.Machine(oldMachine))(t); !m.Legacy() {
		t.Fatal("promoted to a certificate that was not the machine's next one")
	}
	if err := f.store.promote(oldMachine, next.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if m := must(f.store.Machine(oldMachine))(t); m.Legacy() || m.Fingerprint != next.Fingerprint || m.Next != "" {
		t.Fatalf("after promote: %+v", m)
	}
}

// The word "rekey" in a hello moves no machine that is on the profile: a
// master cannot use it to point a machine at another certificate. Only a
// link the machine itself made with its legacy identity listens to it.
func TestRekeyIsALegacyLinksOnly(t *testing.T) {
	dir := t.TempDir()
	id := must(MintIdentity(ChildIdentityPath(dir)))(t)
	master := must(MintIdentity(MasterIdentityPath(t.TempDir())))(t)
	other := must(MintIdentity(MasterIdentityPath(t.TempDir())))(t)
	err := SaveChild(ChildPath(dir), Child{ID: oldMachine, Name: "box", MasterAddresses: []string{"127.0.0.1:1"},
		MasterFingerprint: master.Fingerprint, Sandbox: "host", Ceiling: CeilingFull})
	if err != nil {
		t.Fatal(err)
	}
	asked := false
	c := must(NewClient(ClientConfig{Dir: dir, Tools: noTools{}, Rekeyed: func(string, string) error { asked = true; return nil }}))(t)
	l := &childLink{c: c} // a link on the profile
	params := must(encode(helloParams{Link: LinkVersion, Rekey: other.Fingerprint}))(t)
	if _, err := l.HandleRequest(context.Background(), methodHello, params); err != nil {
		t.Fatal(err)
	}
	c.calls.Wait() // a move, had one started
	if l.rekeying.Load() || asked {
		t.Fatal("a hello on the profile started a move to another certificate")
	}
	if got := must(LoadChild(ChildPath(dir)))(t).MasterFingerprint; got != master.Fingerprint {
		t.Fatalf("child.json now pins %q", got)
	}
	if now := must(LoadIdentity(ChildIdentityPath(dir)))(t); now.Fingerprint != id.Fingerprint {
		t.Fatal("the machine's identity changed")
	}
	if _, err := os.Lstat(ChildNextIdentityPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a next identity was made: %v", err)
	}
	// The master's end takes fleet/rekey from a legacy link only.
	var refused *mcp.Error
	if _, err := (&link{}).rekey(must(encode(rekeyParams{Certificate: id.Certificate.Certificate[0]}))(t)); !errors.As(err, &refused) {
		t.Errorf("fleet/rekey on a link on the profile, at the master: %v", err)
	}
}

// A child stopped between the two writes of its move finishes it when it
// starts.
func TestNewClientFinishesAMove(t *testing.T) {
	dir := t.TempDir()
	oldIdentityAt(t, ChildIdentityPath(dir))
	next := must(MintIdentity(ChildNextIdentityPath(dir)))(t)
	master := must(MintIdentity(MasterIdentityPath(t.TempDir())))(t)
	err := SaveChild(ChildPath(dir), Child{ID: oldMachine, Name: "old-box", MasterAddresses: []string{"127.0.0.1:1"},
		MasterFingerprint: master.Fingerprint, Sandbox: "host", Ceiling: CeilingFull})
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewClient(ClientConfig{Dir: dir, Tools: noTools{}})
	if err != nil {
		t.Fatal(err)
	}
	if c.id.Legacy || c.id.Fingerprint != next.Fingerprint {
		t.Fatalf("the client's identity is %q (legacy=%v), not the one it was moving to", c.id.Fingerprint, c.id.Legacy)
	}
	if on := must(LoadIdentity(ChildIdentityPath(dir)))(t); on.Fingerprint != next.Fingerprint {
		t.Fatal("the identity file was not replaced")
	}
}

// The rules of the handshake keep the two kinds of identity apart.
func TestLegacyAndProfileDoNotMix(t *testing.T) {
	f := &oldFleet{dir: t.TempDir()}
	oldIdentityAt(t, MasterIdentityPath(f.dir))
	f.store = Open(f.dir)
	oldChild := oldIdentityAt(t, ChildIdentityPath(t.TempDir()))
	newChild := must(MintIdentity(ChildIdentityPath(t.TempDir())))(t)
	now := time.Now().UTC().Truncate(time.Second)
	err := f.store.mutate(func(d *fileData) error {
		d.Machines[oldMachine] = &Machine{Name: "old-box", Fingerprint: oldChild.Fingerprint, State: StateConfirmed, Reported: linuxBox, EnrolledAt: now}
		d.Machines["m00000000000000b2"] = &Machine{Name: "new-box", Fingerprint: newChild.Fingerprint, State: StateConfirmed, Reported: linuxBox, EnrolledAt: now}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.id, f.legacy, err = LoadMasterIdentity(f.dir); err != nil || f.legacy == nil {
		t.Fatalf("LoadMasterIdentity: %v, legacy %v", err, f.legacy)
	}
	both := must(MasterTLS(f.id, f.legacy, f.store))(t)

	// Each kind under its own rule.
	state, serverErr, clientErr := handshake(t, both, must(ChildTLS(oldChild, f.legacy.Fingerprint))(t))
	if serverErr != nil || clientErr != nil {
		t.Fatalf("a legacy machine and its master's legacy identity: %v / %v", serverErr, clientErr)
	}
	if state.CipherSuite != tls.TLS_AES_256_GCM_SHA384 || !slices.Contains(legacyGroups, state.CurveID) {
		t.Errorf("a legacy link negotiated %s and %v", tls.CipherSuiteName(state.CipherSuite), state.CurveID)
	}
	if m, err := f.store.Identify(state); err != nil || m.ID != oldMachine || !m.Legacy() {
		t.Errorf("Identify on a legacy connection: %+v, %v", m, err)
	}
	state, serverErr, clientErr = handshake(t, both, must(ChildTLS(newChild, f.id.Fingerprint))(t))
	if serverErr != nil || clientErr != nil {
		t.Fatalf("a machine on the profile: %v / %v", serverErr, clientErr)
	}
	if state.CipherSuite != tls.TLS_AES_256_GCM_SHA384 || state.CurveID != tls.MLKEM1024 || cnsa.CheckLeaf(state.PeerCertificates[0]) != nil {
		t.Errorf("a link negotiated %s and %v", tls.CipherSuiteName(state.CipherSuite), state.CurveID)
	}

	// A pin and an identity of two kinds make no configuration at all.
	if _, err := ChildTLS(oldChild, f.id.Fingerprint); err == nil {
		t.Error("a legacy identity was set up to dial for the master's new certificate")
	}
	if _, err := ChildTLS(newChild, f.legacy.Fingerprint); err == nil {
		t.Error("an identity on the profile was set up to dial for the master's legacy certificate")
	}
	// A machine on the profile that asks for the legacy rule is not served
	// by it, though its certificate is pinned.
	crossed := must(ChildTLS(newChild, f.id.Fingerprint))(t)
	crossed.ServerName = legacyLinkServerName
	crossed.CurvePreferences = legacyGroups
	crossed.VerifyConnection = nil
	if _, serverErr, clientErr := handshake(t, both, crossed); serverErr == nil && clientErr == nil {
		t.Error("an ML-DSA-87 certificate was served under the rule for legacy identities")
	}
	// A legacy machine that asks for the profile's rule is not served either.
	crossed = must(ChildTLS(oldChild, f.legacy.Fingerprint))(t)
	crossed.ServerName = linkServerName
	crossed.VerifyConnection = nil
	if _, serverErr, clientErr := handshake(t, both, crossed); serverErr == nil && clientErr == nil {
		t.Error("an ECDSA certificate was served under the profile's rule")
	}
	// A master with no legacy identity serves no legacy machine, and no
	// Tenant from before the profile enrolls.
	only := must(MasterTLS(f.id, nil, f.store))(t)
	if _, serverErr, clientErr := handshake(t, only, must(ChildTLS(oldChild, f.legacy.Fingerprint))(t)); serverErr == nil && clientErr == nil {
		t.Error("a master without a legacy identity served a legacy machine")
	}
	enroll := must(ChildTLS(oldChild, f.legacy.Fingerprint))(t)
	enroll.ServerName = legacyEnrollServerName
	if _, serverErr, clientErr := handshake(t, both, enroll); serverErr == nil && clientErr == nil {
		t.Error("a Tenant from before the profile reached the enroll endpoint")
	}
}

// The proof of an enrollment is for one connection and one certificate, and
// the code's seed is never sent.
func TestEnrollProofIsBoundToItsConnection(t *testing.T) {
	dir := t.TempDir()
	id := must(MintIdentity(MasterIdentityPath(dir)))(t)
	store := Open(dir)
	var (
		mu      sync.Mutex
		reasons []Reason
	)
	mgr, err := StartManager(ManagerConfig{Store: store, Identity: id, Addr: "127.0.0.1:0", Role: "test",
		Refused: func(r Refusal) { mu.Lock(); reasons = append(reasons, r.Reason); mu.Unlock() }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	addr := mgr.Addr().String()
	if err := store.SetAddresses([]string{addr}); err != nil {
		t.Fatal(err)
	}
	code := must(store.CreateEnrollment(id.Fingerprint, "box", 0, ""))(t)
	ec := must(parseEnrollCode(code))(t)
	key := ec.key()
	child := must(MintIdentity(ChildIdentityPath(t.TempDir())))(t)
	other := must(MintIdentity(ChildIdentityPath(t.TempDir())))(t)

	dial := func(as Identity) *tls.Conn {
		t.Helper()
		return must(tls.Dial("tcp", addr, must(childTLS(as, id.Fingerprint, enrollServerName))(t)))(t)
	}
	// prove signs for conn, as the machine with that fingerprint.
	prove := func(conn *tls.Conn, fingerprint string) []byte {
		t.Helper()
		cs := conn.ConnectionState()
		msg := must(enrollMessage(&cs, fingerprint))(t)
		proof := must(key.Sign(nil, msg, &mldsa.Options{Context: enrollContext}))(t)
		return must(json.Marshal(enrollRequest{Key: key.PublicKey().Bytes(), Proof: proof, Reported: linuxBox}))(t)
	}
	post := func(conn *tls.Conn, body []byte) int {
		t.Helper()
		defer conn.Close()
		req := must(http.NewRequest(http.MethodPost, "https://"+enrollServerName+EnrollPath, bytes.NewReader(body)))(t)
		req.Close = true
		if err := req.Write(conn); err != nil {
			t.Fatal(err)
		}
		resp := must(http.ReadResponse(bufio.NewReader(conn), req))(t)
		defer resp.Body.Close()
		return resp.StatusCode
	}
	last := func() Reason {
		mu.Lock()
		defer mu.Unlock()
		if len(reasons) == 0 {
			return ""
		}
		return reasons[len(reasons)-1]
	}

	first := dial(child)
	body := prove(first, child.Fingerprint)
	if bytes.Contains(body, []byte(ec.Seed)) {
		t.Fatal("the request carries the code's seed")
	}
	// The same bytes on another connection are no proof there.
	if got := post(dial(child), body); got != http.StatusForbidden || last() != RefusedBadProof {
		t.Fatalf("a proof replayed on another connection: status %d, refused as %q", got, last())
	}
	// Nor is a proof for one certificate a proof for another, on its own
	// connection.
	thief := dial(other)
	if got := post(thief, prove(thief, child.Fingerprint)); got != http.StatusForbidden || last() != RefusedBadProof {
		t.Fatalf("a proof for another machine's certificate: status %d, refused as %q", got, last())
	}
	if machines := must(store.Machines())(t); len(machines) != 0 {
		t.Fatalf("a refused proof enrolled %d machines", len(machines))
	}
	// On its own connection it enrolls the certificate of that connection.
	if got := post(first, body); got != http.StatusOK {
		t.Fatalf("the proof on its own connection: status %d, refused as %q", got, last())
	}
	machines := must(store.Machines())(t)
	if len(machines) != 1 || machines[0].Fingerprint != child.Fingerprint || machines[0].State != StatePending {
		t.Fatalf("enrolled: %+v", machines)
	}
	// The code is spent: a good proof on a new connection is a replay.
	again := dial(other)
	if got := post(again, prove(again, other.Fingerprint)); got != http.StatusForbidden || last() != RefusedReplay {
		t.Fatalf("a spent code: status %d, refused as %q", got, last())
	}
	// fleet.json knows the key by its hash and holds nothing that enrolls.
	raw := must(os.ReadFile(StorePath(dir)))(t)
	if bytes.Contains(raw, []byte(ec.Seed)) || !bytes.Contains(raw, []byte(hashKey(key.PublicKey().Bytes()))) {
		t.Error("fleet.json holds the code's seed, or not the hash of its key")
	}
}
