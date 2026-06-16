package peering

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// pairListener builds a listener whose /pair approves or denies per `approve`,
// recording the prompt it was shown.
func pairListener(t *testing.T, dir string, approve bool, gotPrompt *string) *Listener {
	t.Helper()
	store, _ := LoadStore(dir)
	l, err := NewListener(ListenerConfig{
		Store: store, SelfID: "hub-id", SelfName: "hub", SelfVersion: "test",
		PairApprover: func(_ context.Context, prompt string) bool {
			if gotPrompt != nil {
				*gotPrompt = prompt
			}
			return approve
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestPair_ApproveFlow(t *testing.T) {
	dir := t.TempDir()
	var prompt string
	l := pairListener(t, dir, true, &prompt)
	srv := httptest.NewServer(l.Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pr, err := RequestPair(ctx, srv.URL, PairRequest{Name: "laptop", InstanceID: "laptop-id", PIN: "418207"}, true /*overlay/plain http*/)
	if err != nil {
		t.Fatalf("approve flow: %v", err)
	}
	if pr.Token == "" || pr.Name != "hub" || pr.InstanceID != "hub-id" {
		t.Fatalf("bad pair response: %+v", pr)
	}
	// The prompt shown to the operator carries the PIN (formatted) + the name.
	if !strings.Contains(prompt, "418 207") || !strings.Contains(prompt, "laptop") {
		t.Errorf("approval prompt should carry the PIN + name: %q", prompt)
	}
	// The accepter now holds the inviter as an accept-side peer that presents pr.Token.
	store, _ := LoadStore(dir)
	p, ok := store.Get("laptop")
	if !ok || p.Dial || p.Token != pr.Token {
		t.Fatalf("inviter not stored as accept peer: %+v ok=%v", p, ok)
	}
	if _, _, ok := store.VerifyToken(pr.Token); !ok {
		t.Error("the minted token should authenticate the peer")
	}
	// All-deny share by default.
	if p.Share.Wiki || p.Share.Memory || p.Share.Skills || p.Share.Exec || p.Share.LLM {
		t.Errorf("new peer must start all-deny: %+v", p.Share)
	}
}

func TestPair_Deny(t *testing.T) {
	l := pairListener(t, t.TempDir(), false, nil)
	srv := httptest.NewServer(l.Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := RequestPair(ctx, srv.URL, PairRequest{Name: "x", InstanceID: "x", PIN: "123456"}, true); err == nil || !strings.Contains(err.Error(), "DENIED") {
		t.Errorf("deny should surface a DENIED error, got %v", err)
	}
}

func TestPair_Validation(t *testing.T) {
	l := pairListener(t, t.TempDir(), true, nil)
	srv := httptest.NewServer(l.Handler())
	defer srv.Close()
	ctx := context.Background()
	// Non-6-digit PIN → rejected before any approval.
	if _, err := RequestPair(ctx, srv.URL, PairRequest{Name: "x", InstanceID: "x", PIN: "12"}, true); err == nil {
		t.Error("short PIN must be rejected")
	}
	// Missing name → rejected.
	if _, err := RequestPair(ctx, srv.URL, PairRequest{Name: "", InstanceID: "x", PIN: "123456"}, true); err == nil {
		t.Error("missing name must be rejected")
	}
}

func TestPair_NoApprover503(t *testing.T) {
	store, _ := LoadStore(t.TempDir())
	l, _ := NewListener(ListenerConfig{Store: store, SelfID: "h", SelfName: "h"}) // no PairApprover
	srv := httptest.NewServer(l.Handler())
	defer srv.Close()
	if _, err := RequestPair(context.Background(), srv.URL, PairRequest{Name: "x", InstanceID: "x", PIN: "123456"}, true); err == nil {
		t.Error("pairing with no approver wired must fail (503)")
	}
}

func TestPair_RateLimit(t *testing.T) {
	l := &pairLimiter{max: 3}
	// One pending per source key, AND a global cap.
	if !l.acquire("a") || !l.acquire("b") {
		t.Fatal("distinct sources should each get a slot")
	}
	if l.acquire("a") {
		t.Error("a second concurrent request from the SAME source must be refused (no monopoly)")
	}
	if !l.acquire("c") {
		t.Error("a third distinct source within the global cap should succeed")
	}
	if l.acquire("d") {
		t.Error("over the global cap must fail")
	}
	l.release("a")
	if !l.acquire("a") {
		t.Error("after release the source can pair again")
	}
}

// TestPair_NameCollisionNoClobber: an approved pairing whose name collides with
// an EXISTING DIFFERENT peer must not overwrite it — it's filed under a unique name.
func TestPair_NameCollisionNoClobber(t *testing.T) {
	dir := t.TempDir()
	store, _ := LoadStore(dir)
	// Pre-existing trusted peer "laptop" (a DIFFERENT instance).
	store.Put(&Peer{Name: "laptop", InstanceID: "trusted-id", Token: "trusted-token", Dial: true})

	l := pairListener(t, dir, true, nil) // shares dir, but its own store; reload picks up the pre-existing peer
	srv := httptest.NewServer(l.Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := RequestPair(ctx, srv.URL, PairRequest{Name: "laptop", InstanceID: "attacker-id", PIN: "123456"}, true); err != nil {
		t.Fatal(err)
	}
	reloaded, _ := LoadStore(dir)
	// The original trusted peer is intact.
	if p, ok := reloaded.Get("laptop"); !ok || p.InstanceID != "trusted-id" || p.Token != "trusted-token" {
		t.Errorf("the pre-existing trusted peer must NOT be overwritten: %+v ok=%v", p, ok)
	}
	// The new peer landed under a uniquified name.
	if _, ok := reloaded.Get("laptop-2"); !ok {
		t.Error("the colliding new peer should be filed under laptop-2")
	}
}

func TestGeneratePIN(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		pin, err := GeneratePIN()
		if err != nil {
			t.Fatal(err)
		}
		if !isSixDigits(pin) {
			t.Fatalf("PIN must be 6 digits: %q", pin)
		}
		seen[pin] = true
	}
	if len(seen) < 40 {
		t.Errorf("PINs should be well-distributed, got %d distinct of 50", len(seen))
	}
}

// TestPair_TLSFingerprintCrossCheck runs the real TLS path: the inviter captures
// the served cert and cross-checks it against the fingerprint the peer reports.
func TestPair_TLSFingerprintCrossCheck(t *testing.T) {
	dir := t.TempDir()
	store, _ := LoadStore(dir)
	cert, fp, _ := LoadOrMintCert(dir)
	l, _ := NewListener(ListenerConfig{
		Store: store, SelfID: "hub-id", SelfName: "hub", SelfFinger: fp, TLSCert: &cert,
		PairApprover: func(context.Context, string) bool { return true },
	})
	ln, err := l.Bind("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Serve(ctx, ln)

	url := "https://" + ln.Addr().String()
	pr, err := RequestPair(ctx, url, PairRequest{Name: "laptop", InstanceID: "laptop-id", PIN: "111111"}, false /*TLS*/)
	if err != nil {
		t.Fatalf("TLS pair: %v", err)
	}
	if pr.Fingerprint != fp {
		t.Errorf("peer should report its real cert fingerprint: got %s want %s", pr.Fingerprint, fp)
	}
}
