package onepassword

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"tenant/internal/model"
)

// fake is a Backend in memory: two vaults, one login with a concealed
// password, and a record of what was asked.
type fake struct {
	vaults   []Vault
	items    map[string][]Item // by vault id
	reads    []string
	created  []CreateParams
	edited   []EditParams
	archived []string
	err      error
}

func newFake() *fake {
	ops := Vault{ID: "v-ops", Name: "Ops"}
	private := Vault{ID: "v-home", Name: "Home"}
	return &fake{
		vaults: []Vault{ops, private},
		items: map[string][]Item{
			"v-ops": {{
				ID: "it-1", Title: "Stripe", Category: "LOGIN", Vault: ops, Tags: []string{"billing"},
				URLs: []URL{{Label: "website", Href: "https://dashboard.stripe.com", Primary: true}},
				Fields: []Field{
					{ID: "username", Label: "username", Type: "STRING", Purpose: "USERNAME", Value: "ops@example.test", Reference: "op://Ops/Stripe/username"},
					{ID: "password", Label: "password", Type: "CONCEALED", Purpose: "PASSWORD", Value: "hunter2-hunter2", Reference: "op://Ops/Stripe/password"},
					{ID: "notesPlain", Label: "notesPlain", Type: "STRING", Purpose: "NOTES", Value: "test mode only"},
					{ID: "otp", Label: "one-time password", Type: "OTP", Value: "otpauth://x", Reference: "op://Ops/Stripe/one-time password"},
				},
			}},
			"v-home": {{ID: "it-9", Title: "Bank", Category: "LOGIN", Vault: private}},
		},
	}
}

func (f *fake) vault(name string) (Vault, bool) {
	for _, v := range f.vaults {
		if strings.EqualFold(v.ID, name) || strings.EqualFold(v.Name, name) {
			return v, true
		}
	}
	return Vault{}, false
}

