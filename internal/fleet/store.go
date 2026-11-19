package fleet

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"filippo.io/age"

	"tenant/internal/datalock"
	"tenant/internal/secrets"
)

const (
	storeFileName = "fleet.json"
	storeVersion  = 1
	// maxStoreBytes bounds fleet.json, read or written. A record is a few
	// hundred bytes, so this is thousands of machines.
	maxStoreBytes = 4 << 20

	maxNameLen     = 63
	maxAddresses   = 16
	maxAddressLen  = 255
	maxReportedLen = 128
)

// WaitingForConfirmation is the one thing a master says to a machine that
// has enrolled and has not been confirmed yet.
const WaitingForConfirmation = "waiting for confirmation"

// What a lookup or a change can fail with. Test with errors.Is.
var (
	// ErrUnreadable: fleet.json is there and cannot be read or understood.
	// Every lookup fails with it until the file can be read again.
	ErrUnreadable = errors.New("fleet: fleet.json cannot be read")
	// ErrUnknown: no machine with that id, name or certificate.
	ErrUnknown = errors.New("fleet: unknown machine")
	// ErrPending: the machine enrolled and waits for the operator.
	ErrPending = errors.New("fleet: " + WaitingForConfirmation)
	// ErrPaused: the operator paused the machine.
	ErrPaused = errors.New("fleet: the machine is paused")
	// ErrRevoked: the machine was revoked.
	ErrRevoked = errors.New("fleet: the machine is revoked")
)

// State is where a machine stands with its master.
type State string

const (
	// StatePending: enrolled, its certificate pinned, not confirmed. It may
	// connect and is told WaitingForConfirmation and nothing else.
	StatePending State = "pending"
	// StateConfirmed: the only state in which calls are made.
	StateConfirmed State = "confirmed"
	// StatePaused: confirmed, and switched off by the operator for now. It
	// may connect, and no call is made.
	StatePaused State = "paused"
	// StateRevoked: its certificate is refused at the handshake. The record
	// stays, so the id can be adopted by a machine enrolled later, until
	// Remove deletes it.
	StateRevoked State = "revoked"
)

func (s State) valid() bool {
	switch s {
	case StatePending, StateConfirmed, StatePaused, StateRevoked:
		return true
	}
	return false
}

// Reported is what a machine said about itself. It is that machine's word
// and nothing more: text to show the operator, marked as reported, and never
// something to decide by. OS and Ceiling are one of their known values; the
// rest is printable text of bounded length.
type Reported struct {
	OS      string `json:"os"` // windows, linux or darwin
	User    string `json:"user,omitempty"`
	Version string `json:"version,omitempty"`
	Sandbox string `json:"sandbox,omitempty"`
	Ceiling string `json:"ceiling"` // full, no-exec or read-only
}

func (r Reported) validate() error {
	switch r.OS {
	case "windows", "linux", "darwin":
	default:
		return errors.New("fleet: the reported OS is not windows, linux or darwin")
	}
	if !Ceiling(r.Ceiling).valid() {
		return errors.New("fleet: the reported ceiling is not full, no-exec or read-only")
	}
	for _, text := range []string{r.User, r.Version, r.Sandbox} {
		if len(text) > maxReportedLen || !printable(text) {
			return fmt.Errorf("fleet: a reported value is longer than %d bytes or is not printable text", maxReportedLen)
		}
	}
	return nil
}

func printable(s string) bool {
	return utf8.ValidString(s) && strings.IndexFunc(s, func(r rune) bool { return !unicode.IsPrint(r) }) < 0
}

// Machine is one machine of the fleet.
type Machine struct {
	// ID is assigned by the master at enrollment and never changes. Rules,
	// memory and everything else about a machine is kept under it.
	ID string `json:"-"`
	// Name is the operator's label. It can change, and a revoked machine's
	// name can be given to another.
	Name string `json:"name"`
	// Fingerprint is the machine's pinned certificate, its only credential.
	Fingerprint string    `json:"fingerprint"`
	State       State     `json:"state"`
	Reported    Reported  `json:"reported"`
	EnrolledAt  time.Time `json:"enrolled_at"`
	ConfirmedAt time.Time `json:"confirmed_at,omitzero"`
	RevokedAt   time.Time `json:"revoked_at,omitzero"`
}

