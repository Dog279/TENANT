// Package cnsa is the one transport profile every Fleet and Peer connection
// has to meet: the CNSA 2.0 suite, as TLS 1.3 carries it.
//
//	TLS 1.3 and nothing older
//	TLS_AES_256_GCM_SHA384     AES-256 (FIPS 197), SHA-384 (FIPS 180-4)
//	ML-KEM-1024 key exchange   FIPS 203
//	ML-DSA-87 certificates     FIPS 204
//	SHA-384 certificate pins
//
// Apply asks for the profile; Check and CheckLeaf refuse a connection that
// did not get it. Both ends call Check, and each end that is shown a
// certificate calls CheckLeaf on it, so a connection that negotiated
// anything else is closed in the handshake, whichever end was at fault.
//
// # The cipher suite
//
// crypto/tls has no setting for the TLS 1.3 cipher suite: it offers and
// prefers AES-128. This package narrows the two lists crypto/tls reads to
// the one suite, through the linkname the standard library keeps for them.
//
// ponytail: that is not public API, it holds for every TLS 1.3 connection
// this process makes (a server that has no AES-256 suite is not reached over
// TLS 1.3), and Go's FIPS 140-3 mode reads a list of its own, so there the
// suite is AES-128 and Check refuses the connection. Replace it with a
// crypto/tls setting when there is one; TestSuite fails the day it stops
// working.
package cnsa

import (
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	_ "unsafe" // for linkname
)

//go:linkname defaultCipherSuitesTLS13 crypto/tls.defaultCipherSuitesTLS13
var defaultCipherSuitesTLS13 []uint16

//go:linkname defaultCipherSuitesTLS13NoAES crypto/tls.defaultCipherSuitesTLS13NoAES
var defaultCipherSuitesTLS13NoAES []uint16

func init() {
	only := []uint16{tls.TLS_AES_256_GCM_SHA384}
	defaultCipherSuitesTLS13, defaultCipherSuitesTLS13NoAES = only, only
}

// FingerprintLen is the length of a Fingerprint: SHA-384 in lowercase hex.
const FingerprintLen = 2 * sha512.Size384

// Apply sets cfg to ask for the profile: TLS 1.3 and ML-KEM-1024.
func Apply(cfg *tls.Config) {
	cfg.MinVersion = tls.VersionTLS13
	cfg.CurvePreferences = []tls.CurveID{tls.MLKEM1024}
}

// Suite refuses a connection that is not TLS 1.3 with AES-256-GCM and
// SHA-384.
func Suite(cs tls.ConnectionState) error {
	if cs.Version != tls.VersionTLS13 {
		return fmt.Errorf("cnsa: the connection is %s, not TLS 1.3", tls.VersionName(cs.Version))
	}
	if cs.CipherSuite != tls.TLS_AES_256_GCM_SHA384 {
		return fmt.Errorf("cnsa: the connection uses %s, not TLS_AES_256_GCM_SHA384", tls.CipherSuiteName(cs.CipherSuite))
	}
	return nil
}

// Check refuses a connection that did not negotiate the profile. Call it
// from tls.Config.VerifyConnection, on both ends.
func Check(cs tls.ConnectionState) error {
	if err := Suite(cs); err != nil {
		return err
	}
	if cs.CurveID != tls.MLKEM1024 {
		return fmt.Errorf("cnsa: the key exchange is %v, not ML-KEM-1024", cs.CurveID)
	}
	return nil
}

// CheckLeaf refuses a certificate that is not an ML-DSA-87 key signed with
// ML-DSA-87.
func CheckLeaf(c *x509.Certificate) error {
	pub, ok := c.PublicKey.(*mldsa.PublicKey)
	if !ok || pub.Parameters() != mldsa.MLDSA87() || c.SignatureAlgorithm != x509.MLDSA87 {
		return fmt.Errorf("cnsa: the certificate is %v signed with %v, not ML-DSA-87", c.PublicKeyAlgorithm, c.SignatureAlgorithm)
	}
	return nil
}

// Fingerprint is the lowercase hex SHA-384 of a DER certificate: what the
// other side pins.
func Fingerprint(der []byte) string {
	sum := sha512.Sum384(der)
	return hex.EncodeToString(sum[:])
}

// SelfSigned makes a new ML-DSA-87 key and signs tmpl with it. Both come
// back as PEM, the key as PKCS #8.
func SelfSigned(tmpl *x509.Certificate) (certPEM, keyPEM []byte, err error) {
	priv, err := mldsa.GenerateKey(mldsa.MLDSA87())
	if err != nil {
		return nil, nil, fmt.Errorf("cnsa: generate key: %w", err)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, priv.Public(), priv)
	if err != nil {
		return nil, nil, fmt.Errorf("cnsa: create certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("cnsa: encode key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}
