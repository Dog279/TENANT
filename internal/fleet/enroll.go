package fleet

import (
	"bufio"
	"bytes"
	"context"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha512"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	// DefaultEnrollTTL is how long an enrollment code can be spent.
	DefaultEnrollTTL = 15 * time.Minute
	maxEnrollTTL     = 24 * time.Hour
	// enrollKeep is how long the record of a spent or expired code stays, so
	// that a second attempt with it is recognised for what it is.
	enrollKeep = 7 * 24 * time.Hour

	// codePrefix marks a fleet enrollment code. A peer invite starts with
	// peerInvitePrefix, which no fleet code does, so neither parses as the
	// other. legacyCodePrefix is a code from before the cnsa profile, whose
	// secret was sent to the master as it was: nothing takes one any more.
	codePrefix       = "tntfleet2_"
	legacyCodePrefix = "tntfleet1_"
	peerInvitePrefix = "tnt1_"
	maxCodeLen       = 8 << 10
	seedLen          = 32 // an ML-DSA private key's seed

	// What the proof of an enrollment is signed under, and the label the
	// connection's own secret is taken from (RFC 8446, section 7.5).
	enrollContext = "tenant fleet enroll 2"
	enrollLabel   = "EXPORTER-tenant-fleet-enroll"

	// EnrollPath is where the master serves NewEnrollHandler and where Enroll
	// posts to.
	EnrollPath = "/fleet/enroll"

	maxEnrollBody    = 16 << 10 // an ML-DSA-87 public key and signature are 7 KiB, and more in base64
	enrollAttempts   = 5        // per source, per enrollWindow
	enrollWindow     = time.Minute
	maxEnrollSources = 4096
	enrollDialWait   = 10 * time.Second
	enrollAnswerWait = 30 * time.Second
)

// ErrWrongCode is returned by Confirm for a code that is not the machine's.
var ErrWrongCode = errors.New("fleet: that is not the code the machine shows")

// enrollCode is what an enrollment code carries. Short keys keep the code a
// line that can be pasted.
type enrollCode struct {
	Name        string   `json:"n"`
	Addresses   []string `json:"a"`
	Fingerprint string   `json:"f"` // the master's certificate
	// Seed is the seed of the code's one-time ML-DSA-87 key, in base64url.
	// It never leaves the machine the code is typed into: that machine signs
	// with the key, and the master knows the key by its public half.
	Seed    string `json:"s"`
	Expires int64  `json:"e"` // unix seconds
}

func (c enrollCode) encode() string {
	b, _ := json.Marshal(c) // strings and numbers: cannot fail
	return codePrefix + base64.RawURLEncoding.EncodeToString(b)
}

// parseEnrollCode reads a code on the machine being enrolled. The code is
// all that machine knows about its master, so every part is checked before
// anything is dialed.
func parseEnrollCode(code string) (enrollCode, error) {
	var c enrollCode
	code = strings.TrimSpace(code)
	switch {
	case strings.HasPrefix(code, codePrefix):
	case strings.HasPrefix(code, legacyCodePrefix):
		return c, errors.New("fleet: this enrollment code is from a master that has not moved to ML-DSA-87 certificates: update the master's Tenant and ask it for a new code")
	case strings.HasPrefix(code, peerInvitePrefix):
		return c, errors.New("fleet: this is a peer invite, not a fleet enrollment code")
	default:
		return c, fmt.Errorf("fleet: not a fleet enrollment code: one starts with %q", codePrefix)
	}
	if len(code) > maxCodeLen {
		return c, errors.New("fleet: the enrollment code is too long")
	}
	raw, err := base64.RawURLEncoding.DecodeString(code[len(codePrefix):])
	if err == nil {
		err = decodeStrict(raw, &c)
	}
	if err != nil {
		return enrollCode{}, fmt.Errorf("fleet: the enrollment code is damaged: %w", err)
	}
	if _, err := parseFingerprint(c.Fingerprint); err != nil || !validName(c.Name) || c.key() == nil ||
		len(c.Addresses) == 0 || validAddresses(c.Addresses) != nil || c.Expires <= 0 {
		return enrollCode{}, errors.New("fleet: the enrollment code is damaged")
	}
	if now := time.Now().Unix(); now > c.Expires {
		return enrollCode{}, fmt.Errorf("fleet: the enrollment code expired %s ago; ask for a new one",
			(time.Duration(now-c.Expires) * time.Second).String())
	}
	return c, nil
}

