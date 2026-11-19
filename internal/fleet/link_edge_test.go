package fleet

import (
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tenant/internal/mcp"
)

// The corners: what happens when the thing that usually notices is not the
// one that does.

// With nothing watching fleet.json, the machine's own next message is what
// finds it paused or revoked: the state is checked on every one.
func TestLinkStateIsCheckedOnEveryMessage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Store, string) error
		notice bool
	}{
		{"paused", (*Store).Pause, false},
		{"revoked", (*Store).Revoke, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHostile(t, func(m *Manager) { m.store.watchEvery = time.Hour })
			c, id := h.raw(t, "bad")
			if err := tc.change(h.store, id); err != nil {
				t.Fatal(err)
			}
			// Nothing has looked: the socket is still held. But no call is made.
			time.Sleep(4 * testWatch)
			if m := h.status(t, id); !m.Online {
				t.Fatal("something closed the link without a watch and without a message")
			}
			if _, err := h.call(t, id, "echo", `1`); err == nil {
				t.Fatal("a call was made to a machine that is no longer confirmed")
			}
			// Its next message, whatever it is, ends the link instead of being answered.
			if err := c.send(`{"jsonrpc":"2.0","id":1,"method":"ping"}`); err != nil {
				t.Fatal(err)
			}
			c.ended()
			if m := h.awaitOnline(t, id, false); !strings.Contains(m.LastError, tc.name) {
				t.Errorf("last error = %q", m.LastError)
			}
			told := false
			for len(c.in) > 0 {
				msg := <-c.in
				told = told || (msg.Method == noticeRevoked && msg.IsNotification())
				if msg.IsResponse() {
					t.Errorf("the message was answered: %+v", msg)
				}
			}
			if told != tc.notice {
				t.Errorf("told it was revoked: %v", told)
			}
			h.intact(t)
		})
	}
}

// A machine paused or revoked between its upgrade and the end of its hello
// gets no link: the hello's answer is a message like any other.
func TestLinkStateChangesDuringTheHello(t *testing.T) {
	h := newHostile(t, func(m *Manager) { m.store.watchEvery = time.Hour })
	for name, change := range map[string]func(*Store, string) error{"paused": (*Store).Pause, "revoked": (*Store).Revoke} {
		mach, id := h.add(t, name, StateConfirmed)
		c, err := h.dialRaw(t, id, func(c *rawChild, msg mcp.Message) bool {
			if msg.Method != methodHello {
				return false
			}
			if err := change(h.store, mach.ID); err != nil {
				t.Error(err)
			}
			c.reply(msg, `{"link":1,"os":"linux","ceiling":"full"}`)
			return true
		})
		if err != nil {
			t.Fatal(err)
		}
		c.ended()
		for deadline := time.Now().Add(waitLong); ; time.Sleep(5 * time.Millisecond) {
			m := h.status(t, mach.ID)
			if m.Online {
				t.Fatalf("a machine %s during its hello has a link", name)
			}
			if strings.Contains(m.LastError, name) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s during the hello: last error = %q", name, m.LastError)
			}
		}
	}
	h.intact(t)
}

// A revoked machine that does not hang up on the notice is given a second
// and then cut off.
func TestLinkRevokedMachineThatWillNotHangUp(t *testing.T) {
	h := newHostile(t)
	c, id := h.raw(t, "bad")
	start := time.Now()
	if err := h.store.Revoke(id); err != nil {
		t.Fatal(err)
	}
	c.ended()
	if took := time.Since(start); took < 900*time.Millisecond || took > 3*time.Second {
		t.Errorf("cut off after %v, want about a second", took)
	}
	told := 0
	for len(c.in) > 0 {
		if msg := <-c.in; msg.Method == noticeRevoked {
			told++
		}
	}
	if told != 1 {
		t.Errorf("it was told %d times", told)
	}
	h.awaitOnline(t, id, false)
	h.intact(t)
}

