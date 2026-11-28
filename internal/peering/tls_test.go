package peering

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tenant/internal/secrets"
)

func TestLoadOrMintCert_StableAndProtection(t *testing.T) {
	dir := t.TempDir()
	cert1, fp1, err := LoadOrMintCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fp1 == "" || len(cert1.Certificate) == 0 {
		t.Fatal("minted cert/fingerprint empty")
	}
	for _, path := range []string{filepath.Join(dir, peerCertFile), filepath.Join(dir, peerKeyFile)} {
		if report := secrets.Probe(path); !report.Protected {
			t.Errorf("%s is not protected: %+v", filepath.Base(path), report)
		}
	}
	// Reload is idempotent: same fingerprint across "restarts".
	_, fp2, err := LoadOrMintCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fp1 != fp2 {
		t.Errorf("fingerprint not stable across reload: %s vs %s", fp1, fp2)
	}
	// A different dir mints a DIFFERENT cert.
	_, fp3, _ := LoadOrMintCert(t.TempDir())
	if fp3 == fp1 {
		t.Error("distinct installs must mint distinct certs")
	}
}

func TestPinnedTLSClientConfig(t *testing.T) {
	if PinnedTLSClientConfig("") != nil {
		t.Error("empty fingerprint → nil config (plain HTTP / overlay)")
	}
	cert, fp, _ := LoadOrMintCert(t.TempDir())
	ln, err := tls.Listen("tcp", "127.0.0.1:0", ServerTLSConfig(cert))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.(*tls.Conn).Handshake()
				_, _ = c.Read(make([]byte, 1))
			}()
		}
	}()
	dial := func(cfg *tls.Config) (tls.ConnectionState, error) {
		c, err := tls.Dial("tcp", ln.Addr().String(), cfg)
		if err != nil {
			return tls.ConnectionState{}, err
		}
		defer c.Close()
		return c.ConnectionState(), nil
	}

	// Matching fingerprint → the handshake passes, on the cnsa profile.
	cs, err := dial(PinnedTLSClientConfig(fp))
	if err != nil {
		t.Fatalf("matching cert should verify: %v", err)
	}
	if cs.Version != tls.VersionTLS13 || cs.CipherSuite != tls.TLS_AES_256_GCM_SHA384 || cs.CurveID != tls.MLKEM1024 ||
		cs.PeerCertificates[0].SignatureAlgorithm != x509.MLDSA87 {
		t.Errorf("negotiated %s %s %v, certificate %v", tls.VersionName(cs.Version), tls.CipherSuiteName(cs.CipherSuite),
			cs.CurveID, cs.PeerCertificates[0].SignatureAlgorithm)
	}
	// Mismatched fingerprint → rejected (MITM defense).
	_, otherFP, _ := LoadOrMintCert(t.TempDir())
	if _, err := dial(PinnedTLSClientConfig(otherFP)); err == nil {
		t.Error("a cert that doesn't match the pinned fingerprint must be rejected")
	}
	// A pin from before SHA-384 (64 hex digits) → told to pair again.
	if _, err := dial(PinnedTLSClientConfig(strings.Repeat("ab", 32))); err == nil || !strings.Contains(err.Error(), "pair with it again") {
		t.Errorf("an old SHA-256 pin: %v", err)
	}
	// The listener serves nothing older than TLS 1.3, and no other key exchange.
	old := PinnedTLSClientConfig(fp)
	old.MinVersion, old.MaxVersion = tls.VersionTLS12, tls.VersionTLS12
	if _, err := dial(old); err == nil {
		t.Error("a TLS 1.2 client must be refused")
	}
	hybrid := PinnedTLSClientConfig(fp)
	hybrid.CurvePreferences = []tls.CurveID{tls.X25519MLKEM768, tls.X25519}
	if _, err := dial(hybrid); err == nil {
		t.Error("a client without ML-KEM-1024 must be refused")
	}
}

// A certificate from before the ML-DSA-87 profile is replaced, not served.
func TestLoadOrMintCert_ReplacesOlderCertificate(t *testing.T) {
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	certPath, keyPath := CertPaths(dir)
	if err := secrets.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); err != nil {
		t.Fatal(err)
	}
	if err := secrets.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})); err != nil {
		t.Fatal(err)
	}
	cert, fp, err := LoadOrMintCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(cert.Certificate[0])
	if leaf.SignatureAlgorithm != x509.MLDSA87 || len(fp) != 96 {
		t.Fatalf("still serving %v with pin %q", leaf.SignatureAlgorithm, fp)
	}
	if _, again, _ := LoadOrMintCert(dir); again != fp {
		t.Error("the replacement is not stable across reloads")
	}
}

// TestListener_TLSEndToEnd stands up a REAL TLS peer listener (Bind wraps the
// socket in tls.NewListener) and dials it with a fingerprint-pinned client —
// the full TEN-185 transport path. A client pinning the WRONG fingerprint must
// fail the handshake.
func TestListener_TLSEndToEnd(t *testing.T) {
	dir := t.TempDir()
	store, _ := LoadStore(dir)
	store.CreateInvite("hub", "hub-id", "https://hub", "", time.Hour, "spoke")
	p, _ := store.Get("spoke")
	cert, fp, _ := LoadOrMintCert(dir)

	l, _ := NewListener(ListenerConfig{Store: store, SelfID: "hub-id", SelfVersion: "1.0", TLSCert: &cert})
	ln, err := l.Bind("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Serve(ctx, ln)
	url := "https://" + ln.Addr().String()

	dial := func(pinFP string) (*mcp.ClientSession, error) {
		httpClient := &http.Client{Transport: &http.Transport{
			TLSClientConfig: PinnedTLSClientConfig(pinFP),
			// inject the bearer too
			Proxy: nil,
		}}
		// wrap with bearer
		httpClient.Transport = bearerOverTLS{token: p.Token, tlsCfg: PinnedTLSClientConfig(pinFP)}
		transport := &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: httpClient, DisableStandaloneSSE: true}
		client := mcp.NewClient(&mcp.Implementation{Name: "tenant", Version: "test"}, nil)
		dctx, dcancel := context.WithTimeout(ctx, 10*time.Second)
		defer dcancel()
		return client.Connect(dctx, transport, nil)
	}

	// Correct pin → connects + handshake.
	sess, err := dial(fp)
	if err != nil {
		t.Fatalf("pinned-correct TLS dial failed: %v", err)
	}
	cctx, ccancel := context.WithTimeout(ctx, 10*time.Second)
	defer ccancel()
	if _, err := sess.CallTool(cctx, &mcp.CallToolParams{Name: "peer_hello"}); err != nil {
		t.Fatalf("peer_hello over TLS: %v", err)
	}
	sess.Close()

	// Wrong pin → TLS handshake refused (MITM defense).
	_, otherFP, _ := LoadOrMintCert(t.TempDir())
	if bad, err := dial(otherFP); err == nil {
		bad.Close()
		t.Error("a client pinning the WRONG fingerprint must fail the TLS handshake")
	}
}

// bearerOverTLS injects the bearer header AND uses the pinned TLS config.
type bearerOverTLS struct {
	token  string
	tlsCfg *tls.Config
}

func (b bearerOverTLS) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return (&http.Transport{TLSClientConfig: b.tlsCfg}).RoundTrip(r)
}
