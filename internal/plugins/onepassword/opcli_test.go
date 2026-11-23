package onepassword

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// recorder stands in for the op process: it records the call and answers
// from a table keyed by the first two arguments.
type recorder struct {
	calls   [][]string
	envs    [][]string
	stdins  []string
	answers map[string]string // "item get" -> stdout
	stderr  string
	fail    bool
}

func (r *recorder) run(_ context.Context, _ string, env []string, stdin string, args ...string) ([]byte, []byte, error) {
	r.calls = append(r.calls, args)
	r.envs = append(r.envs, env)
	r.stdins = append(r.stdins, stdin)
	if r.fail {
		return nil, []byte(r.stderr), errors.New("exit status 1")
	}
	return []byte(r.answers[opName(args)]), nil, nil
}

func testCLI(r *recorder) *CLI {
	return &CLI{path: "op", token: "ops_test_token_1234567890", timeout: time.Second, run: r.run}
}

const itemJSON = `{"id":"it-1","title":"Stripe","category":"LOGIN","vault":{"id":"v-ops","name":"Ops"},"tags":["billing"],"updated_at":"2026-10-28T10:00:00Z",
 "fields":[{"id":"username","type":"STRING","purpose":"USERNAME","label":"username","value":"ops@example.test","reference":"op://Ops/Stripe/username"},
           {"id":"password","type":"CONCEALED","purpose":"PASSWORD","label":"password","value":"hunter2-hunter2","reference":"op://Ops/Stripe/password","section":{"id":"s1","label":"Keys"}}],
 "urls":[{"label":"website","primary":true,"href":"https://dashboard.stripe.com"}]}`

func TestTheTokenRidesInTheEnvironmentNotTheArguments(t *testing.T) {
	t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "ops_from_the_parent_environment")
	t.Setenv("OP_SESSION_my", "stale-session")
	r := &recorder{answers: map[string]string{"vault list": `[{"id":"v-ops","name":"Ops"}]`}}
	c := testCLI(r)
	vaults, err := c.Vaults(context.Background())
	if err != nil || len(vaults) != 1 || vaults[0].Name != "Ops" {
		t.Fatalf("vaults: %+v %v", vaults, err)
	}
	for _, a := range r.calls[0] {
		if strings.Contains(a, "ops_") {
			t.Errorf("the token is in the arguments: %v", r.calls[0])
		}
	}
	var tokens, sessions int
	for _, kv := range r.envs[0] {
		switch {
		case kv == "OP_SERVICE_ACCOUNT_TOKEN=ops_test_token_1234567890":
			tokens++
		case strings.HasPrefix(kv, "OP_SERVICE_ACCOUNT_TOKEN="):
			t.Errorf("a parent token leaked into the child: %s", kv)
		case strings.HasPrefix(kv, "OP_SESSION"):
			sessions++
		}
	}
	if tokens != 1 || sessions != 0 {
		t.Errorf("child env: %d tokens, %d sessions", tokens, sessions)
	}
	if got := strings.Join(r.calls[0], " "); got != "vault list --format json" {
		t.Errorf("args: %q", got)
	}
}

func TestItemDecodesFieldsSectionsAndURLs(t *testing.T) {
	r := &recorder{answers: map[string]string{"item get": itemJSON}}
	it, err := testCLI(r).Item(context.Background(), "Ops", "Stripe")
	if err != nil {
		t.Fatal(err)
	}
	if it.ID != "it-1" || it.Vault.Name != "Ops" || len(it.Fields) != 2 || len(it.URLs) != 1 || it.URLs[0].Href != "https://dashboard.stripe.com" {
		t.Errorf("item: %+v", it)
	}
	if f := it.Fields[1]; f.Section != "Keys" || f.Reference != "op://Ops/Stripe/password" || !Concealed(f) {
		t.Errorf("field: %+v", f)
	}
	if got := strings.Join(r.calls[0], " "); got != "item get Stripe --vault Ops --format json" {
		t.Errorf("args: %q", got)
	}
}

