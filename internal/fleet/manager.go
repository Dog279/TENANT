package fleet

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tenant/internal/datalock"
	"tenant/internal/fleet/shadow"
	"tenant/internal/httplimits"
	"tenant/internal/mcp"
	"tenant/internal/mcp/transport"
	"tenant/internal/model"
)

const holderFileName = "fleet.holder"

// ManagerConfig is what a master needs to hold its fleet.
type ManagerConfig struct {
	// Store is the master's fleet.json and Identity its certificate.
	Store    *Store
	Identity Identity
	// Legacy is the identity this master had before the cnsa profile, when
	// machines still pin it (LoadMasterIdentity): they are served it, and
	// moved off it (rekey.go). Nil serves no such machine.
	Legacy *Identity
	// Addr is the host:port the one fleet port is bound to.
	Addr string
	// Role names this process to another that finds the fleet held: "serve",
	// "tui".
	Role string
	// Refused and Alert are the enroll endpoint's audit (NewEnrollHandler).
	Refused, Alert func(Refusal)
	// Enrolled, when not nil, is told of a machine that has just spent its
	// enrollment code, with the address it came from, before the machine is
	// answered. It reports whether it confirmed the machine there and then
	// (TEN-553); the machine is told, and shows no digits.
	Enrolled func(m Machine, remote string) bool
	// Connected, when not nil, is told the id of each machine whose link is
	// up, once per link, on a goroutine of its own: the master's memory
	// compares its copy with the machine's then (TEN-532).
	Connected func(id string)
}

// Holder is the process that holds a fleet's links.
type Holder struct {
	PID     int       `json:"pid"`
	Role    string    `json:"role,omitempty"`
	Address string    `json:"address"`
	Since   time.Time `json:"since"`
}

// HeldError is what StartManager fails with when another process holds the
// fleet. Holder is that process, by its own account; its PID is zero if it
// has not said yet.
type HeldError struct{ Holder Holder }

func (e *HeldError) Error() string {
	h := e.Holder
	if h.PID == 0 {
		return "fleet: another process holds the fleet"
	}
	who := fmt.Sprintf("process %d", h.PID)
	if h.Role != "" {
		who = fmt.Sprintf("tenant %s (process %d)", clip(h.Role), h.PID)
	}
	return fmt.Sprintf("fleet: %s holds the fleet, on %s since %s", who, clip(h.Address), h.Since.Local().Format(time.DateTime))
}

// MachineStatus is a machine of the fleet and the state of its link.
type MachineStatus struct {
	Machine
	Online bool
	// Since is when the machine connected, or when its link ended. Zero for a
	// machine that has not connected since this master started.
	Since time.Time
	// Link is the link version the machine said it speaks; zero when offline.
	Link     int
	Remote   string // the address it connected from
	InFlight int    // tool calls in flight
	// LastError is why its last link ended or was not made.
	LastError string
	// Arch is the architecture the machine said it runs on in its hello:
	// amd64 or arm64, "" when it did not say (an older child) or is offline.
	Arch string
	// Update is what the machine said about its last update in its hello;
	// nil when it said nothing or is offline. Its word, like Reported.
	Update *UpdateReport
}

// Manager is the master's end of the fleet: the one fleet port, with the
// enroll endpoint and the link endpoint, and one session per connected
// child. One process holds a fleet at a time: StartManager takes a lock
// beside fleet.json that the operating system drops when the process ends,
// and a second process is told who has it (HeldError).
//
// An idle link costs the master one goroutine, blocked on a read. Everything
// a child sends is checked on that goroutine before the session sees it (see
// guard), so no child can make the master start work it has not asked for.
type Manager struct {
	cfg    ManagerConfig
	store  *Store
	tls    *tls.Config
	holder string // the holder file

	// Replaced by tests, before serve.
	ws           transport.WSConfig
	linkVersion  int
	inFlight     int
	callTime     time.Duration
	helloTime    time.Duration
	headerTime   time.Duration
	addressEvery time.Duration

	ctx     context.Context
	cancel  context.CancelFunc
	srv     *http.Server
	ln      net.Listener
	lock    *datalock.Lock
	sem     chan struct{}  // connections that are not a link yet
	wg      sync.WaitGroup // the manager's goroutines, and one count per link until its transport is closed
	started time.Time

	mu       sync.Mutex
	closed   bool
	links    map[string]*link
	greeting map[string]bool // machines with a connection between its upgrade and the end of its hello
	last     map[string]lastSeen
	held     map[string]hold // machines applying an update: the calls for them wait (awaitUpdate)
	holdFor  time.Duration   // replaced by tests, before serve
}

// UpdateHold is the longest the master keeps calls back for a machine that
// said it is applying an update: the time the machine gives the calls it is
// running, the two starts of the new version, and room for the restarts. By
// then the machine has said how the update went, or it is not coming back
// soon.
const UpdateHold = 7 * time.Minute

