package main

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/agent"
	"tenant/internal/control"
	"tenant/internal/dashboard"
)

// safetyDash is a real approval broker behind a real dashboard, wired as
// serve wires them (TEN-207): the broker is both the ApprovalControl and the
// SafetyControl, its persist hook saves settings.main.json, its events reach
// the dashboard's stream, and every request goes through the production
// handler chain (dashboardManager.Enable → Serve → served()) signed in with
// the cookie and a same-origin Origin, as a browser's form or Datastar post.
type safetyDash struct {
	t      *testing.T
	broker *approvalBroker
	rules  *toolRules // the broker's tool rules, also the Tools page's
	base   string
	cfgDir string
	ev     *channelEvents
	cookie *http.Cookie // the session the sign-in form started
}

const safetyDashToken = "safety-dash-test-token"

func newSafetyDash(t *testing.T) *safetyDash {
	t.Helper()
	return newSafetyDashIn(t, t.TempDir())
}

// newSafetyDashIn is newSafetyDash on the settings already in cfgDir.
func newSafetyDashIn(t *testing.T, cfgDir string) *safetyDash {
	t.Helper()
	d := &safetyDash{t: t, ev: captureChannelEvents(t), cfgDir: cfgDir, broker: newApprovalBroker(nil), rules: newToolRules()}
	d.broker.SetRules(d.rules)
	stg, err := loadSettings(d.cfgDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	d.broker.LoadModes(stg.Permissions, stg.KnownCategories)
	var mu sync.Mutex
	d.broker.SetPersist(func(modes map[string]string, known []string) {
		mu.Lock()
		defer mu.Unlock()
		stg.Permissions, stg.KnownCategories = modes, known
		saveSettingsOrTell(stg, d.cfgDir, "main", func(msg string) { t.Errorf("settings not saved: %s", msg) })
	})
	events := agent.NewBroker(0)
	d.broker.SetEvents(events.Publish)
	mgr := &dashboardManager{
		base:        context.Background(),
		cfg:         dashboard.Config{Addr: "127.0.0.1:0", Auth: safetyDashToken},
		broker:      events,
		approvals:   d.broker,
		rules:       dashRules{d.rules},
		safety:      d.broker,
		safetyFacts: newSafetyFacts(),
		notify:      func(string) {},
	}
	addr, err := mgr.Enable()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.broker.DenyAll(); _ = mgr.Shutdown() })
	if !healthzUp(addr, 2*time.Second) {
		t.Fatal("dashboard did not come up")
	}
	d.base = "http://" + addr
	d.cookie = d.signIn()
	return d
}

// signIn posts the key to the sign-in form, as the operator does, and
// returns the session cookie it sets.
func (d *safetyDash) signIn() *http.Cookie {
	d.t.Helper()
	req, _ := http.NewRequest("POST", d.base+"/auth/login", strings.NewReader(url.Values{"token": {safetyDashToken}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", d.base)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		d.t.Fatal(err)
	}
	resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == "tenant-dash" && c.Value != "" {
			return c
		}
	}
	d.t.Fatalf("sign-in set no session cookie (%d)", resp.StatusCode)
	return nil
}

func (d *safetyDash) do(method, path string, form url.Values) *http.Response {
	d.t.Helper()
	req, err := http.NewRequest(method, d.base+path, strings.NewReader(form.Encode()))
	if err != nil {
		d.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", d.base)
	req.AddCookie(d.cookie)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		d.t.Fatal(err)
	}
	d.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// post POSTs a form and returns the redirect's flash.
func (d *safetyDash) post(path string, form url.Values) (msg, errMsg string) {
	d.t.Helper()
	resp := d.do("POST", path, form)
	if resp.StatusCode != http.StatusSeeOther {
		b, _ := io.ReadAll(resp.Body)
		d.t.Fatalf("POST %s = %d, want 303: %s", path, resp.StatusCode, b)
	}
	u, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		d.t.Fatal(err)
	}
	return u.Query().Get("msg"), u.Query().Get("err")
}

func (d *safetyDash) decide(id, decision string, confirm bool) (msg, errMsg string) {
	q := url.Values{"decision": {decision}}
	if confirm {
		q.Set("confirm", "yes")
	}
	return d.post("/safety/approvals/"+url.PathEscape(id)+"?"+q.Encode(), nil)
}

func (d *safetyDash) page() string {
	d.t.Helper()
	b, _ := io.ReadAll(d.do("GET", "/safety", nil).Body)
	return html.UnescapeString(string(b))
}

// ask runs Confirm(action, detail) as a tool call would, waits for its
// request to be pending, and returns its ID and Confirm's answer to come.
func (d *safetyDash) ask(action, detail string) (string, <-chan bool) {
	d.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	d.t.Cleanup(cancel)
	res := make(chan bool, 1)
	go func() { res <- d.broker.Confirm(ctx, action, detail) }()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		for _, p := range d.broker.Pending() {
			if p.Detail == detail {
				return p.ID, res
			}
		}
	}
	d.t.Fatalf("%s %q never asked", action, detail)
	return "", nil
}

