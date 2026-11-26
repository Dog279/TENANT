package fleet

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	// other.
	codePrefix       = "tntfleet1_"
	peerInvitePrefix = "tnt1_"
	maxCodeLen       = 8 << 10
	maxSecretLen     = 128

	// EnrollPath is where the master serves NewEnrollHandler and where Enroll
	// posts to.
	EnrollPath = "/fleet/enroll"

	maxEnrollBody    = 4 << 10
	enrollAttempts   = 5 // per source, per enrollWindow
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
	Secret      string   `json:"s"`
	Expires     int64    `json:"e"` // unix seconds
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
	if _, err := parseFingerprint(c.Fingerprint); err != nil || !validName(c.Name) || !validSecret(c.Secret) ||
		len(c.Addresses) == 0 || validAddresses(c.Addresses) != nil || c.Expires <= 0 {
		return enrollCode{}, errors.New("fleet: the enrollment code is damaged")
	}
	if now := time.Now().Unix(); now > c.Expires {
		return enrollCode{}, fmt.Errorf("fleet: the enrollment code expired %s ago; ask for a new one",
			(time.Duration(now-c.Expires) * time.Second).String())
	}
	return c, nil
}

// validSecret accepts the base64url text newSecret makes, and nothing that
// could not be one.
func validSecret(s string) bool {
	if len(s) == 0 || len(s) > maxSecretLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

func newSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand fills b or stops the program; it returns no error
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// enrollment finds the record of the code with that secret hash. Every
// record is compared, whichever matches.
func (d *fileData) enrollment(hash string) *enrollment {
	var found *enrollment
	for _, e := range d.Enrollments {
		if subtle.ConstantTimeCompare([]byte(e.Secret), []byte(hash)) == 1 {
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

// prune drops the records of codes that expired long ago.
func (d *fileData) prune(now time.Time) {
	cutoff := now.Add(-enrollKeep).Unix()
	d.Enrollments = slices.DeleteFunc(d.Enrollments, func(e *enrollment) bool { return e.Expires < cutoff })
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
// (the master's Identity.Fingerprint, which the new machine pins) and a
// secret; fleet.json keeps only the secret's hash. ttl is how long the code
// can be spent: zero means DefaultEnrollTTL, and a day is the most.
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
	secret := newSecret()
	code := enrollCode{Name: name, Fingerprint: masterFingerprint, Secret: secret}
	err := s.mutate(func(d *fileData) error {
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
		d.Enrollments = append(d.Enrollments, &enrollment{Name: name, Secret: hashSecret(secret), Expires: code.Expires, Adopt: adopt})
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
	sum := sha256.Sum256([]byte("tenant fleet confirm 1\n" + masterFingerprint + "\n" + childFingerprint))
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
func (s *Store) enroll(secret, fingerprint string, rep Reported) (Machine, error) {
	hash := hashSecret(secret)
	// A secret nobody was given is turned away from memory: a stranger does
	// not get to take the file's lock.
	d, _, err := s.view()
	if err != nil {
		return Machine{}, err
	}
	if d.enrollment(hash) == nil {
		return Machine{}, &Refusal{Reason: RefusedUnknownCode}
	}
	var out Machine
	err = s.mutate(func(d *fileData) error {
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
			if other != id && m.Fingerprint == fingerprint {
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

// enrollRequest is what a new machine posts. Its certificate is not in it:
// the master takes that from the connection.
type enrollRequest struct {
	Secret string `json:"secret"`
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
	if !validSecret(req.Secret) {
		return enrollRequest{}, errors.New("no enrollment secret")
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
	leaf, err := presented(r.TLS)
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
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxEnrollBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
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
	m, err := h.store.enroll(req.Secret, ref.Fingerprint, req.Reported)
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
// the one the code names, and presents the code's secret with id, this
// machine's new identity. self is what the machine reports about itself; its
// Sandbox and Ceiling are also what the returned Child is set to.
//
// It returns the Child to save as child.json and the six digits to show the
// operator. The machine is then pending: it connects like any other and is
// told WaitingForConfirmation until those digits are typed on the master.
// The digits are "" when the master confirmed the machine as it enrolled
// (TEN-553): nothing is left to type.
//
// The secret is sent once, to the first address that proves to be the
// master. A code is spent by that, so a failure after the secret went out is
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
	body, _ := json.Marshal(enrollRequest{Secret: ec.Secret, Reported: self}) // strings: cannot fail

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