// holdPoll is how often a call that is kept back looks again.
const holdPoll = 100 * time.Millisecond

// hold is a machine that said it is applying an update.
type hold struct {
	to    string // the version it was sent
	until time.Time
}

type lastSeen struct {
	at  time.Time // when the last link ended
	err string
}

// StartManager takes the fleet, binds cfg.Addr and serves it until Close.
// It fails with a *HeldError when another process holds the fleet.
func StartManager(cfg ManagerConfig) (*Manager, error) {
	m, err := newManager(cfg)
	if err != nil {
		return nil, err
	}
	if err := m.hold(); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		m.release()
		return nil, fmt.Errorf("fleet: listen on %q: %w", cfg.Addr, err)
	}
	m.serve(ln)
	return m, nil
}

func newManager(cfg ManagerConfig) (*Manager, error) {
	tlsCfg, err := MasterTLS(cfg.Identity, cfg.Legacy, cfg.Store)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		cfg: cfg, store: cfg.Store, tls: tlsCfg,
		holder:      filepath.Join(filepath.Dir(cfg.Store.path), holderFileName),
		linkVersion: LinkVersion, inFlight: defaultInFlight, callTime: defaultCallTime, helloTime: helloTime,
		headerTime: httplimits.ReadHeaderTimeout, addressEvery: 2 * time.Second,
		ctx: ctx, cancel: cancel,
		sem:   make(chan struct{}, maxPreUpgrade),
		links: map[string]*link{}, greeting: map[string]bool{}, last: map[string]lastSeen{},
		held: map[string]hold{}, holdFor: UpdateHold,
	}, nil
}

// hold takes the fleet for this process, or says who has it.
func (m *Manager) hold() error {
	lock, err := datalock.ExclusiveFile(m.holder + ".lock")
	if errors.Is(err, datalock.ErrBusy) {
		held := &HeldError{}
		if b, err := os.ReadFile(m.holder); err == nil && len(b) <= 4<<10 {
			_ = json.Unmarshal(b, &held.Holder)
		}
		return held
	}
	if err != nil {
		return fmt.Errorf("fleet: take the fleet: %w", err)
	}
	m.lock = lock
	return nil
}

func (m *Manager) release() {
	m.cancel()
	_ = os.Remove(m.holder)
	_ = m.lock.Release()
}

// label is a machine's name for the fleet log, or its id when fleet.json
// cannot say.
func (m *Manager) label(id string) string {
	if mach, err := m.store.Machine(id); err == nil && mach.Name != "" {
		return mach.Name
	}
	return id
}

// serverErrors files the fleet port's HTTP server's own error log (a refused
// TLS handshake, mostly) in the fleet log.
type serverErrors struct{ ctx context.Context }

func (w serverErrors) Write(p []byte) (int, error) {
	ServerError.Log(w.ctx, "err", strings.TrimSpace(string(p)))
	return len(p), nil
}

// serve starts the manager on a listener it now owns.
func (m *Manager) serve(ln net.Listener) {
	m.ln, m.started = ln, time.Now()
	b, _ := json.Marshal(Holder{PID: os.Getpid(), Role: m.cfg.Role, Address: ln.Addr().String(), Since: m.started.UTC().Truncate(time.Second)})
	if err := writePrivate(m.holder, append(b, '\n')); err != nil {
		HolderNotRecorded.Log(m.ctx, "err", err)
	}

	mux := http.NewServeMux()
	mux.Handle(EnrollPath, newEnrollHandler(m.store, m.cfg.Refused, m.cfg.Alert, m.cfg.Enrolled))
	mux.HandleFunc(LinkPath, m.serveLink)
	m.srv = &http.Server{
		Handler:      mux,
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){}, // no HTTP/2: a link is an HTTP/1.1 upgrade
		ConnState: func(_ net.Conn, s http.ConnState) {
			if s == http.StateHijacked || s == http.StateClosed {
				<-m.sem
			}
		},
	}
	httplimits.Configure(m.srv)
	// Nothing on this port streams, so the whole of a request and the whole
	// of an answer have a time limit too: a connection has headerTime to
	// finish its handshake and its request line and headers, and a refusal
	// that long again to be read. An upgraded connection leaves the server
	// and lives by the transport's deadlines.
	m.srv.ReadHeaderTimeout = m.headerTime
	m.srv.ReadTimeout, m.srv.WriteTimeout, m.srv.IdleTimeout = 3*m.headerTime, 3*m.headerTime, 3*m.headerTime
	m.srv.ErrorLog = log.New(serverErrors{m.ctx}, "", 0) // refused handshakes are worth a line
	// Seeded with the list as it is: every child is sent it when it connects.
	pushed, _ := m.store.Addresses()
	m.wg.Add(3)
	go func() {
		defer m.wg.Done()
		_ = m.srv.Serve(tls.NewListener(limited{ln, m}, m.tls))
	}()
	go func() {
		defer m.wg.Done()
		m.store.Watch(m.ctx, m.onState)
	}()
	go func() {
		defer m.wg.Done()
		tick := time.NewTicker(m.addressEvery)
		defer tick.Stop()
		for {
			select {
			case <-m.ctx.Done():
				return
			case <-tick.C:
			}
			// One stat while fleet.json is unchanged.
			if now, err := m.store.Addresses(); err == nil && !slices.Equal(now, pushed) {
				pushed = now
				m.each(func(l *link) { l.sendAddresses() })
			}
		}
	}()
}