// fileData is fleet.json.
type fileData struct {
	Version int `json:"version"`
	// Addresses are the host:port pairs children dial the master at.
	Addresses []string `json:"addresses,omitempty"`
	// EnrollOff closes the enroll endpoint.
	EnrollOff   bool                `json:"enroll_off,omitempty"`
	Machines    map[string]*Machine `json:"machines"`
	Enrollments []*enrollment       `json:"enrollments,omitempty"`
	// Recovery is the public half of the fleet's recovery identity (an age
	// X25519 recipient, "age1…"): every snapshot of a machine's memory is
	// encrypted to it as well as to the fleet key (TEN-532). It is public;
	// the identity itself is kept by the operator and never by Tenant.
	Recovery string `json:"recovery,omitempty"`
	// RecoveryProven is when the operator last gave the recovery identity
	// back and a test decryption opened with it. Zero: never.
	RecoveryProven time.Time `json:"recovery_proven,omitzero"`
}

// enrollment is one enrollment code the master gave out. The secret itself
// is only in the code: the file holds its SHA-256, which is enough to
// recognise it and useless for enrolling.
type enrollment struct {
	Name    string `json:"name"`
	Secret  string `json:"secret_sha256"`
	Expires int64  `json:"expires"`           // unix seconds
	Adopt   string `json:"adopt,omitempty"`   // a revoked machine's id the new machine takes over
	UsedBy  string `json:"used_by,omitempty"` // the machine this code enrolled: it is spent
}

func emptyData() *fileData {
	return &fileData{Version: storeVersion, Machines: map[string]*Machine{}}
}

// decodeStrict reads exactly one JSON value into v. It refuses a key v has
// no field for, so a misspelt setting is an error and not a default, and a
// key that is there twice, so no file means one thing to its reader and
// another to this parser, which would take the last.
func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("unexpected data after the JSON value")
	}
	// After Decode, which has bounded how deep the value nests.
	return uniqueKeys(json.NewDecoder(bytes.NewReader(b)))
}

// uniqueKeys reads one JSON value and fails if an object in it has a key
// twice. Keys are compared without regard to case, as the decoder matches
// them to fields.
func uniqueKeys(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	open, _ := tok.(json.Delim)
	if open != '{' && open != '[' {
		return nil // a string, a number, true, false or null
	}
	seen := map[string]bool{}
	for dec.More() {
		if open == '{' {
			tok, err := dec.Token()
			if err != nil {
				return err
			}
			key := strings.ToLower(tok.(string)) // the decoder gives an object's key as a string
			if seen[key] {
				return fmt.Errorf("the key %q is there twice", tok)
			}
			seen[key] = true
		}
		if err := uniqueKeys(dec); err != nil {
			return err
		}
	}
	_, err = dec.Token() // the closing bracket
	return err
}