func (f *fake) WhoAmI(context.Context) (Identity, error) {
	return Identity{URL: "https://example.1password.com", Type: "SERVICE_ACCOUNT"}, f.err
}
func (f *fake) Vaults(context.Context) ([]Vault, error) { return f.vaults, f.err }
func (f *fake) Items(_ context.Context, vault string, _, _ []string) ([]ItemOverview, error) {
	if f.err != nil {
		return nil, f.err
	}
	v, ok := f.vault(vault)
	if !ok {
		return nil, ErrNotFound
	}
	var out []ItemOverview
	for _, it := range f.items[v.ID] {
		out = append(out, ItemOverview{ID: it.ID, Title: it.Title, Category: it.Category, Vault: it.Vault, Tags: it.Tags})
	}
	return out, nil
}
func (f *fake) Item(_ context.Context, vault, item string) (Item, error) {
	if f.err != nil {
		return Item{}, f.err
	}
	v, ok := f.vault(vault)
	if !ok {
		return Item{}, ErrNotFound
	}
	for _, it := range f.items[v.ID] {
		if it.ID == item || strings.EqualFold(it.Title, item) {
			return it, nil
		}
	}
	return Item{}, ErrNotFound
}
func (f *fake) Read(_ context.Context, ref string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.reads = append(f.reads, ref)
	for _, items := range f.items {
		for _, it := range items {
			for _, fl := range it.Fields {
				if fl.Reference == ref {
					return fl.Value, nil
				}
			}
		}
	}
	return "", ErrNotFound
}
func (f *fake) Create(_ context.Context, p CreateParams) (Item, error) {
	if f.err != nil {
		return Item{}, f.err
	}
	f.created = append(f.created, p)
	v, _ := f.vault(p.Vault)
	return Item{ID: "it-new", Title: p.Title, Category: "LOGIN", Vault: v, Fields: []Field{
		{Label: "username", Type: "STRING", Purpose: "USERNAME", Value: p.Username, Reference: "op://Ops/" + p.Title + "/username"},
		{Label: "password", Type: "CONCEALED", Purpose: "PASSWORD", Value: "generated-by-1password", Reference: "op://Ops/" + p.Title + "/password"},
	}}, nil
}
func (f *fake) Edit(_ context.Context, p EditParams) (Item, error) {
	if f.err != nil {
		return Item{}, f.err
	}
	f.edited = append(f.edited, p)
	v, _ := f.vault(p.Vault)
	return Item{ID: "it-1", Title: firstNonEmpty(p.Title, "Stripe"), Category: "LOGIN", Vault: v}, nil
}
func (f *fake) Archive(_ context.Context, _, item string) error {
	if f.err != nil {
		return f.err
	}
	f.archived = append(f.archived, item)
	return nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func call(t *testing.T, d *Dispatcher, name, args string) (string, bool) {
	t.Helper()
	out, isErr, err := d.Dispatch(context.Background(), model.ToolCall{Name: name, Arguments: json.RawMessage(args)})
	if err != nil {
		t.Fatalf("%s: transport error: %v", name, err)
	}
	return out, isErr
}

func allow(string, string) bool { return true }

func TestToolsAreDeclaredAndGated(t *testing.T) {
	tools := NewDispatcher(nil, Policy{}).Tools()
	gated := map[string]bool{}
	for _, tl := range tools {
		if tl.Name == "" || tl.Description == "" || len(tl.Parameters) == 0 {
			t.Errorf("tool %+v is incomplete", tl)
		}
		if !strings.HasPrefix(tl.Name, "op_") {
			t.Errorf("tool %q is not under the op_ prefix", tl.Name)
		}
		var schema map[string]any
		if err := json.Unmarshal(tl.Parameters, &schema); err != nil {
			t.Errorf("%s: parameters are not JSON: %v", tl.Name, err)
		}
		gated[tl.Name] = tl.Gated
	}
	for _, name := range []string{"op_read", "op_create", "op_edit", "op_delete"} {
		if !gated[name] {
			t.Errorf("%s must be gated", name)
		}
	}
	for _, name := range []string{"op_vaults", "op_items", "op_item"} {
		if gated[name] {
			t.Errorf("%s is a read and must not be gated", name)
		}
	}
	out, isErr, err := NewDispatcher(nil, Policy{}).Dispatch(context.Background(), model.ToolCall{Name: "op_vaults", Arguments: json.RawMessage(`{}`)})
	if err != nil || !isErr || !strings.Contains(out, "/configure onepassword") {
		t.Errorf("stub: %q %v %v", out, isErr, err)
	}
}

func TestVaultAllowlistByNameAndID(t *testing.T) {
	d := NewDispatcher(newFake(), Policy{Vaults: []string{"ops"}})
	out, isErr := call(t, d, "op_vaults", `{}`)
	if isErr || strings.Contains(out, "Home") || !strings.Contains(out, "Ops") {
		t.Errorf("op_vaults lists only the allowed vault: %q", out)
	}
	for _, vault := range []string{"Ops", "v-ops", "OPS"} {
		if out, isErr := call(t, d, "op_items", `{"vault":"`+vault+`"}`); isErr || !strings.Contains(out, "Stripe") {
			t.Errorf("vault %q should be allowed by name or id: %q", vault, out)
		}
	}
	for _, vault := range []string{"Home", "v-home"} {
		if out, isErr := call(t, d, "op_items", `{"vault":"`+vault+`"}`); !isErr || !strings.Contains(out, "refused") {
			t.Errorf("vault %q must be refused: %q", vault, out)
		}
	}
	if out, isErr := call(t, d, "op_read", `{"reference":"op://Home/Bank/password"}`); !isErr || !strings.Contains(out, "refused") {
		t.Errorf("a reference into a refused vault must be refused: %q", out)
	}
}

func TestItemConcealsSecretsAndKeepsReferences(t *testing.T) {
	d := NewDispatcher(newFake(), Policy{})
	out, isErr := call(t, d, "op_item", `{"vault":"Ops","item":"Stripe"}`)
	if isErr {
		t.Fatalf("op_item: %q", out)
	}
	for _, want := range []string{"ops@example.test", "test mode only", "https://dashboard.stripe.com", "[concealed] ref=op://Ops/Stripe/password", "one-time password: [concealed]"} {
		if !strings.Contains(out, want) {
			t.Errorf("op_item lacks %q:\n%s", want, out)
		}
	}
	for _, leak := range []string{"hunter2", "otpauth"} {
		if strings.Contains(out, leak) {
			t.Errorf("op_item leaked %q:\n%s", leak, out)
		}
	}
}

func TestReadIsGatedAndReportsToTheHost(t *testing.T) {
	f := newFake()
	denied := NewDispatcher(f, Policy{Confirm: func(context.Context, string, string) bool { return false }})
	out, isErr := call(t, denied, "op_read", `{"reference":"op://Ops/Stripe/password"}`)
	if !isErr || !strings.Contains(out, "blocked") || strings.Contains(out, "hunter2") {
		t.Errorf("denied read: %q", out)
	}
	if len(f.reads) != 0 {
		t.Errorf("a denied read must not reach the backend: %v", f.reads)
	}
	noConfirm := NewDispatcher(f, Policy{})
	if out, isErr := call(t, noConfirm, "op_read", `{"reference":"op://Ops/Stripe/password"}`); !isErr || strings.Contains(out, "hunter2") {
		t.Errorf("nil confirm denies: %q", out)
	}

	var action, got string
	var revealed []string
	allowed := NewDispatcher(f, Policy{
		Confirm:  func(_ context.Context, a, detail string) bool { action, got = a, detail; return true },
		Revealed: func(ref, value string) { revealed = append(revealed, ref+"="+value) },
	})
	out, isErr = call(t, allowed, "op_read", `{"reference":"op://Ops/Stripe/password"}`)
	if isErr || out != "hunter2-hunter2" {
		t.Errorf("approved read: %q %v", out, isErr)
	}
	if action != ActionReveal || got != "op://Ops/Stripe/password" {
		t.Errorf("confirm asked %q %q", action, got)
	}
	if len(revealed) != 1 || revealed[0] != "op://Ops/Stripe/password=hunter2-hunter2" {
		t.Errorf("the host hears the value: %v", revealed)
	}
	if out, isErr := call(t, allowed, "op_read", `{"reference":"Stripe/password"}`); !isErr || !strings.Contains(out, "op://") {
		t.Errorf("a non-reference is refused: %q", out)
	}
	if out, isErr := call(t, NewDispatcher(f, Policy{AllowReveal: true}), "op_read", `{"reference":"op://Ops/Stripe/username"}`); isErr || out != "ops@example.test" {
		t.Errorf("AllowReveal reads without a confirm: %q", out)
	}
}

func TestWritesNeedTheFlagOrAConfirm(t *testing.T) {
	f := newFake()
	off := NewDispatcher(f, Policy{})
	if out, isErr := call(t, off, "op_create", `{"vault":"Ops","title":"Vercel","username":"u","generate_password":true}`); !isErr || !strings.Contains(out, "blocked") {
		t.Errorf("create without AllowWrite: %q", out)
	}
	if out, isErr := call(t, off, "op_edit", `{"vault":"Ops","item":"Stripe","tags":["x"]}`); !isErr || !strings.Contains(out, "blocked") {
		t.Errorf("edit without AllowWrite: %q", out)
	}
	if len(f.created)+len(f.edited) != 0 {
		t.Fatalf("blocked writes reached the backend")
	}

	on := NewDispatcher(f, Policy{AllowWrite: true})
	out, isErr := call(t, on, "op_create", `{"vault":"Ops","title":"Vercel","username":"u","generate_password":true,"url":"https://vercel.com","tags":["hosting"],"fields":[{"label":"team","value":"acme"}]}`)
	if isErr || !strings.Contains(out, "created") || !strings.Contains(out, "[concealed] ref=op://Ops/Vercel/password") || strings.Contains(out, "generated-by-1password") {
		t.Errorf("create: %q", out)
	}
	if len(f.created) != 1 || !f.created[0].GeneratePassword || f.created[0].URL != "https://vercel.com" || len(f.created[0].Fields) != 1 || f.created[0].Fields[0].Label != "team" {
		t.Errorf("create params: %+v", f.created)
	}
	if out, isErr := call(t, on, "op_create", `{"vault":"Ops","title":"x","category":"ssh"}`); !isErr || !strings.Contains(out, "category") {
		t.Errorf("a bad category is refused before the gate: %q", out)
	}
	if out, isErr := call(t, on, "op_edit", `{"vault":"Ops","item":"Stripe"}`); !isErr || !strings.Contains(out, "nothing to change") {
		t.Errorf("an empty edit is refused: %q", out)
	}
	if out, isErr := call(t, on, "op_edit", `{"vault":"Ops","item":"Stripe","fields":[{"label":"api key","value":"sk_test","concealed":true}]}`); isErr || !strings.Contains(out, "edited") {
		t.Errorf("edit: %q", out)
	}
	if len(f.edited) != 1 || !f.edited[0].Fields[0].Concealed {
		t.Errorf("edit params: %+v", f.edited)
	}

	var asked string
	confirmed := NewDispatcher(f, Policy{Confirm: func(_ context.Context, a, _ string) bool { asked = a; return true }})
	if out, isErr := call(t, confirmed, "op_edit", `{"vault":"Ops","item":"Stripe","title":"Stripe (test)"}`); isErr || asked != ActionWrite {
		t.Errorf("a confirm approves a write: %q asked=%q", out, asked)
	}
}

func TestArchiveAlwaysAsks(t *testing.T) {
	f := newFake()
	if out, isErr := call(t, NewDispatcher(f, Policy{AllowWrite: true}), "op_delete", `{"vault":"Ops","item":"Stripe"}`); !isErr || !strings.Contains(out, "blocked") {
		t.Errorf("AllowWrite never covers an archive: %q", out)
	}
	if len(f.archived) != 0 {
		t.Fatalf("a blocked archive reached the backend")
	}
	var asked string
	d := NewDispatcher(f, Policy{Confirm: func(_ context.Context, a, _ string) bool { asked = a; return true }})
	if out, isErr := call(t, d, "op_delete", `{"vault":"Ops","item":"Stripe"}`); isErr || asked != ActionArchive || len(f.archived) != 1 {
		t.Errorf("confirmed archive: %q asked=%q archived=%v", out, asked, f.archived)
	}
}

func TestBackendErrorsAreAnswersTheModelCanActOn(t *testing.T) {
	f := newFake()
	f.err = ErrRateLimited
	d := NewDispatcher(f, Policy{})
	out, isErr, err := d.Dispatch(context.Background(), model.ToolCall{Name: "op_vaults", Arguments: json.RawMessage(`{}`)})
	if err != nil || !isErr || !strings.Contains(out, "rate limit") {
		t.Errorf("rate limit: %q %v %v", out, isErr, err)
	}
	f.err = ErrCLINotFound
	_, _, err = d.Dispatch(context.Background(), model.ToolCall{Name: "op_vaults", Arguments: json.RawMessage(`{}`)})
	if !errors.Is(err, ErrCLINotFound) {
		t.Errorf("a missing CLI is a transport failure: %v", err)
	}
	f.err = nil
	if out, isErr := call(t, d, "op_item", `{"vault":"Ops","item":"Nope"}`); !isErr || !strings.Contains(out, "not found") {
		t.Errorf("not found: %q", out)
	}
}

func TestParseReference(t *testing.T) {
	cases := map[string][3]string{
		"op://Ops/Stripe/password":            {"Ops", "Stripe", "password"},
		"op://Ops/Stripe/Section one/api key": {"Ops", "Stripe", "Section one/api key"},
		" op://v/i/f ":                        {"v", "i", "f"},
	}
	for in, want := range cases {
		v, i, f, ok := ParseReference(in)
		if !ok || v != want[0] || i != want[1] || f != want[2] {
			t.Errorf("ParseReference(%q) = %q %q %q %v", in, v, i, f, ok)
		}
	}
	for _, bad := range []string{"", "Ops/Stripe/password", "op://Ops/Stripe", "op:///i/f", "op://v//f"} {
		if _, _, _, ok := ParseReference(bad); ok {
			t.Errorf("ParseReference(%q) accepted", bad)
		}
	}
}

func TestConcealed(t *testing.T) {
	for _, f := range []Field{{Type: "CONCEALED"}, {Type: "concealed"}, {Type: "OTP"}, {Type: "SSHKEY"}, {Type: "STRING", Purpose: "PASSWORD"}, {Type: "CREDIT_CARD_NUMBER"}} {
		if !Concealed(f) {
			t.Errorf("%+v should be concealed", f)
		}
	}
	for _, f := range []Field{{Type: "STRING"}, {Type: "URL"}, {Type: "EMAIL", Purpose: "USERNAME"}, {Type: "STRING", Purpose: "NOTES"}} {
		if Concealed(f) {
			t.Errorf("%+v should not be concealed", f)
		}
	}
}