// limited bounds how many connections can be in their handshake or their
// HTTP request at once: a connection is handed to the server only when there
// is a place for it, and the ones behind it wait in the listener's queue. A
// connection gives its place back when it is closed or becomes a link
// (ConnState).
type limited struct {
	net.Listener
	m *Manager
}

func (l limited) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	select {
	case l.m.sem <- struct{}{}:
		return c, nil
	case <-l.m.ctx.Done():
		_ = c.Close()
		return nil, net.ErrClosed
	}
}

// Addr is the address the fleet port is bound to.
func (m *Manager) Addr() net.Addr { return m.ln.Addr() }

// Close ends every link, closes the port and gives the fleet up. Calls in
// flight end with ErrOutcomeUnknown.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.mu.Unlock()
	m.cancel()
	_ = m.srv.Close()
	m.each(func(l *link) { l.end(errors.New("the master closed the fleet")) })
	m.wg.Wait()
	m.release()
	return nil
}

// each runs fn for every link, at once, and waits.
func (m *Manager) each(fn func(*link)) {
	m.mu.Lock()
	links := make([]*link, 0, len(m.links))
	for _, l := range m.links {
		links = append(links, l)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, l := range links {
		wg.Go(func() { fn(l) })
	}
	wg.Wait()
}

// Machines lists every machine of the fleet with the state of its link.
func (m *Manager) Machines() ([]MachineStatus, error) {
	machines, err := m.store.Machines()
	if err != nil {
		return nil, err
	}
	out := make([]MachineStatus, len(machines))
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, mach := range machines {
		seen := m.last[mach.ID]
		out[i] = MachineStatus{Machine: mach, Since: seen.at, LastError: seen.err}
		if l := m.links[mach.ID]; l != nil {
			out[i].Online, out[i].Since, out[i].Link, out[i].Remote, out[i].InFlight = true, l.connected, l.version, l.remote, len(l.slots)
			out[i].Arch, out[i].Update = l.arch, l.update
		}
	}
	return out, nil
}

// live is the link to a machine calls may be made to right now.
func (m *Manager) live(id string, toolCall bool) (*link, error) {
	if err := m.store.Check(id); err != nil {
		return nil, err
	}
	m.mu.Lock()
	l, seen := m.links[id], m.last[id]
	m.mu.Unlock()
	if l != nil && l.sess.Err() != nil {
		l, seen = nil, lastSeen{at: time.Now()} // it is ending this moment
	}
	if l == nil {
		return nil, &OfflineError{Machine: id, Since: seen.at, Started: m.started}
	}
	if toolCall && l.legacy {
		return nil, fmt.Errorf("%w: %w", ErrIncompatible, ErrLegacy)
	}
	if toolCall && !speaks(m.linkVersion, l.version) {
		return nil, fmt.Errorf("%w: %s", ErrIncompatible, incompatible("the master", m.linkVersion, l.version))
	}
	return l, nil
}

// Tools asks a machine which tools it serves right now. Names are checked;
// descriptions and schemas are the machine's own text.
func (m *Manager) Tools(ctx context.Context, id string) ([]model.ToolSpec, error) {
	if err := m.awaitUpdate(ctx, id); err != nil {
		return nil, err
	}
	l, err := m.live(id, true)
	if err != nil {
		return nil, err
	}
	var res toolsResult
	if err := l.request(ctx, requestTime, methodToolsList, nil, &res); err != nil {
		return nil, err
	}
	if len(res.Tools) > maxTools {
		return nil, fmt.Errorf("fleet: machine %s lists more than %d tools", id, maxTools)
	}
	seen := map[string]bool{}
	for i := range res.Tools {
		t := &res.Tools[i]
		if !validToolName(t.Name) || seen[t.Name] {
			return nil, fmt.Errorf("fleet: machine %s lists a tool whose name is not valid or is there twice", id)
		}
		seen[t.Name] = true
		t.Description, _ = cut(t.Description, maxToolDesc)
		if len(t.Parameters) == 0 || t.Parameters[0] != '{' {
			t.Parameters = json.RawMessage(`{"type":"object"}`)
		}
	}
	return res.Tools, nil
}

// Call runs one tool on a machine and waits for its result. The call gets an
// id, which the result or the *CallError carries. It fails at once for a
// machine that is offline (*OfflineError), not confirmed, at its limit of
// calls in flight (ErrBusy), or unreadable in fleet.json. A context without
// a deadline gets one of fifteen minutes; when the context ends the machine is
// told to stop. A link that drops mid-call ends it with ErrOutcomeUnknown,
// and nothing is sent again. A call for a machine that is applying an update
// is kept back, unsent, until the machine says how the update went
// (awaitUpdate).
func (m *Manager) Call(ctx context.Context, id string, c Call) (Result, error) {
	if err := m.awaitUpdate(ctx, id); err != nil {
		return Result{}, err
	}
	l, err := m.live(id, true)
	if err != nil {
		return Result{}, err
	}
	select {
	case l.slots <- struct{}{}:
	default:
		return Result{}, fmt.Errorf("%w: %d", ErrBusy, cap(l.slots))
	}
	defer func() { <-l.slots }()
	ctx, cancel := withDeadline(ctx, m.callTime)
	defer cancel()

	callID := newCallID()
	l.mu.Lock()
	l.calls[callID] = &call{ctx: ctx, confirm: c.Confirm}
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.calls, callID)
		l.mu.Unlock()
	}()

	deadline, _ := ctx.Deadline()
	var res callResult
	err = l.request(ctx, 0, methodToolsCall, callParams{CallID: callID, Name: c.Tool, Arguments: c.Arguments,
		TimeoutMS: max(1, time.Until(deadline).Milliseconds())}, &res)
	if err != nil {
		return Result{CallID: callID}, &CallError{Machine: id, CallID: callID, Err: err}
	}
	return res.result(callID), nil
}

