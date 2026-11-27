package fleet

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gobwas/ws"

	"tenant/internal/cnsa"
	"tenant/internal/fleet/shadow"
	"tenant/internal/mcp"
	"tenant/internal/mcp/transport"
	"tenant/internal/model"
	"tenant/internal/secrets"
)

const childStatusFileName = "child-link.json"

// LinkState is where a child stands with its master.
type LinkState string

const (
	// LinkOffline: no link, and the master could not be reached or the link
	// dropped. The child keeps dialing.
	LinkOffline LinkState = "offline"
	// LinkConnected: the link is up.
	LinkConnected LinkState = "connected"
	// LinkWaiting: enrolled, and the operator has not confirmed this machine
	// on the master yet. The child keeps dialing.
	LinkWaiting LinkState = "waiting"
	// LinkPaused: the operator paused this machine on the master. The child
	// keeps dialing.
	LinkPaused LinkState = "paused"
	// LinkRejected: the master was reached, proved to be the master, and
	// would not have this machine: its certificate is not one the master
	// pins (or the master cannot read its fleet just now). The child keeps
	// dialing, slowly.
	LinkRejected LinkState = "rejected"
	// LinkRevoked: the master said this machine is revoked. The child has
	// stopped and does not dial again with this certificate.
	LinkRevoked LinkState = "revoked"
)

// ChildStatus is the state of a child's link, kept in child-link.json beside
// child.json so that `tenant child status`, another process, can read it. It
// is written when State or Address changes, never on a timer.
type ChildStatus struct {
	State LinkState `json:"state"`
	// Since is when the link came to be in State.
	Since time.Time `json:"since"`
	// Address is the master address this is about, when there is one.
	Address string `json:"address,omitempty"`
	// Detail says why, in words.
	Detail string `json:"detail,omitempty"`
	// Certificate is the fingerprint of this machine's certificate the state
	// is about. A machine enrolled again has a new one, and an old "revoked"
	// no longer applies.
	Certificate string `json:"certificate"`
}

// ChildStatusPath is child-link.json in dir.
func ChildStatusPath(dir string) string { return filepath.Join(dir, childStatusFileName) }

// ReadChildStatus reads what the child running in dir last recorded about
// its link. A child that has never run has no file (fs.ErrNotExist).
func ReadChildStatus(dir string) (ChildStatus, error) {
	var st ChildStatus
	b, err := secrets.Peek(ChildStatusPath(dir))
	if err != nil {
		return st, fmt.Errorf("fleet: child status: %w", err)
	}
	if len(b) > maxChildBytes {
		return st, errors.New("fleet: child status: the file is too large")
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return ChildStatus{}, fmt.Errorf("fleet: child status: %w", err)
	}
	return st, nil
}

// ClientConfig is what a child needs to hold its link.
type ClientConfig struct {
	// Dir holds child.json and the child's identity; the link's status is
	// written there too.
	Dir string
	// Tools is what this machine serves: its tool mux.
	Tools Tools
	// Version and User are reported to the master in the hello: this build,
	// and the account the child runs as.
	Version, User string
	// Update takes one piece of a new Tenant (fleet/update). Nil answers that
	// this machine cannot be updated over the link.
	Update func(ctx context.Context, u Update) (UpdateAck, error)
	// Hello is asked at every hello for what to say about the last update;
	// nil, or a nil report, says nothing. That the master is saying hello is
	// the news an update in progress waits for: the link is back.
	Hello func() *UpdateReport
	// Rekeyed is told, before it happens, that this machine is about to pin
	// its master's new certificate at the master's word (rekey.go): from and
	// to are the old and the new fingerprint. It is where the machine's audit
	// log records it (AuditLog.Rekey); an error leaves the machine as it is.
	// Nil records nothing.
	Rekeyed func(from, to string) error
	// Memory keeps the copies of this machine's memory the master sends
	// (TEN-532): encrypted snapshots the child cannot read. Nil answers the
	// memory methods as a child that does not know them would.
	Memory *shadow.ChildStore
	// Log gets what is worth a line on the machine. Nil is the process logger.
	Log *slog.Logger
}