// What the child makes of each answer a master can give to its dial.
func TestClientReadsTheMastersAnswer(t *testing.T) {
	r := newRig(t)
	// A server with the master's own certificate that answers the link
	// endpoint with whatever status the test sets, or with nothing.
	var status atomic.Int32
	fake := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if s := status.Load(); s != 0 {
			http.Error(w, "x", int(s))
			return
		}
		<-req.Context().Done()
	}))
	fake.TLS = must(MasterEnrollTLS(r.id))(t)
	fake.Config.ErrorLog = log.New(io.Discard, "", 0)
	fake.StartTLS()
	defer fake.Close()

	k := r.kid(t, "box", StateConfirmed)
	if err := UpdateChild(ChildPath(k.dir), func(c *Child) error {
		c.MasterAddresses = []string{fake.Listener.Addr().String()}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	k.c.dialTime = 300 * time.Millisecond
	status.Store(http.StatusTooEarly)
	k.start()
	for _, tc := range []struct {
		status int
		state  LinkState
		detail string
	}{
		{http.StatusTooEarly, LinkWaiting, WaitingForConfirmation},
		{http.StatusLocked, LinkPaused, "paused this machine"},
		{http.StatusForbidden, LinkRejected, "the master rejected this machine: it is not part of the fleet"},
		{http.StatusServiceUnavailable, LinkOffline, "the master answered 503"},
		{http.StatusTooManyRequests, LinkOffline, "the master answered 429"},
		{http.StatusOK, LinkOffline, "the master answered 200"},
		{0, LinkOffline, "deadline exceeded"}, // it never answers: the dial gives up
	} {
		status.Store(int32(tc.status))
		for deadline := time.Now().Add(waitLong); ; time.Sleep(5 * time.Millisecond) {
			st := k.c.Status()
			if st.State == tc.state && strings.Contains(st.Detail, tc.detail) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("after the answer %d the child is %q (%s), want %q (%s)", tc.status, st.State, st.Detail, tc.state, tc.detail)
			}
		}
	}
	// "Revoked" from the master itself ends it for good.
	status.Store(http.StatusGone)
	select {
	case err := <-k.done:
		k.done <- err
		if !errors.Is(err, ErrRevoked) {
			t.Errorf("Run = %v", err)
		}
	case <-time.After(waitLong):
		t.Fatal("the child went on after the master said revoked")
	}
	if st := k.awaitDisk(LinkRevoked); !strings.Contains(st.Detail, "the master rejected this machine") {
		t.Errorf("child-link.json = %+v", st)
	}
}

// Connections that are not links yet have a fixed number of places; one more
// waits its turn, and gets it when a place comes back.
func TestHostileConnectionsWaitTheirTurn(t *testing.T) {
	// The master's patience with a connection that says nothing is long here,
	// so a place comes back when the test gives it back, and not by a clock
	// the test would be racing.
	h := newHostile(t, func(m *Manager) {
		m.sem = make(chan struct{}, 2)
		m.headerTime = waitLong
	})
	hog := func() net.Conn {
		c := must(net.Dial("tcp", h.addr))(t)
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	taken := func(n int) {
		t.Helper()
		for deadline := time.Now().Add(waitLong); len(h.mgr.sem) != n; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%d places are taken, want %d", len(h.mgr.sem), n)
			}
		}
	}
	late := h.kid(t, "late", StateConfirmed)
	first := hog()
	hog()
	taken(2)
	// A third connection is not served while both places are taken, however
	// long it waits.
	late.start()
	time.Sleep(300 * time.Millisecond)
	if st := late.c.Status(); st.State == LinkConnected || len(h.mgr.sem) != 2 {
		t.Fatalf("a third connection was served while both places were taken: the child is %q, %d places taken", st.State, len(h.mgr.sem))
	}
	h.intact(t) // the links there are, are not held up by it
	// One of the two goes away: its place is the third's.
	_ = first.Close()
	late.await(LinkConnected)
	h.awaitOnline(t, late.mach.ID, true)
	h.intact(t)
	// The master closes with a connection waiting for a place: it is let go.
	hog()
	hog()
	taken(2)
	time.Sleep(50 * time.Millisecond) // the one without a place is in the listener's hands by now
	_ = h.mgr.Close()
}

