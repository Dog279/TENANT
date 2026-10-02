package main

// dashboard_security_e2e_test.go is TEN-213's end-to-end gate (E1–E5 of
// specs/security-runbook-213.md): a real approval broker behind the real
// dashboard, every request real HTTP through the production chain
// (safetyDash: dashboardManager.Enable → Serve → served()). Nothing on the
// path is a fake. The other deadlines are covered where they live:
// loginvestigate_test.go (an alert's investigation, 15 min) and
// discordapprove_card_test.go (a Discord card).

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/agent"
	"tenant/internal/control"
	"tenant/internal/logs/catalog"
	"tenant/internal/logs/event"
)

// e2eAnswer is one response: its status, the flash its redirect carries,
// and its body.
type e2eAnswer struct {
	code         int
	msg, errMsg  string
	body         string
	transportErr error
}

// send is one request to the dashboard, shaped by set (credentials, Origin,
// Host). Safe from any goroutine: it reports rather than fails.
func (d *safetyDash) send(method, path, ctype, body string, set func(*http.Request)) e2eAnswer {
	req, err := http.NewRequest(method, d.base+path, strings.NewReader(body))
	if err != nil {
		return e2eAnswer{transportErr: err}
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if set != nil {
		set(req)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return e2eAnswer{transportErr: err}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	a := e2eAnswer{code: resp.StatusCode, body: string(b)}
	if u, err := url.Parse(resp.Header.Get("Location")); err == nil {
		a.msg, a.errMsg = u.Query().Get("msg"), u.Query().Get("err")
	}
	return a
}

const formType = "application/x-www-form-urlencoded"

func (d *safetyDash) signedInCookie(r *http.Request) { r.AddCookie(d.cookie) }

// sentFrom is the signed-in browser's request, sent by a page at origin.
func (d *safetyDash) sentFrom(origin string) func(*http.Request) {
	return func(r *http.Request) { d.signedInCookie(r); r.Header.Set("Origin", origin) }
}

func withKey(key string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+key) }
}

func pagePath(id, decision string) string {
	return "/safety/approvals/" + url.PathEscape(id) + "?" + url.Values{"decision": {decision}}.Encode()
}

func apiPath(id string) string { return "/api/approvals/" + url.PathEscape(id) }

// stillWaiting fails unless id is still pending and Confirm hasn't returned.
func (d *safetyDash) stillWaiting(id string, res <-chan bool, after string) {
	d.t.Helper()
	pending := false
	for _, p := range d.broker.Pending() {
		pending = pending || p.ID == id
	}
	select {
	case ok := <-res:
		d.t.Fatalf("after %s, Confirm returned %v", after, ok)
	default:
	}
	if !pending {
		d.t.Fatalf("after %s, the request is no longer waiting", after)
	}
}

// decisions lists the approval decision records (granted or denied) whose
// detail is detail.
func (d *safetyDash) decisions(detail string) []*event.Event {
	var out []*event.Event
	for _, def := range []*event.Def{catalog.ApprovalYes, catalog.ApprovalNo} {
		for _, e := range d.ev.matching(def, "") {
			if eventField(e, "detail").Text() == detail {
				out = append(out, e)
			}
		}
	}
	return out
}

