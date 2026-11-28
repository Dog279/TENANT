package fleet

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"time"

	"tenant/internal/cnsa"
	"tenant/internal/datalock"
	"tenant/internal/secrets"
)

const (
	masterIdentityFile = "fleet-identity.pem"
	childIdentityFile  = "child-identity.pem"
	// The master's identity from before the cnsa profile, kept so that the
	// machines enrolled under it can still be reached, updated and moved to
	// the new one (rekey.go).
	masterLegacyIdentityFile = "fleet-identity-legacy.pem"
	// The identity a child has made for itself and not moved to yet.
	childNextIdentityFile = "child-identity-next.pem"

	// What a child puts in the handshake's server-name field. Nothing
	// resolves these; the name only tells a master that serves several
	// handshake rules on one port which of them the connection asks for.
	linkServerName   = "link.cnsa.fleet.invalid"
	enrollServerName = "enroll.cnsa.fleet.invalid"
	// What a Tenant from before the cnsa profile sends, and what a machine
	// still on its legacy identity sends, whatever its Tenant.
	legacyLinkServerName   = "link.fleet.invalid"
	legacyEnrollServerName = "enroll.fleet.invalid"
)

// legacyGroups are the key exchanges a legacy connection may use: the
// profile's own, and the one hybrid with ML-KEM-1024 that a Tenant from
// before the profile can do.
var legacyGroups = []tls.CurveID{tls.MLKEM1024, tls.SecP384r1MLKEM1024}

// ErrNoCertificate is returned for a connection that presented no
// certificate, or is not a TLS connection at all.
var ErrNoCertificate = errors.New("fleet: the connection presented no certificate")

// Identity is one machine's fleet key pair and self-signed certificate.
type Identity struct {
	Certificate tls.Certificate
	// Fingerprint is the lowercase hex SHA-384 of the certificate: what the
	// other side pins. Of a Legacy identity it is the SHA-256.
	Fingerprint string
	// Legacy marks an identity from before the cnsa profile: an ECDSA
	// certificate. Nothing is done over a connection it made but saying
	// hello, updating the machine and moving it to a new identity.
	Legacy bool
}

// MasterIdentityPath is where a master keeps its fleet identity under its
// config dir.
func MasterIdentityPath(dir string) string { return filepath.Join(dir, masterIdentityFile) }

// MasterLegacyIdentityPath is where a master keeps the identity it had
// before the cnsa profile, while machines still pin it.
func MasterLegacyIdentityPath(dir string) string {
	return filepath.Join(dir, masterLegacyIdentityFile)
}

// ChildIdentityPath is where a child keeps its identity, beside child.json.
func ChildIdentityPath(dir string) string { return filepath.Join(dir, childIdentityFile) }

// ChildNextIdentityPath is where a child keeps the identity it is moving to.
func ChildNextIdentityPath(dir string) string { return filepath.Join(dir, childNextIdentityFile) }

// MintIdentity makes a new ML-DSA-87 key pair and a self-signed certificate
// valid for server and client use for ten years, and writes both to path as
// one owner-only file, replacing what was there. The private key exists
// nowhere else. A machine whose identity is replaced has to be enrolled
// again, unless its master moved it (rekey.go).
func MintIdentity(path string) (Identity, error) {
	now := time.Now()
	// The hour back is for a clock that runs a little behind this one.
	pemBytes, err := mintPEM(now.Add(-time.Hour), now.AddDate(10, 0, 0))
	if err != nil {
		return Identity{}, err
	}
	if err := writePrivate(path, pemBytes); err != nil {
		return Identity{}, fmt.Errorf("fleet: save identity: %w", err)
	}
	return parseIdentity(pemBytes)
}

// LoadIdentity reads the identity MintIdentity wrote. A missing file is
// reported with fs.ErrNotExist, and is never replaced by a new identity:
// a new key is a machine nobody has enrolled.
func LoadIdentity(path string) (Identity, error) {
	pemBytes, err := secrets.ReadFile(path)
	if err != nil {
		return Identity{}, fmt.Errorf("fleet: read identity: %w", err)
	}
	id, err := parseIdentity(pemBytes)
	if err != nil {
		return Identity{}, err
	}
	if leaf := id.Certificate.Leaf; time.Now().After(leaf.NotAfter) {
		return Identity{}, fmt.Errorf("fleet: this machine's certificate expired on %s; it has to be enrolled again",
			leaf.NotAfter.Format(time.DateOnly))
	}
	return id, nil
}

