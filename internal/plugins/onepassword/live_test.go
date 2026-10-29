package onepassword

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"tenant/internal/model"
)

// TestLive_OnePassword drives the dispatcher over the real op CLI against a
// vault made for it. It needs:
//
//	TENANT_LIVE_OP_TOKEN  a service account token scoped to that vault (read + write)
//	TENANT_LIVE_OP_VAULT  the vault's name
//	TENANT_LIVE_OP_PATH   the op binary when it is not on PATH (optional)
//
// Without them it is skipped. It creates items named tenant-live-<time>,
// reads them back, edits them, and archives them; nothing else in the vault
// is touched. A value it reads is compared, never printed.
func TestLive_OnePassword(t *testing.T) {
	token, vault := os.Getenv("TENANT_LIVE_OP_TOKEN"), os.Getenv("TENANT_LIVE_OP_VAULT")
	if token == "" || vault == "" {
		t.Skip("set TENANT_LIVE_OP_TOKEN and TENANT_LIVE_OP_VAULT to run against 1Password")
	}
	cli, err := NewCLI(os.Getenv("TENANT_LIVE_OP_PATH"), token)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	id, err := cli.WhoAmI(ctx)
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	t.Logf("whoami: type=%q url=%q id=%q email=%q", id.Type, id.URL, id.ID, id.Email)

	var actions []string
	var revealed int
	d := NewDispatcher(cli, Policy{
		Confirm: func(_ context.Context, action, detail string) bool {
			actions = append(actions, action+": "+detail)
			return true
		},
		Revealed: func(string, string) { revealed++ },
	})
	call := func(name string, args map[string]any) (string, bool) {
		t.Helper()
		raw, _ := json.Marshal(args)
		out, isErr, err := d.Dispatch(ctx, model.ToolCall{Name: name, Arguments: raw})
		if err != nil {
			t.Fatalf("%s: transport error: %v", name, err)
		}
		return out, isErr
	}
	mustOK := func(name string, args map[string]any) string {
		t.Helper()
		out, isErr := call(name, args)
		if isErr {
			t.Fatalf("%s %v: %s", name, args, out)
		}
		return out
	}

	stamp := time.Now().UTC().Format("20060102-150405")
	loginTitle := "tenant-live-" + stamp + "-login"
	apiTitle := "tenant-live-" + stamp + "-api"
	noteTitle := "tenant-live-" + stamp + "-note"
	var created []string
	defer func() {
		for _, title := range created {
			if out, isErr := call("op_delete", map[string]any{"vault": vault, "item": title}); isErr {
				t.Logf("cleanup %s: %s", title, out)
			}
		}
	}()

	// The vault is listed.
	if out := mustOK("op_vaults", nil); !strings.Contains(out, vault) {
		t.Fatalf("op_vaults lacks %q:\n%s", vault, out)
	}

	// A login with a generated password, a custom concealed field, notes,
	// tags and a URL: the template on stdin and --generate-password together.
	out := mustOK("op_create", map[string]any{
		"vault": vault, "title": loginTitle, "username": "ops@example.test", "generate_password": true,
		"url": "https://dashboard.example.test", "notes": "made by the live test", "tags": []string{"tenant-live"},
		"fields": []map[string]any{{"label": "Team ID", "value": "team_42", "concealed": true}, {"label": "Region", "value": "us-west"}},
	})
	created = append(created, loginTitle)
	t.Logf("op_create (login):\n%s", out)
	if !strings.Contains(out, "created") || !strings.Contains(out, "[concealed]") || !strings.Contains(out, "ops@example.test") {
		t.Fatalf("op_create answer:\n%s", out)
	}
	idRe := regexp.MustCompile(`id=(\S+)`)
	m := idRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no item id in:\n%s", out)
	}
	loginID := m[1]

	// It is listed, by tag too.
	if out := mustOK("op_items", map[string]any{"vault": vault, "tag": "tenant-live"}); !strings.Contains(out, loginTitle) {
		t.Fatalf("op_items by tag lacks %q:\n%s", loginTitle, out)
	}
	if out := mustOK("op_items", map[string]any{"vault": vault, "category": "login"}); !strings.Contains(out, loginID) {
		t.Fatalf("op_items by category lacks %q:\n%s", loginID, out)
	}

	// Its shape conceals the password and the concealed custom field, and
	// shows the rest.
	out = mustOK("op_item", map[string]any{"vault": vault, "item": loginID})
	t.Logf("op_item:\n%s", out)
	for _, want := range []string{"ops@example.test", "made by the live test", "https://dashboard.example.test", "us-west", "password: [concealed] ref=op://"} {
		if !strings.Contains(out, want) {
			t.Errorf("op_item lacks %q", want)
		}
	}
	if strings.Contains(out, "team_42") {
		t.Errorf("op_item shows a concealed custom field's value")
	}
	// A vault or item name may hold spaces, so the reference runs to the
	// end of its line.
	refRe := regexp.MustCompile(`(?m)^password: \[concealed\] ref=(.+)$`)
	rm := refRe.FindStringSubmatch(out)
	if rm == nil {
		t.Fatalf("no password reference in:\n%s", out)
	}
	passwordRef := strings.TrimSpace(rm[1])

	// op_read resolves the generated password through the confirm.
	first := mustOK("op_read", map[string]any{"reference": passwordRef})
	if len(first) < 16 || strings.ContainsAny(first, "\n\r") {
		t.Errorf("op_read: a %d-character value with a line break %v", len(first), strings.ContainsAny(first, "\n\r"))
	}
	if revealed != 1 || len(actions) == 0 || !strings.HasPrefix(actions[len(actions)-1], ActionReveal+": ") {
		t.Errorf("reveal hook %d, actions %v", revealed, actions)
	}
	teamRef := "op://" + vault + "/" + loginTitle + "/Team ID"
	if got := mustOK("op_read", map[string]any{"reference": teamRef}); got != "team_42" {
		t.Errorf("op_read of a concealed custom field: %d characters, want team_42", len(got))
	}

	// An edit: a new title, a new text field, a regenerated password.
	newTitle := loginTitle + "-renamed"
	out = mustOK("op_edit", map[string]any{"vault": vault, "item": loginID, "title": newTitle, "generate_password": true,
		"fields": []map[string]any{{"label": "Plan", "value": "starter"}}})
	created[0] = newTitle
	t.Logf("op_edit:\n%s", out)
	if !strings.Contains(out, "edited") || !strings.Contains(out, newTitle) || !strings.Contains(out, "starter") {
		t.Errorf("op_edit answer:\n%s", out)
	}
	second := mustOK("op_read", map[string]any{"reference": "op://" + vault + "/" + newTitle + "/password"})
	if second == first {
		t.Errorf("generate_password on edit kept the old password")
	}

	// An API credential with an explicit secret: the value goes in on
	// stdin and comes back exactly.
	secret := "sk_test_" + stamp + "_live"
	out = mustOK("op_create", map[string]any{"vault": vault, "title": apiTitle, "category": "api_credential", "username": "svc", "password": secret})
	created = append(created, apiTitle)
	t.Logf("op_create (api_credential):\n%s", out)
	if got := mustOK("op_read", map[string]any{"reference": "op://" + vault + "/" + apiTitle + "/credential"}); got != secret {
		t.Errorf("api credential round trip: %d characters, want %d", len(got), len(secret))
	}

	// A secure note.
	out = mustOK("op_create", map[string]any{"vault": vault, "title": noteTitle, "category": "secure_note", "notes": "runbook: step one"})
	created = append(created, noteTitle)
	if out := mustOK("op_item", map[string]any{"vault": vault, "item": noteTitle}); !strings.Contains(out, "runbook: step one") {
		t.Errorf("secure note:\n%s", out)
	}

	// The vault fence resolves an id to the allowed name.
	vaults, err := cli.Vaults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var vaultID string
	for _, v := range vaults {
		if v.Name == vault {
			vaultID = v.ID
		}
	}
	fenced := NewDispatcher(cli, Policy{Vaults: []string{vault}})
	raw, _ := json.Marshal(map[string]any{"vault": vaultID, "tag": "tenant-live"})
	if out, isErr, _ := fenced.Dispatch(ctx, model.ToolCall{Name: "op_items", Arguments: raw}); isErr || !strings.Contains(out, newTitle) {
		t.Errorf("the fence by name must allow the vault's id: %v %s", isErr, out)
	}
	raw, _ = json.Marshal(map[string]any{"vault": "no-such-vault-" + stamp})
	if out, isErr, _ := fenced.Dispatch(ctx, model.ToolCall{Name: "op_items", Arguments: raw}); !isErr || !strings.Contains(out, "refused") {
		t.Errorf("an unknown vault is refused by the fence: %v %s", isErr, out)
	}

	// Archiving asks and works; an archived item is gone from the listing.
	for _, title := range created {
		if out, isErr := call("op_delete", map[string]any{"vault": vault, "item": title}); isErr {
			t.Errorf("op_delete %s: %s", title, out)
		}
	}
	created = nil
	if out := mustOK("op_items", map[string]any{"vault": vault, "tag": "tenant-live"}); strings.Contains(out, apiTitle) {
		t.Errorf("an archived item is still listed:\n%s", out)
	}
	fmt.Fprintln(os.Stderr, "live test done; revealed", revealed, "values, actions:", len(actions))
}
