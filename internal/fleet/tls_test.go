package fleet

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"tenant/internal/secrets"
)

func TestMintIdentity(t *testing.T) {
	path := MasterIdentityPath(t.TempDir())
	id := must(MintIdentity(path))(t)
	leaf := id.Certificate.Leaf
	if !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) || !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageClientAuth) {
		t.Errorf("extended key usage = %v, want server and client", leaf.ExtKeyUsage)
	}
	tenYears := time.Now().AddDate(10, 0, 0)
	if d := leaf.NotAfter.Sub(tenYears); d < -time.Hour || d > time.Hour {
		t.Errorf("certificate ends %s, want ten years from now", leaf.NotAfter)
	}
	if _, err := parseFingerprint(id.Fingerprint); err != nil || id.Fingerprint != fingerprintOf(leaf.Raw) {
		t.Errorf("fingerprint %q: %v", id.Fingerprint, err)
	}
	if report := secrets.Probe(path); !report.Protected {
		t.Errorf("identity file is not owner-only: %+v", report)
	}

	again := must(LoadIdentity(path))(t)
	if again.Fingerprint != id.Fingerprint {
		t.Error("loading the identity gave another certificate")
	}
	// Minting again replaces the identity: a new key, a new machine.
	if other := must(MintIdentity(path))(t); other.Fingerprint == id.Fingerprint {
		t.Error("a second mint kept the old certificate")
	} else if now := must(LoadIdentity(path))(t); now.Fingerprint != other.Fingerprint {
		t.Error("the file does not hold the newest identity")
	}
}

