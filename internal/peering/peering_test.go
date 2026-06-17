package peering

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestNewInstanceID(t *testing.T) {
	a, err := NewInstanceID()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewInstanceID()
	if a == b {
		t.Error("instance IDs must be unique")
	}
	// UUIDv4 shape: 8-4-4-4-12, version nibble 4, variant 8|9|a|b.
	parts := strings.Split(a, "-")
	if len(parts) != 5 || len(parts[0]) != 8 || len(parts[2]) != 4 || parts[2][0] != '4' {
		t.Errorf("not a v4 UUID: %q", a)
	}
}

func TestInviteCodec(t *testing.T) {
	iv := Invite{Name: "hub", URL: "https://h:9100", Secret: "s3cr3t", InstanceID: "id-1", Fingerprint: "fp", Expiry: nowUnix() + 3600}
	code, err := iv.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(code, invitePrefix) {
		t.Errorf("missing prefix: %q", code)
	}
	got, err := ParseInvite(code)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "hub" || got.URL != "https://h:9100" || got.Secret != "s3cr3t" || got.InstanceID != "id-1" || got.Fingerprint != "fp" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}

	// Rejections.
	if _, err := ParseInvite("garbage"); err == nil {
		t.Error("non-prefixed code should be rejected")
	}
	if _, err := ParseInvite(invitePrefix + "!!!notbase64!!!"); err == nil {
		t.Error("corrupt base64 should be rejected")
	}
	expired := Invite{Name: "h", URL: "u", Secret: "s", InstanceID: "i", Expiry: nowUnix() - 10}
	ec, _ := expired.Encode()
	if _, err := ParseInvite(ec); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("expired code should be rejected, got %v", err)
	}
	missing := Invite{Name: "h", Expiry: nowUnix() + 99} // no URL/Secret/InstanceID
	mc, _ := missing.Encode()
	if _, err := ParseInvite(mc); err == nil {
		t.Error("invite missing required fields should be rejected")
	}
}

