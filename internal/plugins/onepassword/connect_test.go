package onepassword

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const connectToken = "eyJconnect_test_token_1234567890"

// fakeConnect stands in for a Connect server: it records every request and
// answers from its vaults and items, or with status when that is set.
type fakeConnect struct {
	vaults  []cVault
	items   map[string][]cItem // vault id -> full items
	status  int
	message string
	reqs    []fakeReq
}

type fakeReq struct {
	method, uri, auth, contentType, body string
}

func (f *fakeConnect) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.reqs = append(f.reqs, fakeReq{method: r.Method, uri: r.URL.RequestURI(), auth: r.Header.Get("Authorization"), contentType: r.Header.Get("Content-Type"), body: string(body)})
	w.Header().Set("Content-Type", "application/json")
	answer := func(status int, v any) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	if f.status != 0 {
		answer(f.status, map[string]any{"status": f.status, "message": f.message})
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/"), "/")
	switch {
	case r.Method == "GET" && len(parts) == 1 && parts[0] == "vaults":
		answer(200, f.vaults)
	case r.Method == "GET" && len(parts) == 3 && parts[2] == "items":
		list := []cItem{}
		for _, it := range f.items[parts[1]] {
			it.Fields, it.Sections = nil, nil
			list = append(list, it)
		}
		answer(200, list)
	case r.Method == "GET" && len(parts) == 4:
		for _, it := range f.items[parts[1]] {
			if it.ID == parts[3] {
				answer(200, it)
				return
			}
		}
		answer(404, map[string]any{"status": 404, "message": "item " + parts[3] + " not found"})
	case r.Method == "POST" && len(parts) == 3:
		var it cItem
		_ = json.Unmarshal(body, &it)
		it.ID = "newitem"
		answer(200, it)
	case r.Method == "PATCH" && len(parts) == 4:
		for _, it := range f.items[parts[1]] {
			if it.ID == parts[3] {
				answer(200, it)
				return
			}
		}
		answer(404, map[string]any{"status": 404, "message": "item not found"})
	default:
		answer(400, map[string]any{"status": 400, "message": "unexpected " + r.Method + " " + r.URL.Path})
	}
}