// key is the code's one-time signing key, or nil for a code whose seed is
// not one.
func (c enrollCode) key() *mldsa.PrivateKey {
	seed, err := base64.RawURLEncoding.DecodeString(c.Seed)
	if err != nil || len(seed) != seedLen {
		return nil
	}
	key, err := mldsa.NewPrivateKey(mldsa.MLDSA87(), seed)
	if err != nil {
		return nil
	}
	return key
}

// newEnrollKey makes a code's one-time key and returns its seed, which goes
// into the code, and the hash of its public half, which the master keeps.
func newEnrollKey() (seed, keyHash string, err error) {
	b := make([]byte, seedLen)
	_, _ = rand.Read(b) // crypto/rand fills b or stops the program; it returns no error
	key, err := mldsa.NewPrivateKey(mldsa.MLDSA87(), b)
	if err != nil {
		return "", "", fmt.Errorf("fleet: make the enrollment key: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), hashKey(key.PublicKey().Bytes()), nil
}

// hashKey is how the master recognises a code's key: the SHA-384 of its
// public half, in lowercase hex.
func hashKey(public []byte) string {
	sum := sha512.Sum384(public)
	return hex.EncodeToString(sum[:])
}

// enrollMessage is what a new machine signs with its code's key: the secret
// of the one connection it enrolls over, which no other connection has, and
// the certificate it presented there. A proof read off one connection is
// therefore no proof on another, and enrolls no certificate but its own.
// (The master's certificate is in the connection's secret already; the
// client's is not, which is why it is named.)
func enrollMessage(cs *tls.ConnectionState, childFingerprint string) ([]byte, error) {
	if cs == nil {
		return nil, ErrNoCertificate
	}
	binding, err := cs.ExportKeyingMaterial(enrollLabel, nil, sha512.Size384)
	if err != nil {
		return nil, fmt.Errorf("fleet: the connection has no secret to bind the enrollment to: %w", err)
	}
	return []byte(strings.Join([]string{enrollContext, hex.EncodeToString(binding), childFingerprint, ""}, "\n")), nil
}

// enrollment finds the record of the code whose key has that hash. Every
// record is compared, whichever matches.
func (d *fileData) enrollment(hash string) *enrollment {
	var found *enrollment
	for _, e := range d.Enrollments {
		if subtle.ConstantTimeCompare([]byte(e.Key), []byte(hash)) == 1 {
			found = e
		}
	}
	return found
}

// reserved reports whether a code that can still be spent holds that name.
func (d *fileData) reserved(name string, now time.Time) bool {
	return slices.ContainsFunc(d.Enrollments, func(e *enrollment) bool {
		return e.UsedBy == "" && e.Expires >= now.Unix() && strings.EqualFold(e.Name, name)
	})
}

// prune drops the records of codes that expired long ago, and of codes from
// before the cnsa profile, which enroll nothing.
func (d *fileData) prune(now time.Time) {
	cutoff := now.Add(-enrollKeep).Unix()
	d.Enrollments = slices.DeleteFunc(d.Enrollments, func(e *enrollment) bool { return e.Expires < cutoff || e.Key == "" })
}

// newID makes an id no machine in the file has.
func (d *fileData) newID() string {
	for {
		var b [8]byte
		_, _ = rand.Read(b[:]) // as in newSecret
		if id := "m" + hex.EncodeToString(b[:]); d.Machines[id] == nil {
			return id
		}
	}
}

// CreateEnrollment makes the one-time code that enrolls one machine under
// name. The code carries the name, the master's addresses, masterFingerprint
// (the master's Identity.Fingerprint, which the new machine pins) and the
// seed of a one-time ML-DSA-87 key; fleet.json keeps only the hash of that
// key's public half. The new machine signs with the key and sends the
// signature, never the seed. ttl is how long the code can be spent: zero
// means DefaultEnrollTTL, and a day is the most.
//
// adopt is empty for a machine that starts clean. Given the id of a revoked
// machine, the new machine takes that id, and with it whatever was kept under
// it. A new code for a name replaces one for that name that was not spent.
func (s *Store) CreateEnrollment(masterFingerprint, name string, ttl time.Duration, adopt string) (string, error) {
	if _, err := parseFingerprint(masterFingerprint); err != nil {
		return "", fmt.Errorf("the master's fingerprint: %w", err)
	}
	if !validName(name) {
		return "", errInvalidName(name)
	}
	if adopt != "" && !validID(adopt) {
		return "", ErrUnknown
	}
	if ttl == 0 {
		ttl = DefaultEnrollTTL
	}
	if ttl < time.Second || ttl > maxEnrollTTL {
		return "", fmt.Errorf("fleet: an enrollment code lasts between a second and %s", maxEnrollTTL)
	}
	seed, keyHash, err := newEnrollKey()
	if err != nil {
		return "", err
	}
	code := enrollCode{Name: name, Fingerprint: masterFingerprint, Seed: seed}
	err = s.mutate(func(d *fileData) error {
		if len(d.Addresses) == 0 {
			return errors.New("fleet: the master has no address for a child to dial yet")
		}
		if d.named(name, "") != nil {
			return fmt.Errorf("fleet: the name %q is taken", name)
		}
		if adopt != "" {
			if old, ok := d.Machines[adopt]; !ok {
				return ErrUnknown
			} else if old.State != StateRevoked {
				return errors.New("fleet: only a revoked machine's id can be adopted")
			}
		}
		d.Enrollments = slices.DeleteFunc(d.Enrollments, func(e *enrollment) bool {
			return e.UsedBy == "" && strings.EqualFold(e.Name, name)
		})
		code.Addresses = slices.Clone(d.Addresses)
		code.Expires = s.now().Add(ttl).Unix()
		d.Enrollments = append(d.Enrollments, &enrollment{Name: name, Key: keyHash, Expires: code.Expires, Adopt: adopt})
		return nil
	})
	if err != nil {
		return "", err
	}
	return code.encode(), nil
}

// ConfirmCode is the six digits a newly enrolled machine prints and the
// operator types on the master. Both sides derive them from the two
// certificates of the connection the machine enrolled over, so a code typed
// from one machine's screen cannot confirm another machine that got hold of
// the enrollment code first.
func ConfirmCode(masterFingerprint, childFingerprint string) string {
	sum := sha512.Sum384([]byte("tenant fleet confirm 2\n" + masterFingerprint + "\n" + childFingerprint))
	return fmt.Sprintf("%06d", binary.BigEndian.Uint64(sum[:8])%1_000_000)
}

// Confirm makes a pending machine a confirmed one. code is what the machine
// printed (spaces are ignored); masterFingerprint is the master's
// Identity.Fingerprint. A code that does not match fails with ErrWrongCode
// and changes nothing, and there is no other way to confirm a machine. Who
// may call this is the caller's to enforce: the operator at the master, never
// a relay and never the agent.
func (s *Store) Confirm(masterFingerprint, id, code string) (Machine, error) {
	if _, err := parseFingerprint(masterFingerprint); err != nil {
		return Machine{}, fmt.Errorf("the master's fingerprint: %w", err)
	}
	code = strings.Join(strings.Fields(code), "")
	var out Machine
	err := s.change(id, func(_ *fileData, m *Machine) error {
		if m.State != StatePending {
			return fmt.Errorf("fleet: the machine is %s, not waiting for confirmation", m.State)
		}
		if subtle.ConstantTimeCompare([]byte(ConfirmCode(masterFingerprint, m.Fingerprint)), []byte(code)) != 1 {
			return ErrWrongCode
		}
		m.State, m.ConfirmedAt = StateConfirmed, s.now().UTC().Truncate(time.Second)
		out = *m
		return nil
	})
	return out, err
}

// Reason is why the enroll endpoint refused an attempt.
type Reason string

const (
	RefusedRateLimit   Reason = "rate_limited"     // too many attempts from this source
	RefusedMethod      Reason = "bad_method"       // not a POST
	RefusedNoCert      Reason = "no_certificate"   // no usable client certificate
	RefusedOff         Reason = "enrollment_off"   // SetEnrollOpen(false)
	RefusedUnavailable Reason = "store_unreadable" // fleet.json could not be read or saved
	RefusedTooLarge    Reason = "body_too_large"
	RefusedBadRequest  Reason = "bad_request"
	RefusedUnknownCode Reason = "unknown_code"
	RefusedBadProof    Reason = "bad_proof" // the code's key did not sign for this connection and certificate
	RefusedExpired     Reason = "expired_code"
	RefusedReplay      Reason = "replayed_code" // the code was spent already
	RefusedConflict    Reason = "conflict"      // the name, id or certificate is in use
)

// Refusal is the audit record of one refused enrollment attempt.
type Refusal struct {
	Time   time.Time
	Source string // the remote address
	Reason Reason
	// Fingerprint is the certificate the attempt presented, if it got that far.
	Fingerprint string
	// Name is the machine name on the code, when the code was recognised.
	Name string
	// Machine is, for a replayed code, the id of the machine that code
	// enrolled: that machine or this attempt is not the operator's.
	Machine string
}

func (r *Refusal) Error() string { return "fleet: enrollment refused: " + string(r.Reason) }

// enroll spends a code: in one step under the file's lock the code is marked
// spent and the certificate is pinned to a new pending machine. Two attempts
// with one code cannot both get through, and a spent code enrolls nothing
// again. A refusal is returned as a *Refusal.
//
// hash is the hash of the key the machine proved it holds (enrollHandler):
// the code is known by it.
func (s *Store) enroll(hash, fingerprint string, rep Reported) (Machine, error) {
	var out Machine
	err := s.mutate(func(d *fileData) error {
		now := s.now()
		e := d.enrollment(hash)
		switch {
		case e == nil:
			return &Refusal{Reason: RefusedUnknownCode}
		case e.UsedBy != "":
			return &Refusal{Reason: RefusedReplay, Name: e.Name, Machine: e.UsedBy}
		case now.Unix() > e.Expires:
			return &Refusal{Reason: RefusedExpired, Name: e.Name}
		}
		conflict := &Refusal{Reason: RefusedConflict, Name: e.Name}
		id := e.Adopt
		if old, ok := d.Machines[id]; id != "" && (!ok || old.State != StateRevoked) {
			return conflict
		}
		for other, m := range d.Machines {
			if other != id && (m.Fingerprint == fingerprint || m.Next == fingerprint) {
				return conflict // one certificate, one machine
			}
		}
		if d.named(e.Name, "") != nil {
			return conflict
		}
		if id == "" {
			id = d.newID()
		}
		m := &Machine{ID: id, Name: e.Name, Fingerprint: fingerprint, State: StatePending, Reported: rep,
			EnrolledAt: now.UTC().Truncate(time.Second)}
		d.Machines[id], e.UsedBy, out = m, id, *m
		return nil
	})
	return out, err
}

// enrollRequest is what a new machine posts: the public half of its code's
// key, and that key's signature over enrollMessage. Its certificate is not
// in it: the master takes that from the connection. The code's seed is not
// in it either, so nothing in a request enrolls anything a second time.
type enrollRequest struct {
	Key   []byte `json:"key"`
	Proof []byte `json:"proof"`
	Reported
}

type enrollResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Confirmed says the master confirmed the machine as it enrolled, on the
	// strength of the code alone (TEN-553): there are no digits to type. A
	// master from before that never sets it.
	Confirmed bool `json:"confirmed,omitempty"`
}

// parseEnrollRequest reads the body a stranger sent. Keys it does not know
// are skipped, so a newer child can say more; what it does read is checked.
func parseEnrollRequest(b []byte) (enrollRequest, error) {
	var req enrollRequest
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&req); err != nil {
		return enrollRequest{}, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return enrollRequest{}, errors.New("unexpected data after the request")
	}
	if len(req.Key) != mldsa.MLDSA87PublicKeySize || len(req.Proof) != mldsa.MLDSA87SignatureSize {
		return enrollRequest{}, errors.New("no enrollment key and proof")
	}
	if err := req.Reported.validate(); err != nil {
		return enrollRequest{}, err
	}
	return req, nil
}

