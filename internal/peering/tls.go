package peering

import (
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"tenant/internal/cnsa"
	"tenant/internal/secrets"
)

// TEN-185: peer-link transport security. The peer listener serves TLS with a
// self-signed cert minted once at first launch (stable across restarts); the
// invite code carries the cert's SHA-384 fingerprint, and the dialing peer pins
// it (TOFU-by-invite — no CA, no hostname dependency). The overlay alternative
// (Tailscale/WireGuard, plain HTTP) is handled by the bind policy in listener.go.
//
// Every TLS peer connection meets internal/cnsa: TLS 1.3, AES-256-GCM,
// ML-KEM-1024 and an ML-DSA-87 certificate, checked on both ends.

// certFileNames under cfgDir. Both use the peering secret-storage boundary.
const (
	peerCertFile = "peer-cert.pem"
	peerKeyFile  = "peer-key.pem"
)

// AtRestPaths returns the peering files whose effective protection is audited
// by tenant doctor. The certificate is public material, but is kept beside and
// protected consistently with the peer store and private key.
func AtRestPaths(cfgDir string) []string {
	return []string{
		PeersPath(cfgDir),
		filepath.Join(cfgDir, peerCertFile),
		filepath.Join(cfgDir, peerKeyFile),
	}
}

// CertFingerprint is the lowercase hex SHA-384 of a DER-encoded certificate —
// the value carried in an invite and pinned by the dialing peer.
func CertFingerprint(der []byte) string { return cnsa.Fingerprint(der) }

// CertPaths returns where this instance's peer TLS cert and private key live.
func CertPaths(cfgDir string) (cert, key string) {
	return filepath.Join(cfgDir, peerCertFile), filepath.Join(cfgDir, peerKeyFile)
}

// LoadOrMintCert returns this instance's stable self-signed TLS cert and its
// fingerprint, minting and persisting one with owner-only protection under cfgDir on first use. The
// fingerprint is stable across restarts because the cert is reused.
//
// A certificate from before the ML-DSA-87 profile (an ECDSA one) is replaced
// by a new one: no peer on this profile would accept it. Every peer that
// pinned the old one has to pair again.
func LoadOrMintCert(cfgDir string) (tls.Certificate, string, error) {
	certPath, keyPath := CertPaths(cfgDir)

	certPEM, certErr := secrets.ReadFile(certPath)
	keyPEM, keyErr := secrets.ReadFile(keyPath)
	if certErr == nil && keyErr == nil {
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return tls.Certificate{}, "", fmt.Errorf("peering: load peer cert: %w", err)
		}
		if len(cert.Certificate) == 0 {
			return tls.Certificate{}, "", fmt.Errorf("peering: peer cert file has no certificate")
		}
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return tls.Certificate{}, "", fmt.Errorf("peering: load peer cert: %w", err)
		}
		if cnsa.CheckLeaf(leaf) == nil {
			return cert, CertFingerprint(cert.Certificate[0]), nil
		}
		// An older certificate: fall through and mint its replacement.
	} else {
		if certErr != nil && !os.IsNotExist(certErr) {
			return tls.Certificate{}, "", fmt.Errorf("peering: read peer cert: %w", certErr)
		}
		if keyErr != nil && !os.IsNotExist(keyErr) {
			return tls.Certificate{}, "", fmt.Errorf("peering: read peer key: %w", keyErr)
		}
	}

	certPEM, keyPEM, err := mintSelfSigned()
	if err != nil {
		return tls.Certificate{}, "", err
	}
	if err := secrets.WriteFile(certPath, certPEM); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := secrets.WriteFile(keyPath, keyPEM); err != nil {
		return tls.Certificate{}, "", err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("peering: assemble minted cert: %w", err)
	}
	return cert, CertFingerprint(cert.Certificate[0]), nil
}

// mintSelfSigned creates a fresh ML-DSA-87 self-signed cert (10-year validity,
// loopback SANs). Because peers pin the fingerprint, the subject/SAN are not
// trust-bearing — they only keep the cert a well-formed x509.
func mintSelfSigned() (certPEM, keyPEM []byte, err error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("peering: gen serial: %w", err)
	}
	now := time.Now()
	certPEM, keyPEM, err = cnsa.SelfSigned(&x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "tenant-peer"},
		NotBefore:             now.Add(-1 * time.Hour), // small backdate for clock skew
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"tenant-peer", "localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("peering: %w", err)
	}
	return certPEM, keyPEM, nil
}

// ServerTLSConfig is the listening peer's TLS configuration: this instance's
// certificate, and a handshake that ends unless it negotiated the cnsa
// profile. The dialing peer sends no certificate; it proves itself with its
// bearer token, inside the connection.
func ServerTLSConfig(cert tls.Certificate) *tls.Config {
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}, VerifyConnection: cnsa.Check}
	cnsa.Apply(cfg)
	return cfg
}

// checkServer is the dialing side's whole judgement of a listening peer: the
// connection is on the cnsa profile and its certificate is an ML-DSA-87 one.
// It returns that certificate's fingerprint.
func checkServer(cs tls.ConnectionState) (string, error) {
	if err := cnsa.Check(cs); err != nil {
		return "", err
	}
	if len(cs.PeerCertificates) == 0 {
		return "", errors.New("peering: peer presented no certificate")
	}
	if err := cnsa.CheckLeaf(cs.PeerCertificates[0]); err != nil {
		return "", err
	}
	return CertFingerprint(cs.PeerCertificates[0].Raw), nil
}

// PinnedTLSClientConfig builds a *tls.Config for the DIALING peer (TEN-186):
// standard chain/hostname verification is disabled (self-signed, no CA) and
// replaced with a constant-time fingerprint pin against the leaf cert — the
// TOFU-by-invite contract. An empty fingerprint returns nil (overlay/plain HTTP;
// the caller dials http://). A connection whose leaf doesn't match is refused,
// which defeats a MITM that swapped the cert.
func PinnedTLSClientConfig(fingerprint string) *tls.Config {
	if fingerprint == "" {
		return nil
	}
	want := []byte(fingerprint)
	cfg := &tls.Config{
		InsecureSkipVerify: true, // we pin the leaf fingerprint instead of a CA chain
		VerifyConnection: func(cs tls.ConnectionState) error {
			got, err := checkServer(cs)
			if err != nil {
				return err
			}
			if subtle.ConstantTimeCompare([]byte(got), want) == 1 {
				return nil
			}
			if len(want) != cnsa.FingerprintLen {
				return fmt.Errorf("peering: this peer was paired before Tenant moved to ML-DSA-87 certificates and SHA-384 pins — pair with it again")
			}
			return fmt.Errorf("peering: peer cert fingerprint mismatch — expected the pinned cert from the invite (possible MITM); got %s", got)
		},
	}
	cnsa.Apply(cfg)
	return cfg
}