// parseStore reads and checks fleet.json. Anything it does not accept makes
// the whole file unreadable: half a fleet is not a state to run in.
func parseStore(b []byte) (*fileData, error) {
	if len(b) > maxStoreBytes {
		return nil, fmt.Errorf("fleet.json is larger than %d bytes", maxStoreBytes)
	}
	d := &fileData{}
	if err := decodeStrict(b, d); err != nil {
		return nil, err
	}
	if d.Version != storeVersion {
		return nil, fmt.Errorf("fleet.json is version %d and this Tenant reads version %d", d.Version, storeVersion)
	}
	if err := validAddresses(d.Addresses); err != nil {
		return nil, err
	}
	if err := validRecovery(d.Recovery, d.RecoveryProven); err != nil {
		return nil, err
	}
	if d.Machines == nil {
		d.Machines = map[string]*Machine{}
	}
	prints, names := map[string]bool{}, map[string]bool{}
	for id, m := range d.Machines {
		if !validID(id) || m == nil {
			return nil, fmt.Errorf("machine id %q is not valid", id)
		}
		m.ID = id
		if !validName(m.Name) {
			return nil, fmt.Errorf("machine %s has an invalid name", id)
		}
		if _, err := parseFingerprint(m.Fingerprint); err != nil {
			return nil, fmt.Errorf("machine %s: %w", id, err)
		}
		if !m.State.valid() {
			return nil, fmt.Errorf("machine %s has an unknown state", id)
		}
		if err := m.Reported.validate(); err != nil {
			return nil, fmt.Errorf("machine %s: %w", id, err)
		}
		if prints[m.Fingerprint] {
			return nil, fmt.Errorf("machine %s has the certificate of another machine", id)
		}
		prints[m.Fingerprint] = true
		if m.State == StateRevoked {
			continue
		}
		if key := strings.ToLower(m.Name); names[key] {
			return nil, fmt.Errorf("two machines are named %q", m.Name)
		} else {
			names[key] = true
		}
	}
	for _, e := range d.Enrollments {
		if e == nil || !validName(e.Name) || e.Expires <= 0 ||
			(e.Adopt != "" && !validID(e.Adopt)) || (e.UsedBy != "" && !validID(e.UsedBy)) {
			return nil, errors.New("an enrollment record is not valid")
		}
		if _, err := parseFingerprint(e.Secret); err != nil {
			return nil, errors.New("an enrollment record is not valid")
		}
	}
	return d, nil
}

// validID accepts what newID makes: m and sixteen lowercase hex digits. Ids
// name directories and settings keys, so nothing else is let in.
func validID(s string) bool {
	if len(s) != 17 || s[0] != 'm' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// validName accepts a label of letters, digits, '.', '-' and '_' that starts
// with a letter or digit and cannot be mistaken for an id.
func validName(s string) bool {
	if len(s) == 0 || len(s) > maxNameLen || validID(s) {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case i > 0 && (c == '.' || c == '-' || c == '_'):
		default:
			return false
		}
	}
	return true
}

// validAddresses checks a list of host:port addresses of the master.
func validAddresses(list []string) error {
	if len(list) > maxAddresses {
		return fmt.Errorf("fleet: more than %d master addresses", maxAddresses)
	}
	for _, a := range list {
		if !validAddress(a) {
			return fmt.Errorf("fleet: %q is not a host:port address", a)
		}
	}
	return nil
}

func validAddress(a string) bool {
	if len(a) > maxAddressLen {
		return false
	}
	host, port, err := net.SplitHostPort(a)
	if err != nil || host == "" || len(port) == 0 || len(port) > 5 {
		return false
	}
	n := 0
	for i := 0; i < len(port); i++ {
		if port[i] < '0' || port[i] > '9' {
			return false
		}
		n = n*10 + int(port[i]-'0')
	}
	if n < 1 || n > 65535 {
		return false
	}
	for i := 0; i < len(host); i++ {
		switch c := host[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '-' || c == '_' || c == ':' || c == '%': // a name, an IPv4 or an IPv6 address with its zone
		default:
			return false
		}
	}
	return true
}

// pin is one machine's fingerprint, decoded for the handshake's comparison.
type pin struct {
	sum     [sha256.Size]byte
	machine *Machine
}

// Store is the master's fleet.json: its machines, the addresses children
// dial, and the enrollment codes it has given out. All its methods are safe
// for concurrent use, and several processes may hold a Store of one file.
//
// A lookup answers from memory after one stat that tells whether the file
// changed. While the file cannot be read every lookup fails with
// ErrUnreadable. A change re-reads the file under a lock every Tenant process
// takes, applies itself to what it read, and replaces the file whole.
type Store struct {
	path string
	// now and watchEvery are replaced by tests.
	now        func() time.Time
	watchEvery time.Duration

	wmu sync.Mutex // one change at a time in this process; taken before mu

	mu    sync.Mutex
	data  *fileData // what was last read; replaced, never changed in place
	pins  []pin
	stamp stamp
	err   error // why the file cannot be read; nil when it can
}

// stamp is what one stat says about a version of the file.
type stamp struct {
	mod    time.Time
	size   int64
	exists bool
}

func (a stamp) same(b stamp) bool {
	return a.exists == b.exists && a.size == b.size && a.mod.Equal(b.mod)
}