// result bounds what a machine sent as a tool result, whatever it promised.
func (r callResult) result(callID string) Result {
	text, clipped := cut(r.Text, maxResultText)
	res := Result{CallID: callID, Text: text, IsError: r.IsError, Clipped: r.Clipped || clipped}
	if _, err := parseDigest(r.Audit, 32); err == nil {
		res.Audit = r.Audit
	}
	return res
}

// Result asks a machine for the result of an earlier call, by its id: the
// way to learn what became of a call that ended with ErrOutcomeUnknown. It
// fails with ErrCallRunning while the call runs, and with ErrCallUnknown
// when the machine never got it or no longer keeps it.
func (m *Manager) Result(ctx context.Context, id, callID string) (Result, error) {
	if !validCallID(callID) {
		return Result{}, ErrCallUnknown
	}
	l, err := m.live(id, true)
	if err != nil {
		return Result{}, err
	}
	var res resultResult
	if err := l.request(ctx, requestTime, methodResult, resultParams{CallID: callID}, &res); err != nil {
		return Result{}, err
	}
	switch res.State {
	case callDone:
		return res.result(callID), nil
	case callRunning:
		return Result{CallID: callID}, ErrCallRunning
	}
	return Result{CallID: callID}, ErrCallUnknown
}

// Update sends a machine one piece of a new Tenant. It is the one operation
// besides Ping that a machine of any link version is sent.
func (m *Manager) Update(ctx context.Context, id string, u Update) (UpdateAck, error) {
	if err := u.validate(); err != nil {
		return UpdateAck{}, err
	}
	l, err := m.live(id, false)
	if err != nil {
		return UpdateAck{}, err
	}
	var ack UpdateAck
	err = l.request(ctx, requestTime, methodUpdate, u, &ack)
	if err == nil && ack.Applying {
		// From here the machine restarts into the new version, more than once.
		m.mu.Lock()
		m.held[id] = hold{to: u.Version, until: time.Now().Add(m.holdFor)}
		m.mu.Unlock()
	}
	return ack, err
}