func TestLoadIdentityRefusals(t *testing.T) {
	dir := t.TempDir()
	// A missing identity is reported, never replaced by a fresh one.
	if _, err := LoadIdentity(ChildIdentityPath(dir)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing identity: err = %v, want fs.ErrNotExist", err)
	}
	if _, err := os.Stat(ChildIdentityPath(dir)); !errors.Is(err, fs.ErrNotExist) {
		t.Error("loading a missing identity created one")
	}

	garbage := filepath.Join(dir, "garbage.pem")
	if err := secrets.WriteFile(garbage, []byte("not a certificate")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadIdentity(garbage); err == nil {
		t.Error("garbage loaded as an identity")
	}

	if _, err := MintIdentity(underAFile(t, "identity.pem")); err == nil {
		t.Error("minted an identity where no file can be written")
	}

	expired := filepath.Join(dir, "expired.pem")
	if err := secrets.WriteFile(expired, expiredPEM(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadIdentity(expired); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("expired identity: err = %v", err)
	}
}

func expiredPEM(t *testing.T) []byte {
	t.Helper()
	now := time.Now()
	return must(mintPEM(now.AddDate(-11, 0, 0), now.Add(-time.Hour)))(t)
}

func TestParseFingerprint(t *testing.T) {
	good := strings.Repeat("0a", 32)
	if _, err := parseFingerprint(good); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"", "0a", good[:63], good + "0", strings.ToUpper(good), "zz" + good[2:], " " + good[1:],
		strings.Repeat("0a:", 21) + "0",
	} {
		if _, err := parseFingerprint(bad); err == nil {
			t.Errorf("parseFingerprint(%q) accepted", bad)
		}
	}
}

// No fleet TLS configuration can be built without an identity, a pin and a
// store: there is nothing to fall back to.
func TestTLSConfigsCannotBeBuiltOpen(t *testing.T) {
	m := newMaster(t)
	child := newIdentity(t)

	for _, fp := range []string{"", "abc", strings.ToUpper(m.id.Fingerprint), m.id.Fingerprint[:63], m.id.Fingerprint + "0", "g" + m.id.Fingerprint[1:]} {
		if cfg, err := ChildTLS(child, fp); err == nil || cfg != nil {
			t.Errorf("ChildTLS with fingerprint %q: cfg = %v, err = %v; want an error and no configuration", fp, cfg != nil, err)
		}
	}
	if cfg, err := ChildTLS(Identity{}, m.id.Fingerprint); err == nil || cfg != nil {
		t.Error("ChildTLS without an identity must fail")
	}
	if cfg, err := MasterLinkTLS(Identity{}, m.store); err == nil || cfg != nil {
		t.Error("MasterLinkTLS without an identity must fail")
	}
	if cfg, err := MasterLinkTLS(m.id, nil); err == nil || cfg != nil {
		t.Error("MasterLinkTLS without a store must fail")
	}
	if cfg, err := MasterEnrollTLS(Identity{}); err == nil || cfg != nil {
		t.Error("MasterEnrollTLS without an identity must fail")
	}
	if cfg, err := MasterTLS(Identity{}, m.store); err == nil || cfg != nil {
		t.Error("MasterTLS without an identity must fail")
	}
	if cfg, err := MasterTLS(m.id, nil); err == nil || cfg != nil {
		t.Error("MasterTLS without a store must fail")
	}

	// What can be built demands TLS 1.3 and a certificate from the other side.
	link := must(MasterLinkTLS(m.id, m.store))(t)
	enroll := must(MasterEnrollTLS(m.id))(t)
	both := must(MasterTLS(m.id, m.store))(t)
	for name, cfg := range map[string]*tls.Config{"link": link, "enroll": enroll, "both": both} {
		if cfg.MinVersion != tls.VersionTLS13 || cfg.ClientAuth != tls.RequireAnyClientCert || cfg.VerifyConnection == nil || !cfg.SessionTicketsDisabled {
			t.Errorf("%s: MinVersion %x, ClientAuth %v, VerifyConnection set %v, tickets off %v", name, cfg.MinVersion, cfg.ClientAuth, cfg.VerifyConnection != nil, cfg.SessionTicketsDisabled)
		}
	}
	c := must(ChildTLS(child, m.id.Fingerprint))(t)
	if c.MinVersion != tls.VersionTLS13 || c.VerifyConnection == nil || len(c.Certificates) != 1 {
		t.Errorf("child: MinVersion %x, VerifyConnection set %v, %d certificates", c.MinVersion, c.VerifyConnection != nil, len(c.Certificates))
	}
}

// The handshake matrix, over real loopback TLS.
func TestLinkHandshake(t *testing.T) {
	m := newMaster(t)
	confirmed, confirmedID := m.add(t, "confirmed", StateConfirmed)
	pending, pendingID := m.add(t, "pending", StatePending)
	paused, pausedID := m.add(t, "paused", StatePaused)
	_, revokedID := m.add(t, "revoked", StateRevoked)
	stranger := newIdentity(t)

	link := must(MasterLinkTLS(m.id, m.store))(t)
	dial := func(id Identity) *tls.Config { return must(ChildTLS(id, m.id.Fingerprint))(t) }

	// A pinned machine gets through, and the master knows which one it is
	// from the connection alone. Knowing is not permission: Check decides.
	for _, tc := range []struct {
		name  string
		id    Identity
		want  Machine
		check error
	}{
		{"confirmed", confirmedID, confirmed, nil},
		{"pending", pendingID, pending, ErrPending},
		{"paused", pausedID, paused, ErrPaused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, serverErr, clientErr := handshake(t, link, dial(tc.id))
			if serverErr != nil || clientErr != nil {
				t.Fatalf("handshake: server %v, client %v", serverErr, clientErr)
			}
			if state.Version != tls.VersionTLS13 {
				t.Errorf("negotiated %x, want TLS 1.3", state.Version)
			}
			got, err := m.store.Identify(state)
			if err != nil || got.ID != tc.want.ID || got.Name != tc.name || got.State != tc.want.State {
				t.Errorf("Identify = %+v, %v; want machine %s (%s)", got, err, tc.want.ID, tc.want.State)
			}
			if err := m.store.Check(got.ID); !errors.Is(err, tc.check) {
				t.Errorf("Check = %v, want %v", err, tc.check)
			}
		})
	}
	if !strings.Contains(ErrPending.Error(), WaitingForConfirmation) {
		t.Errorf("a pending machine's answer is %q, want it to say %q", ErrPending, WaitingForConfirmation)
	}

	// Everything else is dropped by the master during the handshake.
	old := tls.Config{MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12}
	tls12 := dial(confirmedID)
	tls12.MinVersion, tls12.MaxVersion = old.MinVersion, old.MaxVersion
	noCert := dial(confirmedID)
	noCert.Certificates = nil
	for _, tc := range []struct {
		name   string
		client *tls.Config
		want   string // in the server's error
	}{
		{"a certificate that is not pinned", dial(stranger), ErrUnknown.Error()},
		{"a revoked machine", dial(revokedID), ErrRevoked.Error()},
		{"TLS 1.2", tls12, "unsupported versions"},
		{"no client certificate", noCert, "didn't provide a certificate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, serverErr, clientErr := handshake(t, link, tc.client)
			if state != nil || serverErr == nil || !strings.Contains(serverErr.Error(), tc.want) {
				t.Errorf("server: connected %v, err %v; want a handshake error containing %q", state != nil, serverErr, tc.want)
			}
			if clientErr == nil {
				t.Error("the client got through")
			}
		})
	}

	// A machine revoked or paused after it connected once is judged again at
	// its next handshake.
	if err := m.store.Revoke(confirmed.ID); err != nil {
		t.Fatal(err)
	}
	if state, serverErr, _ := handshake(t, link, dial(confirmedID)); state != nil || !errors.Is(serverErr, ErrRevoked) {
		t.Errorf("after revoke: connected %v, err %v", state != nil, serverErr)
	}
}