// StorePath is fleet.json under the master's config dir.
func StorePath(dir string) string { return filepath.Join(dir, storeFileName) }

// Open returns the store for the fleet.json in dir. A file that is not
// there is an empty fleet. A file that cannot be read does not fail Open: the
// store reports it from Err and from every lookup, and recovers by itself
// once the file can be read.
func Open(dir string) *Store {
	s := &Store{path: StorePath(dir), now: time.Now, watchEvery: time.Second, data: emptyData()}
	s.mu.Lock()
	s.refreshLocked(false)
	s.mu.Unlock()
	return s
}

// readStore reads fleet.json as it is right now. With lock it takes the
// file's lock shared for the read; a caller that holds the lock passes false.
func readStore(path string, lock bool) (*fileData, stamp, error) {
	if lock {
		// No fleet was ever written here if even its lock file is missing: a
		// Tenant without a fleet leaves nothing behind for having looked.
		// Once there is a lock file, only a stat made under the lock says
		// whether fleet.json is there, since a save is replacing it.
		if _, err := os.Lstat(path + ".lock"); errors.Is(err, fs.ErrNotExist) {
			if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
				return emptyData(), stamp{}, nil
			}
		}
		// Shared with other readers, excluded by a writer: on Windows a file
		// that is open cannot be replaced, so an unlocked read would make a
		// writer's save fail.
		l, err := datalock.File(path, false)
		if err != nil {
			return nil, stamp{}, err
		}
		defer l.Release()
	}
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return emptyData(), stamp{}, nil
	}
	if err != nil {
		return nil, stamp{}, err
	}
	st := stamp{mod: fi.ModTime(), size: fi.Size(), exists: true}
	if fi.Size() > maxStoreBytes {
		return nil, st, fmt.Errorf("fleet.json is larger than %d bytes", maxStoreBytes)
	}
	b, err := secrets.ReadFile(path)
	if err != nil {
		return nil, stamp{}, err
	}
	d, err := parseStore(b)
	return d, st, err
}

// refreshLocked brings memory up to date with the file. Without force it
// reads only when a stat says the file changed. Caller holds s.mu.
func (s *Store) refreshLocked(force bool) {
	if !force {
		fi, err := os.Lstat(s.path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if !s.stamp.exists && s.err == nil {
				return
			}
		case err == nil:
			if s.stamp.same(stamp{mod: fi.ModTime(), size: fi.Size(), exists: true}) {
				return // the version in memory, or the one that did not parse
			}
		}
	}
	d, st, err := readStore(s.path, true)
	if err != nil {
		// Nothing from the last good read stays usable.
		s.data, s.pins, s.stamp, s.err = emptyData(), nil, st, err
		return
	}
	pins := make([]pin, 0, len(d.Machines))
	for _, m := range d.Machines {
		sum, _ := parseFingerprint(m.Fingerprint) // parseStore checked it
		pins = append(pins, pin{sum: sum, machine: m})
	}
	s.data, s.pins, s.stamp, s.err = d, pins, st, nil
}

// view is the fleet as of now, or the reason it cannot be read. What it
// returns is shared and must not be changed.
func (s *Store) view() (*fileData, []pin, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked(false)
	if s.err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrUnreadable, s.err)
	}
	return s.data, s.pins, nil
}

// Err is nil while fleet.json can be read (or is not there), and otherwise
// says why not, wrapped in ErrUnreadable.
func (s *Store) Err() error {
	_, _, err := s.view()
	return err
}

// Machines lists every machine, revoked ones included, by name.
func (s *Store) Machines() ([]Machine, error) {
	d, _, err := s.view()
	if err != nil {
		return nil, err
	}
	out := make([]Machine, 0, len(d.Machines))
	for _, m := range d.Machines {
		out = append(out, *m)
	}
	slices.SortFunc(out, func(a, b Machine) int {
		return cmp.Or(strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), strings.Compare(a.ID, b.ID))
	})
	return out, nil
}