// answered is Confirm's answer, or a failure if it never comes.
func answered(t *testing.T, res <-chan bool) bool {
	t.Helper()
	select {
	case ok := <-res:
		return ok
	case <-time.After(5 * time.Second):
		t.Fatal("Confirm never returned")
		return false
	}
}

// confirmNow is Confirm for an action that must not prompt.
func (d *safetyDash) confirmNow(action, detail string) (ok, prompted bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res := make(chan bool, 1)
	go func() { res <- d.broker.Confirm(ctx, action, detail) }()
	for {
		select {
		case ok := <-res:
			return ok, prompted
		case <-time.After(5 * time.Millisecond):
			for _, p := range d.broker.Pending() {
				if p.Detail == detail {
					prompted = true
					d.broker.Resolve(p.ID, control.DenyOnce) // let Confirm return
				}
			}
		}
	}
}

// A paused action is on the page, and Allow once or Deny from the page is
// what Confirm returns.
func TestSafetyDash_AllowAndDeny(t *testing.T) {
	d := newSafetyDash(t)
	id, res := d.ask("os_exec", "test -d .")
	if p := d.page(); !strings.Contains(p, `id="q-`+id+`"`) || !strings.Contains(p, "Your agent wants to run commands and programs on this computer") {
		t.Fatalf("the request isn't on the page:\n%s", p)
	}
	if msg, e := d.decide(id, "approve", false); msg != "Allowed once. Your agent is carrying on." || e != "" {
		t.Errorf("approve: msg %q err %q", msg, e)
	}
	if !answered(t, res) {
		t.Error("Allow once on the page: Confirm said no")
	}

	id, res = d.ask("os_exec", "test -f x")
	if msg, _ := d.decide(id, "deny", false); msg != "Denied. Your agent was told no." {
		t.Errorf("deny: %q", msg)
	}
	if answered(t, res) {
		t.Error("Deny on the page: Confirm said yes")
	}
	if len(d.broker.Pending()) != 0 {
		t.Errorf("still pending: %+v", d.broker.Pending())
	}
	// The outcome is a Security record from the dashboard.
	var denied bool
	for _, r := range d.ev.audited() {
		denied = denied || strings.Contains(r, "test -f x") && strings.Contains(r, "surface=dashboard") && strings.Contains(r, "actor=operator")
	}
	if !denied {
		t.Errorf("no dashboard record of the deny: %q", d.ev.audited())
	}
}

// The first answer wins, whichever surface gives it; the other changes
// nothing and says so.
func TestSafetyDash_FirstAnswerWins(t *testing.T) {
	d := newSafetyDash(t)
	// The terminal first: the page's Allow is too late.
	id, res := d.ask("os_exec", "test -e y")
	if !d.broker.Resolve(id, control.DenyOnce) {
		t.Fatal("the terminal's answer was refused")
	}
	if _, e := d.decide(id, "approve", false); !strings.Contains(e, "already answered") || !strings.Contains(e, "didn't change anything") {
		t.Errorf("the page after the terminal: err %q", e)
	}
	if answered(t, res) {
		t.Error("a late Allow on the page overrode the terminal's Deny")
	}
	// The page first: the terminal's answer is a no-op.
	id, res = d.ask("os_exec", "test -e z")
	if msg, _ := d.decide(id, "deny", false); msg == "" {
		t.Fatal("the page's Deny failed")
	}
	if d.broker.Resolve(id, control.ApproveOnce) {
		t.Error("the terminal answered a request the page already had")
	}
	if answered(t, res) {
		t.Error("a late Allow in the terminal overrode the page's Deny")
	}
}