func TestChildRefusesTheWrongMaster(t *testing.T) {
	m := newMaster(t)
	_, childID := m.add(t, "box", StateConfirmed)
	link := must(MasterLinkTLS(m.id, m.store))(t)

	// Pinned to another master's certificate.
	other := newIdentity(t)
	state, serverErr, clientErr := handshake(t, link, must(ChildTLS(childID, other.Fingerprint))(t))
	if clientErr == nil || !strings.Contains(clientErr.Error(), "not the master") {
		t.Errorf("client err = %v, want a refusal of the master's certificate", clientErr)
	}
	if state != nil || serverErr == nil {
		t.Error("the master saw a finished handshake")
	}

	// A master that only speaks TLS 1.2.
	weak := link.Clone()
	weak.MinVersion, weak.MaxVersion = tls.VersionTLS12, tls.VersionTLS12
	if _, _, clientErr := handshake(t, weak, must(ChildTLS(childID, m.id.Fingerprint))(t)); clientErr == nil {
		t.Error("the child accepted a TLS 1.2 master")
	}

	// The right master, with a certificate past its ten years.
	dir := t.TempDir()
	if err := secrets.WriteFile(MasterIdentityPath(dir), expiredPEM(t)); err != nil {
		t.Fatal(err)
	}
	stale := must(parseIdentity(must(secrets.ReadFile(MasterIdentityPath(dir)))(t)))(t)
	staleLink := must(MasterLinkTLS(stale, m.store))(t)
	if _, _, clientErr := handshake(t, staleLink, must(ChildTLS(childID, stale.Fingerprint))(t)); clientErr == nil || !strings.Contains(clientErr.Error(), "expired") {
		t.Errorf("expired master certificate: client err = %v", clientErr)
	}
}

func TestMasterRefusesAnExpiredChildCertificate(t *testing.T) {
	m := newMaster(t)
	stale := must(parseIdentity(expiredPEM(t)))(t)
	m.addIdentity(t, "old", StateConfirmed, stale)
	link := must(MasterLinkTLS(m.id, m.store))(t)
	state, serverErr, _ := handshake(t, link, must(ChildTLS(stale, m.id.Fingerprint))(t))
	if state != nil || serverErr == nil || !strings.Contains(serverErr.Error(), "expired") {
		t.Errorf("connected %v, err %v; want the expired certificate refused", state != nil, serverErr)
	}
}