// attemptLimiter counts attempts per source in fixed windows.
type attemptLimiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	sources map[string]*attempts
	fullAt  time.Time // when a full table was last reported
}

type attempts struct {
	n     int
	start time.Time
}

// allow counts one attempt from key. report is true for the refusal that
// should be put on record: the first one of a window, not every one of a
// flood.
func (l *attemptLimiter) allow(key string, now time.Time) (ok, report bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.sources[key]
	if a != nil && now.Sub(a.start) >= l.window {
		a = nil
	}
	if a == nil {
		if len(l.sources) >= maxEnrollSources {
			for k, v := range l.sources {
				if now.Sub(v.start) >= l.window {
					delete(l.sources, k)
				}
			}
		}
		if len(l.sources) >= maxEnrollSources && l.sources[key] == nil {
			// More sources than the table holds inside one window: new ones
			// wait it out.
			report = now.Sub(l.fullAt) >= l.window
			if report {
				l.fullAt = now
			}
			return false, report
		}
		if l.sources == nil {
			l.sources = map[string]*attempts{}
		}
		a = &attempts{start: now}
		l.sources[key] = a
	}
	a.n++
	return a.n <= l.max, a.n == l.max+1
}

// sourceKey is what attempts are counted by: the remote IPv4 address, or the
// /64 of an IPv6 one, since one host owns that many addresses.
func sourceKey(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	if addr = addr.Unmap().WithZone(""); addr.Is6() {
		if p, err := addr.Prefix(64); err == nil {
			return p.String()
		}
	}
	return addr.String()
}

