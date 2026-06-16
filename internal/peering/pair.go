package peering

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// TEN-239: push-invite pairing. Instead of exchanging an out-of-band secret
// code, the inviter POSTs a pairing request to the peer's UNAUTHENTICATED /pair
// endpoint; the peer's operator sees an Approve/Deny prompt carrying a 6-digit
// PIN that must match the inviter's screen (the second factor — it defeats a
// stranger spoofing a trusted name on an open port, since only the real inviter
// can tell the approver the PIN out of band). On approve the peer mints a token
// and TOFU-pins the inviter; the resulting peer record is identical to the
// code-exchange flow (TEN-183), so the listener/client are unchanged.

// pairPath is the listener path the inviter POSTs to. Separate from the
// bearer-gated MCP root so it can be unauthenticated yet only ever create a
// pending approval — it reads nothing.
const pairPath = "/pair"

// maxPairBody caps the pairing request body (anti-abuse).
const maxPairBody = 4 << 10

// PairRequest is what the inviter POSTs to /pair.
type PairRequest struct {
	Name       string `json:"name"`        // inviter's self-name (the peer labels them this)
	InstanceID string `json:"instance_id"` // inviter's stable id
	PIN        string `json:"pin"`         // 6 digits, shown on the inviter's screen
}

// PairResponse is what the accepter returns on approval.
type PairResponse struct {
	Name        string `json:"name"`        // accepter's self-name
	InstanceID  string `json:"instance_id"` // accepter's stable id
	Fingerprint string `json:"fingerprint"` // accepter's cert fp (the dialer pins this)
	Token       string `json:"token"`       // bearer the dialer presents
}

// GeneratePIN returns a uniformly-random 6-digit PIN (as a zero-padded string).
func GeneratePIN() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// FormatPIN renders a PIN as "123 456" for readability.
func FormatPIN(pin string) string {
	if len(pin) == 6 {
		return pin[:3] + " " + pin[3:]
	}
	return pin
}

// pairLimiter bounds concurrent pending pairing approvals so a flood can't spam
// the operator with prompts. A global cap (max) bounds the total, AND each
// source key (the remote host) may hold at most ONE pending slot — so a single
// unauthenticated client can't monopolize every slot and 429 legitimate
// pairings. Excess requests get 429 immediately.
type pairLimiter struct {
	mu     sync.Mutex
	max    int
	total  int
	perKey map[string]bool
}

func (p *pairLimiter) acquire(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.total >= p.max || p.perKey[key] {
		return false
	}
	if p.perKey == nil {
		p.perKey = map[string]bool{}
	}
	p.perKey[key] = true
	p.total++
	return true
}

func (p *pairLimiter) release(key string) {
	p.mu.Lock()
	if p.perKey[key] {
		delete(p.perKey, key)
		p.total--
	}
	p.mu.Unlock()
}