// awaitUpdate keeps a call back while its machine is applying an update
// (TEN-552). A machine that takes an update finishes the calls it is running
// and restarts into the new version; a call that reached it in between would
// end with the process that had it, failed as offline or with an outcome
// nobody knows. So from the machine's "applying" until a link on which it
// says how that update went (done, or rolled back), a call waits here and is
// not sent. It goes on when the machine has said so or the hold's limit has
// passed, and whatever the machine is then decides it as it does any call. It
// fails when ctx ends first, or when fleet.json no longer lets the machine
// have a link.
//
// A link that drops and comes back between "applying" and the restart is
// read like any other: if what the machine says then is of an earlier try at
// the same version, the hold ends early and a call may still meet the
// restart, as every call did before there was a hold.
func (m *Manager) awaitUpdate(ctx context.Context, id string) error {
	var (
		since time.Time
		to    string
		tick  *time.Ticker
	)
	for {
		m.mu.Lock()
		h, ok := m.held[id]
		if ok && !time.Now().Before(h.until) {
			delete(m.held, id)
			ok = false
		}
		m.mu.Unlock()
		if !ok {
			if !since.IsZero() {
				CallHeld.Log(m.ctx, "name", m.label(id), "machine", id, "to", to, "waited", time.Since(since).Round(time.Millisecond))
			}
			return nil
		}
		if err := m.store.Check(id); err != nil {
			return err
		}
		if tick == nil {
			// ponytail: each held call polls; one broadcast for all of them if
			// held calls are ever many.
			since, to, tick = time.Now(), h.to, time.NewTicker(holdPoll)
			defer tick.Stop()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("fleet: the call was not sent: the machine is applying an update to %s and has not said how it went: %w", h.to, ctx.Err())
		case <-m.ctx.Done():
			return nil // the fleet is closing: live says so
		case <-tick.C:
		}
	}
}

// Ping asks a machine's session whether it answers, and how fast.
func (m *Manager) Ping(ctx context.Context, id string) (time.Duration, error) {
	l, err := m.live(id, false)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	err = l.request(ctx, requestTime, methodPing, nil, nil)
	return time.Since(start), err
}

// --- the link endpoint ---

func (m *Manager) serveLink(w http.ResponseWriter, r *http.Request) {
	// Who is asking comes from the certificate the handshake checked, and
	// whether it may have a link from fleet.json as it is this moment.
	mach, err := m.store.Identify(r.TLS)
	if err == nil {
		err = m.store.Check(mach.ID)
	}
	if err != nil {
		status, answer := statusUnknown, "this machine is not part of the fleet"
		switch {
		case errors.Is(err, ErrPending):
			status, answer = statusWaiting, WaitingForConfirmation
		case errors.Is(err, ErrPaused):
			status, answer = statusPaused, "paused"
		case errors.Is(err, ErrRevoked):
			status, answer = statusRevoked, "revoked"
		case errors.Is(err, ErrUnreadable):
			status, answer = statusUnavailable, "the fleet is not available"
		}
		w.Header().Set("Connection", "close")
		http.Error(w, answer, status)
		return
	}
	// A machine that connects with the certificate it said it is moving to
	// has moved (rekey.go): that certificate is its only one from here on.
	if mach.Next != "" {
		if leaf, perr := presented(r.TLS); perr == nil && fingerprintOf(leaf.Raw) == mach.Next {
			if err := m.store.promote(mach.ID, mach.Next); err != nil {
				RekeyRefused.Log(m.ctx, "name", mach.Name, "machine", mach.ID, "err", err)
				w.Header().Set("Connection", "close")
				http.Error(w, "the fleet is not available", statusUnavailable)
				return
			}
			Rekeyed.Log(m.ctx, "name", mach.Name, "machine", mach.ID, "certificate", mach.Next)
			mach.Fingerprint, mach.Next = mach.Next, ""
		}
	}
	// One connection per machine at a time gets as far as the hello: a machine
	// holds its link and at most one more that is not a link yet, however many
	// times it dials.
	m.mu.Lock()
	if m.closed || m.greeting[mach.ID] {
		m.mu.Unlock()
		w.Header().Set("Connection", "close")
		http.Error(w, "another connection from this machine is saying hello", http.StatusTooManyRequests)
		return
	}
	m.greeting[mach.ID] = true
	m.wg.Add(1) // given back when the link's transport is closed (guard.Close)
	m.mu.Unlock()
	greeted := func() {
		m.mu.Lock()
		delete(m.greeting, mach.ID)
		m.mu.Unlock()
	}
	t, err := transport.UpgradeWSServer(w, r, m.ws)
	if err != nil {
		greeted()
		m.wg.Done()
		LinkNotUpgraded.Log(m.ctx, "machine", mach.ID, "remote", r.RemoteAddr, "err", err)
		return
	}
	l := &link{m: m, id: mach.ID, remote: r.RemoteAddr, legacy: mach.Legacy(), slots: make(chan struct{}, m.inFlight), calls: map[string]*call{}}
	l.sess = mcp.NewSession(&guard{Transport: t, l: l, tokens: requestBurst, refilled: time.Now()}, l)
	l.sess.Start(m.ctx)

	err = l.hello(mach)
	greeted()
	if err != nil {
		l.end(fmt.Errorf("no hello: %w", err))
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		l.end(errors.New("the master closed the fleet"))
		return
	}
	old := m.links[l.id]
	m.links[l.id], l.connected = l, time.Now()
	// A machine that says how the update went is over it, and the calls kept
	// back for it go on. A trial run's hello is not that: the trial is stopped
	// as soon as it has said hello.
	if h, ok := m.held[l.id]; ok && l.update != nil && l.update.To == h.to && l.update.State != UpdateTrial {
		delete(m.held, l.id)
	}
	m.mu.Unlock()
	if old != nil {
		// The machine connected again: the link it had is one it has given up.
		old.end(errors.New("replaced by a newer connection from the machine"))
	}
	// A revoke or a pause since the check above found no link to close.
	if err := m.store.Check(l.id); err != nil && !errors.Is(err, ErrUnreadable) {
		l.refuse(err, true)
		return
	}
	Connected.Log(m.ctx, "name", m.label(l.id), "machine", l.id, "remote", l.remote, "link", l.version)
	if m.cfg.Connected != nil {
		go m.cfg.Connected(l.id)
	}
	l.sendAddresses()
}