// Client is the child's end of the link. Run dials the master, serves its
// requests and dials again when the link drops.
//
// While the link is idle it costs one goroutine blocked on a read, beside
// the one that called Run and waits for the link to end; no timer runs, and
// the transport sends one ping per thirty seconds.
type Client struct {
	cfg  ClientConfig
	log  *slog.Logger
	id   Identity
	file *ChildFile

	// Replaced by tests, before Run.
	ws          transport.WSConfig
	linkVersion int
	inFlight    int
	backoffMin  time.Duration
	backoffMax  time.Duration
	dialTime    time.Duration
	dial        func(ctx context.Context, network, addr string) (net.Conn, error)

	runCtx context.Context // Run's: a tool call outlives its link, not the child

	mu      sync.Mutex
	status  ChildStatus
	sess    *mcp.Session        // the link that is up
	pushed  []string            // an address list the master pushed that could not be saved
	stopped bool                // Run is returning: no call starts
	running map[string]struct{} // tools/call in flight, by call id, whatever link they came on
	kept    []keptResult        // finished calls, oldest first
	keptLen int                 // bytes of text in kept
	calls   sync.WaitGroup

	mem memoryEnd // the copies of this machine's memory going in and out
}

type keptResult struct {
	id  string
	at  time.Time
	res callResult
}

// NewClient prepares the child's end from what is in cfg.Dir. A machine
// without an identity or with a child.json that does not load, an empty or
// malformed master fingerprint included, gets no client.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.Tools == nil {
		return nil, errors.New("fleet: a child needs the tools it serves")
	}
	// Loading it checks it, the master's fingerprint included.
	child, err := LoadChild(ChildPath(cfg.Dir))
	if err != nil {
		return nil, err
	}
	id, err := loadChildIdentity(cfg.Dir, child)
	if err != nil {
		return nil, err
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		cfg: cfg, log: log, id: id, file: OpenChild(ChildPath(cfg.Dir)),
		linkVersion: LinkVersion, inFlight: defaultInFlight,
		backoffMin: time.Second, backoffMax: time.Minute, dialTime: 15 * time.Second,
		dial:    (&net.Dialer{}).DialContext,
		running: map[string]struct{}{},
	}, nil
}

// Status is the state of the link right now.
func (c *Client) Status() ChildStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// Running is how many tool calls this machine is in the middle of.
func (c *Client) Running() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.running)
}

// Run holds the link until ctx ends, and returns ctx's error then. It dials
// each of the master's addresses in turn, serves the master's requests, and
// when the link drops dials again, waiting longer after each failure, with
// jitter. A master that says this machine waits for confirmation or is
// paused is asked again; one that refuses the machine's certificate is asked
// again slowly. Only a master that says "revoked" ends Run, with ErrRevoked:
// that is recorded, and Run returns it at once from then on.
//
// A tool call that is running when the link drops keeps running. When Run
// returns, for either reason, the calls still running are told to stop and
// waited for. Call it once.
func (c *Client) Run(ctx context.Context) error {
	if st, err := ReadChildStatus(c.cfg.Dir); err == nil && st.State == LinkRevoked && st.Certificate == c.id.Fingerprint {
		c.mu.Lock()
		c.status = st
		c.mu.Unlock()
		return ErrRevoked
	}
	ctx, cancel := context.WithCancel(ctx)
	c.runCtx = ctx
	defer func() {
		c.mu.Lock()
		c.stopped = true
		c.mu.Unlock()
		cancel() // a revoked child's calls end with it
		c.calls.Wait()
	}()
	wait := c.backoffMin
	for {
		state, addr, detail, served := c.connect(ctx)
		if ctx.Err() != nil {
			c.setStatus(LinkOffline, "", "the child stopped")
			return ctx.Err()
		}
		c.setStatus(state, addr, detail)
		c.log.Debug("fleet: no link to the master", "state", state, "address", addr, "why", detail)
		limit := c.backoffMax
		switch state {
		case LinkRevoked:
			return ErrRevoked
		case LinkWaiting:
			limit = min(limit, 5*time.Second) // the operator is typing the code
		case LinkRejected:
			limit *= 5
		}
		if served {
			wait = c.backoffMin
		}
		pause := time.NewTimer(wait/2 + rand.N(wait/2+1))
		select {
		case <-ctx.Done():
			pause.Stop()
			c.setStatus(LinkOffline, "", "the child stopped")
			return ctx.Err()
		case <-pause.C:
		}
		wait = min(2*wait, limit)
	}
}

// Ping asks the master's session whether it answers.
func (c *Client) Ping(ctx context.Context) error {
	c.mu.Lock()
	sess := c.sess
	c.mu.Unlock()
	if sess == nil {
		return ErrOffline
	}
	ctx, cancel := withDeadline(ctx, requestTime)
	defer cancel()
	_, err := sess.Call(ctx, methodPing, nil)
	return err
}