func newFakeConnect(t *testing.T) (*fakeConnect, *Connect) {
	t.Helper()
	f := &fakeConnect{
		vaults: []cVault{{ID: "vops", Name: "Ops"}, {ID: "vshared", Name: "Team/Shared"}},
		items: map[string][]cItem{
			"vops": {
				{ID: "itstripe", Title: "Stripe", Category: "LOGIN", Vault: cRef{ID: "vops"}, Tags: []string{"billing"}, UpdatedAt: "2026-10-28T10:00:00Z",
					URLs:     []cURL{{Label: "website", Primary: true, Href: "https://dashboard.stripe.com"}, {Label: "docs", Href: "https://docs.stripe.com"}},
					Sections: []cSection{{ID: "s1", Label: "Keys"}, {ID: "s2", Label: "Odd/Section"}, {ID: "s3"}},
					Fields: []cField{
						{ID: "username", Type: "STRING", Purpose: "USERNAME", Label: "username", Value: "ops@example.test"},
						{ID: "password", Type: "CONCEALED", Purpose: "PASSWORD", Label: "password", Value: "hunter2-hunter2"},
						{ID: "fsecret", Section: &cRef{ID: "s1"}, Type: "CONCEALED", Label: "Secret Key", Value: "sk_test_1"},
						{ID: "fpin1", Section: &cRef{ID: "s1"}, Type: "STRING", Label: "pin", Value: "1111"},
						{ID: "fpin2", Section: &cRef{ID: "s2"}, Type: "STRING", Label: "pin", Value: "2222"},
						{ID: "fslash", Section: &cRef{ID: "s3"}, Type: "STRING", Label: "a/b", Value: "slashed"},
					}},
				{ID: "itold", Title: "Old", Category: "LOGIN", Vault: cRef{ID: "vops"}, Tags: []string{"billing"}, State: "ARCHIVED"},
				{ID: "itaws", Title: "AWS", Category: "API_CREDENTIAL", Vault: cRef{ID: "vops"}, Tags: []string{"Cloud"},
					Fields: []cField{{ID: "credential", Type: "CONCEALED", Label: "credential", Value: "AKIA"}}},
				{ID: "itdup1", Title: "Dup", Category: "SECURE_NOTE", Vault: cRef{ID: "vops"}},
				{ID: "itdup2", Title: "dup", Category: "SECURE_NOTE", Vault: cRef{ID: "vops"}},
			},
			"vshared": {
				{ID: "itcfg", Title: "cfg (prod)", Category: "PASSWORD", Vault: cRef{ID: "vshared"},
					Fields: []cField{{ID: "password", Type: "CONCEALED", Purpose: "PASSWORD", Label: "password", Value: "p"}}},
			},
		},
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := NewConnect(srv.URL+"/v1/", connectToken)
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

func TestConnectTokenRidesOnlyInTheAuthorizationHeader(t *testing.T) {
	f, c := newFakeConnect(t)
	ctx := context.Background()
	id, err := c.WhoAmI(ctx)
	if err != nil || id.Type != "CONNECT" || id.URL != c.Host() {
		t.Fatalf("whoami: %+v %v", id, err)
	}
	if _, err := c.Item(ctx, "Ops", "Stripe"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Create(ctx, CreateParams{Vault: "Ops", Title: "X", Category: "login", Password: "pw"}); err != nil {
		t.Fatal(err)
	}
	if len(f.reqs) != 6 {
		t.Fatalf("requests: %+v", f.reqs)
	}
	for _, r := range f.reqs {
		if r.auth != "Bearer "+connectToken {
			t.Errorf("%s %s: authorization %q", r.method, r.uri, r.auth)
		}
		if strings.Contains(r.uri, connectToken) || strings.Contains(r.body, connectToken) {
			t.Errorf("the token is in %s %s", r.method, r.uri)
		}
		if r.method == "POST" && r.contentType != "application/json" {
			t.Errorf("POST content type %q", r.contentType)
		}
	}
	f.status, f.message = 401, "Invalid token signature"
	if _, err := c.Vaults(ctx); err == nil || strings.Contains(err.Error(), connectToken) {
		t.Errorf("401: %v", err)
	}
}

func TestNewConnectChecksTheHostAndToken(t *testing.T) {
	for in, want := range map[string]string{
		"https://connect.example.test":         "https://connect.example.test",
		"https://connect.example.test/":        "https://connect.example.test",
		"https://connect.example.test/v1":      "https://connect.example.test",
		"http://10.0.0.5:8080/v1/":             "http://10.0.0.5:8080",
		" http://localhost:8080/prefix/v1/ ":   "http://localhost:8080/prefix",
		"https://connect.example.test/v1beta/": "https://connect.example.test/v1beta",
	} {
		c, err := NewConnect(in, "tok")
		if err != nil || c.Host() != want {
			t.Errorf("%q -> %v %v, want %q", in, c, err, want)
		}
	}
	for _, in := range []string{"", "connect.example.test", "ftp://connect.example.test", "https://", "localhost:8080"} {
		if _, err := NewConnect(in, "tok"); err == nil {
			t.Errorf("%q is accepted", in)
		}
	}
	if _, err := NewConnect("https://connect.example.test", "  "); err == nil {
		t.Errorf("an empty token is accepted")
	}
}

func TestConnectVaultsAndVaultLookup(t *testing.T) {
	_, c := newFakeConnect(t)
	ctx := context.Background()
	vaults, err := c.Vaults(ctx)
	if err != nil || len(vaults) != 2 || vaults[0] != (Vault{ID: "vops", Name: "Ops"}) {
		t.Fatalf("vaults: %+v %v", vaults, err)
	}
	for _, v := range []string{"Ops", "ops", "vops"} {
		items, err := c.Items(ctx, v, nil, nil)
		if err != nil || len(items) != 4 {
			t.Errorf("vault %q: %d items %v", v, len(items), err)
		}
	}
	if _, err := c.Items(ctx, "Nope", nil, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown vault: %v", err)
	}
}

func TestConnectItemsFilterOnTheClient(t *testing.T) {
	_, c := newFakeConnect(t)
	ctx := context.Background()
	titles := func(cats, tags []string) string {
		items, err := c.Items(ctx, "Ops", cats, tags)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, it := range items {
			if it.Vault != (Vault{ID: "vops", Name: "Ops"}) {
				t.Errorf("vault on %s: %+v", it.Title, it.Vault)
			}
			out = append(out, it.Title)
		}
		return strings.Join(out, ",")
	}
	cases := []struct {
		cats, tags []string
		want       string
	}{
		{[]string{"Login"}, nil, "Stripe"},
		{[]string{"API Credential"}, nil, "AWS"},
		{[]string{"api-credential"}, nil, "AWS"},
		{[]string{"secure_note"}, nil, "Dup,dup"},
		{nil, []string{"BILLING"}, "Stripe"},
		{nil, []string{"cloud"}, "AWS"},
		{[]string{"Login"}, []string{"cloud"}, ""},
		{[]string{"server"}, nil, ""},
	}
	for _, tc := range cases {
		if got := titles(tc.cats, tc.tags); got != tc.want {
			t.Errorf("categories %v tags %v: %q, want %q", tc.cats, tc.tags, got, tc.want)
		}
	}
}

func TestConnectItemMapsSectionsAndBuildsReferences(t *testing.T) {
	_, c := newFakeConnect(t)
	ctx := context.Background()
	it, err := c.Item(ctx, "ops", "stripe")
	if err != nil {
		t.Fatal(err)
	}
	if it.ID != "itstripe" || it.Vault != (Vault{ID: "vops", Name: "Ops"}) || len(it.Fields) != 6 || len(it.URLs) != 2 || !it.URLs[0].Primary {
		t.Fatalf("item: %+v", it)
	}
	want := map[string][2]string{ // field id -> section, reference
		"username": {"", "op://Ops/Stripe/username"},
		"password": {"", "op://Ops/Stripe/password"},
		"fsecret":  {"Keys", "op://Ops/Stripe/Keys/Secret Key"},
		"fpin2":    {"Odd/Section", "op://Ops/Stripe/s2/pin"},
		"fslash":   {"s3", "op://Ops/Stripe/s3/fslash"},
	}
	for _, f := range it.Fields {
		if w, ok := want[f.ID]; ok && (f.Section != w[0] || f.Reference != w[1]) {
			t.Errorf("%s: section %q reference %q, want %q %q", f.ID, f.Section, f.Reference, w[0], w[1])
		}
	}
	if !Concealed(it.Fields[2]) {
		t.Errorf("a concealed field: %+v", it.Fields[2])
	}
	// A vault name and a title outside the plain set fall back to their ids.
	it, err = c.Item(ctx, "Team/Shared", "itcfg")
	if err != nil || it.Fields[0].Reference != "op://vshared/itcfg/password" {
		t.Errorf("fallback: %+v %v", it, err)
	}
	if _, err := c.Item(ctx, "Ops", "Dup"); err == nil || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "id") {
		t.Errorf("two items titled the same: %v", err)
	}
	if it, err := c.Item(ctx, "Ops", "itdup2"); err != nil || it.ID != "itdup2" {
		t.Errorf("by id: %+v %v", it, err)
	}
	if _, err := c.Item(ctx, "Ops", "Old"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an archived item by title: %v", err)
	}
	if _, err := c.Item(ctx, "Ops", "Nothing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing item: %v", err)
	}
}

func TestConnectRead(t *testing.T) {
	f, c := newFakeConnect(t)
	ctx := context.Background()
	for ref, want := range map[string]string{
		"op://Ops/Stripe/password":          "hunter2-hunter2",
		"op://ops/stripe/Secret key":        "sk_test_1",
		"op://Ops/Stripe/fsecret":           "sk_test_1",
		"op://Ops/Stripe/Keys/pin":          "1111",
		"op://Ops/Stripe/s2/pin":            "2222",
		"op://Ops/Stripe/odd/section/pin":   "", // a label with a slash cannot be named; this is a miss
		"op://vops/itstripe/s3/fslash":      "slashed",
		"op://Team/Shared/cfg (prod)/pw":    "",
		"op://vshared/itcfg/password":       "p",
		"op://Ops/Stripe/Keys/Secret Key":   "sk_test_1",
		"op://Ops/Stripe/username":          "ops@example.test",
		"op://Ops/AWS/credential":           "AKIA",
		"op://Ops/Stripe/fpin1":             "1111",
		"op://Ops/Stripe/Keys/fpin1":        "1111",
		"op://Ops/Stripe/Keys/fpin2":        "",
		"op://Ops/Stripe/Keys/no such pin":  "",
		"op://Ops/Stripe/nosection/fsecret": "",
	} {
		got, err := c.Read(ctx, ref)
		if want == "" {
			if err == nil {
				t.Errorf("%s: %q, want an error", ref, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", ref, got, err, want)
		}
	}
	if _, err := c.Read(ctx, "op://Ops/Stripe/pin"); err == nil || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "id") {
		t.Errorf("ambiguous: %v", err)
	}
	if _, err := c.Read(ctx, "op://Ops/Stripe/nothing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	n := len(f.reqs)
	if _, err := c.Read(ctx, "op://Ops/Stripe/password?attribute=otp"); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("query: %v", err)
	}
	if _, err := c.Read(ctx, "Ops/Stripe/password"); err == nil {
		t.Errorf("not a reference")
	}
	if len(f.reqs) != n {
		t.Errorf("a refused reference reached the server")
	}
}

// postedItem is the body of the last POST.
func postedItem(t *testing.T, f *fakeConnect) (cItem, map[string]map[string]any) {
	t.Helper()
	last := f.reqs[len(f.reqs)-1]
	if last.method != "POST" || last.uri != "/v1/vaults/vops/items" {
		t.Fatalf("last request: %s %s", last.method, last.uri)
	}
	var it cItem
	var raw struct {
		Fields []map[string]any `json:"fields"`
	}
	if err := json.Unmarshal([]byte(last.body), &it); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(last.body), &raw)
	byLabel := map[string]map[string]any{}
	for _, f := range raw.Fields {
		byLabel[f["label"].(string)] = f
	}
	return it, byLabel
}

func TestConnectCreateBodies(t *testing.T) {
	f, c := newFakeConnect(t)
	ctx := context.Background()

	it, err := c.Create(ctx, CreateParams{Vault: "ops", Title: "Vercel", Category: "login", Username: "ops@example.test", Password: "given-pw",
		URL: "https://vercel.com", Notes: "hosting", Tags: []string{"hosting"}, Fields: []FieldInput{{Label: "Team ID", Value: "team_1", Concealed: true}}})
	if err != nil || it.ID != "newitem" || it.Vault.Name != "Ops" {
		t.Fatalf("create: %+v %v", it, err)
	}
	body, fields := postedItem(t, f)
	if body.Vault.ID != "vops" || body.Title != "Vercel" || body.Category != "LOGIN" || strings.Join(body.Tags, ",") != "hosting" ||
		len(body.URLs) != 1 || body.URLs[0] != (cURL{Label: "website", Primary: true, Href: "https://vercel.com"}) {
		t.Errorf("body: %+v", body)
	}
	if p := fields["password"]; p["value"] != "given-pw" || p["purpose"] != "PASSWORD" || p["type"] != "CONCEALED" || p["generate"] != nil {
		t.Errorf("password: %v", p)
	}
	if u := fields["username"]; u["value"] != "ops@example.test" || u["purpose"] != "USERNAME" {
		t.Errorf("username: %v", u)
	}
	if n := fields["notesPlain"]; n["value"] != "hosting" || n["purpose"] != "NOTES" {
		t.Errorf("notes: %v", n)
	}
	if x := fields["Team ID"]; x["id"] != "team_id" || x["type"] != "CONCEALED" || x["value"] != "team_1" {
		t.Errorf("custom: %v", x)
	}

	if _, err := c.Create(ctx, CreateParams{Vault: "Ops", Title: "Gen", Category: "login", Password: "ignored", GeneratePassword: true}); err != nil {
		t.Fatal(err)
	}
	_, fields = postedItem(t, f)
	p := fields["password"]
	recipe, _ := p["recipe"].(map[string]any)
	if _, has := p["value"]; has || p["generate"] != true || recipe["length"] != float64(32) {
		t.Errorf("generated password: %v", p)
	}
	if sets, _ := recipe["characterSets"].([]any); len(sets) != 3 || sets[0] != "LETTERS" || sets[1] != "DIGITS" || sets[2] != "SYMBOLS" {
		t.Errorf("recipe: %v", recipe)
	}
	if strings.Contains(f.reqs[len(f.reqs)-1].body, "ignored") {
		t.Errorf("a given password rides along with generate")
	}

	if _, err := c.Create(ctx, CreateParams{Vault: "Ops", Title: "K", Category: "api_credential", Username: "svc", Password: "sk_live"}); err != nil {
		t.Fatal(err)
	}
	body, fields = postedItem(t, f)
	if body.Category != "API_CREDENTIAL" || fields["credential"]["value"] != "sk_live" || fields["credential"]["type"] != "CONCEALED" || fields["username"]["value"] != "svc" || fields["password"] != nil {
		t.Errorf("api credential: %+v %v", body, fields)
	}
	if _, err := c.Create(ctx, CreateParams{Vault: "Ops", Title: "K2", Category: "api credential", GeneratePassword: true}); err != nil {
		t.Fatal(err)
	}
	if _, fields = postedItem(t, f); fields["credential"]["generate"] != true {
		t.Errorf("generated credential: %v", fields)
	}

	if _, err := c.Create(ctx, CreateParams{Vault: "Ops", Title: "Note", Category: "secure_note", Notes: "remember"}); err != nil {
		t.Fatal(err)
	}
	body, fields = postedItem(t, f)
	if body.Category != "SECURE_NOTE" || len(fields) != 1 || fields["notesPlain"]["value"] != "remember" || body.URLs != nil {
		t.Errorf("secure note: %+v %v", body, fields)
	}

	n := len(f.reqs)
	if _, err := c.Create(ctx, CreateParams{Vault: "Ops", Title: "K", Category: "wireless router"}); err == nil || len(f.reqs) != n {
		t.Errorf("an unsupported category: %v, %d requests", err, len(f.reqs)-n)
	}
}

// patchOps is the body of the last PATCH.
func patchOps(t *testing.T, f *fakeConnect) []map[string]any {
	t.Helper()
	last := f.reqs[len(f.reqs)-1]
	if last.method != "PATCH" || last.contentType != "application/json" {
		t.Fatalf("last request: %s %s %q", last.method, last.uri, last.contentType)
	}
	var ops []map[string]any
	if err := json.Unmarshal([]byte(last.body), &ops); err != nil {
		t.Fatal(err)
	}
	return ops
}

func TestConnectEditSendsOnePatch(t *testing.T) {
	f, c := newFakeConnect(t)
	ctx := context.Background()
	_, err := c.Edit(ctx, EditParams{Vault: "Ops", Item: "Stripe", Title: "Stripe (test)", Tags: []string{}, URL: "https://new.stripe.com",
		Fields: []FieldInput{{Label: "USERNAME", Value: "new@example.test"}, {Label: "pin", Value: "9999", Concealed: true}, {Label: "Region", Value: "eu"}}})
	if err != nil {
		t.Fatal(err)
	}
	if last := f.reqs[len(f.reqs)-1]; last.uri != "/v1/vaults/vops/items/itstripe" {
		t.Errorf("patch uri: %s", last.uri)
	}
	var patches int
	for _, r := range f.reqs {
		if r.method == "PATCH" {
			patches++
		}
	}
	if patches != 1 {
		t.Errorf("%d patches", patches)
	}
	got, _ := json.Marshal(patchOps(t, f))
	want := `[{"op":"replace","path":"/title","value":"Stripe (test)"},` +
		`{"op":"replace","path":"/tags","value":[]},` +
		`{"op":"replace","path":"/urls","value":[{"href":"https://new.stripe.com","label":"website","primary":true},{"href":"https://docs.stripe.com","label":"docs"}]},` +
		`{"op":"replace","path":"/fields/username/value","value":"new@example.test"},` +
		`{"op":"replace","path":"/fields/fpin1/value","value":"9999"},` +
		`{"op":"replace","path":"/fields/fpin1/type","value":"CONCEALED"},` +
		`{"op":"add","path":"/fields","value":{"label":"Region","type":"STRING","value":"eu"}}]`
	if string(got) != want {
		t.Errorf("ops:\n got %s\nwant %s", got, want)
	}

	// An item with no URL gets a primary one, added since it has none.
	if _, err := c.Edit(ctx, EditParams{Vault: "Ops", Item: "AWS", URL: "https://aws.amazon.com", Tags: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	got, _ = json.Marshal(patchOps(t, f))
	if want := `[{"op":"replace","path":"/tags","value":["a"]},{"op":"add","path":"/urls","value":[{"href":"https://aws.amazon.com","label":"website","primary":true}]}]`; string(got) != want {
		t.Errorf("ops:\n got %s\nwant %s", got, want)
	}

	generated := func() string {
		if _, err := c.Edit(ctx, EditParams{Vault: "Ops", Item: "itstripe", GeneratePassword: true}); err != nil {
			t.Fatal(err)
		}
		ops := patchOps(t, f)
		if len(ops) != 1 || ops[0]["op"] != "replace" || ops[0]["path"] != "/fields/password/value" {
			t.Fatalf("generate ops: %v", ops)
		}
		v, _ := ops[0]["value"].(string)
		return v
	}
	a, b := generated(), generated()
	if len(a) != 32 || len(b) != 32 || a == b {
		t.Errorf("generated passwords: %d and %d characters, equal %v", len(a), len(b), a == b)
	}
	for _, r := range a {
		if !strings.ContainsRune(passwordAlphabet, r) {
			t.Errorf("character %q is outside the alphabet", r)
		}
	}
	n := len(f.reqs)
	if _, err := c.Edit(ctx, EditParams{Vault: "Ops", Item: "AWS", GeneratePassword: true}); err == nil {
		t.Errorf("an item with no password field")
	}
	for _, r := range f.reqs[n:] {
		if r.method == "PATCH" {
			t.Errorf("a patch was sent for an item with no password field")
		}
	}
}

func TestConnectArchiveIsUnsupportedAndSendsNothing(t *testing.T) {
	f, c := newFakeConnect(t)
	err := c.Archive(context.Background(), "Ops", "Stripe")
	if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "1Password app") {
		t.Errorf("archive: %v", err)
	}
	if len(f.reqs) != 0 {
		t.Errorf("archive reached the server: %+v", f.reqs)
	}
}

func TestConnectErrorsAreClassified(t *testing.T) {
	f, c := newFakeConnect(t)
	ctx := context.Background()
	for _, tc := range []struct {
		status int
		msg    string
		want   error
	}{
		{401, "Invalid token signature", ErrAuth},
		{403, "vault vops is not in scope", ErrNotFound},
		{404, "vault vops not found", ErrNotFound},
		{429, "Too Many Requests", ErrRateLimited},
	} {
		f.status, f.message = tc.status, tc.msg
		_, err := c.Vaults(ctx)
		if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("%d -> %v, want %v", tc.status, err, tc.want)
		}
	}
	f.status, f.message = 500, "sync is not ready"
	_, err := c.Vaults(ctx)
	if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "sync is not ready") {
		t.Errorf("500: %v", err)
	}
	for _, known := range []error{ErrAuth, ErrNotFound, ErrRateLimited, ErrConnectUnreachable} {
		if errors.Is(err, known) {
			t.Errorf("500 is classified as %v", known)
		}
	}

	srv := httptest.NewServer(http.NotFoundHandler())
	gone, err := NewConnect(srv.URL, connectToken)
	if err != nil {
		t.Fatal(err)
	}
	srv.Close()
	_, err = gone.Vaults(ctx)
	if !errors.Is(err, ErrConnectUnreachable) || strings.Contains(err.Error(), connectToken) {
		t.Errorf("closed server: %v", err)
	}
}
