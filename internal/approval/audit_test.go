package approval

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/control"
	"tenant/internal/logs"
	"tenant/internal/logs/event"
	"tenant/internal/logs/event/eventtest"
	"tenant/internal/logs/store"
	"tenant/internal/runtrace"
)

// securityLog installs a log book with a store in a temp dir, as serve runs
// one (key nil: a store with no chain key, which refuses security records),
// and returns a reader of the events committed to one of its logs.
func securityLog(t *testing.T, opts logs.Options, key []byte) func(log string) []event.Event {
	t.Helper()
	h, err := logs.Open(t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(h.Root(), "logbook.db")
	st, err := store.Open(path, store.Options{ChainKey: key})
	if err != nil {
		t.Fatal(err)
	}
	h.AttachStore(st)
	logs.Install(h)
	r, err := store.Open(path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logs.Install(nil); _ = h.Close(); _ = r.Close() })
	return func(log string) []event.Event {
		var out []event.Event
		var cur store.Cursor
		for {
			evs, next, _, err := r.Since(context.Background(), cur, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(evs) == 0 {
				return out
			}
			for _, e := range evs {
				if e.Log == log {
					out = append(out, e)
				}
			}
			cur = next
		}
	}
}

var testChainKey = []byte("approval test chain key")

// rec renders a security record as "id actor@surface k=v…" over the fields
// asked for, for comparing.
func rec(e event.Event, keys ...string) string {
	s := strconv.FormatUint(uint64(e.ID), 10) + " " + e.Actor + "@" + e.Surface
	for _, k := range keys {
		for _, f := range e.Data {
			if f.Key == k {
				s += " " + k + "=" + f.Text()
			}
		}
	}
	return s
}

func wantRecords(t *testing.T, got []event.Event, keys []string, want ...string) {
	t.Helper()
	var lines []string
	for _, e := range got {
		lines = append(lines, rec(e, keys...))
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("security records:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

// Each decision a standing setting makes is a grant or a denial record,
// attributed to Tenant on the broker's surface, saying what decided it.
func TestSecurityRecordsPolicyVerdicts(t *testing.T) {
	read := securityLog(t, logs.Options{}, testChainKey)
	ctx := context.Background()
	b := newTestBroker(testLog())
	b.SetRules(NewRules())
	b.LoadModes(map[string]string{CatExec: "allow", CatSend: "deny"}, nil)
	b.Confirm(ctx, "os_exec", "ls")
	b.Confirm(ctx, "gsuite_send", "mail bob")
	_ = b.rules.Set("web_interact", "allow")
	b.Confirm(ctx, "web_interact", "click")
	_ = b.rules.Set("os_write", "deny")
	b.Confirm(ctx, "os_write", "write /tmp/x")
	b.mu.Lock()
	b.session[CatDestructive] = true
	b.mu.Unlock()
	b.Confirm(ctx, "os_exec_dangerous", "rm x")
	b.SetLock(func(action, _ string) bool { return action == "os_exec" })
	b.Confirm(ctx, "os_exec", "ls")
	NewDiscordBroker(testLog(), testCategorize).Confirm(ctx, "os_exec", "ls") // ask, with no backend to ask through

	got := read("security")
	wantRecords(t, got, []string{"category", "action", "detail", "grant", "via"},
		"4102 tenant@local category=exec action=os_exec detail=ls grant=always via=mode",
		"4103 tenant@local category=send action=gsuite_send detail=mail bob via=mode",
		"4102 tenant@local category=web action=web_interact detail=click grant=always via=rule",
		"4103 tenant@local category=write action=os_write detail=write /tmp/x via=rule",
		"4102 tenant@local category=destructive action=os_exec_dangerous detail=rm x grant=session via=session",
		"4103 tenant@local category=exec action=os_exec detail=ls via=lock",
		"4103 tenant@discord category=exec action=os_exec detail=ls via=no_surface")
	for _, e := range got {
		if !strings.HasPrefix(e.Corr.ApprovalID, "policy:") || !hasKeyword(e, "audit.") {
			t.Errorf("record %d: approval %q, keywords %v", e.ID, e.Corr.ApprovalID, e.Keywords)
		}
	}
}

func hasKeyword(e event.Event, prefix string) bool {
	for _, k := range e.Keywords {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// A prompt answered on a host surface: its request, from the agent, then
// its outcome by the operator on the surface that resolved it, one approval
// ID and the turn's trace on both. An unanswered one expires; a shutdown
// denies; a peer's pairing request is the peer's.
func TestSecurityRecordsHostPrompts(t *testing.T) {
	read := securityLog(t, logs.Options{}, testChainKey)
	b := newTestBroker(testLog())
	trace := runtrace.NewID()
	ctx := runtrace.WithSpan(context.Background(), runtrace.NewSpan(trace))

	answer := func(fn func(id string)) chan bool {
		done := make(chan bool, 1)
		go func() { done <- b.Confirm(ctx, "os_exec", "make test") }()
		id := waitPending(t, b)
		fn(id)
		return done
	}
	if !<-answer(func(id string) { _ = b.Decide(id, "approve_session") }) {
		t.Fatal("approved session: refused")
	}
	b.mu.Lock()
	delete(b.session, CatExec)
	b.mu.Unlock()
	if <-answer(func(id string) { b.Resolve(id, control.DenyOnce) }) {
		t.Fatal("denied: allowed")
	}
	if <-answer(func(string) { b.DenyAll() }) {
		t.Fatal("shutdown: allowed")
	}
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan bool, 1)
	go func() { done <- b.Confirm(cctx, "os_exec", "make test") }()
	waitPending(t, b)
	cancel()
	<-done
	go func() { done <- b.AskPairing(context.Background(), "peer bob wants to pair") }()
	_ = b.Decide(waitPending(t, b), "approve")
	<-done

	got := read("security")
	wantRecords(t, got, []string{"category", "action", "detail", "origin", "grant", "via"},
		"4101 agent@local category=exec action=os_exec detail=make test origin=local",
		"4102 operator@dashboard category=exec action=os_exec detail=make test origin=local grant=session via=operator",
		"4101 agent@local category=exec action=os_exec detail=make test origin=local",
		"4103 operator@tui category=exec action=os_exec detail=make test origin=local via=operator",
		"4101 agent@local category=exec action=os_exec detail=make test origin=local",
		"4104 tenant@local category=exec action=os_exec detail=make test origin=local",
		"4101 agent@local category=exec action=os_exec detail=make test origin=local",
		"4105 tenant@local category=exec action=os_exec detail=make test origin=local",
		"4101 peer@local category=pairing action=peer pairing detail=peer bob wants to pair origin=local",
		"4102 operator@dashboard category=pairing action=peer pairing detail=peer bob wants to pair origin=local grant=once via=operator")
	for i := 0; i+1 < len(got) && i < 8; i += 2 {
		if a, o := got[i], got[i+1]; a.Corr.ApprovalID == "" || !strings.HasPrefix(a.Corr.ApprovalID, "ap-") ||
			o.Corr.ApprovalID != a.Corr.ApprovalID || a.Corr.TraceID != trace || o.Corr.TraceID != trace {
			t.Errorf("records %d/%d: approvals %q/%q, traces %q/%q, want one ap- ID and trace %s",
				a.ID, o.ID, a.Corr.ApprovalID, o.Corr.ApprovalID, a.Corr.TraceID, o.Corr.TraceID, trace)
		}
	}
}

// waitPending returns the ID of the one pending approval, once there is one.
func waitPending(t *testing.T, b *Broker) string {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if p := b.Pending(); len(p) == 1 {
			return p[0].ID
		}
	}
	t.Fatal("no pending approval")
	return ""
}

// A channel's backend (the Discord card) names who decided and its nonce;
// one that says nothing leaves Tenant as the actor, and the gated action's
// approval slot gets the approval that decided it.
func TestSecurityRecordsChannelBackend(t *testing.T) {
	read := securityLog(t, logs.Options{}, testChainKey)
	d := newTestDiscordBroker(testLog())
	d.SetAsk(func(ctx context.Context, _ control.ApprovalRequest) control.ApprovalDecision {
		DecidedBy(ctx, "discord:42", "nonce", "Q7X2KA")
		return control.ApproveOnce
	})
	slot := event.WithApproval(context.Background(), "")
	if !d.Confirm(slot, "os_exec", "uptime") {
		t.Fatal("approved on the card: refused")
	}
	d.SetAsk(func(context.Context, control.ApprovalRequest) control.ApprovalDecision { return control.Expired })
	d.Confirm(context.Background(), "os_exec", "uptime")

	got := read("security")
	wantRecords(t, got, []string{"action", "grant", "via", "nonce"},
		"4101 agent@discord action=os_exec",
		"4102 discord:42@discord action=os_exec grant=once via=operator nonce=Q7X2KA",
		"4101 agent@discord action=os_exec",
		"4105 tenant@discord action=os_exec")
	if len(got) == 4 {
		// A record written after the gate, under the action's context, carries its approval.
		c := eventtest.Capture(t)
		_ = ApprovalProbe.Audit(slot)
		evs := c.Events()
		if last := evs[len(evs)-1]; last.Corr.ApprovalID != got[1].Corr.ApprovalID || got[0].Corr.ApprovalID != got[1].Corr.ApprovalID {
			t.Errorf("the action's slot holds %q; the card's records %q/%q", last.Corr.ApprovalID, got[0].Corr.ApprovalID, got[1].Corr.ApprovalID)
		}
	}
}

var ApprovalProbe = event.NewProvider("approvaltest", "approvaltest").Event(1, event.Info, "Probe", "probe")

// Changing a category's mode is recorded with the surface it was changed on.
func TestSecurityRecordsModeChange(t *testing.T) {
	read := securityLog(t, logs.Options{}, testChainKey)
	b := newTestDiscordBroker(testLog())
	if ok, err := b.SetPermissionFrom("dashboard", CatExec, "allow"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	b.SetPermission(CatSend, "deny")
	wantRecords(t, read("security"), []string{"broker", "category", "mode", "was"},
		"4106 operator@dashboard broker=discord category=exec mode=allow was=ask",
		"4106 operator@tui broker=discord category=send mode=deny was=ask")
}

// logs.security.on_failure: with continue (the default) a gated action goes
// ahead when its record can't be written; with deny it is refused, until
// records are written again. Either way the failure is a critical system
// event and one operator banner.
func TestAuditFailurePolicy(t *testing.T) {
	for _, deny := range []bool{false, true} {
		var mu sync.Mutex
		var notices []string
		// No chain key: the store refuses every security record.
		read := securityLog(t, logs.Options{AuditDeny: func() bool { return deny }}, nil)
		logs.Default().OnAlert(func(s string) { // the status feed
			mu.Lock()
			notices = append(notices, s)
			mu.Unlock()
		})
		b := newTestBroker(testLog())
		b.Allow(CatExec)
		for range 2 {
			if got := b.Confirm(context.Background(), "os_exec", "ls"); got == deny {
				t.Errorf("deny=%v: Confirm = %v with the Security log unwritable", deny, got)
			}
		}
		mu.Lock()
		if len(notices) != 1 || !strings.Contains(notices[0], "security log") {
			t.Errorf("deny=%v: notices %q, want one banner", deny, notices)
		}
		mu.Unlock()
		var critical []event.Event
		for deadline := time.Now().Add(5 * time.Second); len(critical) < 2 && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			critical = critical[:0]
			for _, e := range read("system") {
				if e.ID == 4001 && e.Level == event.Critical {
					critical = append(critical, e)
				}
			}
		}
		if len(critical) != 2 || len(read("security")) != 0 {
			t.Errorf("deny=%v: %d critical system/4001 events, want one per lost record", deny, len(critical))
		}
	}
}
