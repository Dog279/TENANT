package onepassword

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- desktop sign-in through op ---

func testDesktopCLI(r *recorder) *CLI {
	return &CLI{path: "op", desktop: true, account: "team.1password.com", timeout: time.Second, run: r.run}
}

func TestDesktopSignInCarriesNoTokenAndNamesTheAccount(t *testing.T) {
	t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "ops_from_the_parent_environment")
	t.Setenv("OP_CONNECT_HOST", "http://connect.invalid:8080")
	t.Setenv("OP_CONNECT_TOKEN", "eyJ-parent")
	t.Setenv("OP_BIOMETRIC_UNLOCK_ENABLED", "false")
	r := &recorder{answers: map[string]string{
		"vault list":   `[{"id":"v-ops","name":"Ops"}]`,
		"account list": `[{"url":"team.1password.com","email":"wendy@example.test","user_uuid":"U1","account_uuid":"A1"}]`,
	}}
	c := testDesktopCLI(r)
	if _, err := c.Vaults(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.calls[0], " "); got != "vault list --format json --account team.1password.com" {
		t.Errorf("args: %q", got)
	}
	var unlock int
	for _, kv := range r.envs[0] {
		name, _, _ := strings.Cut(kv, "=")
		switch {
		case kv == "OP_BIOMETRIC_UNLOCK_ENABLED=true":
			unlock++
		case name == "OP_SERVICE_ACCOUNT_TOKEN", strings.HasPrefix(name, "OP_CONNECT_"), name == "OP_BIOMETRIC_UNLOCK_ENABLED":
			t.Errorf("the parent's sign-in leaked into the child: %s", name)
		}
	}
	if unlock != 1 {
		t.Errorf("the child is sent to the app %d times, want once", unlock)
	}
	// Listing the app's accounts is the probe that never prompts, and it is
	// not narrowed to one account.
	accounts, err := c.Accounts(context.Background())
	if err != nil || len(accounts) != 1 || accounts[0].URL != "team.1password.com" || accounts[0].ID != "A1" || accounts[0].UserID != "U1" {
		t.Fatalf("accounts: %+v %v", accounts, err)
	}
	if got := strings.Join(r.calls[1], " "); got != "account list --format json" {
		t.Errorf("args: %q", got)
	}
}

// With the app, op whoami always answers "not signed in" and the app then
// refuses the next connection for a moment, so desktop WhoAmI asks for the
// user and never runs whoami.
func TestDesktopWhoAmIAsksForTheUserAndNeverRunsWhoami(t *testing.T) {
	r := &recorder{answers: map[string]string{
		"user get": `{"id":"U1","name":"Wendy","email":"wendy@example.test","type":"MEMBER","state":"ACTIVE"}`,
	}}
	id, err := testDesktopCLI(r).WhoAmI(context.Background())
	if err != nil || id != (Identity{URL: "team.1password.com", Email: "wendy@example.test", Type: "MEMBER", ID: "U1"}) {
		t.Fatalf("whoami: %+v %v", id, err)
	}
	if len(r.calls) != 1 || opName(r.calls[0]) != "user get" {
		t.Errorf("calls: %v", r.calls)
	}
}

// The app refuses a connection for a moment now and then. op gave up before
// sending anything, so the call is made again; a refusal that lasts is
// reported, and nothing else is ever repeated.
func TestDesktopAsksAgainWhenTheAppRefusesTheConnection(t *testing.T) {
	old := appRetry
	appRetry = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	defer func() { appRetry = old }()
	const refused = "[ERROR] 2026/11/22 15:44:02 connecting to desktop app: cannot connect to 1Password app, make sure it is running"
	const settingOff = "[ERROR] 2026/11/22 15:13:13 connecting to desktop app: connection reset, make sure 1Password CLI is installed correctly"
	flaky := func(failures int, stderr string, calls *int) func(context.Context, string, []string, string, ...string) ([]byte, []byte, error) {
		return func(context.Context, string, []string, string, ...string) ([]byte, []byte, error) {
			*calls++
			if *calls <= failures {
				return nil, []byte(stderr), errors.New("exit status 1")
			}
			return []byte(`[{"id":"v-ops","name":"Ops"}]`), nil, nil
		}
	}
	for _, tc := range []struct {
		name      string
		desktop   bool
		failures  int
		stderr    string
		wantCalls int
		wantErr   error
	}{
		{"refused twice, then answered", true, 2, refused, 3, nil},
		{"refused for good", true, 99, refused, 4, ErrAppUnavailable},
		{"the setting is off: asking again cannot help", true, 99, settingOff, 1, ErrAppUnavailable},
		{"a service account never asks again", false, 99, refused, 1, ErrAppUnavailable},
	} {
		var calls int
		c := &CLI{path: "op", desktop: tc.desktop, timeout: time.Second, run: flaky(tc.failures, tc.stderr, &calls)}
		_, err := c.Vaults(context.Background())
		if calls != tc.wantCalls || !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: %d call(s), err %v; want %d call(s), err %v", tc.name, calls, err, tc.wantCalls, tc.wantErr)
		}
	}
}