type enrollHandler struct {
	store   *Store
	refused func(Refusal)
	alert   func(Refusal)
	// enrolled is told of a machine that has just spent its code and is
	// pending, before the machine is answered. It reports whether it confirmed
	// the machine there and then; nil confirms nothing.
	enrolled func(m Machine, remote string) bool
	limit    attemptLimiter
}

// NewEnrollHandler is the master's enroll endpoint, to be served at
// EnrollPath over MasterTLS or MasterEnrollTLS and nothing else. It takes a
// POST from a machine holding an enrollment code, pins the certificate of
// that connection to a new pending machine, and answers with the machine's
// id. Bodies are capped, each source gets a few attempts a minute, and
// SetEnrollOpen(false) closes it without touching the link.
//
// refused is called with the audit record of every attempt turned away (for
// a source over its limit: once per minute). alert is called as well when a
// code is presented that was already spent: someone other than the machine it
// enrolled has the code, or had it first. Either may be nil. They run on the
// request's goroutine.
func NewEnrollHandler(s *Store, refused, alert func(Refusal)) http.Handler {
	return newEnrollHandler(s, refused, alert, nil)
}

// newEnrollHandler is NewEnrollHandler with the hook that may confirm a
// machine as it enrolls (ManagerConfig.Enrolled).
func newEnrollHandler(s *Store, refused, alert func(Refusal), enrolled func(Machine, string) bool) http.Handler {
	return &enrollHandler{store: s, refused: refused, alert: alert, enrolled: enrolled,
		limit: attemptLimiter{max: enrollAttempts, window: enrollWindow}}
}