// The enroll configuration is the only one that lets a certificate through
// that is not pinned; the link's never does, whatever name the client asks
// for.
func TestOnlyTheEnrollEndpointTakesAnUnpinnedCertificate(t *testing.T) {
	m := newMaster(t)
	_, pinnedID := m.add(t, "box", StateConfirmed)
	stranger := newIdentity(t)
	asEnroll := func(id Identity) *tls.Config { return must(childTLS(id, m.id.Fingerprint, enrollServerName))(t) }
	asLink := func(id Identity) *tls.Config { return must(ChildTLS(id, m.id.Fingerprint))(t) }

	enroll := must(MasterEnrollTLS(m.id))(t)
	link := must(MasterLinkTLS(m.id, m.store))(t)
	both := must(MasterTLS(m.id, m.store))(t)

	connects := func(server, client *tls.Config) *tls.ConnectionState {
		t.Helper()
		state, serverErr, clientErr := handshake(t, server, client)
		if (state == nil) != (serverErr != nil) || (state == nil) != (clientErr != nil) {
			t.Fatalf("the two ends disagree: server %v, client %v", serverErr, clientErr)
		}
		return state
	}

	if connects(enroll, asEnroll(stranger)) == nil {
		t.Error("the enroll endpoint refused a new machine's certificate")
	}
	if connects(link, asEnroll(stranger)) != nil || connects(link, asLink(stranger)) != nil {
		t.Error("the link let a certificate through that is not pinned")
	}

	// One port for both: the server name picks the rule.
	if connects(both, asLink(stranger)) != nil {
		t.Error("one port: a certificate that is not pinned got in by the link's name")
	}
	state := connects(both, asEnroll(stranger))
	if state == nil {
		t.Fatal("one port: the enroll name did not reach the enroll rule")
	}
	// It is through the handshake and is still nobody to the link.
	if _, err := m.store.Identify(state); !errors.Is(err, ErrUnknown) {
		t.Errorf("Identify of a new machine = %v, want ErrUnknown", err)
	}
	if connects(both, asLink(pinnedID)) == nil || connects(both, asEnroll(pinnedID)) == nil {
		t.Error("one port: a pinned machine was refused")
	}

	// Enrollment switched off: the one port has no way in for a stranger.
	if err := m.store.SetEnrollOpen(false); err != nil {
		t.Fatal(err)
	}
	if connects(both, asEnroll(stranger)) != nil {
		t.Error("enrollment is off and a certificate that is not pinned got through")
	}
	if connects(both, asLink(pinnedID)) == nil {
		t.Error("switching enrollment off closed the link")
	}
	// The enroll rule still refuses TLS 1.2, no certificate and an expired one.
	weak := asEnroll(stranger)
	weak.MinVersion, weak.MaxVersion = tls.VersionTLS12, tls.VersionTLS12
	bare := asEnroll(stranger)
	bare.Certificates = nil
	stale := must(parseIdentity(expiredPEM(t)))(t)
	for name, client := range map[string]*tls.Config{"TLS 1.2": weak, "no certificate": bare, "expired": asEnroll(stale)} {
		if state, serverErr, _ := handshake(t, enroll, client); state != nil || serverErr == nil {
			t.Errorf("enroll endpoint, %s: got through", name)
		}
	}
}

func TestIdentifyNeedsACertificate(t *testing.T) {
	m := newMaster(t)
	if _, err := m.store.Identify(nil); !errors.Is(err, ErrNoCertificate) {
		t.Errorf("no TLS state: %v", err)
	}
	if _, err := m.store.Identify(&tls.ConnectionState{}); !errors.Is(err, ErrNoCertificate) {
		t.Errorf("no certificate: %v", err)
	}
}