func TestStore_PersistAndPerms(t *testing.T) {
	dir := t.TempDir()
	s, _ := LoadStore(dir)
	if err := s.Put(&Peer{Name: "edge", InstanceID: "i-edge", Token: "tok", Dial: true, URL: "https://edge:9100"}); err != nil {
		t.Fatal(err)
	}
	// 0600 perms on peers.json.
	info, err := os.Stat(PeersPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("peers.json perms = %o, want 600", info.Mode().Perm())
	}
	// Reload sees it.
	s2, _ := LoadStore(dir)
	p, ok := s2.Get("edge")
	if !ok || p.InstanceID != "i-edge" || p.Token != "tok" || !p.Dial {
		t.Fatalf("reload mismatch: %+v ok=%v", p, ok)
	}
	if got := s2.List(); len(got) != 1 {
		t.Errorf("List len = %d, want 1", len(got))
	}
	if err := s2.Remove("edge"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Get("edge"); ok {
		t.Error("Remove did not delete")
	}
}

// TestPairing exercises the two-sided invite/accept flow and that the serving
// side accepts exactly the secret the joiner holds.
func TestPairing(t *testing.T) {
	hubDir, spokeDir := t.TempDir(), t.TempDir()
	hub, _ := LoadStore(hubDir)
	spoke, _ := LoadStore(spokeDir)

	code, err := hub.CreateInvite("hub", "hub-instance", "https://hub:9100", "fp-hub", time.Hour, "spoke")
	if err != nil {
		t.Fatal(err)
	}
	// Serving side recorded an accept slot, not a dial record.
	hp, _ := hub.Get("spoke")
	if hp.Dial || hp.URL != "" || hp.Token == "" {
		t.Fatalf("hub slot wrong: %+v", hp)
	}

	// Joiner accepts.
	got, err := spoke.AcceptInvite(code, "")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Dial || got.URL != "https://hub:9100" || got.InstanceID != "hub-instance" || got.Fingerprint != "fp-hub" {
		t.Fatalf("spoke record wrong: %+v", got)
	}
	// The secret the joiner holds is exactly what the hub accepts.
	if got.Token != hp.Token {
		t.Fatal("joiner token != hub-accepted token")
	}
	if _, _, ok := hub.VerifyToken(got.Token); !ok {
		t.Error("hub should accept the joiner's token")
	}
	if _, _, ok := hub.VerifyToken("wrong"); ok {
		t.Error("hub must reject a wrong token")
	}
	if _, _, ok := hub.VerifyToken(""); ok {
		t.Error("empty token must never match")
	}
}

func TestInviteExpiryGatesVerify(t *testing.T) {
	dir := t.TempDir()
	hub, _ := LoadStore(dir)
	// An invite that's already expired must not authenticate, even with the
	// right secret (unused-invite bound).
	code, _ := hub.CreateInvite("hub", "i", "https://h", "", -1*time.Second, "spoke")
	iv, _ := ParseInvite(code) // ParseInvite itself rejects expiry; read the secret via the store instead
	_ = iv
	p, _ := hub.Get("spoke")
	if _, _, ok := hub.VerifyToken(p.Token); ok {
		t.Error("expired unused invite must not verify")
	}
	// After re-inviting with a live TTL it verifies; MarkAuthenticated clears the bound.
	hub.CreateInvite("hub", "i", "https://h", "", time.Hour, "spoke")
	p, _ = hub.Get("spoke")
	if _, _, ok := hub.VerifyToken(p.Token); !ok {
		t.Fatal("live invite should verify")
	}
	if err := hub.MarkAuthenticated("spoke"); err != nil {
		t.Fatal(err)
	}
	if p, _ := hub.Get("spoke"); p.InviteExpiry != 0 {
		t.Error("MarkAuthenticated should clear InviteExpiry")
	}
}

func TestRevoke(t *testing.T) {
	dir := t.TempDir()
	hub, _ := LoadStore(dir)
	hub.CreateInvite("hub", "i", "https://h", "", time.Hour, "spoke")
	hub.MarkAuthenticated("spoke")
	p, _ := hub.Get("spoke")
	tok := p.Token
	if _, _, ok := hub.VerifyToken(tok); !ok {
		t.Fatal("precondition: token should verify")
	}
	ok, err := hub.Revoke("spoke")
	if err != nil || !ok {
		t.Fatalf("revoke failed: ok=%v err=%v", ok, err)
	}
	if _, _, ok := hub.VerifyToken(tok); ok {
		t.Error("revoked token must not verify")
	}
}

// TestRotateStagedPull is the load-bearing security test: rotation must never
// leave a zero-valid-token window, and must survive a crash before confirm.
func TestRotateStagedPull(t *testing.T) {
	dir := t.TempDir()
	hub, _ := LoadStore(dir)
	hub.CreateInvite("hub", "i", "https://h", "", time.Hour, "spoke")
	hub.MarkAuthenticated("spoke")
	old := mustToken(t, hub, "spoke")

	newSec, err := hub.Rotate("spoke")
	if err != nil {
		t.Fatal(err)
	}
	if newSec == old {
		t.Fatal("rotate must mint a different secret")
	}
	// During the window BOTH verify; the new one is flagged as pending.
	if _, pending, ok := hub.VerifyToken(old); !ok || pending {
		t.Errorf("old token should still verify (not pending) during rotation: ok=%v pending=%v", ok, pending)
	}
	if _, pending, ok := hub.VerifyToken(newSec); !ok || !pending {
		t.Errorf("new token should verify AND report pending: ok=%v pending=%v", ok, pending)
	}

	// Crash-window: reload from disk before confirming — both still valid (no gap).
	reloaded, _ := LoadStore(dir)
	if _, _, ok := reloaded.VerifyToken(old); !ok {
		t.Error("after crash before confirm, OLD token must still verify (no zero-valid-token window)")
	}
	if _, _, ok := reloaded.VerifyToken(newSec); !ok {
		t.Error("after crash before confirm, NEW token must still verify")
	}

	// Confirm promotes new, retires old.
	if err := hub.ConfirmRotation("spoke"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := hub.VerifyToken(old); ok {
		t.Error("after confirm, OLD token must stop verifying")
	}
	if _, pending, ok := hub.VerifyToken(newSec); !ok || pending {
		t.Errorf("after confirm, NEW token is the active token: ok=%v pending=%v", ok, pending)
	}

	// Rotate on a revoked peer is refused (no token to rotate).
	hub.Revoke("spoke")
	if _, err := hub.Rotate("spoke"); err == nil {
		t.Error("rotate on a revoked peer should error")
	}
}

// TestDoubleRotateRefused guards the staged-pull machine: a second Rotate
// before the first is adopted must NOT silently discard the first pending
// secret (which would strand a peer that already adopted it).
func TestDoubleRotateRefused(t *testing.T) {
	dir := t.TempDir()
	hub, _ := LoadStore(dir)
	hub.CreateInvite("hub", "i", "https://h", "", time.Hour, "spoke")
	hub.MarkAuthenticated("spoke")

	sec1, err := hub.Rotate("spoke")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Rotate("spoke"); err == nil {
		t.Fatal("a second rotate before adoption must be refused")
	}
	// The first staged secret is intact (not clobbered).
	if _, pending, ok := hub.VerifyToken(sec1); !ok || !pending {
		t.Errorf("first pending secret must survive the refused second rotate: ok=%v pending=%v", ok, pending)
	}
	// After adoption, rotating again is allowed.
	if err := hub.ConfirmRotation("spoke"); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Rotate("spoke"); err != nil {
		t.Errorf("rotate after ConfirmRotation should succeed: %v", err)
	}
}

func TestSetShareDefaultsDeny(t *testing.T) {
	dir := t.TempDir()
	hub, _ := LoadStore(dir)
	hub.CreateInvite("hub", "i", "https://h", "", time.Hour, "spoke")
	p, _ := hub.Get("spoke")
	if p.Share.Wiki || p.Share.Memory || p.Share.Skills || p.Share.Exec || p.Share.LLM {
		t.Fatalf("share policy must default all-deny, got %+v", p.Share)
	}
	if err := hub.SetShare("spoke", "wiki", true); err != nil {
		t.Fatal(err)
	}
	if p, _ := hub.Get("spoke"); !p.Share.Wiki || p.Share.Memory {
		t.Errorf("only wiki should be on: %+v", p.Share)
	}
	if err := hub.SetShare("spoke", "bogus", true); err == nil {
		t.Error("unknown share key should error")
	}
	if err := hub.SetShare("nope", "wiki", true); err == nil {
		t.Error("unknown peer should error")
	}
}

func TestRename(t *testing.T) {
	dir := t.TempDir()
	s, _ := LoadStore(dir)
	s.Put(&Peer{Name: "long-hostname.local", InstanceID: "id1", Token: "tok", Dial: true, Share: SharePolicy{Wiki: true}})

	if err := s.Rename("long-hostname.local", "mac"); err != nil {
		t.Fatal(err)
	}
	// Moved, preserving token + share.
	if _, ok := s.Get("long-hostname.local"); ok {
		t.Error("old name should be gone")
	}
	p, ok := s.Get("mac")
	if !ok || p.Token != "tok" || !p.Share.Wiki || p.InstanceID != "id1" || p.Name != "mac" {
		t.Fatalf("rename should preserve everything under the new name: %+v ok=%v", p, ok)
	}
	// Survives reload.
	if r, _ := LoadStore(dir); func() bool { _, ok := r.Get("mac"); return ok }() == false {
		t.Error("rename should persist")
	}
	// Errors: unknown old, collision on new.
	if err := s.Rename("nope", "x"); err == nil {
		t.Error("renaming an unknown peer should error")
	}
	s.Put(&Peer{Name: "other", InstanceID: "id2"})
	if err := s.Rename("mac", "other"); err == nil {
		t.Error("renaming onto an existing name should error")
	}
}

func mustToken(t *testing.T, s *Store, name string) string {
	t.Helper()
	p, ok := s.Get(name)
	if !ok {
		t.Fatalf("no peer %q", name)
	}
	return p.Token
}