// handlePair is the unauthenticated /pair handler. It validates the request,
// raises the operator Approve/Deny prompt (with the PIN), and on approval mints
// a token, stores the inviter as an accept-side peer (all-deny share), and
// returns this instance's identity + token. Rate-limited; fails closed.
func (l *Listener) handlePair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if l.pairApprover == nil {
		http.Error(w, "pairing not accepting requests", http.StatusServiceUnavailable)
		return
	}
	srcKey := r.RemoteAddr
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		srcKey = host // one pending pairing per source host
	}
	if !l.pairLimiter.acquire(srcKey) {
		http.Error(w, "too many pending pairing requests — try again shortly", http.StatusTooManyRequests)
		return
	}
	defer l.pairLimiter.release(srcKey)

	var req PairRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxPairBody)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || req.InstanceID == "" || !isSixDigits(req.PIN) {
		http.Error(w, "missing name/instance_id, or PIN is not 6 digits", http.StatusBadRequest)
		return
	}

	prompt := fmt.Sprintf("peer %q (%s) wants to pair — PIN %s.\nApprove ONLY if that PIN matches what their screen shows.",
		req.Name, r.RemoteAddr, FormatPIN(req.PIN))

	// Bound the wait so a never-answered prompt doesn't pin a slot for long.
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	if !l.pairApprover(ctx, prompt) {
		http.Error(w, "pairing denied", http.StatusForbidden)
		return
	}

	secret, err := newSecret()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Store the inviter as an accept-side peer (they dial us). All-deny share.
	// The remote chose req.Name; if that collides with an EXISTING peer that is a
	// DIFFERENT instance, file under a uniquified name so an approved pairing can
	// never silently overwrite (de-authenticate) a trusted peer. A same-instance
	// collision is a legitimate re-pair and refreshes in place.
	name := uniqueAcceptName(l.store, req.Name, req.InstanceID)
	if err := l.store.Put(&Peer{
		Name:       name,
		InstanceID: req.InstanceID,
		Dial:       false,
		Token:      secret,
		CreatedAt:  nowRFC3339(),
	}); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(PairResponse{
		Name:        l.selfName,
		InstanceID:  l.selfID,
		Fingerprint: l.selfFinger,
		Token:       secret,
	})
}

// uniqueAcceptName returns name unchanged when it's free or already belongs to
// the same instance (a re-pair); otherwise it appends -2, -3, … so a new peer
// never clobbers an existing DIFFERENT peer's record.
func uniqueAcceptName(s *Store, name, instanceID string) string {
	existing, ok := s.Get(name)
	if !ok || existing.InstanceID == instanceID {
		return name
	}
	for i := 2; i < 1000; i++ {
		cand := fmt.Sprintf("%s-%d", name, i)
		if e, ok := s.Get(cand); !ok || e.InstanceID == instanceID {
			return cand
		}
	}
	return name + "-" + instanceID[:min(8, len(instanceID))]
}

func isSixDigits(s string) bool {
	if len(s) != 6 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// RequestPair is the INVITER side: POST a pairing request to the peer at
// baseURL and return the peer's response. Over https the peer's cert is NOT
// pre-pinned (no fingerprint yet) — we capture it via a TOFU verifier and
// cross-check it against the fingerprint the peer reports, so a response-body
// lie can't substitute a different cert. The operator-confirmed PIN is the
// trust anchor. overlay ⇒ plain HTTP (no TLS). The supplied ctx should carry a
// generous timeout (approval is interactive).
func RequestPair(ctx context.Context, baseURL string, req PairRequest, overlay bool) (*PairResponse, error) {
	var capturedFP string
	tr := &http.Transport{}
	if !overlay && strings.HasPrefix(strings.ToLower(baseURL), "https://") {
		tr.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true, // TOFU: no fingerprint yet; the PIN+approval is the trust
			VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
				if len(rawCerts) > 0 {
					capturedFP = CertFingerprint(rawCerts[0])
				}
				return nil
			},
		}
	}
	client := &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	body, _ := json.Marshal(req)
	url := strings.TrimRight(baseURL, "/") + pairPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("could not reach peer at %s: %w", baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("the peer DENIED the pairing request")
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("peer returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var pr PairResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxPairBody)).Decode(&pr); err != nil {
		return nil, fmt.Errorf("could not parse peer response: %w", err)
	}
	if pr.Token == "" {
		return nil, fmt.Errorf("peer approved but returned no token")
	}
	// Cross-check: the cert the peer actually served must match the fingerprint
	// it claims (defeats a man-in-the-middle returning someone else's fp).
	if !overlay && capturedFP != "" {
		if pr.Fingerprint == "" {
			return nil, fmt.Errorf("peer served TLS but reported no cert fingerprint to pin")
		}
		if capturedFP != pr.Fingerprint {
			return nil, fmt.Errorf("peer cert fingerprint mismatch (served %s, claimed %s) — possible MITM", capturedFP, pr.Fingerprint)
		}
	}
	return &pr, nil
}