// setStatus records the link's state, and writes it down when the state or
// the address is not what it was.
//
// ponytail: the file's Detail is the one of the first try in a state; the
// live one is in Status. Write every change if a status reader needs it.
func (c *Client) setStatus(state LinkState, addr, detail string) {
	c.mu.Lock()
	moved := c.status.State != state || c.status.Address != addr
	// Why a machine is offline differs a little with every try; that is not
	// worth a write each time. What a link that is up says about itself is.
	write := moved || (state == LinkConnected && c.status.Detail != detail)
	if moved {
		c.status = ChildStatus{State: state, Since: time.Now().UTC().Truncate(time.Second), Address: addr, Certificate: c.id.Fingerprint}
	}
	c.status.Detail = detail
	st := c.status
	c.mu.Unlock()
	if !write {
		return
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	if err := writePrivate(ChildStatusPath(c.cfg.Dir), append(b, '\n')); err != nil {
		c.log.Warn("fleet: could not record the link's state", "err", err)
	}
}

// connect tries the master's addresses in turn and serves the first link it
// gets until that link ends. It returns the state the child is left in.
// served reports that a link was up.
func (c *Client) connect(ctx context.Context) (state LinkState, addr, detail string, served bool) {
	child, err := c.file.Current()
	if err != nil {
		return LinkOffline, "", err.Error(), false
	}
	c.mu.Lock()
	// A move to a new identity that got as far as child.json and no further
	// (rekey.go) is finished here, as NewClient would at the next start.
	if c.id.Legacy && len(child.MasterFingerprint) == cnsa.FingerprintLen {
		if id, err := finishRekey(c.cfg.Dir); err == nil {
			c.id = id
		}
	}
	id := c.id
	addrs := append(child.MasterAddresses, c.pushed...)
	c.mu.Unlock()
	cfg, err := ChildTLS(id, child.MasterFingerprint)
	if err != nil {
		return LinkOffline, "", err.Error(), false
	}
	var failed []string
	for i, addr := range addrs {
		if slices.Contains(addrs[:i], addr) {
			continue
		}
		t, state, err := c.dialLink(ctx, cfg, addr)
		if t != nil {
			return c.serve(ctx, t, addr, id.Legacy)
		}
		if state != LinkOffline {
			// The master itself answered. No other address says otherwise.
			return state, addr, err.Error(), false
		}
		failed = append(failed, addr+": "+err.Error())
	}
	return LinkOffline, "", strings.Join(failed, "; "), false
}

// dialLink makes one connection to the master and asks for the link.
func (c *Client) dialLink(ctx context.Context, cfg *tls.Config, addr string) (*transport.WS, LinkState, error) {
	ctx, cancel := context.WithTimeout(ctx, c.dialTime)
	defer cancel()
	raw, err := c.dial(ctx, "tcp", addr)
	if err != nil {
		return nil, LinkOffline, err
	}
	conn := tls.Client(raw, cfg)
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, LinkOffline, err // nobody there, or not the master
	}
	t, err := transport.UpgradeWSClient(ctx, conn, "wss://"+addr+LinkPath, c.ws)
	if err == nil {
		return t, LinkConnected, nil
	}
	var status ws.StatusError
	switch {
	case ctx.Err() != nil:
		return nil, LinkOffline, err
	case !errors.As(err, &status):
		// The handshake proved this is the master, and it hung up instead of
		// answering: that is how a certificate it does not pin is refused, a
		// revoked one too. (A TLS 1.3 server judges the client's certificate
		// after the client's handshake is done.) It is also what a master that
		// cannot read its fleet does, so the child does not stop for it.
		return nil, LinkRejected, fmt.Errorf("the master rejected this machine: %w", err)
	}
	switch int(status) {
	case statusWaiting:
		return nil, LinkWaiting, errors.New(WaitingForConfirmation)
	case statusPaused:
		return nil, LinkPaused, errors.New("the operator paused this machine on the master")
	case statusRevoked:
		return nil, LinkRevoked, errors.New("the master rejected this machine: it is revoked")
	case statusUnknown:
		return nil, LinkRejected, errors.New("the master rejected this machine: it is not part of the fleet")
	}
	return nil, LinkOffline, fmt.Errorf("the master answered %d", int(status))
}

