package onepassword

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"unicode"
)

// Connect is the Backend over a 1Password Connect server's REST API. The
// token travels only in the Authorization header, never in a URL, and no
// error message quotes it.
type Connect struct {
	base   string
	token  string
	client *http.Client
}

// ErrConnectUnreachable is a Connect server that did not answer at all:
// refused, timed out, or a name that does not resolve.
var ErrConnectUnreachable = errors.New("the 1Password Connect server did not answer")

// ErrUnsupported is an operation the Connect API does not offer.
var ErrUnsupported = errors.New("not available through a 1Password Connect server")

// maxConnectBody caps what one answer may be, so a confused server cannot
// fill memory.
const maxConnectBody = 16 << 20

// NewConnect checks host is an http or https URL (trailing slash trimmed, a
// trailing /v1 tolerated) and that token is non-empty.
func NewConnect(host, token string) (*Connect, error) {
	host = strings.TrimSpace(host)
	u, err := url.Parse(host)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("connect host: give an http:// or https:// URL")
	}
	// The address is shown by doctor and whoami and quoted in errors, so it
	// may carry nothing secret and nothing a request would misread.
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("connect host: give the server's address alone, with no user, password, query or fragment")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("connect: the access token is empty")
	}
	base := strings.TrimRight(host, "/")
	base = strings.TrimRight(strings.TrimSuffix(base, "/v1"), "/")
	client := &http.Client{
		Timeout: DefaultTimeout,
		// The token is for this server alone: a redirect is an answer to
		// report, never somewhere to follow with it.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &Connect{base: base, token: strings.TrimSpace(token), client: client}, nil
}

// Host is the server's base URL, for doctor and whoami output.
func (c *Connect) Host() string { return c.base }

// do sends one request to {base}/v1{path} and decodes a 2xx answer into out
// (when set). op names the call in errors, the way opcli.go names op's
// commands.
func (c *Connect) do(ctx context.Context, op, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+"/v1"+path, rd)
	if err != nil {
		return fmt.Errorf("connect %s: %w", op, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("connect %s: %w", op, ctx.Err())
		}
		return fmt.Errorf("%w (%s): %v", ErrConnectUnreachable, op, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxConnectBody))
	if err != nil {
		return fmt.Errorf("%w (%s): %v", ErrConnectUnreachable, op, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return connectError(op, resp.StatusCode, data)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("connect %s: unreadable answer: %w", op, err)
	}
	return nil
}

// connectError turns a non-2xx answer into one of the package errors, with
// the server's message when it sent one.
func connectError(op string, status int, body []byte) error {
	var e struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &e)
	msg := cut(firstLine(e.Message), 200)
	if msg == "" {
		msg = http.StatusText(status)
	}
	switch status {
	case http.StatusUnauthorized:
		return fmt.Errorf("%w (%s): %s", ErrAuth, op, msg)
	case http.StatusForbidden, http.StatusNotFound:
		// 403 is a vault outside the token's scope: to the agent it is not there.
		return fmt.Errorf("%w (%s): %s", ErrNotFound, op, msg)
	case http.StatusTooManyRequests:
		return fmt.Errorf("%w (%s): %s", ErrRateLimited, op, msg)
	}
	return fmt.Errorf("connect %s failed (%d): %s", op, status, msg)
}

// --- the wire shapes ---

type cVault struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type cRef struct {
	ID string `json:"id"`
}

type cRecipe struct {
	Length        int      `json:"length"`
	CharacterSets []string `json:"characterSets"`
}

type cField struct {
	ID       string   `json:"id,omitempty"`
	Section  *cRef    `json:"section,omitempty"`
	Type     string   `json:"type,omitempty"`
	Purpose  string   `json:"purpose,omitempty"`
	Label    string   `json:"label,omitempty"`
	Value    string   `json:"value,omitempty"`
	Generate bool     `json:"generate,omitempty"`
	Recipe   *cRecipe `json:"recipe,omitempty"`
}