// LoadMasterIdentity is LoadIdentity for a master, and the step that moves a
// master to the cnsa profile: a master whose identity is a legacy one keeps
// it as its legacy identity and gets a new one. legacy is the identity the
// machines enrolled before the profile still pin, while there is such a
// machine: once the last of them has moved or was revoked, the legacy
// identity is deleted, here, at the master's next start.
func LoadMasterIdentity(dir string) (id Identity, legacy *Identity, err error) {
	old := MasterLegacyIdentityPath(dir)
	if id, err = LoadIdentity(MasterIdentityPath(dir)); err == nil && id.Legacy {
		id, err = replaceLegacyIdentity(dir)
	}
	if err != nil {
		return Identity{}, nil, err
	}
	switch l, lerr := LoadIdentity(old); {
	case lerr == nil && l.Legacy && legacyInUse(dir):
		legacy = &l
	case lerr == nil && l.Legacy:
		// The last machine that pinned it has moved or was revoked: the old
		// key is of no use to this fleet any more, only to someone else.
		_ = os.Remove(old)
	case lerr == nil || errors.Is(lerr, fs.ErrNotExist):
	default:
		// Not deleted here: without it the machines that pin it are cut off
		// and have to be enrolled again, which is the operator's to decide.
		return Identity{}, nil, fmt.Errorf("fleet: the master's legacy identity (%s): %w; delete the file to go on without it, and enroll the machines that still pin it again", old, lerr)
	}
	return id, legacy, nil
}

// legacyInUse reports whether a machine that may still connect is pinned by
// the certificate it had before the cnsa profile. While fleet.json cannot be
// read the answer is yes: nothing is given up on a guess.
func legacyInUse(dir string) bool {
	machines, err := Open(dir).Machines()
	if err != nil {
		return true
	}
	return slices.ContainsFunc(machines, func(m Machine) bool { return m.Legacy() && m.State != StateRevoked })
}

// replaceLegacyIdentity keeps a master's legacy identity under its legacy
// name and gives the master a new one. It works under fleet.json's lock,
// which every Tenant process takes: of two that start at the same moment
// one makes the new identity and the other finds it, so no machine is ever
// told of an identity that is then replaced.
func replaceLegacyIdentity(dir string) (Identity, error) {
	lock, err := datalock.File(StorePath(dir), true)
	if err != nil {
		return Identity{}, fmt.Errorf("fleet: lock fleet.json: %w", err)
	}
	defer lock.Release()
	path := MasterIdentityPath(dir)
	b, err := secrets.ReadFile(path)
	if err != nil {
		return Identity{}, fmt.Errorf("fleet: read identity: %w", err)
	}
	if id, err := parseIdentity(b); err != nil || !id.Legacy {
		return id, err // another process was here first
	}
	// The legacy copy first: until the new identity is written the old file
	// still stands, and a second start does the same again.
	if err := writePrivate(MasterLegacyIdentityPath(dir), b); err != nil {
		return Identity{}, fmt.Errorf("fleet: keep the legacy identity: %w", err)
	}
	return MintIdentity(path)
}

// mintPEM returns a certificate and its private key as one PEM text.
func mintPEM(notBefore, notAfter time.Time) ([]byte, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("fleet: generate serial: %w", err)
	}
	// The other side pins the whole certificate, so nothing in the subject is
	// trusted or needs to say which machine this is.
	certPEM, keyPEM, err := cnsa.SelfSigned(&x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "tenant-fleet"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	})
	if err != nil {
		return nil, fmt.Errorf("fleet: %w", err)
	}
	return append(certPEM, keyPEM...), nil
}

func parseIdentity(pemBytes []byte) (Identity, error) {
	cert, err := tls.X509KeyPair(pemBytes, pemBytes)
	if err != nil {
		return Identity{}, fmt.Errorf("fleet: identity file: %w", err)
	}
	// Parsed here, not left to the runtime setting that decides whether
	// X509KeyPair keeps the parsed certificate.
	if cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
		return Identity{}, fmt.Errorf("fleet: identity file: %w", err)
	}
	if cnsa.CheckLeaf(cert.Leaf) != nil {
		return Identity{Certificate: cert, Fingerprint: legacyFingerprintOf(cert.Certificate[0]), Legacy: true}, nil
	}
	return Identity{Certificate: cert, Fingerprint: fingerprintOf(cert.Certificate[0])}, nil
}

func fingerprintOf(der []byte) string { return cnsa.Fingerprint(der) }

// legacyFingerprintOf is how a certificate was pinned before the cnsa
// profile: by its SHA-256.
func legacyFingerprintOf(der []byte) string { return sha256Hex(der) }