// E1: nothing but the operator's own answer, on this page or with the key,
// resolves a request. Each forgery leaves it waiting and Confirm blocked;
// then a real Deny gives Confirm false and one record, the dashboard's.
func TestE2E_ForgedResolveCannotApprove(t *testing.T) {
	d := newSafetyDash(t)
	id, res := d.ask("os_exec", "touch hi")
	for _, tc := range []struct {
		name string
		set  func(*http.Request)
		code int
	}{
		{"no credential", nil, http.StatusUnauthorized},
		{"a wrong key", withKey("nope"), http.StatusUnauthorized},
		{"the cookie without an Origin", d.signedInCookie, http.StatusForbidden},
		{"another port", d.sentFrom("http://127.0.0.1:1"), http.StatusForbidden},
		{"another site", d.sentFrom("http://evil.example"), http.StatusForbidden},
		{"a rebinding host", func(r *http.Request) { d.sentFrom("http://evil.example")(r); r.Host = "evil.example" }, http.StatusForbidden},
	} {
		if a := d.send("POST", pagePath(id, "approve"), formType, "", tc.set); a.code != tc.code {
			t.Errorf("page, %s: %d, want %d", tc.name, a.code, tc.code)
		}
		if a := d.send("POST", apiPath(id), "application/json", `{"decision":"approve"}`, tc.set); a.code != tc.code {
			t.Errorf("API, %s: %d, want %d", tc.name, a.code, tc.code)
		}
		d.stillWaiting(id, res, tc.name)
	}
	ours := d.sentFrom(d.base)
	// Signed in, from this page, confirmed, with an answer the page doesn't offer.
	for _, decision := range []string{"yes", "", "APPROVE", "approve_always", "always", "session", "approve_once"} {
		if a := d.send("POST", pagePath(id, decision)+"&confirm=yes", formType, "", ours); a.code != http.StatusSeeOther || !strings.Contains(a.errMsg, "Nothing was allowed") {
			t.Errorf("page decision %q: %d %q, want refused", decision, a.code, a.errMsg)
		}
		d.stillWaiting(id, res, "page decision "+decision)
	}
	for _, decision := range []string{"yes", "", "APPROVE"} {
		if a := d.send("POST", apiPath(id), "application/json", `{"decision":"`+decision+`"}`, withKey(safetyDashToken)); a.code != http.StatusBadRequest {
			t.Errorf("API decision %q: %d, want 400", decision, a.code)
		}
		d.stillWaiting(id, res, "API decision "+decision)
	}
	// Another request's ID: a Discord card's, which only Discord answers, or none.
	for _, other := range []string{"beside-discord", "ap-00000000-999"} {
		if a := d.send("POST", pagePath(other, "approve"), formType, "", ours); a.code != http.StatusSeeOther || !strings.Contains(a.errMsg, "already answered") {
			t.Errorf("page, id %s: %d %q", other, a.code, a.errMsg)
		}
		if a := d.send("POST", apiPath(other), "application/json", `{"decision":"approve"}`, withKey(safetyDashToken)); a.code != http.StatusBadRequest || !strings.Contains(a.body, "not found") {
			t.Errorf("API, id %s: %d %s", other, a.code, a.body)
		}
		d.stillWaiting(id, res, "id "+other)
	}
	if len(d.decisions("touch hi")) != 0 {
		t.Fatalf("a forgery was recorded as a decision: %v", d.decisions("touch hi"))
	}
	if msg, _ := d.decide(id, "deny", false); msg != "Denied. Your agent was told no." {
		t.Fatalf("the operator's Deny: %q", msg)
	}
	if answered(t, res) {
		t.Error("the operator denied; Confirm said yes")
	}
	if recs := d.decisions("touch hi"); len(recs) != 1 || recs[0].ID != catalog.ApprovalNo.Spec().ID || recs[0].Surface != "dashboard" {
		t.Errorf("decision records = %d, want one deny from the dashboard: %v", len(recs), recs)
	}
}

// E2: sixteen answers at once, allows and denies, from the page: exactly one
// counts and Confirm returns once with its decision; the other fifteen are
// told it was already answered. Run under -race -count=20 in the gate.
func TestE2E_NoDoubleApprove(t *testing.T) {
	d := newSafetyDash(t)
	id, res := d.ask("os_exec", "touch race")
	ours := d.sentFrom(d.base)
	start := make(chan struct{})
	answers := make(chan e2eAnswer, 16)
	var wg sync.WaitGroup
	for i := range 16 {
		decision := []string{"approve", "deny"}[i%2]
		wg.Go(func() {
			<-start
			answers <- d.send("POST", pagePath(id, decision), formType, "", ours)
		})
	}
	close(start)
	wg.Wait()
	close(answers)
	won, stale, winner := 0, 0, ""
	for a := range answers {
		switch {
		case a.transportErr != nil || a.code != http.StatusSeeOther:
			t.Errorf("an answer: %d %v %s", a.code, a.transportErr, a.body)
		case a.msg != "":
			won, winner = won+1, a.msg
		case strings.Contains(a.errMsg, "already answered"):
			stale++
		default:
			t.Errorf("an answer said %q / %q", a.msg, a.errMsg)
		}
	}
	if won != 1 || stale != 15 {
		t.Fatalf("%d answers counted and %d told it was already answered, want 1 and 15", won, stale)
	}
	if got, want := answered(t, res), winner == "Allowed once. Your agent is carrying on."; got != want {
		t.Errorf("Confirm = %v, but the answer that counted was %q", got, winner)
	}
	select {
	case ok := <-res:
		t.Errorf("Confirm returned twice (again: %v)", ok)
	default:
	}
	if n := len(d.broker.Pending()); n != 0 {
		t.Errorf("%d still waiting", n)
	}
	if n := len(d.decisions("touch race")); n != 1 {
		t.Errorf("%d decision records, want 1", n)
	}
}