type cSection struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type cURL struct {
	Label   string `json:"label,omitempty"`
	Primary bool   `json:"primary,omitempty"`
	Href    string `json:"href"`
}

// cItem is the Connect Item and FullItem: a listing leaves out sections and
// fields.
type cItem struct {
	ID        string     `json:"id,omitempty"`
	Title     string     `json:"title"`
	Vault     cRef       `json:"vault"`
	Category  string     `json:"category"`
	URLs      []cURL     `json:"urls,omitempty"`
	Tags      []string   `json:"tags,omitempty"`
	State     string     `json:"state,omitempty"`
	UpdatedAt string     `json:"updatedAt,omitempty"`
	Sections  []cSection `json:"sections,omitempty"`
	Fields    []cField   `json:"fields,omitempty"`
}

// patchOp is one RFC 6902 operation; Connect takes add, remove and replace.
type patchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"`
}

// generateRecipe is the password Create asks the server for, the same as
// opcli.go's --generate-password=letters,digits,symbols,32.
var generateRecipe = &cRecipe{Length: 32, CharacterSets: []string{"LETTERS", "DIGITS", "SYMBOLS"}}

func active(it cItem) bool {
	s := strings.ToUpper(it.State)
	return s != "ARCHIVED" && s != "DELETED"
}

// refPart is a name as an op:// reference spells it: the name when it is
// plain letters, digits, space, -, _ and ., otherwise the id.
func refPart(name, id string) string {
	if name == "" {
		return id
	}
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune(" -_.", r) {
			return id
		}
	}
	return name
}

func (o cItem) overview(v Vault) ItemOverview {
	return ItemOverview{ID: o.ID, Title: o.Title, Category: o.Category, Vault: v, Tags: o.Tags, UpdatedAt: o.UpdatedAt}
}

// item maps a FullItem. Connect reports no reference, so it is built the
// way op builds one: op://vault/item/[section/]field.
func (o cItem) item(v Vault) Item {
	it := Item{ID: o.ID, Title: o.Title, Category: o.Category, Vault: v, Tags: o.Tags, UpdatedAt: o.UpdatedAt}
	prefix := "op://" + refPart(v.Name, v.ID) + "/" + refPart(o.Title, o.ID) + "/"
	for _, f := range o.Fields {
		field := Field{ID: f.ID, Label: f.Label, Type: f.Type, Purpose: f.Purpose, Value: f.Value}
		ref := prefix
		if f.Section != nil && f.Section.ID != "" {
			label := o.sectionLabel(f.Section.ID)
			field.Section = label
			if field.Section == "" {
				field.Section = f.Section.ID
			}
			ref += refPart(label, f.Section.ID) + "/"
		}
		field.Reference = ref + refPart(f.Label, f.ID)
		it.Fields = append(it.Fields, field)
	}
	for _, u := range o.URLs {
		it.URLs = append(it.URLs, URL{Label: u.Label, Href: u.Href, Primary: u.Primary})
	}
	return it
}

func (o cItem) sectionLabel(id string) string {
	for _, s := range o.Sections {
		if s.ID == id {
			return s.Label
		}
	}
	return ""
}

// --- lookups ---

func (c *Connect) vaults(ctx context.Context) ([]Vault, error) {
	var raw []cVault
	if err := c.do(ctx, "vault list", http.MethodGet, "/vaults", nil, &raw); err != nil {
		return nil, err
	}
	vaults := make([]Vault, 0, len(raw))
	for _, v := range raw {
		vaults = append(vaults, Vault{ID: v.ID, Name: v.Name})
	}
	return vaults, nil
}