// sha256Hex is the SHA-256 of b in lowercase hex: what the audit log chains
// its records by and names a call's arguments and result by (audit.go,
// gate.go), and an update's checksum. It is not how a certificate is pinned:
// that is fingerprintOf.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

const legacyFingerprintLen = 2 * sha256.Size

// parseDigest accepts exactly what hex.EncodeToString writes for a digest of
// size bytes: twice as many lowercase hex digits.
func parseDigest(s string, size int) ([]byte, error) {
	if len(s) != 2*size {
		return nil, fmt.Errorf("%d lowercase hex digits are expected, not %d characters", 2*size, len(s))
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return nil, fmt.Errorf("%d lowercase hex digits are expected", 2*size)
		}
	}
	return hex.DecodeString(s)
}

// parseFingerprint accepts exactly what fingerprintOf writes.
func parseFingerprint(s string) ([]byte, error) {
	b, err := parseDigest(s, cnsa.FingerprintLen/2)
	if err != nil {
		return nil, fmt.Errorf("fleet: a certificate fingerprint: %w", err)
	}
	return b, nil
}

// parsePin accepts a fingerprint, and a legacy one: what a file written
// before the cnsa profile pins a certificate by.
func parsePin(s string) ([]byte, error) {
	if len(s) == legacyFingerprintLen {
		if b, err := parseDigest(s, sha256.Size); err == nil {
			return b, nil
		}
	}
	return parseFingerprint(s)
}

// presented is the certificate the other side of a connection proved it
// holds the key of. An expired one is refused: a certificate lasts ten years
// and a machine past that is enrolled again. Its start date is not checked,
// because it says nothing about a pinned certificate and a clock that is
// behind would lock a machine out.
func presented(cs *tls.ConnectionState) (*x509.Certificate, error) {
	if cs == nil || len(cs.PeerCertificates) == 0 {
		return nil, ErrNoCertificate
	}
	leaf := cs.PeerCertificates[0]
	if time.Now().After(leaf.NotAfter) {
		return nil, fmt.Errorf("fleet: the certificate expired on %s", leaf.NotAfter.Format(time.DateOnly))
	}
	return leaf, nil
}

// verified is presented for a connection that also has to be on the cnsa
// profile, with an ML-DSA-87 certificate on the other side; or, with legacy,
// on what the profile leaves of it for a legacy identity: the same cipher
// suite, a key exchange with ML-KEM-1024 in it, and a certificate that is
// not an ML-DSA-87 one. It returns the certificate and the digest it is
// pinned by.
func verified(cs *tls.ConnectionState, legacy bool) (*x509.Certificate, []byte, error) {
	leaf, err := presented(cs)
	if err != nil {
		return nil, nil, err
	}
	if !legacy {
		if err := cnsa.Check(*cs); err != nil {
			return nil, nil, err
		}
		if err := cnsa.CheckLeaf(leaf); err != nil {
			return nil, nil, err
		}
		sum, _ := hex.DecodeString(fingerprintOf(leaf.Raw))
		return leaf, sum, nil
	}
	if err := cnsa.Suite(*cs); err != nil {
		return nil, nil, err
	}
	if !slices.Contains(legacyGroups, cs.CurveID) {
		return nil, nil, fmt.Errorf("fleet: the key exchange is %v, which has no ML-KEM-1024 in it", cs.CurveID)
	}
	if cnsa.CheckLeaf(leaf) == nil {
		return nil, nil, errors.New("fleet: an ML-DSA-87 certificate on a connection for legacy identities")
	}
	sum := sha256.Sum256(leaf.Raw)
	return leaf, sum[:], nil
}

// baseTLS is what every fleet connection has on both ends: the cnsa profile
// (TLS 1.3, AES-256-GCM, ML-KEM-1024), this machine's certificate, and no
// resumption, so every connection proves its key and is checked again. A
// legacy identity gets the legacy key exchanges with it.
func baseTLS(id Identity) (*tls.Config, error) {
	if len(id.Certificate.Certificate) == 0 || id.Certificate.PrivateKey == nil {
		return nil, errors.New("fleet: no identity: a fleet connection needs this machine's certificate and key")
	}
	cfg := &tls.Config{
		Certificates:           []tls.Certificate{id.Certificate},
		NextProtos:             []string{"http/1.1"}, // the link is a WebSocket
		SessionTicketsDisabled: true,
	}
	cnsa.Apply(cfg)
	if id.Legacy {
		cfg.CurvePreferences = legacyGroups
	}
	return cfg, nil
}