// serve runs the session of one link until it ends.
func (c *Client) serve(ctx context.Context, t *transport.WS, addr string, legacy bool) (LinkState, string, string, bool) {
	l := &childLink{c: c, addr: addr, legacy: legacy}
	l.sess = mcp.NewSession(noticing{t, l}, l)
	c.setStatus(LinkConnected, addr, "") // before the hello can add to it
	c.log.Info("fleet: linked to the master", "address", addr)
	l.sess.Start(ctx)
	c.mu.Lock()
	c.sess = l.sess
	c.mu.Unlock()
	select {
	case <-l.sess.Done():
	case <-ctx.Done():
	}
	_ = l.sess.Close()
	c.mu.Lock()
	c.sess = nil
	c.mu.Unlock()
	if l.revoked.Load() {
		return LinkRevoked, addr, "the master rejected this machine: it is revoked", true
	}
	why := "the master closed the link"
	if err := l.sess.Err(); err != nil && !errors.Is(err, mcp.ErrSessionClosed) {
		why = err.Error()
	}
	return LinkOffline, addr, why, true
}

// --- one link ---

// childLink is the child's end of one link: what it answers its master.
type childLink struct {
	c    *Client
	addr string
	sess *mcp.Session
	// legacy: this machine made the link with the certificate it had before
	// the cnsa profile. It serves no tool and keeps no memory over it; it is
	// updated over it, and moved off it (rekey.go).
	legacy bool

	master   atomic.Int32 // the link version the master said in its hello; 0 before
	revoked  atomic.Bool
	rekeying atomic.Bool // this link's one try at moving off a legacy identity has started
}

// errLegacyLink is what a child answers a call on a legacy link.
var errLegacyLink = mcp.NewError(codeIncompatible, "fleet: this machine is still on its certificate from before the ML-DSA-87 profile and serves nothing over it: update the master's Tenant, and the machine moves to a new one by itself")

func (l *childLink) HandleRequest(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	c := l.c
	switch method {
	case methodHello:
		return l.hello(params)
	case methodPing:
		return emptyObject, nil
	case methodUpdate:
		return c.update(ctx, params)
	case methodAddresses:
		return c.addresses(params)
	case methodMemoryList, methodMemoryPut, methodMemoryGet, methodMemoryPurge:
		if c.cfg.Memory == nil {
			break // a child that keeps no copies is a child that does not know these
		}
		if l.legacy {
			return nil, errLegacyLink
		}
		if v := int(l.master.Load()); !speaks(c.linkVersion, v) {
			return nil, mcp.NewError(codeIncompatible, incompatible("this machine", c.linkVersion, v))
		}
		return c.memory(method, params)
	case methodToolsList, methodToolsCall, methodResult:
		if l.legacy {
			return nil, errLegacyLink
		}
		if v := int(l.master.Load()); !speaks(c.linkVersion, v) {
			return nil, mcp.NewError(codeIncompatible, incompatible("this machine", c.linkVersion, v))
		}
		switch method {
		case methodToolsList:
			raw, err := encode(toolsResult{Tools: c.cfg.Tools.All()})
			if err != nil || len(raw) > frameMax(c.ws)-frameReserve {
				return nil, mcp.NewError(mcp.ErrInternalError, "fleet: this machine's tool list does not fit in a frame")
			}
			return raw, nil
		case methodToolsCall:
			return l.call(ctx, params)
		}
		var p resultParams
		if err := json.Unmarshal(params, &p); err != nil || !validCallID(p.CallID) {
			return nil, mcp.NewError(mcp.ErrInvalidParams, "fleet: fleet/result needs a call id")
		}
		return encode(c.lookup(p.CallID)) // a kept result was cut to a frame when it was kept
	}
	return nil, mcp.NewError(mcp.ErrMethodNotFound, "fleet: this machine does not know that method")
}

// HandleNotification hangs up on the "revoked" notice. noticing has already
// taken note of it.
func (l *childLink) HandleNotification(_ context.Context, method string, _ json.RawMessage) {
	if method == noticeRevoked {
		_ = l.sess.Close()
	}
}

// noticing is the link's transport as the child's session reads it. The
// session hands a notification to a goroutine of its own, and the master
// closes the link right behind its "revoked" notice: the link could be seen
// to end before the notice was acted on, and the child would dial again. So
// the notice is read here, on the read loop, before the session gets it.
type noticing struct {
	transport.Transport
	l *childLink
}