// vault finds a vault by id (exact) or name (any case). 1Password lets two
// vaults share a name; a name that fits more than one is refused, so the
// agent never lands in the wrong one of them.
func (c *Connect) vault(ctx context.Context, nameOrID string) (Vault, error) {
	vaults, err := c.vaults(ctx)
	if err != nil {
		return Vault{}, err
	}
	for _, v := range vaults {
		if v.ID == nameOrID {
			return v, nil
		}
	}
	var named []Vault
	for _, v := range vaults {
		if strings.EqualFold(v.Name, nameOrID) {
			named = append(named, v)
		}
	}
	switch len(named) {
	case 0:
		return Vault{}, fmt.Errorf("%w (vault get): no vault %q", ErrNotFound, nameOrID)
	case 1:
		return named[0], nil
	}
	return Vault{}, fmt.Errorf("vault get: %d vaults are named %q; give the vault's id", len(named), nameOrID)
}

func (c *Connect) listItems(ctx context.Context, v Vault) ([]cItem, error) {
	var raw []cItem
	if err := c.do(ctx, "item list", http.MethodGet, "/vaults/"+url.PathEscape(v.ID)+"/items", nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// fullItem finds an item by id (exact) or title (any case) and fetches it
// whole.
// ponytail: lists the whole vault per lookup; switch to ?filter=title eq "..." if vaults get large.
func (c *Connect) fullItem(ctx context.Context, vault, item string) (Vault, cItem, error) {
	v, err := c.vault(ctx, vault)
	if err != nil {
		return Vault{}, cItem{}, err
	}
	list, err := c.listItems(ctx, v)
	if err != nil {
		return Vault{}, cItem{}, err
	}
	id := ""
	for _, it := range list {
		if it.ID == item {
			id = it.ID
			break
		}
	}
	if id == "" {
		var n int
		for _, it := range list {
			if active(it) && strings.EqualFold(it.Title, item) {
				id = it.ID
				n++
			}
		}
		if n > 1 {
			return Vault{}, cItem{}, fmt.Errorf("item get: %d items in %q are titled %q; give the item's id", n, v.Name, item)
		}
	}
	if id == "" {
		return Vault{}, cItem{}, fmt.Errorf("%w (item get): no item %q in vault %q", ErrNotFound, item, v.Name)
	}
	var full cItem
	if err := c.do(ctx, "item get", http.MethodGet, "/vaults/"+url.PathEscape(v.ID)+"/items/"+url.PathEscape(id), nil, &full); err != nil {
		return Vault{}, cItem{}, err
	}
	return v, full, nil
}

// --- the Backend ---

func (c *Connect) WhoAmI(ctx context.Context) (Identity, error) {
	if _, err := c.vaults(ctx); err != nil {
		return Identity{}, err
	}
	return Identity{URL: c.base, Type: "CONNECT"}, nil
}

func (c *Connect) Vaults(ctx context.Context) ([]Vault, error) { return c.vaults(ctx) }

// normCategory folds the spellings a category arrives in (Login, API
// Credential, api-credential) to Connect's LOGIN, API_CREDENTIAL.
func normCategory(s string) string {
	return strings.NewReplacer(" ", "_", "-", "_").Replace(strings.ToUpper(strings.TrimSpace(s)))
}

// Items filters on the client: Connect's filter only matches titles.
func (c *Connect) Items(ctx context.Context, vault string, categories, tags []string) ([]ItemOverview, error) {
	v, err := c.vault(ctx, vault)
	if err != nil {
		return nil, err
	}
	list, err := c.listItems(ctx, v)
	if err != nil {
		return nil, err
	}
	items := []ItemOverview{}
	for _, it := range list {
		if !active(it) {
			continue
		}
		if len(categories) > 0 && !anyMatch(categories, []string{it.Category}, func(a, b string) bool { return normCategory(a) == normCategory(b) }) {
			continue
		}
		if len(tags) > 0 && !anyMatch(tags, it.Tags, strings.EqualFold) {
			continue
		}
		items = append(items, it.overview(v))
	}
	return items, nil
}

func anyMatch(want, have []string, eq func(a, b string) bool) bool {
	for _, w := range want {
		for _, h := range have {
			if eq(w, h) {
				return true
			}
		}
	}
	return false
}

func (c *Connect) Item(ctx context.Context, vault, item string) (Item, error) {
	v, full, err := c.fullItem(ctx, vault, item)
	if err != nil {
		return Item{}, err
	}
	return full.item(v), nil
}

// Read resolves op://vault/item/[section/]field. A ?attribute= query (an
// OTP code, a field's type) is op's own and is refused here.
func (c *Connect) Read(ctx context.Context, reference string) (string, error) {
	vault, item, field, ok := ParseReference(reference)
	if !ok {
		return "", fmt.Errorf("read: %q is not an op://vault/item/field reference", reference)
	}
	if strings.Contains(field, "?") {
		return "", errors.New("read: a ?query in the reference (such as ?attribute=otp) is not supported through a Connect server; read the field itself")
	}
	section, name, hasSection := strings.Cut(field, "/")
	if !hasSection {
		name, section = section, ""
	}
	_, full, err := c.fullItem(ctx, vault, item)
	if err != nil {
		return "", err
	}
	inSection := func(f cField) bool {
		if !hasSection {
			return true
		}
		if f.Section == nil {
			return false
		}
		return f.Section.ID == section || strings.EqualFold(full.sectionLabel(f.Section.ID), section)
	}
	var byID, byLabel []cField
	for _, f := range full.Fields {
		if !inSection(f) {
			continue
		}
		switch {
		case f.ID == name:
			byID = append(byID, f)
		case strings.EqualFold(f.Label, name):
			byLabel = append(byLabel, f)
		}
	}
	matches := byID
	if len(matches) == 0 {
		matches = byLabel
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%w (read): no field %q on item %q", ErrNotFound, field, full.Title)
	case 1:
		return matches[0].Value, nil
	}
	return "", fmt.Errorf("read: %d fields on item %q match %q; use the field's id in the reference", len(matches), full.Title, field)
}

func (c *Connect) Create(ctx context.Context, p CreateParams) (Item, error) {
	cat, err := categoryName(p.Category)
	if err != nil {
		return Item{}, err
	}
	v, err := c.vault(ctx, p.Vault)
	if err != nil {
		return Item{}, err
	}
	body := cItem{Vault: cRef{ID: v.ID}, Title: p.Title, Category: cat, Tags: p.Tags, Fields: []cField{}}
	secret := func(id, purpose string) cField {
		f := cField{ID: id, Type: "CONCEALED", Purpose: purpose, Label: id, Value: p.Password}
		if p.GeneratePassword {
			f.Value, f.Generate, f.Recipe = "", true, generateRecipe
		}
		return f
	}
	switch cat {
	case "LOGIN":
		body.Fields = append(body.Fields, cField{ID: "username", Type: "STRING", Purpose: "USERNAME", Label: "username", Value: p.Username})
		body.Fields = append(body.Fields, secret("password", "PASSWORD"))
	case "PASSWORD":
		body.Fields = append(body.Fields, secret("password", "PASSWORD"))
	case "API_CREDENTIAL":
		if p.Username != "" {
			body.Fields = append(body.Fields, cField{ID: "username", Type: "STRING", Label: "username", Value: p.Username})
		}
		if p.Password != "" || p.GeneratePassword {
			body.Fields = append(body.Fields, secret("credential", ""))
		}
	}
	if p.Notes != "" {
		body.Fields = append(body.Fields, cField{ID: "notesPlain", Type: "STRING", Purpose: "NOTES", Label: "notesPlain", Value: p.Notes})
	}
	for _, f := range p.Fields {
		body.Fields = append(body.Fields, cField{ID: fieldID(f.Label), Type: fieldType(f.Concealed), Label: f.Label, Value: f.Value})
	}
	if p.URL != "" {
		body.URLs = []cURL{{Label: "website", Primary: true, Href: p.URL}}
	}
	var out cItem
	if err := c.do(ctx, "item create", http.MethodPost, "/vaults/"+url.PathEscape(v.ID)+"/items", body, &out); err != nil {
		return Item{}, err
	}
	return out.item(v), nil
}

// pointer escapes one JSON Pointer token (RFC 6901).
func pointer(s string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(s)
}

// Edit sends one PATCH with only the operations the change needs. A
// generated password is made here, since a patch cannot ask the server to
// generate one.
func (c *Connect) Edit(ctx context.Context, p EditParams) (Item, error) {
	v, full, err := c.fullItem(ctx, p.Vault, p.Item)
	if err != nil {
		return Item{}, err
	}
	var ops []patchOp
	// set is replace for an attribute the item has and add for one it lacks,
	// since RFC 6902 replace needs the target to exist.
	set := func(path string, has bool, value any) {
		op := "replace"
		if !has {
			op = "add"
		}
		ops = append(ops, patchOp{Op: op, Path: path, Value: value})
	}
	if p.Title != "" {
		set("/title", true, p.Title)
	}
	if p.Tags != nil {
		set("/tags", full.Tags != nil, p.Tags)
	}
	if p.URL != "" {
		urls := append([]cURL(nil), full.URLs...)
		replaced := false
		for i := range urls {
			if urls[i].Primary {
				urls[i].Href, replaced = p.URL, true
				break
			}
		}
		if !replaced {
			urls = append(urls, cURL{Label: "website", Primary: true, Href: p.URL})
		}
		set("/urls", full.URLs != nil, urls)
	}
	for _, in := range p.Fields {
		var existing *cField
		for i := range full.Fields {
			if strings.EqualFold(full.Fields[i].Label, in.Label) {
				existing = &full.Fields[i]
				break
			}
		}
		if existing == nil {
			ops = append(ops, patchOp{Op: "add", Path: "/fields", Value: cField{Label: in.Label, Type: fieldType(in.Concealed), Value: in.Value}})
			continue
		}
		path := "/fields/" + pointer(existing.ID)
		set(path+"/value", true, in.Value)
		if in.Concealed && !strings.EqualFold(existing.Type, "CONCEALED") {
			set(path+"/type", existing.Type != "", "CONCEALED")
		}
	}
	if p.GeneratePassword {
		var target *cField
		for i := range full.Fields {
			if strings.EqualFold(full.Fields[i].Purpose, "PASSWORD") {
				target = &full.Fields[i]
				break
			}
		}
		if target == nil {
			return Item{}, fmt.Errorf("item edit: %q has no password field to generate into; set the field's value instead", full.Title)
		}
		pw, err := generatePassword(32)
		if err != nil {
			return Item{}, err
		}
		set("/fields/"+pointer(target.ID)+"/value", true, pw)
	}
	if len(ops) == 0 {
		return full.item(v), nil
	}
	var out cItem
	if err := c.do(ctx, "item edit", http.MethodPatch, "/vaults/"+url.PathEscape(v.ID)+"/items/"+url.PathEscape(full.ID), ops, &out); err != nil {
		return Item{}, err
	}
	return out.item(v), nil
}

const passwordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789!@#$%^&*()-_=+[]{};:,.?/~"

// generatePassword draws n characters from letters, digits and symbols with
// crypto/rand; rand.Int is uniform, so no character is favoured.
func generatePassword(n int) (string, error) {
	size := big.NewInt(int64(len(passwordAlphabet)))
	b := make([]byte, n)
	for i := range b {
		k, err := rand.Int(rand.Reader, size)
		if err != nil {
			return "", fmt.Errorf("item edit: no randomness for a password: %w", err)
		}
		b[i] = passwordAlphabet[k.Int64()]
	}
	return string(b), nil
}

// Archive is refused: Connect can delete an item but not archive it, and a
// delete is not what the agent asked for.
func (c *Connect) Archive(ctx context.Context, vault, item string) error {
	return fmt.Errorf("%w: archiving an item; archive it in the 1Password app, or use a service account token instead of Connect", ErrUnsupported)
}