func (h *enrollHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ref := Refusal{Time: h.store.now(), Source: r.RemoteAddr}
	refuse := func(status int, reason Reason, answer string) {
		ref.Reason = reason
		if h.refused != nil {
			h.refused(ref)
		}
		if reason == RefusedReplay && h.alert != nil {
			h.alert(ref)
		}
		http.Error(w, answer, status)
	}
	// The body is read before anything is answered. A request is about
	// 10 KiB, more than the server has taken in when the handler starts, and
	// an answer sent with the rest unread closes the connection with data
	// pending: that resets it, and the machine sees a dropped connection in
	// place of the reason. The server's ReadTimeout bounds a slow body.
	body, bodyErr := io.ReadAll(http.MaxBytesReader(w, r.Body, maxEnrollBody))
	if ok, report := h.limit.allow(sourceKey(r.RemoteAddr), ref.Time); !ok {
		if report {
			refuse(http.StatusTooManyRequests, RefusedRateLimit, "too many enrollment attempts; try again in a minute")
		} else {
			http.Error(w, "too many enrollment attempts; try again in a minute", http.StatusTooManyRequests)
		}
		return
	}
	// The certificate comes from the handshake, which proved the machine
	// holds its key. Without one there is nothing to pin.
	leaf, _, err := verified(r.TLS, false)
	if err != nil {
		refuse(http.StatusForbidden, RefusedNoCert, "a client certificate is required")
		return
	}
	ref.Fingerprint = fingerprintOf(leaf.Raw)
	if r.Method != http.MethodPost {
		refuse(http.StatusMethodNotAllowed, RefusedMethod, "POST only")
		return
	}
	if err := h.store.Err(); err != nil {
		refuse(http.StatusServiceUnavailable, RefusedUnavailable, "enrollment is not available")
		return
	}
	if !h.store.EnrollOpen() {
		refuse(http.StatusServiceUnavailable, RefusedOff, "enrollment is switched off on the master")
		return
	}
	if bodyErr != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(bodyErr, &tooLarge) {
			refuse(http.StatusRequestEntityTooLarge, RefusedTooLarge, "request body too large")
		} else {
			refuse(http.StatusBadRequest, RefusedBadRequest, "bad request")
		}
		return
	}
	req, err := parseEnrollRequest(body)
	if err != nil {
		refuse(http.StatusBadRequest, RefusedBadRequest, "bad request")
		return
	}
	// A key nobody was given a code for is turned away from memory: a
	// stranger gets no signature checked and does not take the file's lock.
	hash := hashKey(req.Key)
	d, _, err := h.store.view()
	if err != nil {
		refuse(http.StatusServiceUnavailable, RefusedUnavailable, "enrollment is not available")
		return
	}
	e := d.enrollment(hash)
	if e == nil {
		refuse(http.StatusForbidden, RefusedUnknownCode, "enrollment refused")
		return
	}
	ref.Name = e.Name
	// The proof: the code's key signed this connection and this certificate.
	key, err := mldsa.NewPublicKey(mldsa.MLDSA87(), req.Key)
	var msg []byte
	if err == nil {
		msg, err = enrollMessage(r.TLS, ref.Fingerprint)
	}
	if err == nil {
		err = mldsa.Verify(key, msg, req.Proof, &mldsa.Options{Context: enrollContext})
	}
	if err != nil {
		refuse(http.StatusForbidden, RefusedBadProof, "enrollment refused")
		return
	}
	m, err := h.store.enroll(hash, ref.Fingerprint, req.Reported)
	if err != nil {
		var why *Refusal
		if !errors.As(err, &why) {
			refuse(http.StatusServiceUnavailable, RefusedUnavailable, "enrollment is not available")
			return
		}
		// One answer for every code that does not enroll: the reason is the
		// operator's to read, not the sender's.
		ref.Name, ref.Machine = why.Name, why.Machine
		refuse(http.StatusForbidden, why.Reason, "enrollment refused")
		return
	}
	Enrolled.Log(r.Context(), "name", m.Name, "machine", m.ID, "remote", r.RemoteAddr, "certificate", ref.Fingerprint)
	confirmed := h.enrolled != nil && h.enrolled(m, r.RemoteAddr)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(enrollResponse{ID: m.ID, Name: m.Name, Confirmed: confirmed})
}