// onState is told by Store.Watch of every machine that is not what it was:
// a link to a machine that is no longer confirmed is closed.
func (m *Manager) onState(id string, state State) {
	if state == StateConfirmed {
		return
	}
	m.mu.Lock()
	l := m.links[id]
	m.mu.Unlock()
	if l == nil {
		return
	}
	err := ErrPending
	switch state {
	case StateRevoked:
		err = ErrRevoked
	case StatePaused:
		err = ErrPaused
	}
	// Not on Watch's goroutine: a machine that is slow to hang up must not
	// hold up the news about the next one.
	m.mu.Lock()
	if !m.closed {
		m.wg.Go(func() { l.refuse(err, true) })
	}
	m.mu.Unlock()
}

// ended records that a link's transport is closed. It runs once per link.
func (m *Manager) ended(l *link) {
	why := l.reason()
	m.mu.Lock()
	seen := m.last[l.id]
	switch m.links[l.id] {
	case l:
		delete(m.links, l.id)
		m.last[l.id] = lastSeen{at: time.Now(), err: why}
	case nil: // it never got as far as being the machine's link
		m.last[l.id] = lastSeen{at: seen.at, err: why}
	}
	m.mu.Unlock()
	Disconnected.Log(m.ctx, "name", m.label(l.id), "machine", l.id, "remote", l.remote, "why", why)
	m.wg.Done()
}

// --- one link ---

// link is the master's end of one child's link.
type link struct {
	m      *Manager
	id     string // the machine
	remote string
	sess   *mcp.Session
	// legacy: the machine made this link with the certificate it had before
	// the cnsa profile. No call is made on it (Manager.live).
	legacy bool

	version   int           // the link version the child said in its hello
	arch      string        // the architecture it said, when it is a known one
	update    *UpdateReport // what it said about its last update, bounded
	connected time.Time     // set when the hello is done, under m.mu

	slots  chan struct{}         // tools/call in flight
	credit atomic.Int64          // results the child may still send: one per request sent
	why    atomic.Pointer[error] // why the master ended the link

	mu    sync.Mutex
	calls map[string]*call // tools/call in flight, by call id
}

// call is one tools/call in flight.
type call struct {
	ctx      context.Context
	confirm  func(ctx context.Context, action, detail string) bool
	confirms int  // how often the child has asked during this call
	asking   bool // a confirm is being decided
}

