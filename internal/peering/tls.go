package peering

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
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// TEN-185: peer-link transport security. The peer listener serves TLS with a
// self-signed cert minted once at first launch (stable across restarts); the
// invite code carries the cert's SHA-256 fingerprint, and the dialing peer pins
// it (TOFU-by-invite — no CA, no hostname dependency). The overlay alternative
// (Tailscale/WireGuard, plain HTTP) is handled by the bind policy in listener.go.

// certFileNames under cfgDir. 0600 like every other peering secret.
const (
	peerCertFile = "peer-cert.pem"
	peerKeyFile  = "peer-key.pem"
)

// CertFingerprint is the lowercase hex SHA-256 of a DER-encoded certificate —
// the value carried in an invite and pinned by the dialing peer.
func CertFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// LoadOrMintCert returns this instance's stable self-signed TLS cert and its
// fingerprint, minting + persisting one (0600) under cfgDir on first use. The
// fingerprint is stable across restarts because the cert is reused.
func LoadOrMintCert(cfgDir string) (tls.Certificate, string, error) {
	certPath := filepath.Join(cfgDir, peerCertFile)
	keyPath := filepath.Join(cfgDir, peerKeyFile)

	if fileExists(certPath) && fileExists(keyPath) {
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return tls.Certificate{}, "", fmt.Errorf("peering: load peer cert: %w", err)
		}
		if len(cert.Certificate) == 0 {
			return tls.Certificate{}, "", fmt.Errorf("peering: peer cert file has no certificate")
		}
		return cert, CertFingerprint(cert.Certificate[0]), nil
	}

	certPEM, keyPEM, err := mintSelfSigned()
	if err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		return tls.Certificate{}, "", fmt.Errorf("peering: mkdir: %w", err)
	}
	if err := atomicWriteFile(certPath, certPEM, 0o600); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := atomicWriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, "", err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("peering: assemble minted cert: %w", err)
	}
	return cert, CertFingerprint(cert.Certificate[0]), nil
}

// mintSelfSigned creates a fresh ECDSA P-256 self-signed cert (10-year validity,
// loopback SANs). Because peers pin the fingerprint, the subject/SAN are not
// trust-bearing — they only keep the cert a well-formed x509.
func mintSelfSigned() (certPEM, keyPEM []byte, err error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("peering: gen key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("peering: gen serial: %w", err)
	}
	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "tenant-peer"},
		NotBefore:             now.Add(-1 * time.Hour), // small backdate for clock skew
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"tenant-peer", "localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, fmt.Errorf("peering: create cert: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("peering: marshal key: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
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
	return &tls.Config{
		InsecureSkipVerify: true, // we pin the leaf fingerprint instead of a CA chain
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("peering: peer presented no certificate")
			}
			got := []byte(CertFingerprint(rawCerts[0]))
			if subtle.ConstantTimeCompare(got, want) != 1 {
				return fmt.Errorf("peering: peer cert fingerprint mismatch — expected the pinned cert from the invite (possible MITM); got %s", CertFingerprint(rawCerts[0]))
			}
			return nil
		},
	}
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}
