package peering

import (
	"fmt"
	"strings"
	"time"
)

// nowRFC3339 is overridable in tests; production stamps wall-clock.
var nowRFC3339 = func() string { return time.Now().UTC().Format(time.RFC3339) }
var nowUnix = func() int64 { return time.Now().Unix() }

// CreateInvite mints a one-time, short-lived invite on the SERVING side: it
// records a peer slot (Dial:false, all-deny share, Token = a fresh secret,
// bounded by InviteExpiry) and returns the code the joiner pastes. selfName is
// how this instance identifies itself; selfURL is where the joiner will dial;
// fingerprint pins our TLS cert (empty under overlay, TEN-185); ttl bounds the
// code. Re-inviting an existing name re-mints (rotates the slot's secret).
func (s *Store) CreateInvite(selfName, selfInstanceID, selfURL, fingerprint string, ttl time.Duration, peerName string) (string, error) {
	if peerName = strings.TrimSpace(peerName); peerName == "" {
		return "", fmt.Errorf("peering: a peer name is required")
	}
	secret, err := newSecret()
	if err != nil {
		return "", err
	}
	exp := nowUnix() + int64(ttl.Seconds())

	s.mu.Lock()
	p := s.peers[peerName]
	if p == nil {
		p = &Peer{Name: peerName, CreatedAt: nowRFC3339()}
	}
	p.Dial = false       // serving side: we accept, we don't dial
	p.URL = ""           // no reverse dial
	p.Token = secret     // the bearer we'll accept from this peer
	p.PendingToken = ""  // a fresh invite supersedes any in-flight rotation
	p.InviteExpiry = exp // unused-invite bound; cleared on first auth (MarkAuthenticated)
	s.peers[peerName] = p
	err = s.save()
	s.mu.Unlock()
	if err != nil {
		return "", err
	}

	return Invite{
		Name:        selfName,
		URL:         selfURL,
		Secret:      secret,
		InstanceID:  selfInstanceID,
		Fingerprint: fingerprint,
		Expiry:      exp,
	}.Encode()
}

// AcceptInvite is the DIALING side of pairing: parse a code and record the peer
// we will dial (Dial:true, holding the secret to present). localName overrides
// the stored name when non-empty (operator can label the peer locally);
// otherwise the inviter's self-name is used.
func (s *Store) AcceptInvite(code, localName string) (*Peer, error) {
	iv, err := ParseInvite(code)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(localName)
	if name == "" {
		name = iv.Name
	}
	p := &Peer{
		Name:        name,
		InstanceID:  iv.InstanceID,
		URL:         iv.URL,
		Dial:        true,
		Token:       iv.Secret,
		Fingerprint: iv.Fingerprint,
		CreatedAt:   nowRFC3339(),
	}
	if err := s.Put(p); err != nil {
		return nil, err
	}
	cp := *p
	return &cp, nil
}

// MarkAuthenticated clears a peer's unused-invite expiry the first time it
// successfully authenticates (the invite is now "used"). No-op if unknown.
func (s *Store) MarkAuthenticated(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.peers[name]
	if !ok || p.InviteExpiry == 0 {
		return nil
	}
	p.InviteExpiry = 0
	return s.save()
}

// Revoke immediately invalidates a peer's bearer(s) so the listener's verifier
// rejects the next call (the record is kept so the operator still sees the
// peer; use Remove to delete it entirely). Returns false if unknown.
func (s *Store) Revoke(name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.peers[name]
	if !ok {
		return false, nil
	}
	p.Token = ""
	p.PendingToken = ""
	return true, s.save()
}

// Rotate stages a NEW secret on the serving side WITHOUT invalidating the old
// one (staged-pull): both Token (old) and PendingToken (new) verify during the
// window. The dialing peer fetches the new secret on its next authenticated
// request; ConfirmRotation promotes it only once the peer presents it — so
// there is never a zero-valid-token window (the hub has no push channel). The
// new secret is returned so the serving side can hand it to the dialing peer
// out of band / over the authenticated channel.
func (s *Store) Rotate(name string) (newSecretValue string, err error) {
	secret, err := newSecret()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.peers[name]
	if !ok {
		return "", fmt.Errorf("peering: no peer named %q", name)
	}
	if p.Token == "" {
		return "", fmt.Errorf("peering: peer %q has no active token to rotate (revoked?)", name)
	}
	// Refuse to clobber an un-adopted staged token: a second Rotate would
	// discard the first pending secret, stranding a peer that already adopted
	// it. The operator must let the peer adopt (ConfirmRotation) or Revoke
	// first. Keeps the staged-pull machine single-in-flight by construction.
	if p.PendingToken != "" {
		return "", fmt.Errorf("peering: a rotation is already staged for %q — wait for the peer to adopt it, or `tenant peer revoke %s` to cancel", name, name)
	}
	p.PendingToken = secret
	if err := s.save(); err != nil {
		return "", err
	}
	return secret, nil
}

// ConfirmRotation promotes a staged PendingToken to the active Token and clears
// the old one — called when the dialing peer has presented the new secret, so
// the old token can finally be retired. No-op (nil) if nothing is staged.
func (s *Store) ConfirmRotation(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.peers[name]
	if !ok || p.PendingToken == "" {
		return nil
	}
	p.Token = p.PendingToken
	p.PendingToken = ""
	return s.save()
}

// AdoptRotatedToken is the DIALING side's counterpart: replace the secret we
// present with the freshly-fetched one. No-op if unknown.
func (s *Store) AdoptRotatedToken(name, secret string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.peers[name]
	if !ok {
		return nil
	}
	p.Token = secret
	return s.save()
}

// SetShare flips one capability flag in a peer's share policy. key ∈
// {wiki,memory,skills,exec,llm}. Unknown key or peer → error.
func (s *Store) SetShare(name, key string, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.peers[name]
	if !ok {
		return fmt.Errorf("peering: no peer named %q", name)
	}
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "wiki":
		p.Share.Wiki = on
	case "memory":
		p.Share.Memory = on
	case "skills":
		p.Share.Skills = on
	case "exec":
		p.Share.Exec = on
	case "llm":
		p.Share.LLM = on
	default:
		return fmt.Errorf("peering: unknown share capability %q (want wiki|memory|skills|exec|llm)", key)
	}
	return s.save()
}