// "Allow for the rest of this session" is a real grant: the next action in
// that category goes ahead unasked, the row says so, and "Ask me again"
// ends it.
func TestSafetyDash_SessionGrant(t *testing.T) {
	d := newSafetyDash(t)
	id, res := d.ask("os_write", "write notes.txt")
	if resp := d.do("POST", "/safety/approvals/"+id+"?decision=approve_session", nil); resp.StatusCode != http.StatusPreconditionRequired {
		t.Errorf("the session grant without its confirm step = %d, want 428", resp.StatusCode)
	}
	if msg, _ := d.decide(id, "approve_session", true); msg != "Allowed for this session." {
		t.Fatalf("session: %q", msg)
	}
	if !answered(t, res) {
		t.Fatal("the session grant: Confirm said no")
	}
	if ok, prompted := d.confirmNow("os_edit", "edit notes.txt"); !ok || prompted {
		t.Errorf("under the grant: ok %v, prompted %v", ok, prompted)
	}
	if !slices.ContainsFunc(d.broker.Permissions(), func(p control.PermissionInfo) bool { return p.Category == catWrite && p.Session }) {
		t.Error("Permissions() doesn't show the grant")
	}
	if p := d.page(); !strings.Contains(p, "Allowed without asking until Tenant restarts.") || !strings.Contains(p, "Ask me again") {
		t.Error("the row doesn't show the grant")
	}
	if msg, _ := d.post("/safety/mode", url.Values{"category": {"write"}, "mode": {"ask"}}); msg != "Change files: your agent asks you again from now on." {
		t.Errorf("Ask me again: %q", msg)
	}
	id, res = d.ask("os_edit", "edit notes.txt again")
	d.decide(id, "deny", false)
	if answered(t, res) {
		t.Error("after Ask me again: Confirm said yes")
	}
}

// A mode set on the page applies at once and is saved, with the
// known_categories marker, and recorded as the dashboard's.
func TestSafetyDash_ModePersistsAndApplies(t *testing.T) {
	d := newSafetyDash(t)
	if resp := d.do("POST", "/safety/mode", url.Values{"category": {"exec"}, "mode": {"allow"}}); resp.StatusCode != http.StatusPreconditionRequired {
		t.Fatalf("Allow without its confirm step = %d, want 428", resp.StatusCode)
	}
	if msg, e := d.post("/safety/mode", url.Values{"category": {"exec"}, "mode": {"allow"}, "confirm": {"yes"}}); msg != "Run commands: your agent goes ahead without asking." || e != "" {
		t.Fatalf("allow: msg %q err %q", msg, e)
	}
	if ok, prompted := d.confirmNow("os_exec", "test -d ."); !ok || prompted {
		t.Errorf("after Allow: ok %v, prompted %v (no restart should be needed)", ok, prompted)
	}
	b, err := os.ReadFile(settingsPath(d.cfgDir, "main"))
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Permissions map[string]string `json:"permissions"`
		Known       []string          `json:"known_categories"`
	}
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Permissions["exec"] != "allow" || !slices.Contains(saved.Known, "exec") {
		t.Errorf("saved %s", b)
	}
	if stg, _ := loadSettings(d.cfgDir, "main"); stg.Permissions["exec"] != "allow" {
		t.Errorf("reloaded exec = %q", stg.Permissions["exec"])
	}
	var recorded bool
	for _, r := range d.ev.audited() {
		recorded = recorded || strings.Contains(r, "category=exec mode=allow was=ask") && strings.Contains(r, "surface=dashboard")
	}
	if !recorded {
		t.Errorf("no ModeChanged record from the dashboard: %q", d.ev.audited())
	}
}