func (n noticing) Recv(ctx context.Context) ([]byte, error) {
	raw, err := n.Transport.Recv(ctx)
	if err == nil && len(raw) <= 256 { // the notice is some fifty bytes; nothing large is parsed twice
		var msg mcp.Message
		if json.Unmarshal(raw, &msg) == nil && msg.IsNotification() && msg.Method == noticeRevoked {
			n.l.revoked.Store(true)
		}
	}
	return raw, err
}

func (l *childLink) hello(params json.RawMessage) (json.RawMessage, error) {
	var p helloParams
	if err := json.Unmarshal(params, &p); err != nil || p.Link < 1 || p.Link > maxLinkVersion {
		return nil, mcp.NewError(mcp.ErrInvalidParams, "fleet: the hello carries a link version")
	}
	c := l.c
	l.master.Store(int32(p.Link))
	if !speaks(c.linkVersion, p.Link) {
		c.setStatus(LinkConnected, l.addr, incompatible("this machine", c.linkVersion, p.Link))
	}
	child, _ := c.file.Current()
	res := helloResult{Link: c.linkVersion, Arch: runtime.GOARCH, Reported: Reported{
		OS: runtime.GOOS, User: c.cfg.User, Version: c.cfg.Version, Sandbox: child.Sandbox, Ceiling: string(child.Ceiling)}}
	if c.cfg.Hello != nil {
		res.Update = c.cfg.Hello()
	}
	// The master asks this machine to move off its legacy identity. Not in
	// the trial run of an update: the Tenant that started it is older, comes
	// back if the trial fails, and could not read what a move leaves behind.
	if p.Rekey != "" && l.legacy && (res.Update == nil || res.Update.State != UpdateTrial) && l.rekeying.CompareAndSwap(false, true) {
		c.mu.Lock()
		start := !c.stopped
		if start {
			c.calls.Add(1) // rekey gives it back
		}
		c.mu.Unlock()
		if start {
			go c.rekey(l, p.Rekey)
		}
	}
	return encode(res)
}

// call runs one tool. The tool's context is the child's, not the link's: a
// link that drops leaves the call running, its result kept for fleet/result.
// The master giving up on the call does end it, and so does its deadline.
func (l *childLink) call(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	var p callParams
	if err := json.Unmarshal(params, &p); err != nil || !validCallID(p.CallID) || !validToolName(p.Name) {
		return nil, mcp.NewError(mcp.ErrInvalidParams, "fleet: tools/call needs a call id and a tool name")
	}
	c := l.c
	if err := c.begin(p.CallID); err != nil {
		return nil, err
	}
	timeout := maxCallTime
	if p.TimeoutMS > 0 && p.TimeoutMS < maxCallTime.Milliseconds() {
		timeout = time.Duration(p.TimeoutMS) * time.Millisecond
	}
	// The master's deadline is the same moment, and its cancel is on the way
	// then. This one is for when the link is not there to carry it.
	tctx, cancel := context.WithTimeout(c.runCtx, timeout+callGrace)
	defer cancel()
	// The request's context ends when the master cancels the call and when
	// the link ends. The session marks itself done before it cancels its
	// requests, which tells the two apart.
	defer context.AfterFunc(ctx, func() {
		select {
		case <-l.sess.Done():
		default:
			cancel()
		}
	})()
	ask := &asker{id: p.CallID, sess: l.sess}
	res := c.dispatch(context.WithValue(tctx, askKey{}, ask), p)
	res.Audit = ask.audit
	return c.finish(p.CallID, res), nil
}

// --- what outlives a link ---

// begin counts a call in, or refuses it.
func (c *Client) begin(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, dup := c.running[id]
	switch {
	case c.stopped:
		return mcp.NewError(codeUnavailable, "fleet: this machine's child is stopping")
	case dup || slices.ContainsFunc(c.kept, func(k keptResult) bool { return k.id == id }):
		return mcp.NewError(codeDuplicate, "fleet: this machine has seen that call id")
	case len(c.running) >= c.inFlight:
		return mcp.NewError(codeBusy, fmt.Sprintf("fleet: this machine already runs %d calls", c.inFlight))
	}
	c.running[id] = struct{}{}
	c.calls.Add(1)
	return nil
}

// dispatch runs the tool. A tool that panics fails its call, not the child.
func (c *Client) dispatch(ctx context.Context, p callParams) (res callResult) {
	defer func() {
		if r := recover(); r != nil {
			res = callResult{Text: fmt.Sprintf("fleet: the tool panicked: %v", r), IsError: true}
		}
	}()
	text, isError, err := c.cfg.Tools.Dispatch(ctx, model.ToolCall{ID: p.CallID, Name: p.Name, Arguments: p.Arguments})
	if err != nil {
		return callResult{Text: err.Error(), IsError: true}
	}
	return callResult{Text: text, IsError: isError}
}

