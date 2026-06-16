package peering

import (
	"context"
	"crypto/tls"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestLoadOrMintCert_StableAndPerms(t *testing.T) {
	dir := t.TempDir()
	cert1, fp1, err := LoadOrMintCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fp1 == "" || len(cert1.Certificate) == 0 {
		t.Fatal("minted cert/fingerprint empty")
	}
	// 0600 perms on both files.
	for _, f := range []string{peerCertFile, peerKeyFile} {
		info, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatalf("stat %s: %v", f, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s perms = %o, want 600", f, info.Mode().Perm())
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
	dir := t.TempDir()
	cert, fp, _ := LoadOrMintCert(dir)

	// Matching fingerprint → VerifyPeerCertificate passes.
	good := PinnedTLSClientConfig(fp)
	if err := good.VerifyPeerCertificate(cert.Certificate, nil); err != nil {
		t.Errorf("matching cert should verify: %v", err)
	}
	// Mismatched fingerprint → rejected (MITM defense).
	_, otherFP, _ := LoadOrMintCert(t.TempDir())
	bad := PinnedTLSClientConfig(otherFP)
	if err := bad.VerifyPeerCertificate(cert.Certificate, nil); err == nil {
		t.Error("a cert that doesn't match the pinned fingerprint must be rejected")
	}
	// No cert presented → rejected.
	if err := good.VerifyPeerCertificate(nil, nil); err == nil {
		t.Error("missing cert must be rejected")
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
	if !l.Secure() {
		t.Fatal("listener with a cert must report Secure()")
	}
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