// After an update (TEN-343): a save from before it knows five categories, so
// the other three are New until "Keep them at Ask" saves the marker; a
// broker loaded from the file afterwards has nothing New.
func TestSafetyDash_KeepNewSavesMarker(t *testing.T) {
	cfg := t.TempDir()
	old := `{"permissions":{"exec":"ask","write":"ask","destructive":"ask","web":"ask","send":"ask"}}`
	if err := os.WriteFile(settingsPath(cfg, "main"), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	d := newSafetyDashIn(t, cfg)
	if p := d.page(); strings.Count(p, `<span class="tag-new">New</span>`) != 3 || !strings.Contains(p, "An update added 3 new safety checks.") {
		t.Fatalf("an older save: no New checks on the page:\n%s", p)
	}
	if msg, e := d.post("/safety/new", nil); msg != "Kept at Ask: Share private data, Connect remote services, Leave the sandbox. Your agent asks you first." || e != "" {
		t.Fatalf("Keep them at Ask: msg %q err %q", msg, e)
	}
	stg, err := loadSettings(cfg, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{catEgress, catMCP, catSandbox} {
		if !slices.Contains(stg.KnownCategories, c) || stg.Permissions[c] != "ask" {
			t.Errorf("%s not saved as chosen: known %q, modes %v", c, stg.KnownCategories, stg.Permissions)
		}
	}
	fresh := newApprovalBroker(nil)
	fresh.LoadModes(stg.Permissions, stg.KnownCategories)
	if n := fresh.NewCategories(); len(n) != 0 {
		t.Errorf("after a restart, still New: %q", n)
	}
	if p := d.page(); strings.Contains(p, "tag-new") || strings.Contains(p, "/safety/new") {
		t.Error("the New banner or tags outlived the choice")
	}
}

// execSandboxed follows the profile dashboard-driven commands run under:
// "local", else "default"; restricted and enforced.
func TestExecSandboxed(t *testing.T) {
	resetExecProfiles(t)
	no := false
	for _, tc := range []struct {
		profiles map[string]execProfileSetting
		want     bool
	}{
		{nil, false},
		{map[string]execProfileSetting{"local": {Backend: "restricted"}}, true},
		{map[string]execProfileSetting{"default": {Backend: "restricted"}}, true},
		{map[string]execProfileSetting{"default": {Backend: "restricted"}, "local": {Backend: "host"}}, false},
		{map[string]execProfileSetting{"local": {Backend: "restricted", Enforce: &no}}, false},
		{map[string]execProfileSetting{"discord": {Backend: "restricted"}}, false},
	} {
		defaultExecProfiles.Load(tc.profiles)
		if got := execSandboxed(); got != tc.want {
			t.Errorf("%v: execSandboxed = %v, want %v", tc.profiles, got, tc.want)
		}
		if got := newSafetyFacts()().Sandboxed; got != tc.want {
			t.Errorf("%v: the facts say sandboxed %v", tc.profiles, got)
		}
	}
	if f := newSafetyFacts()(); f.Account != runAs() || f.System != runsAsSystem() {
		t.Errorf("facts %+v on this computer", f)
	}
	// The built-in profile follows exec_profiles.default.enforce: a dry run is
	// not sandboxed, the switch on is, and a host fallback never is.
	on := true
	withBuiltinExec(t, "restricted", "")
	for _, tc := range []struct {
		profiles map[string]execProfileSetting
		want     bool
	}{
		{nil, false},
		{map[string]execProfileSetting{"default": {Enforce: &on}}, true},
	} {
		defaultExecProfiles.Load(tc.profiles)
		if got := execSandboxed(); got != tc.want {
			t.Errorf("built-in restricted, %v: execSandboxed = %v, want %v", tc.profiles, got, tc.want)
		}
	}
	withBuiltinExec(t, "host", "no backend")
	if execSandboxed() {
		t.Error("a host fallback is never sandboxed")
	}
}