// Enroll runs on the machine being enrolled. It reads code, dials the
// master at the addresses in it, refuses any server whose certificate is not
// the one the code names, and proves it holds the code by signing, with the
// code's key, for that one connection and for id, this machine's new
// identity. The code's seed is not sent. self is what the machine reports
// about itself; its Sandbox and Ceiling are also what the returned Child is
// set to.
//
// It returns the Child to save as child.json and the six digits to show the
// operator. The machine is then pending: it connects like any other and is
// told WaitingForConfirmation until those digits are typed on the master.
// The digits are "" when the master confirmed the machine as it enrolled
// (TEN-553): nothing is left to type.
//
// The proof is sent once, to the first address that proves to be the
// master. A code is spent by that, so a failure after the proof went out is
// not retried here: ask the master whether the machine is pending.
func Enroll(ctx context.Context, code string, id Identity, self Reported) (Child, string, error) {
	fail := func(err error) (Child, string, error) { return Child{}, "", err }
	ec, err := parseEnrollCode(code)
	if err != nil {
		return fail(err)
	}
	if err := self.validate(); err != nil {
		return fail(err)
	}
	if !validSandbox(self.Sandbox) {
		return fail(errInvalidSandbox)
	}
	cfg, err := childTLS(id, ec.Fingerprint, enrollServerName)
	if err != nil {
		return fail(err)
	}
	key := ec.key() // parseEnrollCode checked there is one

	var (
		conn   net.Conn
		failed []error
	)
	for _, addr := range ec.Addresses {
		dctx, cancel := context.WithTimeout(ctx, enrollDialWait)
		c, err := (&tls.Dialer{Config: cfg}).DialContext(dctx, "tcp", addr)
		cancel()
		if err == nil {
			conn = c
			break
		}
		failed = append(failed, fmt.Errorf("%s: %w", addr, err))
	}
	if conn == nil {
		return fail(fmt.Errorf("fleet: could not reach the master: %w", errors.Join(failed...)))
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(ctx, enrollAnswerWait)
	defer cancel()
	defer context.AfterFunc(ctx, func() { _ = conn.Close() })()

	cs := conn.(*tls.Conn).ConnectionState() // a tls.Dialer's connection, its handshake done
	msg, err := enrollMessage(&cs, id.Fingerprint)
	if err != nil {
		return fail(err)
	}
	proof, err := key.Sign(nil, msg, &mldsa.Options{Context: enrollContext})
	if err != nil {
		return fail(fmt.Errorf("fleet: sign the enrollment: %w", err))
	}
	body, _ := json.Marshal(enrollRequest{Key: key.PublicKey().Bytes(), Proof: proof, Reported: self}) // bytes and strings: cannot fail

	// The connection is made and checked; the URL only fills in the request
	// line and the Host header, and being constant it cannot fail to parse.
	req, _ := http.NewRequest(http.MethodPost, "https://"+enrollServerName+EnrollPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Close = true
	if err := req.Write(conn); err != nil {
		return fail(fmt.Errorf("fleet: send the enrollment: %w", err))
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return fail(fmt.Errorf("fleet: the master closed the connection without answering (is enrollment switched off there?): %w", err))
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxEnrollBody))
	if err != nil {
		return fail(fmt.Errorf("fleet: read the master's answer: %w", err))
	}
	if resp.StatusCode != http.StatusOK {
		return fail(fmt.Errorf("fleet: the master answered %d: %s", resp.StatusCode, clip(string(answer))))
	}
	var out enrollResponse
	if err := json.Unmarshal(answer, &out); err != nil {
		return fail(fmt.Errorf("fleet: the master's answer: %w", err))
	}
	child := Child{ID: out.ID, Name: out.Name, MasterAddresses: ec.Addresses, MasterFingerprint: ec.Fingerprint,
		Sandbox: self.Sandbox, Ceiling: Ceiling(self.Ceiling)}
	if err := child.validate(); err != nil {
		return fail(fmt.Errorf("fleet: the master's answer: %w", err))
	}
	if out.Confirmed {
		return child, "", nil
	}
	return child, ConfirmCode(ec.Fingerprint, id.Fingerprint), nil
}

// clip makes a server's words safe to print in an error: one short line of
// printable text.
func clip(s string) string {
	s = strings.Map(func(r rune) rune {
		if !unicode.IsPrint(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, "?"))
	if s = strings.TrimSpace(s); len(s) > 200 {
		s = strings.ToValidUTF8(s[:200], "")
	}
	return s
}
