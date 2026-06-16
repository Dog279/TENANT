package peering

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// SharePolicy is the per-peer capability ACL. ALL fields default false
// (all-deny): a paired peer can reach the listener but is granted nothing by
// capability until the operator opts a flag in. Reads (wiki/memory) are gated
// here; exec/llm only make the capability ELIGIBLE for the approval queue —
// they never bypass the confirm broker (Premise 1: reachability ≠ capability).
type SharePolicy struct {
	Wiki   bool `json:"wiki"`
	Memory bool `json:"memory"`
	Skills bool `json:"skills"`
	Exec   bool `json:"exec"`
	LLM    bool `json:"llm"`
}

// Peer is one paired peer's record. The same pairing secret is the bearer the
// dialing side presents and the serving side accepts; only the dialing side
// records a URL (joiner-dials-inviter topology — a reverse token would be dead
// surface, eng review 8A).
type Peer struct {
	Name       string `json:"name"`
	InstanceID string `json:"instance_id"`     // the peer's stable identity
	URL        string `json:"url,omitempty"`   // set on the dialing side: where we reach them
	Dial       bool   `json:"dial"`            // true ⇒ we dial them (we present Token)
	Token      string `json:"token,omitempty"` // active bearer (present / accept)
	// PendingToken is a freshly-rotated secret staged on the serving side. Both
	// Token and PendingToken are accepted during the rotation window; when the
	// dialing peer presents PendingToken, ConfirmRotation promotes it and clears
	// the old one — so rotation never leaves a zero-valid-token gap (staged-pull;
	// the hub has no push channel to the spoke).
	PendingToken string `json:"pending_token,omitempty"`
	// Fingerprint pins the peer's self-signed TLS cert (TOFU-by-invite, TEN-185).
	// Empty under overlay transport (Tailscale/WireGuard).
	Fingerprint string      `json:"fingerprint,omitempty"`
	Share       SharePolicy `json:"share"`
	// InviteExpiry (unix seconds) bounds an as-yet-unused inbound pairing on the
	// serving side; 0 once the peer has authenticated at least once.
	InviteExpiry int64  `json:"invite_expiry,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
}

// Store is the single authoritative per-peer state, persisted to
// <cfgDir>/peers.json (0600), mirroring the mcpremote token-cache file pattern.
// All mutators are mutex-guarded; Save writes atomically (temp + rename).
type Store struct {
	mu    sync.Mutex
	path  string
	peers map[string]*Peer
}

// PeersPath returns the canonical peers.json location under cfgDir.
func PeersPath(cfgDir string) string { return filepath.Join(cfgDir, "peers.json") }

// LoadStore reads peers.json (creating an empty in-memory store if absent).
func LoadStore(cfgDir string) (*Store, error) {
	s := &Store{path: PeersPath(cfgDir), peers: map[string]*Peer{}}
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("peering: read peers.json: %w", err)
	}
	var list []*Peer
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("peering: parse peers.json: %w", err)
	}
	for _, p := range list {
		if p != nil && p.Name != "" {
			s.peers[p.Name] = p
		}
	}
	return s, nil
}

// save persists the store atomically at 0600. Caller holds s.mu.
func (s *Store) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("peering: mkdir: %w", err)
	}
	list := make([]*Peer, 0, len(s.peers))
	for _, p := range s.peers {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return fmt.Errorf("peering: marshal peers.json: %w", err)
	}
	return atomicWriteFile(s.path, b, 0o600)
}

// Save flushes the store to disk (0600, atomic).
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.save()
}

// Get returns a COPY of the peer by name (callers must not mutate the store
// through the returned pointer; use Put to persist changes).
func (s *Store) Get(name string) (*Peer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.peers[name]
	if !ok {
		return nil, false
	}
	cp := *p
	return &cp, true
}

// List returns copies of all peers, name-sorted.
func (s *Store) List() []*Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Peer, 0, len(s.peers))
	for _, p := range s.peers {
		cp := *p
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Put inserts/updates a peer and persists.
func (s *Store) Put(p *Peer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *p
	s.peers[p.Name] = &cp
	return s.save()
}

// Remove deletes a peer and persists. Removing an unknown name is a no-op.
func (s *Store) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.peers[name]; !ok {
		return nil
	}
	delete(s.peers, name)
	return s.save()
}

// VerifyToken looks up the peer that presents this bearer, in constant time
// per candidate (crypto/subtle), accepting both the active Token and a staged
// PendingToken (rotation window). An expired, still-unused invite (InviteExpiry
// in the past) is rejected. Empty tokens never match. This is the TokenVerifier
// the TEN-184 listener wraps in auth.RequireBearerToken.
//
// matchedPending reports whether the presented bearer was the staged
// PendingToken — the listener uses that to ConfirmRotation on a successful call.
func (s *Store) VerifyToken(token string) (peer *Peer, matchedPending bool, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if token == "" {
		return nil, false, false
	}
	now := time.Now().Unix()
	for _, p := range s.peers {
		if p.InviteExpiry != 0 && p.InviteExpiry < now {
			continue // unused invite expired
		}
		if p.Token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(p.Token)) == 1 {
			cp := *p
			return &cp, false, true
		}
		if p.PendingToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(p.PendingToken)) == 1 {
			cp := *p
			return &cp, true, true
		}
	}
	return nil, false, false
}

// atomicWriteFile writes data to path via a temp file + rename, chmod'd to perm
// (mirrors cmd/tenant's atomicWrite; duplicated to keep peering importable
// without a cmd/tenant dependency).
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir, base := filepath.Dir(path), filepath.Base(path)
	f, err := os.CreateTemp(dir, base+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
