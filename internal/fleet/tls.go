package fleet

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"time"

	"tenant/internal/secrets"
)

const (
	masterIdentityFile = "fleet-identity.pem"
	childIdentityFile  = "child-identity.pem"

	// What a child puts in the handshake's server-name field. Nothing
	// resolves these; the name only tells a master that serves both endpoints
	// on one port which of its two handshake rules the connection asks for.
	linkServerName   = "link.fleet.invalid"
	enrollServerName = "enroll.fleet.invalid"
)

// ErrNoCertificate is returned for a connection that presented no
// certificate, or is not a TLS connection at all.
var ErrNoCertificate = errors.New("fleet: the connection presented no certificate")

// Identity is one machine's fleet key pair and self-signed certificate.
type Identity struct {
	Certificate tls.Certificate
	// Fingerprint is the lowercase hex SHA-256 of the certificate: what the
	// other side pins.
	Fingerprint string
}

// MasterIdentityPath is where a master keeps its fleet identity under its
// config dir.
func MasterIdentityPath(dir string) string { return filepath.Join(dir, masterIdentityFile) }

// ChildIdentityPath is where a child keeps its identity, beside child.json.
func ChildIdentityPath(dir string) string { return filepath.Join(dir, childIdentityFile) }

// MintIdentity makes a new key pair and a self-signed certificate valid for
// server and client use for ten years, and writes both to path as one
// owner-only file, replacing what was there. The private key exists nowhere
// else. A machine whose identity is replaced has to be enrolled again.
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

// mintPEM returns a certificate and its private key as one PEM text.
func mintPEM(notBefore, notAfter time.Time) ([]byte, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("fleet: generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("fleet: generate serial: %w", err)
	}
	// The other side pins the whole certificate, so nothing in the subject is
	// trusted or needs to say which machine this is.
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "tenant-fleet"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, fmt.Errorf("fleet: create certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("fleet: encode key: %w", err)
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return append(out, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})...), nil
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
	return Identity{Certificate: cert, Fingerprint: fingerprintOf(cert.Certificate[0])}, nil
}

func fingerprintOf(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// parseFingerprint accepts exactly what fingerprintOf writes: 64 lowercase
// hex digits.
func parseFingerprint(s string) (fp [sha256.Size]byte, err error) {
	if len(s) != 2*sha256.Size {
		return fp, fmt.Errorf("fleet: a certificate fingerprint is %d lowercase hex digits, not %d characters", 2*sha256.Size, len(s))
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fp, fmt.Errorf("fleet: a certificate fingerprint is %d lowercase hex digits", 2*sha256.Size)
		}
	}
	_, err = hex.Decode(fp[:], []byte(s))
	return fp, err
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

// baseTLS is what every fleet connection has on both ends: TLS 1.3 or
// nothing, this machine's certificate, and no resumption, so every
// connection proves its key and is checked again.
func baseTLS(id Identity) (*tls.Config, error) {
	if len(id.Certificate.Certificate) == 0 || id.Certificate.PrivateKey == nil {
		return nil, errors.New("fleet: no identity: a fleet connection needs this machine's certificate and key")
	}
	return &tls.Config{
		MinVersion:             tls.VersionTLS13,
		Certificates:           []tls.Certificate{id.Certificate},
		NextProtos:             []string{"http/1.1"}, // the link is a WebSocket
		SessionTicketsDisabled: true,
	}, nil
}

// MasterLinkTLS is the master's configuration for the link endpoint. A child
// must present a certificate, and its fingerprint is looked up in s, in
// constant time, during the handshake: a certificate that is not pinned, or
// belongs to a revoked machine, never reaches an HTTP handler. The handler
// learns which machine connected from s.Identify(r.TLS).
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
		_, err := s.Identify(&cs)
		return err
	}
	return cfg, nil
}

// MasterEnrollTLS is the master's configuration for the enroll endpoint, the
// one place a certificate that is not pinned is let through the handshake: a
// new machine has nothing but its one-time code yet. The enroll handler is
// the only thing that may be served over it.
func MasterEnrollTLS(id Identity) (*tls.Config, error) {
	cfg, err := baseTLS(id)
	if err != nil {
		return nil, err
	}
	cfg.ClientAuth = tls.RequireAnyClientCert
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		_, err := presented(&cs)
		return err
	}
	return cfg, nil
}

// MasterTLS serves both endpoints on one port. A connection gets the link's
// rule unless it asks for the enroll endpoint by server name while
// enrollment is switched on (Store.SetEnrollOpen), and only then is a
// certificate that is not pinned let through. Behind it the link handler
// still has to call s.Identify, which fails for such a connection.
func MasterTLS(id Identity, s *Store) (*tls.Config, error) {
	link, err := MasterLinkTLS(id, s)
	if err != nil {
		return nil, err
	}
	enroll, _ := MasterEnrollTLS(id) // id is the one MasterLinkTLS just accepted
	link.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		if hello.ServerName == enrollServerName && s.EnrollOpen() {
			return enroll, nil
		}
		return nil, nil // the link's rule
	}
	return link, nil
}

// ChildTLS is a child's configuration for dialing its master. The master's
// certificate must have exactly the pinned fingerprint, which stands in for
// the certificate chain and host name a public server would be checked by.
// An empty or malformed fingerprint is an error: a child that does not know
// its master does not start.
func ChildTLS(id Identity, masterFingerprint string) (*tls.Config, error) {
	return childTLS(id, masterFingerprint, linkServerName)
}

func childTLS(id Identity, masterFingerprint, serverName string) (*tls.Config, error) {
	want, err := parseFingerprint(masterFingerprint)
	if err != nil {
		return nil, fmt.Errorf("the master's fingerprint: %w", err)
	}
	cfg, err := baseTLS(id)
	if err != nil {
		return nil, err
	}
	cfg.ServerName = serverName
	cfg.InsecureSkipVerify = true // no authority signs fleet certificates; the pin below is the check
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		leaf, err := presented(&cs)
		if err != nil {
			return err
		}
		got := sha256.Sum256(leaf.Raw)
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			return fmt.Errorf("fleet: this is not the master this machine was enrolled with: its certificate is %s", hex.EncodeToString(got[:]))
		}
		return nil
	}
	return cfg, nil
}