// MasterLinkTLS is the master's configuration for the link endpoint. A child
// must present a certificate, and its fingerprint is looked up in s, in
// constant time, during the handshake: a certificate that is not pinned, or
// belongs to a revoked machine, never reaches an HTTP handler, and neither
// does a connection that is not on the cnsa profile. The handler learns
// which machine connected from s.Identify(r.TLS).
//
// With a legacy identity it is the rule for the machines that still pin
// that identity (masterLegacyTLS).
func MasterLinkTLS(id Identity, s *Store) (*tls.Config, error) {
	cfg, err := baseTLS(id)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, errors.New("fleet: the link needs the fleet store to pin certificates against")
	}
	cfg.ClientAuth = tls.RequireAnyClientCert
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		_, err := s.identify(&cs, id.Legacy)
		return err
	}
	return cfg, nil
}

// MasterEnrollTLS is the master's configuration for the enroll endpoint, the
// one place a certificate that is not pinned is let through the handshake: a
// new machine has nothing but its one-time code yet. The enroll handler is
// the only thing that may be served over it.
func MasterEnrollTLS(id Identity) (*tls.Config, error) {
	if id.Legacy {
		return nil, errors.New("fleet: no machine is enrolled under a legacy identity")
	}
	cfg, err := baseTLS(id)
	if err != nil {
		return nil, err
	}
	cfg.ClientAuth = tls.RequireAnyClientCert
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		_, _, err := verified(&cs, false)
		return err
	}
	return cfg, nil
}

// MasterTLS serves every rule on one port. A connection gets the link's
// rule unless it asks, by server name, for one of two others: the enroll
// endpoint, while enrollment is switched on (Store.SetEnrollOpen), and only
// then is a certificate that is not pinned let through; or the link under
// the master's legacy identity, when legacy is one. Behind it the link
// handler still has to call s.Identify, which fails for a connection that
// was let through to enroll.
func MasterTLS(id Identity, legacy *Identity, s *Store) (*tls.Config, error) {
	if id.Legacy {
		return nil, errors.New("fleet: the master's identity is a legacy one (LoadMasterIdentity makes its new one)")
	}
	link, err := MasterLinkTLS(id, s)
	if err != nil {
		return nil, err
	}
	enroll, _ := MasterEnrollTLS(id) // id is the one MasterLinkTLS just accepted
	var old *tls.Config
	if legacy != nil {
		if !legacy.Legacy {
			return nil, errors.New("fleet: the master's legacy identity is not a legacy one")
		}
		if old, err = MasterLinkTLS(*legacy, s); err != nil {
			return nil, err
		}
	}
	link.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		switch hello.ServerName {
		case enrollServerName:
			if s.EnrollOpen() {
				return enroll, nil
			}
		case legacyLinkServerName:
			if old == nil {
				return nil, errors.New("fleet: a machine asked for the master's legacy identity, and this master has none: the machine has to be enrolled again")
			}
			return old, nil
		case legacyEnrollServerName:
			return nil, errors.New("fleet: a Tenant from before the ML-DSA-87 profile tried to enroll: update it first")
		}
		return nil, nil // the link's rule
	}
	return link, nil
}

// ChildTLS is a child's configuration for dialing its master. The master's
// certificate must have exactly the pinned fingerprint, which stands in for
// the certificate chain and host name a public server would be checked by,
// and the connection must be on the cnsa profile. An empty or malformed
// fingerprint is an error: a child that does not know its master does not
// start.
//
// A child still on its legacy identity dials for the master's legacy
// identity, which is the one it pins.
func ChildTLS(id Identity, masterFingerprint string) (*tls.Config, error) {
	name := linkServerName
	if id.Legacy {
		name = legacyLinkServerName
	}
	return childTLS(id, masterFingerprint, name)
}

func childTLS(id Identity, masterFingerprint, serverName string) (*tls.Config, error) {
	want, err := parsePin(masterFingerprint)
	if err != nil {
		return nil, fmt.Errorf("the master's fingerprint: %w", err)
	}
	if legacyPin := len(want) == sha256.Size; legacyPin != id.Legacy {
		return nil, errors.New("fleet: this machine's identity and the master's fingerprint are not of one kind: one is from before the ML-DSA-87 profile and the other is not")
	}
	cfg, err := baseTLS(id)
	if err != nil {
		return nil, err
	}
	cfg.ServerName = serverName
	cfg.InsecureSkipVerify = true // no authority signs fleet certificates; the pin below is the check
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		_, got, err := verified(&cs, id.Legacy)
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare(got, want) != 1 {
			return fmt.Errorf("fleet: this is not the master this machine was enrolled with: its certificate is %s", hex.EncodeToString(got))
		}
		return nil
	}
	return cfg, nil
}