// E3: waiting never approves. A request whose job ends unanswered is turned
// down, leaves the queue with outcome expired and its record, and a late
// Allow is told so (not a 500). One still waiting at shutdown is denied as
// such.
func TestE2E_DenyOnTimeout(t *testing.T) {
	d := newSafetyDash(t)
	var mu sync.Mutex
	outcomes := map[string]string{}
	d.broker.SetEvents(func(e agent.Event) {
		if e.Kind == agent.EventApprovalResolved && e.Approval != nil {
			mu.Lock()
			outcomes[e.Approval.Detail] = e.Approval.Outcome
			mu.Unlock()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	res := make(chan bool, 1)
	began := time.Now()
	go func() { res <- d.broker.Confirm(ctx, "os_exec", "sleep forever") }()
	var id string
	for deadline := time.Now().Add(time.Second); id == "" && time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		for _, p := range d.broker.Pending() {
			if p.Detail == "sleep forever" {
				id = p.ID
			}
		}
	}
	if id == "" {
		t.Fatal("the request never waited")
	}
	select {
	case ok := <-res:
		if ok || time.Since(began) > time.Second {
			t.Errorf("Confirm = %v after %v, want false within 1s", ok, time.Since(began))
		}
	case <-time.After(time.Second):
		t.Fatal("Confirm still waiting a second after its job ended")
	}
	if n := len(d.broker.Pending()); n != 0 {
		t.Errorf("%d still waiting after the job ended", n)
	}
	mu.Lock()
	if outcomes["sleep forever"] != "expired" {
		t.Errorf("resolved as %q, want expired", outcomes["sleep forever"])
	}
	mu.Unlock()
	if len(d.ev.matching(catalog.ApprovalExpired, "")) != 1 {
		t.Errorf("ApprovalExpired records: %v", d.ev.audited())
	}
	if a := d.send("POST", pagePath(id, "approve"), formType, "", d.sentFrom(d.base)); a.code != http.StatusSeeOther || !strings.Contains(a.errMsg, "already answered") {
		t.Errorf("a late Allow: %d %q, want the no-longer-waiting message", a.code, a.errMsg)
	}

	_, left := d.ask("os_exec", "left at shutdown")
	d.broker.DenyAll()
	if answered(t, left) {
		t.Error("DenyAll: Confirm said yes")
	}
	mu.Lock()
	if outcomes["left at shutdown"] != "denied_shutdown" {
		t.Errorf("at shutdown resolved as %q, want denied_shutdown", outcomes["left at shutdown"])
	}
	mu.Unlock()
	if len(d.ev.matching(catalog.ApprovalShutdown, "")) != 1 {
		t.Errorf("ApprovalShutdown records: %v", d.ev.audited())
	}
}

// E4: a mode change from another page is refused and leaves no trace; from
// this page without its confirm step it changes nothing; confirmed, it
// applies, ends the category's session grant, and is recorded as the
// dashboard's.
func TestE2E_CSRFModeChangeRefused(t *testing.T) {
	d := newSafetyDash(t)
	id, res := d.ask("os_exec", "mkdir x")
	d.broker.Resolve(id, control.ApproveSession) // the terminal granted the session
	if !answered(t, res) {
		t.Fatal("the session grant: Confirm said no")
	}
	allow := url.Values{"category": {"exec"}, "mode": {"allow"}, "confirm": {"yes"}}
	for name, set := range map[string]func(*http.Request){
		"another port":     d.sentFrom("http://127.0.0.1:1"),
		"another site":     d.sentFrom("http://evil.example"),
		"no Origin":        d.signedInCookie,
		"a rebinding host": func(r *http.Request) { d.sentFrom("http://evil.example")(r); r.Host = "evil.example" },
	} {
		if a := d.send("POST", "/safety/mode", formType, allow.Encode(), set); a.code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", name, a.code)
		}
	}
	unconfirmed := url.Values{"category": {"exec"}, "mode": {"allow"}}
	if a := d.send("POST", "/safety/mode", formType, unconfirmed.Encode(), d.sentFrom(d.base)); a.code != http.StatusPreconditionRequired {
		t.Errorf("from this page without its confirm step: %d, want 428", a.code)
	}
	if m := d.broker.Modes()["exec"]; m != "ask" || len(d.ev.matching(catalog.ModeChanged, "")) != 0 {
		t.Fatalf("exec is %q with %d ModeChanged records before a confirmed change", m, len(d.ev.matching(catalog.ModeChanged, "")))
	}
	if msg, e := d.post("/safety/mode", allow); msg == "" || e != "" {
		t.Fatalf("the confirmed change: msg %q err %q", msg, e)
	}
	changed := d.ev.matching(catalog.ModeChanged, "")
	if d.broker.Modes()["exec"] != "allow" || len(changed) != 1 || changed[0].Surface != "dashboard" || eventField(changed[0], "category").Text() != "exec" {
		t.Errorf("after the confirmed change: exec %q, records %v", d.broker.Modes()["exec"], d.ev.audited())
	}
	for _, p := range d.broker.Permissions() {
		if p.Category == catExec && p.Session {
			t.Error("the change left the session grant in place")
		}
	}
}

// dashboardRoutes is every "METHOD /pattern" the dashboard mounts, read from
// its sources as TestRouteInventory reads them.
func dashboardRoutes(t *testing.T) []string {
	t.Helper()
	re := regexp.MustCompile(`HandleFunc\("([A-Z]+) ([^"]+)"`)
	var out []string
	for _, glob := range []string{"../../internal/dashboard/*.go", "../../internal/logs/logapi/*.go"} {
		files, err := filepath.Glob(glob)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range re.FindAllStringSubmatch(string(b), -1) {
				out = append(out, m[1]+" "+m[2])
			}
		}
	}
	if len(out) < 100 {
		t.Fatalf("the scan found %d routes: it is broken", len(out))
	}
	return out
}