// request sends the child a request and waits for the answer. ctx gets a
// deadline of d unless it has one. What it returns for a failure:
// the context's error; ErrBusy or ErrIncompatible when the child said so;
// ErrOutcomeUnknown when the link ended first; otherwise the reason the
// request did not go out or its answer could not be read.
func (l *link) request(ctx context.Context, d time.Duration, method string, params, out any) error {
	ctx, cancel := withDeadline(ctx, d)
	defer cancel()
	var raw json.RawMessage
	if params != nil {
		var err error
		if raw, err = encode(params); err != nil {
			return fmt.Errorf("fleet: %s: %w", method, err)
		}
	}
	l.credit.Add(1)
	res, err := l.sess.Call(ctx, method, raw)
	var refused *mcp.Error
	switch {
	case err == nil:
	case errors.As(err, &refused):
		text := fmt.Sprintf("the machine refused %s: %s (%d)", method, clip(refused.Message), refused.Code)
		switch refused.Code {
		case codeBusy:
			return fmt.Errorf("%w: %s", ErrBusy, text)
		case codeIncompatible:
			return fmt.Errorf("%w: %s", ErrIncompatible, text)
		case mcp.ErrMethodNotFound:
			return fmt.Errorf("%w: %s", errNotServed, text)
		case codeStale:
			return fmt.Errorf("%w: %s", shadow.ErrStale, text)
		case codeConflict:
			return fmt.Errorf("%w: %s", shadow.ErrConflict, text)
		case codeNoCopy:
			return fmt.Errorf("%w: %s", shadow.ErrNoCopy, text)
		}
		return errors.New("fleet: " + text)
	case ctx.Err() != nil && errors.Is(err, ctx.Err()):
		return ctx.Err()
	case l.sess.Err() != nil:
		return fmt.Errorf("%w (%s)", ErrOutcomeUnknown, l.reason())
	default:
		return fmt.Errorf("fleet: %s was not sent: %w", method, err)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(res, out); err != nil {
		return fmt.Errorf("fleet: the machine's answer to %s cannot be read: %w", method, err)
	}
	return nil
}

func (l *link) hello(mach Machine) error {
	var res helloResult
	hello := helloParams{Link: l.m.linkVersion}
	if l.legacy {
		hello.Rekey = l.m.cfg.Identity.Fingerprint
	}
	if err := l.request(l.m.ctx, l.m.helloTime, methodHello, hello, &res); err != nil {
		return err
	}
	if res.Link < 1 || res.Link > maxLinkVersion {
		return fmt.Errorf("fleet: the machine claims link version %d", res.Link)
	}
	l.version, l.update = res.Link, res.Update.check()
	if knownArch[res.Arch] {
		l.arch = res.Arch
	}
	// What a machine says about itself is its word, kept to show the operator:
	// its version after an update, for one.
	if rep := res.Reported; rep != mach.Reported && rep.validate() == nil {
		if err := l.m.store.SetReported(l.id, rep); err != nil {
			ReportNotRecorded.Log(l.m.ctx, "name", l.m.label(l.id), "machine", l.id, "err", err)
		}
	}
	return nil
}

// sendAddresses gives the child the master's address list as it is now.
// ponytail: one try per connection and per change of the list; a child that
// fails to take it is sent it again when it reconnects.
func (l *link) sendAddresses() {
	addrs, err := l.m.store.Addresses()
	if err != nil || len(addrs) == 0 {
		return
	}
	if err := l.request(l.m.ctx, requestTime, methodAddresses, addressParams{Addresses: addrs}, nil); err != nil {
		AddressesNotTaken.Log(l.m.ctx, "name", l.m.label(l.id), "machine", l.id, "err", err)
	}
}

// refuse ends the link of a machine that fleet.json no longer lets have one.
// A revoked machine is told first, so that it stops dialing. With wait, the
// machine gets up to a second to hang up itself: a connection closed on data
// the other side has not read yet can lose that data, the notice with it.
// The read loop cannot wait for what only it can read, and passes false. If
// the notice is lost all the same, the machine's next handshake is refused.
func (l *link) refuse(err error, wait bool) {
	l.why.CompareAndSwap(nil, &err)
	if errors.Is(err, ErrRevoked) || errors.Is(err, ErrUnknown) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if l.sess.Notify(ctx, noticeRevoked, nil) == nil && wait {
			select {
			case <-l.sess.Done():
			case <-ctx.Done():
			}
		}
		cancel()
	}
	l.end(err)
}

// end closes the link for a reason of the master's. The first reason stands.
func (l *link) end(why error) {
	l.why.CompareAndSwap(nil, &why)
	_ = l.sess.Close()
}

func (l *link) reason() string {
	if why := l.why.Load(); why != nil {
		return (*why).Error()
	}
	if err := l.sess.Err(); err != nil && !errors.Is(err, mcp.ErrSessionClosed) {
		return err.Error()
	}
	return "the machine closed the link"
}

// HandleRequest serves what a child may ask its master: fleet/confirm,
// fleet/rekey and ping. The guard has let the request through.
func (l *link) HandleRequest(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	switch method {
	case methodPing:
		return emptyObject, nil
	case methodRekey:
		return l.rekey(params)
	case methodConfirm:
		return l.confirm(ctx, params)
	}
	return nil, mcp.NewError(mcp.ErrMethodNotFound, "fleet: a child asks its master for fleet/confirm and ping, and nothing else")
}

// HandleNotification: a child has no notification to send that the session
// does not consume itself, and the guard lets no other through.
func (l *link) HandleNotification(context.Context, string, json.RawMessage) {}