// Machine returns the machine with that id.
func (s *Store) Machine(id string) (Machine, error) {
	d, _, err := s.view()
	if err != nil {
		return Machine{}, err
	}
	m, ok := d.Machines[id]
	if !ok {
		return Machine{}, ErrUnknown
	}
	return *m, nil
}

// ByName returns the machine that carries that name now. Names are compared
// without regard to case; a revoked machine no longer holds its name.
func (s *Store) ByName(name string) (Machine, error) {
	d, _, err := s.view()
	if err != nil {
		return Machine{}, err
	}
	if m := d.named(name, ""); m != nil {
		return *m, nil
	}
	return Machine{}, ErrUnknown
}

// named is the machine that is not revoked, is not except, and has that name.
func (d *fileData) named(name, except string) *Machine {
	for id, m := range d.Machines {
		if id != except && m.State != StateRevoked && strings.EqualFold(m.Name, name) {
			return m
		}
	}
	return nil
}

// Identify returns the machine a TLS connection belongs to, from the
// certificate the handshake verified. Pass r.TLS in an HTTP handler. It
// answers for a pending, confirmed or paused machine, and fails with
// ErrUnknown for a certificate that is not pinned, ErrRevoked for a revoked
// machine, and ErrUnreadable while fleet.json cannot be read.
//
// Knowing the machine is not permission to call it: see Check.
func (s *Store) Identify(cs *tls.ConnectionState) (Machine, error) {
	leaf, err := presented(cs)
	if err != nil {
		return Machine{}, err
	}
	_, pins, err := s.view()
	if err != nil {
		return Machine{}, err
	}
	// Every pinned fingerprint is compared, whichever matches, so how long
	// this takes says nothing about which certificates are pinned.
	sum := sha256.Sum256(leaf.Raw)
	found := -1
	for i := range pins {
		found = subtle.ConstantTimeSelect(subtle.ConstantTimeCompare(pins[i].sum[:], sum[:]), i, found)
	}
	if found < 0 {
		return Machine{}, ErrUnknown
	}
	m := *pins[found].machine
	if m.State == StateRevoked {
		return Machine{}, ErrRevoked
	}
	return m, nil
}

// Check says whether calls may be made to the machine right now: nil for a
// confirmed machine, and otherwise ErrPending, ErrPaused, ErrRevoked,
// ErrUnknown or ErrUnreadable. The link asks before it upgrades a connection
// and again for every message.
func (s *Store) Check(id string) error {
	m, err := s.Machine(id)
	if err != nil {
		return err
	}
	switch m.State {
	case StateConfirmed:
		return nil
	case StatePending:
		return ErrPending
	case StatePaused:
		return ErrPaused
	}
	return ErrRevoked
}

// Addresses are the host:port addresses children dial the master at.
func (s *Store) Addresses() ([]string, error) {
	d, _, err := s.view()
	if err != nil {
		return nil, err
	}
	return slices.Clone(d.Addresses), nil
}

// EnrollOpen reports whether the enroll endpoint is switched on. It is off
// while fleet.json cannot be read.
func (s *Store) EnrollOpen() bool {
	d, _, err := s.view()
	return err == nil && !d.EnrollOff
}