// What a manager cannot be started with, and what it starts in spite of.
func TestManagerStartRefusalsAndRecoveries(t *testing.T) {
	watchLeaks(t)
	old := slog.Default()
	slog.SetDefault(quiet) // a manager and a client given no log use the process's
	defer slog.SetDefault(old)

	m := newMaster(t)
	if _, err := StartManager(ManagerConfig{Store: m.store, Addr: "127.0.0.1:0"}); err == nil {
		t.Error("a manager without an identity started")
	}
	// A config dir nothing can be created in: the fleet cannot be taken.
	if _, err := StartManager(ManagerConfig{Store: Open(underAFile(t, "cfg")), Identity: m.id, Addr: "127.0.0.1:0"}); err == nil || !strings.Contains(err.Error(), "take the fleet") {
		t.Errorf("a manager that cannot take the fleet = %v", err)
	}
	// A holder file that cannot be written does not stop the master; a second
	// process still learns the fleet is held, if not by whom.
	if err := os.Mkdir(m.dir+"/"+holderFileName, 0o700); err != nil {
		t.Fatal(err)
	}
	mgr, err := StartManager(ManagerConfig{Store: m.store, Identity: m.id, Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	_, err = StartManager(ManagerConfig{Store: m.store, Identity: m.id, Addr: "127.0.0.1:0"})
	var held *HeldError
	if !errors.As(err, &held) || held.Holder.PID != 0 || err.Error() != "fleet: another process holds the fleet" {
		t.Errorf("a second manager = %v", err)
	}

	// A child that cannot write its status file links all the same.
	addr := mgr.Addr().String()
	if err := m.store.SetAddresses([]string{addr}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	id := must(MintIdentity(ChildIdentityPath(dir)))(t)
	mach := m.addIdentity(t, "box", StateConfirmed, id)
	if err := SaveChild(ChildPath(dir), Child{ID: mach.ID, Name: mach.Name, MasterAddresses: []string{addr},
		MasterFingerprint: m.id.Fingerprint, Sandbox: "host", Ceiling: CeilingFull}); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(ChildStatusPath(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	c := must(NewClient(ClientConfig{Dir: dir, Tools: newFakeTools(), Version: "7.7.7"}))(t)
	c.backoffMin, c.backoffMax = 10*time.Millisecond, 100*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()
	for deadline := time.Now().Add(waitLong); ; time.Sleep(5 * time.Millisecond) {
		res, err := mgr.Call(ctxFor(t), mach.ID, Call{Tool: "echo", Arguments: []byte(`1`)})
		if err == nil && res.Text == "1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the child never answered: %v", err)
		}
	}
	if got := must(m.store.Machine(mach.ID))(t).Reported.Version; got != "7.7.7" {
		t.Errorf("the version the child reported = %q", got)
	}
	if st := c.Status(); st.State != LinkConnected {
		t.Errorf("status = %+v", st)
	}

	// A link that is ending this very moment is offline already.
	mgr.mu.Lock()
	live := mgr.links[mach.ID]
	gone := &link{m: mgr, id: mach.ID, sess: mcp.NewSession(nowhere{}, nil)}
	_ = gone.sess.Close()
	mgr.links[mach.ID] = gone
	mgr.mu.Unlock()
	_, err = mgr.Call(ctxFor(t), mach.ID, Call{Tool: "echo"})
	var off *OfflineError
	if !errors.As(err, &off) || time.Since(off.Since) > time.Minute {
		t.Errorf("a call on a link that is ending = %v", err)
	}
	mgr.mu.Lock()
	mgr.links[mach.ID] = live
	mgr.mu.Unlock()
}

// An address list that became empty is not pushed: a child keeps the
// addresses it has.
func TestLinkEmptyAddressListIsNotPushed(t *testing.T) {
	r := newRig(t)
	k := r.up(t, "box")
	if err := r.store.SetAddresses(nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(6 * testWatch)
	if c := must(LoadChild(ChildPath(k.dir)))(t); len(c.MasterAddresses) != 1 || c.MasterAddresses[0] != r.addr {
		t.Errorf("child.json addresses = %v", c.MasterAddresses)
	}
	r.echo(t, k.mach.ID)
}
