package cnsa

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"strings"
	"testing"
	"time"
)

func mint(t *testing.T) tls.Certificate {
	t.Helper()
	certPEM, keyPEM, err := SelfSigned(&x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "cnsa-test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// dial makes one connection between srv and cli and returns what the client
// saw, or the client's handshake error.
func dial(t *testing.T, srv, cli *tls.Config) (tls.ConnectionState, error) {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srv)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.(*tls.Conn).Handshake()
		_, _ = c.Read(make([]byte, 1))
	}()
	conn, err := tls.Dial("tcp", ln.Addr().String(), cli)
	if err != nil {
		return tls.ConnectionState{}, err
	}
	defer conn.Close()
	return conn.ConnectionState(), nil
}

func pair(t *testing.T) (srv, cli *tls.Config) {
	check := func(cs tls.ConnectionState) error {
		if err := Check(cs); err != nil {
			return err
		}
		return CheckLeaf(cs.PeerCertificates[0])
	}
	srv = &tls.Config{Certificates: []tls.Certificate{mint(t)}, ClientAuth: tls.RequireAnyClientCert, VerifyConnection: check}
	cli = &tls.Config{Certificates: []tls.Certificate{mint(t)}, InsecureSkipVerify: true, VerifyConnection: check}
	Apply(srv)
	Apply(cli)
	return srv, cli
}

func TestProfile(t *testing.T) {
	srv, cli := pair(t)
	cs, err := dial(t, srv, cli)
	if err != nil {
		t.Fatal(err)
	}
	if cs.Version != tls.VersionTLS13 || cs.CipherSuite != tls.TLS_AES_256_GCM_SHA384 || cs.CurveID != tls.MLKEM1024 {
		t.Fatalf("negotiated %s %s %v", tls.VersionName(cs.Version), tls.CipherSuiteName(cs.CipherSuite), cs.CurveID)
	}
	if leaf := cs.PeerCertificates[0]; leaf.SignatureAlgorithm != x509.MLDSA87 {
		t.Fatalf("the certificate is signed with %v", leaf.SignatureAlgorithm)
	}
	if fp := Fingerprint(cs.PeerCertificates[0].Raw); len(fp) != FingerprintLen || strings.ToLower(fp) != fp {
		t.Fatalf("fingerprint %q", fp)
	}
}

// TestSuite is the alarm on the linkname in cnsa.go: two ends that set
// nothing but TLS 1.3 still get AES-256, because the process offers nothing
// else.
func TestSuite(t *testing.T) {
	srv := &tls.Config{Certificates: []tls.Certificate{mint(t)}, MinVersion: tls.VersionTLS13}
	cs, err := dial(t, srv, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	if cs.CipherSuite != tls.TLS_AES_256_GCM_SHA384 {
		t.Fatalf("the process negotiated %s: the cipher suite list in cnsa.go no longer reaches crypto/tls", tls.CipherSuiteName(cs.CipherSuite))
	}
}

func TestRefused(t *testing.T) {
	// A server that takes any key exchange: the client's Check refuses what
	// it was given.
	srv, cli := pair(t)
	srv.CurvePreferences, srv.VerifyConnection = []tls.CurveID{tls.SecP384r1MLKEM1024}, nil
	cli.CurvePreferences = nil
	if _, err := dial(t, srv, cli); err == nil || !strings.Contains(err.Error(), "ML-KEM-1024") {
		t.Fatalf("a hybrid key exchange: %v", err)
	}
	// TLS 1.2 is not offered at all.
	srv, cli = pair(t)
	cli.MinVersion, cli.MaxVersion = tls.VersionTLS12, tls.VersionTLS12
	if _, err := dial(t, srv, cli); err == nil {
		t.Fatal("a TLS 1.2 client was served")
	}
	// A certificate of another kind.
	if err := CheckLeaf(&x509.Certificate{PublicKeyAlgorithm: x509.ECDSA, SignatureAlgorithm: x509.ECDSAWithSHA384}); err == nil {
		t.Fatal("an ECDSA certificate passed")
	}
}