// Watch tells fn the state of every machine, and from then on each state
// that is no longer what fn was last told, until ctx ends. It is how the link
// learns that a machine it holds a socket for was revoked or paused,
// whichever process did it: fn then closes the socket. A machine whose record
// is gone is reported as StateRevoked.
//
// Watch reads the file once a second whether or not it looks changed, so a
// change is reported within that second. fn runs on Watch's goroutine, one
// call at a time, and may call the store. While the file cannot be read
// nothing is reported; Check already refuses every call.
func (s *Store) Watch(ctx context.Context, fn func(id string, state State)) {
	known := map[string]State{}
	tick := time.NewTicker(s.watchEvery)
	defer tick.Stop()
	for {
		if now := s.states(true); now != nil {
			for id, state := range now {
				if known[id] != state {
					fn(id, state)
				}
			}
			for id, state := range known {
				if _, ok := now[id]; !ok && state != StateRevoked {
					fn(id, StateRevoked)
				}
			}
			known = now
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// states is every machine's state, or nil while the file cannot be read.
func (s *Store) states(force bool) map[string]State {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked(force)
	if s.err != nil {
		return nil
	}
	out := make(map[string]State, len(s.data.Machines))
	for id, m := range s.data.Machines {
		out[id] = m.State
	}
	return out
}

// errUnchanged is returned by a change that found nothing to do.
var errUnchanged = errors.New("unchanged")

// mutate is the one way fleet.json is written. Under the file's exclusive
// lock it reads the file as it is now, lets fn change that, and replaces the
// file; so a change made by another process a moment ago is kept, not
// overwritten with what this process had in memory. A file that cannot be
// read is never written over.
func (s *Store) mutate(fn func(d *fileData) error) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	err := s.write(fn)
	// Memory follows the file, read back the way every other read is.
	s.mu.Lock()
	s.refreshLocked(true)
	s.mu.Unlock()
	if errors.Is(err, errUnchanged) {
		return nil
	}
	return err
}

func (s *Store) write(fn func(d *fileData) error) error {
	lock, err := datalock.File(s.path, true)
	if err != nil {
		return fmt.Errorf("fleet: lock fleet.json: %w", err)
	}
	defer lock.Release()
	d, prev, err := readStore(s.path, false)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnreadable, err)
	}
	if err := fn(d); err != nil {
		return err
	}
	d.prune(s.now())
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("fleet: encode fleet.json: %w", err)
	}
	if b = append(b, '\n'); len(b) > maxStoreBytes {
		return fmt.Errorf("fleet: fleet.json would grow past %d bytes", maxStoreBytes)
	}
	if err := writePrivate(s.path, b); err != nil {
		return fmt.Errorf("fleet: save fleet.json: %w", err)
	}
	// A version is dated after the one it replaces, even when both were
	// written inside one tick of the file system's clock: the stat a lookup
	// makes then always tells them apart.
	if fi, err := os.Lstat(s.path); err == nil && prev.exists && !fi.ModTime().After(prev.mod) {
		_ = os.Chtimes(s.path, time.Time{}, prev.mod.Add(time.Millisecond))
	}
	return nil
}

// writePrivate replaces path with an owner-only file holding b, whole or not
// at all.
//
// On Windows a file cannot be replaced at an instant when anything has it
// open: a virus scanner reading the last version, or another process's stat.
// The rename then fails with "access is denied" or a sharing violation
// (seen about once in a hundred saves with four processes on one file,
// 2026-11-18). The instant passes, so the save is tried again, for about a
// second in all, before the caller is told.
func writePrivate(path string, b []byte) error {
	const sharingViolation = syscall.Errno(32) // ERROR_SHARING_VIOLATION
	for wait := time.Millisecond; ; wait *= 2 {
		err := secrets.WriteFile(path, b)
		if err == nil || runtime.GOOS != "windows" || wait > 512*time.Millisecond ||
			!(errors.Is(err, fs.ErrPermission) || errors.Is(err, sharingViolation)) {
			return err
		}
		time.Sleep(wait)
	}
}

// change applies fn to one machine.
func (s *Store) change(id string, fn func(d *fileData, m *Machine) error) error {
	return s.mutate(func(d *fileData) error {
		m, ok := d.Machines[id]
		if !ok {
			return ErrUnknown
		}
		return fn(d, m)
	})
}

// SetAddresses replaces the host:port addresses children dial the master at.
// Enrollment codes made from now on carry them.
func (s *Store) SetAddresses(addresses []string) error {
	if err := validAddresses(addresses); err != nil {
		return err
	}
	return s.mutate(func(d *fileData) error {
		if slices.Equal(d.Addresses, addresses) {
			return errUnchanged
		}
		d.Addresses = slices.Clone(addresses)
		return nil
	})
}

// validRecovery accepts no recovery key, or an age X25519 recipient with,
// optionally, when it was proven. A proof of no key is not one.
func validRecovery(recipient string, proven time.Time) error {
	if recipient == "" {
		if !proven.IsZero() {
			return errors.New("fleet: a recovery key is recorded as proven and there is none")
		}
		return nil
	}
	if _, err := age.ParseX25519Recipient(recipient); err != nil || recipient != strings.ToLower(recipient) {
		return errors.New("fleet: the recovery key is not an age X25519 recipient")
	}
	return nil
}