func TestDesktopFailuresAreNamed(t *testing.T) {
	// The first line is what op 2.42.0 printed on Windows with the app's CLI
	// integration switched off.
	const appOff = "[ERROR] 2026/11/22 15:13:13 connecting to desktop app: connection reset, make sure 1Password CLI is installed correctly, " +
		"then open the 1Password app, select 1Password > Settings > Developer and make sure the 'Integrate with 1Password CLI' setting is turned on."
	for stderr, want := range map[string]error{
		appOff: ErrAppUnavailable,
		"[ERROR] 2026/11/22 15:13:13 authorization prompt dismissed, please try again": ErrNotApproved,
	} {
		r := &recorder{fail: true, stderr: stderr}
		_, err := testDesktopCLI(r).Vaults(context.Background())
		if !errors.Is(err, want) {
			t.Errorf("%q: %v, want %v", stderr[:60], err, want)
		}
		if err != nil && len(err.Error()) > 260 {
			t.Errorf("the error carries op's whole paragraph: %d bytes", len(err.Error()))
		}
	}
	// A prompt nobody answers is "not approved", not a stall.
	c := &CLI{path: "op", desktop: true, timeout: 10 * time.Millisecond, run: func(ctx context.Context, _ string, _ []string, _ string, _ ...string) ([]byte, []byte, error) {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}}
	if _, err := c.Vaults(context.Background()); !errors.Is(err, ErrNotApproved) {
		t.Errorf("an unanswered prompt: %v", err)
	}
}

// --- the dispatcher's rules for the new modes ---

// Desktop sign-in asks the person at the machine. A turn with nobody there
// is refused before the backend is touched, so no prompt goes up.
func TestATurnWithNobodyPresentNeverReachesTheBackend(t *testing.T) {
	f := newFake()
	present := false
	d := NewDispatcher(f, Policy{
		AllowReveal: true,
		Confirm:     func(context.Context, string, string) bool { return true },
		Present:     func(context.Context) bool { return present },
	})
	for name, args := range map[string]string{
		"op_vaults": `{}`,
		"op_items":  `{"vault":"Ops"}`,
		"op_item":   `{"vault":"Ops","item":"Stripe"}`,
		"op_read":   `{"reference":"op://Ops/Stripe/password"}`,
		"op_delete": `{"vault":"Ops","item":"Stripe"}`,
	} {
		if out, isErr := call(t, d, name, args); !isErr || !strings.Contains(out, "nobody at the machine") {
			t.Errorf("%s with nobody present: %q", name, out)
		}
	}
	if len(f.reads) != 0 || len(f.archived) != 0 {
		t.Fatalf("a refused call reached the backend: reads=%v archived=%v", f.reads, f.archived)
	}
	present = true
	if out, isErr := call(t, d, "op_read", `{"reference":"op://Ops/Stripe/password"}`); isErr || out != "hunter2-hunter2" {
		t.Errorf("with a person present: %q", out)
	}
}

// A Connect server cannot archive: the tool says so without asking anyone to
// approve, and without a request to the server.
func TestArchiveOverConnectIsRefusedBeforeAnyoneIsAsked(t *testing.T) {
	f, c := newFakeConnect(t)
	asked := false
	d := NewDispatcher(c, Policy{Confirm: func(context.Context, string, string) bool { asked = true; return true }})
	out, isErr := call(t, d, "op_delete", `{"vault":"Ops","item":"Stripe"}`)
	if !isErr || !strings.Contains(out, "Connect") || asked || len(f.reqs) != 0 {
		t.Errorf("archive over Connect: %q asked=%v requests=%d", out, asked, len(f.reqs))
	}
}

// --- Connect hardening ---

// The token is for the Connect server alone: a redirect is reported, never
// followed, so the token cannot be walked to another host.
func TestConnectNeverFollowsARedirectWithTheToken(t *testing.T) {
	var elsewhere int
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere++
		_, _ = w.Write([]byte(`[]`))
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	c, err := NewConnect(srv.URL, connectToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Vaults(context.Background()); err == nil || !strings.Contains(err.Error(), "307") {
		t.Errorf("a redirect is an error: %v", err)
	}
	if elsewhere != 0 {
		t.Errorf("the redirect was followed: %d request(s) reached the other host", elsewhere)
	}
}

func TestNewConnectRefusesAnAddressThatCarriesMoreThanTheServer(t *testing.T) {
	for _, in := range []string{"https://user:secret@connect.example.test", "https://connect.example.test/?x=1", "https://connect.example.test/#frag"} {
		_, err := NewConnect(in, "tok")
		if err == nil {
			t.Errorf("%q is accepted", in)
		} else if strings.Contains(err.Error(), "secret") {
			t.Errorf("the refusal quotes the address: %v", err)
		}
	}
}

func TestConnectRefusesAVaultNameTwoVaultsShare(t *testing.T) {
	f, c := newFakeConnect(t)
	f.vaults = append(f.vaults, cVault{ID: "vops2", Name: "ops"})
	if _, err := c.Items(context.Background(), "Ops", nil, nil); err == nil || !strings.Contains(err.Error(), "id") {
		t.Errorf("an ambiguous vault name: %v", err)
	}
	if _, err := c.Items(context.Background(), "vops2", nil, nil); err != nil {
		t.Errorf("the id still works: %v", err)
	}
}