// finish counts a call out and keeps its result, cut to what a frame takes,
// for a master that lost the link while it ran.
func (c *Client) finish(id string, res callResult) json.RawMessage {
	res, raw := fit(res, frameMax(c.ws))
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.running, id)
	c.calls.Done()
	c.kept = append(c.kept, keptResult{id: id, at: time.Now(), res: res})
	c.keptLen += len(res.Text)
	c.expire()
	return raw
}

// expire drops kept results beyond the table's bounds. Caller holds c.mu.
func (c *Client) expire() {
	n := 0
	for n < len(c.kept) && (len(c.kept)-n > resultsKept || c.keptLen > resultBytesKept || time.Since(c.kept[n].at) > resultKeep) {
		c.keptLen -= len(c.kept[n].res.Text)
		n++
	}
	c.kept = slices.Delete(c.kept, 0, n)
}

// lookup answers fleet/result.
func (c *Client) lookup(id string) resultResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expire()
	if _, ok := c.running[id]; ok {
		return resultResult{State: callRunning}
	}
	for _, k := range c.kept {
		if k.id == id {
			return resultResult{State: callDone, callResult: k.res}
		}
	}
	return resultResult{State: callUnknown}
}

// addresses takes the master's address list and saves it in child.json.
func (c *Client) addresses(params json.RawMessage) (json.RawMessage, error) {
	var p addressParams
	if err := json.Unmarshal(params, &p); err != nil || len(p.Addresses) == 0 || validAddresses(p.Addresses) != nil {
		return nil, mcp.NewError(mcp.ErrInvalidParams, "fleet: fleet/addresses needs a list of host:port addresses")
	}
	if cur, err := c.file.Current(); err == nil && slices.Equal(cur.MasterAddresses, p.Addresses) {
		return emptyObject, nil
	}
	err := UpdateChild(ChildPath(c.cfg.Dir), func(child *Child) error {
		child.MasterAddresses = p.Addresses
		return nil
	})
	c.mu.Lock()
	c.pushed = nil
	if err != nil {
		c.pushed = p.Addresses // dialed for as long as this process lives
	}
	c.mu.Unlock()
	if err != nil {
		c.log.Warn("fleet: could not save the master's addresses", "err", err)
		return nil, mcp.NewError(codeUnavailable, "fleet: this machine could not save the addresses: "+err.Error())
	}
	return emptyObject, nil
}

// update hands a piece of a new Tenant to the updater, if there is one.
func (c *Client) update(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	if c.cfg.Update == nil {
		return nil, mcp.NewError(codeUnavailable, "fleet: this machine cannot be updated over the link")
	}
	var u Update
	if err := json.Unmarshal(params, &u); err != nil || u.validate() != nil {
		return nil, mcp.NewError(mcp.ErrInvalidParams, "fleet: not a valid piece of an update")
	}
	ack, err := c.cfg.Update(ctx, u)
	if err != nil {
		return nil, mcp.NewError(codeUnavailable, "fleet: update: "+err.Error())
	}
	return encode(ack)
}

// --- asking the master ---

type askKey struct{}

// asker is a tool call's way back to the master that made it.
type asker struct {
	id   string
	sess *mcp.Session
	// audit is the hash of the call's audit record, left here by the gate
	// (gate.go) on the goroutine that runs the tool, for the call's result.
	audit string
}

// Confirm asks the master to approve an action of the tool call ctx belongs
// to, and waits for the decision. It has the shape of the tools' own Confirm
// hooks, so on a child it is the Confirm of every plugin: each gated action
// goes up the link, and the master's scheme for this machine decides.
//
// The answer is no when ctx is not a call the master made over the link,
// when the link that call came on has dropped, when the call ends first,
// and whenever the master does not say yes.
func Confirm(ctx context.Context, action, detail string) bool {
	a, _ := ctx.Value(askKey{}).(*asker)
	if a == nil {
		return false
	}
	detail, _ = cut(detail, maxDetailLen)
	params, _ := encode(confirmParams{CallID: a.id, Action: action, Detail: detail}) // three strings: cannot fail
	raw, err := a.sess.Call(ctx, methodConfirm, params)
	var res confirmResult
	return err == nil && json.Unmarshal(raw, &res) == nil && res.Allow
}