func TestListFiltersAndRead(t *testing.T) {
	r := &recorder{answers: map[string]string{"item list": `[` + itemJSON + `]`, "read": "s3cret-value"}}
	c := testCLI(r)
	items, err := c.Items(context.Background(), "Ops", []string{"Login"}, []string{"billing"})
	if err != nil || len(items) != 1 || items[0].Title != "Stripe" {
		t.Fatalf("items: %+v %v", items, err)
	}
	if got := strings.Join(r.calls[0], " "); got != "item list --vault Ops --format json --categories Login --tags billing" {
		t.Errorf("args: %q", got)
	}
	v, err := c.Read(context.Background(), "op://Ops/Stripe/password")
	if err != nil || v != "s3cret-value" {
		t.Errorf("read: %q %v", v, err)
	}
	if got := strings.Join(r.calls[1], " "); got != "read op://Ops/Stripe/password --no-newline" {
		t.Errorf("args: %q", got)
	}
}

func TestCreateSendsTheTemplateOnStdin(t *testing.T) {
	r := &recorder{answers: map[string]string{"item create": itemJSON}}
	c := testCLI(r)
	_, err := c.Create(context.Background(), CreateParams{
		Vault: "Ops", Title: "Vercel", Category: "login", Username: "ops@example.test", URL: "https://vercel.com",
		Notes: "hosting", Tags: []string{"hosting"}, Fields: []FieldInput{{Label: "Team ID", Value: "team_1", Concealed: true}}, GeneratePassword: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(r.calls[0], " ")
	if args != "item create - --vault Ops --format json --generate-password=letters,digits,symbols,32" {
		t.Errorf("args: %q", args)
	}
	if strings.Contains(args, "team_1") || strings.Contains(args, "ops@example.test") {
		t.Errorf("a value is in the arguments: %q", args)
	}
	var tpl template
	if err := json.Unmarshal([]byte(r.stdins[0]), &tpl); err != nil {
		t.Fatalf("stdin is not the template: %v\n%s", err, r.stdins[0])
	}
	if tpl.Title != "Vercel" || tpl.Category != "LOGIN" || len(tpl.URLs) != 1 || tpl.URLs[0].Href != "https://vercel.com" {
		t.Errorf("template: %+v", tpl)
	}
	byLabel := map[string]templateField{}
	for _, f := range tpl.Fields {
		byLabel[f.Label] = f
	}
	if _, has := byLabel["password"]; has {
		t.Errorf("generate_password must leave the password to 1Password: %+v", tpl.Fields)
	}
	if f := byLabel["Team ID"]; f.Type != "CONCEALED" || f.Value != "team_1" || f.ID != "team_id" {
		t.Errorf("custom field: %+v", f)
	}
	if f := byLabel["notesPlain"]; f.Purpose != "NOTES" || f.Value != "hosting" {
		t.Errorf("notes: %+v", f)
	}

	r = &recorder{answers: map[string]string{"item create": itemJSON}}
	c = testCLI(r)
	if _, err := c.Create(context.Background(), CreateParams{Vault: "Ops", Title: "K", Category: "api_credential", Password: "sk_live"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.stdins[0], `"credential"`) || !strings.Contains(r.stdins[0], "sk_live") || strings.Contains(strings.Join(r.calls[0], " "), "sk_live") {
		t.Errorf("api credential: stdin %s args %v", r.stdins[0], r.calls[0])
	}
	if _, err := c.Create(context.Background(), CreateParams{Vault: "Ops", Title: "K", Category: "wireless router"}); err == nil {
		t.Errorf("an unsupported category is refused")
	}
}

func TestEditAssignmentsAndArchive(t *testing.T) {
	r := &recorder{answers: map[string]string{"item edit": itemJSON}}
	c := testCLI(r)
	_, err := c.Edit(context.Background(), EditParams{Vault: "Ops", Item: "it-1", Title: "Stripe (test)", Tags: []string{}, GeneratePassword: true,
		Fields: []FieldInput{{Label: "api.key", Value: "v=1", Concealed: true}, {Label: "note", Value: "plain"}}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"item", "edit", "it-1", "--vault", "Ops", "--format", "json", "--title", "Stripe (test)", "--tags", "", "--generate-password=letters,digits,symbols,32", `api\.key[password]=v=1`, "note[text]=plain"}
	if got := r.calls[0]; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("args:\n got %q\nwant %q", got, want)
	}
	if err := c.Archive(context.Background(), "Ops", "it-1"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.calls[1], " "); got != "item delete it-1 --vault Ops --archive" {
		t.Errorf("archive args: %q", got)
	}
}

func TestStderrIsClassified(t *testing.T) {
	cases := []struct {
		stderr string
		want   error
	}{
		{"[ERROR] 2026/10/28 10:00:00 (429) Too Many Requests: You've reached the maximum number of this type of requests", ErrRateLimited},
		{"[ERROR] 2026/10/28 10:00:00 (401) Unauthorized: the token is invalid", ErrAuth},
		{"[ERROR] 2026/10/28 10:00:00 service account token is invalid", ErrAuth},
		{"[ERROR] 2026/10/28 10:00:00 \"Nope\" isn't an item in the \"Ops\" vault. Specify the item with its UUID, name, or domain.", ErrNotFound},
		{"[ERROR] 2026/10/28 10:00:00 \"Zzz\" isn't a vault in this account. Specify the vault with its UUID or name.", ErrNotFound},
	}
	for _, tc := range cases {
		r := &recorder{fail: true, stderr: tc.stderr}
		_, err := testCLI(r).Vaults(context.Background())
		if !errors.Is(err, tc.want) {
			t.Errorf("%q -> %v, want %v", tc.stderr, err, tc.want)
		}
		if strings.Contains(err.Error(), "[ERROR]") || strings.Contains(err.Error(), "2026/10/28") {
			t.Errorf("the prefix survives: %v", err)
		}
	}
	r := &recorder{fail: true, stderr: "[ERROR] 2026/10/28 10:00:00 something else went wrong\nmore detail"}
	_, err := testCLI(r).Vaults(context.Background())
	if err == nil || err.Error() != "op vault list failed: something else went wrong" {
		t.Errorf("plain failure: %v", err)
	}
	if _, err := testCLI(&recorder{answers: map[string]string{"whoami": "not json"}}).WhoAmI(context.Background()); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Errorf("bad JSON: %v", err)
	}
}

func TestWhoAmIReadsEitherSpelling(t *testing.T) {
	r := &recorder{answers: map[string]string{"whoami": `{"url":"https://example.1password.com","user_type":"SERVICE_ACCOUNT","user_uuid":"abc"}`}}
	id, err := testCLI(r).WhoAmI(context.Background())
	if err != nil || id.URL != "https://example.1password.com" || id.Type != "SERVICE_ACCOUNT" || id.ID != "abc" {
		t.Errorf("whoami: %+v %v", id, err)
	}
	r = &recorder{answers: map[string]string{"whoami": `{"URL":"https://x.1password.com","Email":"a@b.c","UserType":"MEMBER"}`}}
	id, err = testCLI(r).WhoAmI(context.Background())
	if err != nil || id.URL != "https://x.1password.com" || id.Email != "a@b.c" || id.Type != "MEMBER" {
		t.Errorf("whoami (capitalised): %+v %v", id, err)
	}
}

func TestNewCLIFindsTheBinaryOrSaysSo(t *testing.T) {
	if _, err := NewCLI(filepath.Join(t.TempDir(), "no-such-op"), "ops_x"); !errors.Is(err, ErrCLINotFound) {
		t.Errorf("missing binary: %v", err)
	}
	// A real executable at a full path is found, whatever it is called.
	self, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	c, err := NewCLI(self, "ops_x")
	if err != nil || c.Path() == "" {
		t.Errorf("executable at a path: %v %v", c, err)
	}
	if runtime.GOOS == "windows" {
		dir := t.TempDir()
		stub := filepath.Join(dir, "op.exe")
		if err := os.WriteFile(stub, []byte("MZ"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir)
		if c, err := NewCLI("", "ops_x"); err != nil || !strings.EqualFold(c.Path(), stub) {
			t.Errorf("op on PATH: %v %v", c, err)
		}
	}
}

func TestATimeoutIsNamed(t *testing.T) {
	c := &CLI{path: "op", token: "ops_x", timeout: 10 * time.Millisecond, run: func(ctx context.Context, _ string, _ []string, _ string, _ ...string) ([]byte, []byte, error) {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}}
	_, err := c.Vaults(context.Background())
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("timeout: %v", err)
	}
	// A binary that cannot start is the CLI missing, not a plain failure.
	c = &CLI{path: "op", token: "ops_x", timeout: time.Second, run: func(context.Context, string, []string, string, ...string) ([]byte, []byte, error) {
		return nil, nil, &exec.Error{Name: "op", Err: exec.ErrNotFound}
	}}
	if _, err := c.Vaults(context.Background()); !errors.Is(err, ErrCLINotFound) {
		t.Errorf("exec error: %v", err)
	}
}