// confirm decides an approval a child asks for. It is valid only for a call
// the master started to this child that is still in flight, one at a time
// per call and not more often than maxConfirms; the decision is the caller's
// of that call, and anything that goes wrong on the way is a denial.
func (l *link) confirm(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	var p confirmParams
	if err := json.Unmarshal(params, &p); err != nil || !validCallID(p.CallID) || !validAction(p.Action) {
		return nil, mcp.NewError(mcp.ErrInvalidParams, "fleet: fleet/confirm needs a call id and an action")
	}
	if err := l.m.store.Check(l.id); err != nil {
		return nil, mcp.NewError(codeUnavailable, "fleet: the master cannot decide right now")
	}
	l.mu.Lock()
	c := l.calls[p.CallID]
	switch {
	case c == nil:
		l.mu.Unlock()
		return nil, mcp.NewError(codeNoCall, "fleet: no such call is in flight to this machine")
	case c.asking || c.confirms >= maxConfirms:
		l.mu.Unlock()
		return nil, mcp.NewError(codeTooMany, "fleet: too many confirmations for one call")
	}
	c.confirms++
	c.asking = true
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		c.asking = false
		l.mu.Unlock()
	}()

	allow := false
	if c.confirm != nil {
		// The call's context, so the decision sees the caller's values and ends
		// with the call; and it ends when the child stops asking or the link
		// drops, which is the request's context.
		cctx, cancel := context.WithCancel(c.ctx)
		defer cancel()
		defer context.AfterFunc(ctx, cancel)()
		detail, _ := cut(p.Detail, maxDetailLen)
		allow = c.confirm(cctx, p.Action, detail) && cctx.Err() == nil
	}
	return encode(confirmResult{Allow: allow})
}

// --- the guard ---

// errStrikes ends the link of a child that keeps sending what the link does
// not have.
var errStrikes = errors.New("fleet: the machine kept sending what the link does not allow")

// guard is the link's transport as the master's session reads it. The
// session starts a goroutine for every request and notification it reads, so
// what a child may make the master do is decided here, on the one goroutine
// that reads, before the session sees a frame:
//
//   - the machine's state is checked for every frame. A machine that is no
//     longer confirmed loses the link; while fleet.json cannot be read,
//     every frame is dropped.
//   - a result is let through only against a request the master sent.
//   - a request or a cancellation is paid for from a bucket (requestBurst,
//     requestsPerSec), which bounds the goroutines one child can cause.
//   - everything else (bytes that are not JSON-RPC, other notifications,
//     anything unpaid) is dropped and counted, and so is a request for a
//     method a child may not call, which is let through to be refused. At
//     maxStrikes the link ends.
type guard struct {
	transport.Transport
	l    *link
	once sync.Once

	// Owned by the one goroutine that calls Recv.
	tokens   float64
	refilled time.Time
	strikes  int
}

func (g *guard) Recv(ctx context.Context) ([]byte, error) {
	for {
		raw, err := g.Transport.Recv(ctx)
		if err != nil {
			return nil, err
		}
		pass, err := g.admit(raw)
		if err != nil {
			return nil, err
		}
		if pass {
			return raw, nil
		}
	}
}

// Close closes the transport and tells the manager the link is over. The
// session calls it when its read loop ends, however it ends.
func (g *guard) Close() error {
	err := g.Transport.Close()
	g.once.Do(func() { g.l.m.ended(g.l) })
	return err
}

func (g *guard) admit(raw []byte) (pass bool, err error) {
	l := g.l
	switch err := l.m.store.Check(l.id); {
	case err == nil:
	case errors.Is(err, ErrUnreadable):
		return false, nil // the fleet refuses everything until fleet.json can be read
	default:
		l.refuse(err, false)
		return false, err
	}
	// Decoded exactly as the session will decode it, so the two cannot
	// disagree about what a frame is.
	var msg mcp.Message
	if json.Unmarshal(raw, &msg) != nil {
		return false, g.strike("bytes that are not a JSON-RPC message")
	}
	switch {
	case msg.IsResponse():
		if l.credit.Add(-1) < 0 {
			l.credit.Add(1)
			return false, g.strike("a result for a request that was not made")
		}
		return true, nil
	case msg.IsRequest(), msg.Method == mcp.MethodCancelled:
		if !g.take() {
			return false, g.strike("more requests than the link allows")
		}
		// fleet/rekey is a legacy link's to ask; from any other machine it is
		// one more thing a child never sends.
		if msg.IsRequest() && msg.Method != methodConfirm && msg.Method != methodPing && !(msg.Method == methodRekey && l.legacy) {
			// Let through to be refused by name, and counted.
			return true, g.strike("a request for " + clip(msg.Method))
		}
		return true, nil
	}
	return false, g.strike("a notification the link does not have")
}

// take pays for one request from the bucket.
func (g *guard) take() bool {
	now := time.Now()
	g.tokens = min(requestBurst, g.tokens+now.Sub(g.refilled).Seconds()*requestsPerSec)
	g.refilled = now
	if g.tokens < 1 {
		return false
	}
	g.tokens--
	return true
}

// strike counts something the link does not allow.
func (g *guard) strike(what string) error {
	g.strikes++
	if g.strikes == 1 {
		NotAllowed.Log(g.l.m.ctx, "name", g.l.m.label(g.l.id), "machine", g.l.id, "what", what)
	}
	if g.strikes < maxStrikes {
		return nil
	}
	g.l.end(fmt.Errorf("%w: %s", errStrikes, what))
	return errStrikes
}