// Recovery is the public half of the fleet's recovery identity, and when the
// operator proved they hold the identity; "" while the fleet has none.
func (s *Store) Recovery() (recipient string, proven time.Time, err error) {
	d, _, err := s.view()
	if err != nil {
		return "", time.Time{}, err
	}
	return d.Recovery, d.RecoveryProven, nil
}

// SetRecovery records the public half of the recovery identity and when it
// was proven. It is written once a test decryption has opened with the
// identity the operator gave back, never before.
func (s *Store) SetRecovery(recipient string, proven time.Time) error {
	if err := validRecovery(recipient, proven); err != nil {
		return err
	}
	return s.mutate(func(d *fileData) error {
		d.Recovery, d.RecoveryProven = recipient, proven.UTC()
		return nil
	})
}

// SetEnrollOpen switches the enroll endpoint on or off, in every process
// that serves it. It is on until switched off. While it is off no code can
// be spent and MasterTLS lets no certificate through that is not pinned.
func (s *Store) SetEnrollOpen(open bool) error {
	return s.mutate(func(d *fileData) error {
		if d.EnrollOff == !open {
			return errUnchanged
		}
		d.EnrollOff = !open
		return nil
	})
}

// Rename gives a machine another name. The id and everything kept under it
// stay as they are.
func (s *Store) Rename(id, name string) error {
	if !validName(name) {
		return errInvalidName(name)
	}
	return s.change(id, func(d *fileData, m *Machine) error {
		if m.State == StateRevoked {
			return ErrRevoked
		}
		if d.named(name, id) != nil || d.reserved(name, s.now()) {
			return fmt.Errorf("fleet: the name %q is taken", name)
		}
		if m.Name == name {
			return errUnchanged
		}
		m.Name = name
		return nil
	})
}

func errInvalidName(name string) error {
	return fmt.Errorf("fleet: %q is not a machine name: use up to %d letters, digits, '.', '-' or '_', starting with a letter or digit", name, maxNameLen)
}

// Revoke ends a machine: its certificate is refused from the next handshake
// on, and Watch tells the link to close the socket it holds. The record and
// its id stay until Remove. Revoking a revoked machine does nothing.
func (s *Store) Revoke(id string) error {
	return s.change(id, func(_ *fileData, m *Machine) error {
		if m.State == StateRevoked {
			return errUnchanged
		}
		m.State, m.RevokedAt = StateRevoked, s.now().UTC()
		return nil
	})
}

// Remove deletes the record of a revoked machine, for an operator who does
// not keep its rules and memory. Its id can then no longer be adopted.
func (s *Store) Remove(id string) error {
	return s.change(id, func(d *fileData, m *Machine) error {
		if m.State != StateRevoked {
			return errors.New("fleet: only a revoked machine can be removed")
		}
		delete(d.Machines, id)
		return nil
	})
}

// Pause stops calls to a confirmed machine until Resume. Its certificate
// stays pinned.
func (s *Store) Pause(id string) error {
	return s.setPaused(id, StateConfirmed, StatePaused)
}

// Resume lets calls to a paused machine go on.
func (s *Store) Resume(id string) error {
	return s.setPaused(id, StatePaused, StateConfirmed)
}

func (s *Store) setPaused(id string, from, to State) error {
	return s.change(id, func(_ *fileData, m *Machine) error {
		switch m.State {
		case to:
			return errUnchanged
		case from:
			m.State = to
			return nil
		}
		return fmt.Errorf("fleet: the machine is %s", m.State)
	})
}

// SetReported records what a machine says about itself now, for example its
// version after an update.
func (s *Store) SetReported(id string, r Reported) error {
	if err := r.validate(); err != nil {
		return err
	}
	return s.change(id, func(_ *fileData, m *Machine) error {
		if m.State == StateRevoked {
			return ErrRevoked
		}
		if m.Reported == r {
			return errUnchanged
		}
		m.Reported = r
		return nil
	})
}