// E5: once the operator denies every category, no web route reopens one
// but the two whose job it is, each behind its confirm step: the Safety
// page's mode and a Tools rule. Every other route that changes something is
// posted signed in from this page, and with the key, confirmed, every field
// asking for more; afterwards every category still refuses without asking.
// Not covered here (REVSEC-2): a remote service set to Don't ask (POST
// /mcp/trust mode=allow, behind its confirm step) runs its own tools past a
// Deny too, as a Tools rule does; this harness wires no remote services.
// Discord's lock outranks both (mcpremote.Policy.Locked).
func TestE2E_NoWebBypass(t *testing.T) {
	d := newSafetyDash(t)
	for _, c := range catOrder {
		if _, err := d.broker.SetPermissionFrom("tui", c, "deny"); err != nil {
			t.Fatal(err)
		}
	}
	more := url.Values{"confirm": {"yes"}}
	for _, k := range strings.Fields(`action allow_send category decision enabled exec handle id kind mode model name on operator
		pattern provider repo target text trusted url value`) {
		more.Set(k, map[string]string{"category": "exec", "mode": "allow", "decision": "approve_always", "pattern": "*",
			"target": "*", "name": "os_exec", "handle": "+15555550100", "operator": "4242", "url": "http://127.0.0.1:1/"}[k])
		if more.Get(k) == "" {
			more.Set(k, "true")
		}
	}
	jsonMore := `{"decision":"approve_always","enabled":true,"allow_send":true,"mode":"allow","category":"exec"}`
	fill := regexp.MustCompile(`\{[^}]*\}`)
	deliberate := map[string]bool{"POST /safety/mode": true, "POST /tools/rules/add": true}
	for _, route := range dashboardRoutes(t) {
		method, pattern, _ := strings.Cut(route, " ")
		if method == http.MethodGet || deliberate[route] {
			continue
		}
		path := fill.ReplaceAllString(pattern, "exec")
		for _, a := range []e2eAnswer{
			d.send(method, path+"?"+more.Encode(), formType, more.Encode(), d.sentFrom(d.base)),
			d.send(method, path, "application/json", jsonMore, withKey(safetyDashToken)),
		} {
			if a.transportErr != nil || a.code == http.StatusInternalServerError || a.code == http.StatusUnauthorized { // 503: its control isn't wired here
				t.Errorf("%s: %d %v", route, a.code, a.transportErr)
			}
		}
		if route == "POST /auth/logout" {
			d.cookie = d.signIn() // it ended the session; the sweep stays signed in
		}
	}
	for _, route := range []string{"/safety/mode", "/tools/rules/add"} {
		f := url.Values{"category": {"exec"}, "pattern": {"os_exec"}, "mode": {"allow"}}
		if a := d.send("POST", route, formType, f.Encode(), d.sentFrom(d.base)); a.code != http.StatusPreconditionRequired {
			t.Errorf("%s allow without its confirm step: %d, want 428", route, a.code)
		}
	}
	for c, m := range d.broker.Modes() {
		if m != "deny" {
			t.Errorf("%s reopened: %s", c, m)
		}
	}
	for _, p := range d.broker.Permissions() {
		if p.Session {
			t.Errorf("%s has a session grant", p.Category)
		}
	}
	if r := d.rules.Rules(); len(r) != 0 {
		t.Errorf("tool rules added: %v", r)
	}
	for _, action := range []string{"os_exec", "os_write", "os_exec_unsandboxed", "mcp_add", "sensitive_egress"} {
		if ok, prompted := d.confirmNow(action, "after the sweep"); ok || prompted {
			t.Errorf("%s after the sweep: ok %v, prompted %v; want refused unasked", action, ok, prompted)
		}
	}
}
