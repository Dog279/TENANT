package onepassword

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"tenant/internal/model"
)

// The approval action ids the plugin raises; the descriptor files each
// under its category.
const (
	// ActionReveal: op_read puts a concealed value in the model's context.
	// Egress: the value leaves the vault for wherever the model runs.
	ActionReveal = "op_reveal"
	// ActionWrite: op_create and op_edit change the vault.
	ActionWrite = "op_write"
	// ActionArchive: op_delete archives an item. Destructive; a nil Confirm
	// denies it, AllowWrite never covers it.
	ActionArchive = "op_archive"
)

// Policy is what the operator decided; the model cannot change it.
type Policy struct {
	// AllowWrite lets op_create and op_edit run without a per-action
	// approval. With an approval broker wired the host drops it to false and
	// the broker decides (gateAllow).
	AllowWrite bool
	// AllowReveal lets op_read return a value without asking. Same rule.
	AllowReveal bool
	// Confirm asks the operator; nil denies every gated action.
	Confirm func(ctx context.Context, action, detail string) bool
	// Vaults, when set, are the only vaults the agent may touch, by name or
	// id, whatever the service account can see. A second fence behind the
	// account's own scope.
	Vaults []string
	// Revealed hears every value op_read returned, with its reference, so
	// the host can hand it to the log scrubber. The value is never logged by
	// the plugin itself.
	Revealed func(reference, value string)
}

// Dispatcher implements the agent tool dispatcher for 1Password.
type Dispatcher struct {
	be     Backend
	policy Policy

	mu       sync.Mutex
	vaults   []Vault
	vaultsAt time.Time
}

// NewDispatcher wires a dispatcher over a backend and the operator's policy.
// A nil backend is the stub: every call says how to configure the plugin.
func NewDispatcher(be Backend, policy Policy) *Dispatcher {
	return &Dispatcher{be: be, policy: policy}
}

const notConfigured = "1Password is not configured - run `/configure onepassword` (a service account token) and `/enable onepassword`"

var sensitive = []model.InfoFlowLabel{{Sensitivity: model.SensitivitySensitive, Provenance: model.ProvenanceService}}

func (d *Dispatcher) Tools() []model.ToolSpec {
	obj := func(props string, req ...string) json.RawMessage {
		r := ""
		for i, x := range req {
			if i > 0 {
				r += ","
			}
			r += `"` + x + `"`
		}
		return json.RawMessage(`{"type":"object","properties":{` + props + `},"required":[` + r + `]}`)
	}
	const vaultArg = `"vault":{"type":"string","description":"the vault's name or id (see op_vaults)"}`
	const fieldsArg = `"fields":{"type":"array","description":"extra fields","items":{"type":"object","properties":{"label":{"type":"string"},"value":{"type":"string"},"concealed":{"type":"boolean","description":"true hides the value like a password"}},"required":["label","value"]}}`
	return []model.ToolSpec{
		{Name: "op_vaults", Description: "List the 1Password vaults the agent may use: id and name. Call this first to learn the vault names.",
			Parameters: obj(``), ResultLabels: sensitive},
		{Name: "op_items", Description: "List the items in one vault: id, title, category, tags and when each changed. No secret values. Filter by category (login, password, api_credential, secure_note, ...) or tag.",
			Parameters: obj(vaultArg+`,"category":{"type":"string"},"tag":{"type":"string"}`, "vault"), ResultLabels: sensitive},
		{Name: "op_item", Description: "Read one item's fields by id or title: usernames, URLs, notes and custom fields. A password or any concealed field shows as [concealed] with its op:// reference; use op_read on that reference when the task truly needs the value.",
			Parameters: obj(vaultArg+`,"item":{"type":"string","description":"the item's id or exact title"}`, "vault", "item"), ResultLabels: sensitive},
		{Name: "op_read", Description: "Resolve one op://vault/item/field reference to its value. GATED (egress): the secret enters your context. Read only what the task needs, and never repeat the value in a message, a file or a tool call unless the task is to use it there.",
			Parameters: obj(`"reference":{"type":"string","description":"op://vault/item/field, as op_item reports it"}`, "reference"), Gated: true, ResultLabels: sensitive},
		{Name: "op_create", Description: "Create an item in a vault: a login (username + password), a password, an api_credential (username + credential) or a secure_note. GATED (write). Set generate_password true to have 1Password make a strong password you never see; prefer that for a new account.",
			Parameters: obj(vaultArg+`,"title":{"type":"string"},"category":{"type":"string","description":"login (default), password, api_credential or secure_note"},"username":{"type":"string"},"password":{"type":"string","description":"the secret; omit it and set generate_password instead when possible"},"generate_password":{"type":"boolean"},"url":{"type":"string"},"notes":{"type":"string"},"tags":{"type":"array","items":{"type":"string"}},`+fieldsArg, "vault", "title"), Gated: true, ResultLabels: sensitive},
		{Name: "op_edit", Description: "Change an item: its title, URL, tags, or fields by label (a new label adds a field). GATED (write). A field value you give travels through the op process's arguments, so set generate_password true for a new password instead of writing one.",
			Parameters: obj(vaultArg+`,"item":{"type":"string","description":"the item's id or exact title"},"title":{"type":"string"},"url":{"type":"string"},"tags":{"type":"array","items":{"type":"string"},"description":"replaces the tags"},"generate_password":{"type":"boolean"},`+fieldsArg, "vault", "item"), Gated: true, ResultLabels: sensitive},
		{Name: "op_delete", Description: "Archive an item (1Password keeps it in the vault's Archive). GATED and destructive: it asks every time.",
			Parameters: obj(vaultArg+`,"item":{"type":"string","description":"the item's id or exact title"}`, "vault", "item"), Gated: true, ResultLabels: sensitive},
	}
}

func (d *Dispatcher) Dispatch(ctx context.Context, call model.ToolCall) (string, bool, error) {
	if d.be == nil {
		return notConfigured, true, nil
	}
	switch call.Name {
	case "op_vaults":
		return d.listVaults(ctx)
	case "op_items":
		return d.listItems(ctx, call.Arguments)
	case "op_item":
		return d.getItem(ctx, call.Arguments)
	case "op_read":
		return d.read(ctx, call.Arguments)
	case "op_create":
		return d.create(ctx, call.Arguments)
	case "op_edit":
		return d.edit(ctx, call.Arguments)
	case "op_delete":
		return d.archive(ctx, call.Arguments)
	}
	return "", true, fmt.Errorf("onepassword: unknown tool %q", call.Name)
}

// fail maps a backend error to what the model sees and what the log keeps.
// The CLI missing is a transport failure (nothing can work); the rest are
// answers the model can act on.
func (d *Dispatcher) fail(ctx context.Context, op string, err error) (string, bool, error) {
	switch {
	case errors.Is(err, ErrCLINotFound):
		CLINotFound.Log(ctx, "err", err.Error())
		return "", true, err
	case errors.Is(err, ErrRateLimited):
		RateLimited.Log(ctx, "op", op)
	case errors.Is(err, ErrAuth):
		AuthFailed.Log(ctx, "err", err.Error())
	}
	return err.Error(), true, nil
}

// --- the vault fence ---

func (d *Dispatcher) cachedVaults(ctx context.Context) ([]Vault, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.vaults != nil && time.Since(d.vaultsAt) < time.Minute {
		return d.vaults, nil
	}
	vaults, err := d.be.Vaults(ctx)
	if err != nil {
		return nil, err
	}
	d.vaults, d.vaultsAt = vaults, time.Now()
	return vaults, nil
}

func (d *Dispatcher) inAllowlist(v Vault, name string) bool {
	for _, a := range d.policy.Vaults {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if strings.EqualFold(a, name) || strings.EqualFold(a, v.ID) || strings.EqualFold(a, v.Name) {
			return true
		}
	}
	return false
}

// allowed checks a vault the model named against the allowlist: by the
// name as given, then by the vault's own id and name.
func (d *Dispatcher) allowed(ctx context.Context, vault string) error {
	vault = strings.TrimSpace(vault)
	if vault == "" {
		return errors.New("vault is required")
	}
	if len(d.policy.Vaults) == 0 || d.inAllowlist(Vault{}, vault) {
		return nil
	}
	vaults, err := d.cachedVaults(ctx)
	if err != nil {
		return err
	}
	for _, v := range vaults {
		if strings.EqualFold(v.ID, vault) || strings.EqualFold(v.Name, vault) {
			if d.inAllowlist(v, "") {
				return nil
			}
			break
		}
	}
	return fmt.Errorf("refused: vault %q is not one the agent may use (allowed: %s). This is the operator's boundary, not a bug", vault, strings.Join(d.policy.Vaults, ", "))
}

// --- reads ---

func (d *Dispatcher) listVaults(ctx context.Context) (string, bool, error) {
	vaults, err := d.be.Vaults(ctx)
	if err != nil {
		return d.fail(ctx, "vault list", err)
	}
	var b strings.Builder
	n := 0
	for _, v := range vaults {
		if len(d.policy.Vaults) > 0 && !d.inAllowlist(v, "") {
			continue
		}
		fmt.Fprintf(&b, "%s\t%s\n", v.ID, v.Name)
		n++
	}
	if n == 0 {
		return "no vaults: the service account sees none the agent may use", true, nil
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

func (d *Dispatcher) listItems(ctx context.Context, raw json.RawMessage) (string, bool, error) {
	var a struct {
		Vault    string `json:"vault"`
		Category string `json:"category"`
		Tag      string `json:"tag"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "bad arguments: " + err.Error(), true, nil
	}
	if err := d.allowed(ctx, a.Vault); err != nil {
		return err.Error(), true, nil
	}
	var cats, tags []string
	if c := strings.TrimSpace(a.Category); c != "" {
		if name, err := categoryName(c); err == nil {
			cats = []string{listCategory(name)}
		} else {
			cats = []string{c}
		}
	}
	if t := strings.TrimSpace(a.Tag); t != "" {
		tags = []string{t}
	}
	items, err := d.be.Items(ctx, a.Vault, cats, tags)
	if err != nil {
		return d.fail(ctx, "item list", err)
	}
	if len(items) == 0 {
		return "no items", false, nil
	}
	sort.SliceStable(items, func(i, j int) bool { return strings.ToLower(items[i].Title) < strings.ToLower(items[j].Title) })
	var b strings.Builder
	for _, it := range items {
		fmt.Fprintf(&b, "%s\t%s\t%s", it.ID, it.Title, strings.ToLower(it.Category))
		if len(it.Tags) > 0 {
			fmt.Fprintf(&b, "\ttags: %s", strings.Join(it.Tags, ", "))
		}
		if it.UpdatedAt != "" {
			fmt.Fprintf(&b, "\tupdated %s", it.UpdatedAt)
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n"), false, nil
}

// listCategory is the --categories spelling of a category (Login, Password,
// API Credential, Secure Note).
func listCategory(name string) string {
	switch name {
	case "LOGIN":
		return "Login"
	case "PASSWORD":
		return "Password"
	case "API_CREDENTIAL":
		return "API Credential"
	case "SECURE_NOTE":
		return "Secure Note"
	}
	return name
}

func (d *Dispatcher) getItem(ctx context.Context, raw json.RawMessage) (string, bool, error) {
	var a struct {
		Vault string `json:"vault"`
		Item  string `json:"item"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "bad arguments: " + err.Error(), true, nil
	}
	if err := d.allowed(ctx, a.Vault); err != nil {
		return err.Error(), true, nil
	}
	if strings.TrimSpace(a.Item) == "" {
		return "item is required", true, nil
	}
	it, err := d.be.Item(ctx, a.Vault, a.Item)
	if err != nil {
		return d.fail(ctx, "item get", err)
	}
	return render(it), false, nil
}

// render shows an item with every concealed value replaced by its reference.
func render(it Item) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s) id=%s vault=%s", it.Title, strings.ToLower(it.Category), it.ID, it.Vault.Name)
	if len(it.Tags) > 0 {
		fmt.Fprintf(&b, " tags=%s", strings.Join(it.Tags, ","))
	}
	if it.UpdatedAt != "" {
		fmt.Fprintf(&b, " updated=%s", it.UpdatedAt)
	}
	b.WriteByte('\n')
	for _, u := range it.URLs {
		fmt.Fprintf(&b, "url: %s\n", u.Href)
	}
	for _, f := range it.Fields {
		label := f.Label
		if label == "" {
			label = f.ID
		}
		if f.Section != "" {
			label = f.Section + "." + label
		}
		switch {
		case Concealed(f):
			fmt.Fprintf(&b, "%s: [concealed]", label)
			if f.Reference != "" {
				fmt.Fprintf(&b, " ref=%s", f.Reference)
			}
			b.WriteByte('\n')
		case f.Value == "":
			continue
		default:
			fmt.Fprintf(&b, "%s: %s\n", label, f.Value)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// --- the secret path ---

func (d *Dispatcher) read(ctx context.Context, raw json.RawMessage) (string, bool, error) {
	var a struct {
		Reference string `json:"reference"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "bad arguments: " + err.Error(), true, nil
	}
	ref := strings.TrimSpace(a.Reference)
	vault, _, _, ok := ParseReference(ref)
	if !ok {
		return "reference must look like op://vault/item/field (op_item reports it as ref=...)", true, nil
	}
	if err := d.allowed(ctx, vault); err != nil {
		return err.Error(), true, nil
	}
	if !d.policy.AllowReveal {
		if d.policy.Confirm == nil || !d.policy.Confirm(ctx, ActionReveal, ref) {
			return "blocked: reading " + ref + " into the conversation was not approved. This is the operator's secrets boundary, not a bug - ask for the reference to be used by a tool instead, or for approval", true, nil
		}
	}
	value, err := d.be.Read(ctx, ref)
	if err != nil {
		return d.fail(ctx, "read", err)
	}
	if d.policy.Revealed != nil {
		d.policy.Revealed(ref, value)
	}
	Revealed.Log(ctx, "reference", ref)
	return value, false, nil
}

// --- writes ---

func (d *Dispatcher) gateWrite(ctx context.Context, detail string) error {
	if d.policy.AllowWrite {
		return nil
	}
	if d.policy.Confirm != nil && d.policy.Confirm(ctx, ActionWrite, detail) {
		return nil
	}
	return fmt.Errorf("blocked: this would change the vault (%s) and was not approved - enable the write flag or confirm. This is the operator's boundary, not a bug", detail)
}

type fieldArg struct {
	Label     string `json:"label"`
	Value     string `json:"value"`
	Concealed bool   `json:"concealed"`
}

func fieldInputs(args []fieldArg) ([]FieldInput, error) {
	var out []FieldInput
	for _, f := range args {
		if strings.TrimSpace(f.Label) == "" {
			return nil, errors.New("every field needs a label")
		}
		out = append(out, FieldInput{Label: f.Label, Value: f.Value, Concealed: f.Concealed})
	}
	return out, nil
}

func (d *Dispatcher) create(ctx context.Context, raw json.RawMessage) (string, bool, error) {
	var a struct {
		Vault            string     `json:"vault"`
		Title            string     `json:"title"`
		Category         string     `json:"category"`
		Username         string     `json:"username"`
		Password         string     `json:"password"`
		GeneratePassword bool       `json:"generate_password"`
		URL              string     `json:"url"`
		Notes            string     `json:"notes"`
		Tags             []string   `json:"tags"`
		Fields           []fieldArg `json:"fields"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "bad arguments: " + err.Error(), true, nil
	}
	if err := d.allowed(ctx, a.Vault); err != nil {
		return err.Error(), true, nil
	}
	if strings.TrimSpace(a.Title) == "" {
		return "title is required", true, nil
	}
	if _, err := categoryName(a.Category); err != nil {
		return err.Error(), true, nil
	}
	fields, err := fieldInputs(a.Fields)
	if err != nil {
		return err.Error(), true, nil
	}
	detail := fmt.Sprintf("create %q in vault %s", a.Title, a.Vault)
	if err := d.gateWrite(ctx, detail); err != nil {
		return err.Error(), true, nil
	}
	it, err := d.be.Create(ctx, CreateParams{
		Vault: a.Vault, Title: a.Title, Category: a.Category, Username: a.Username, Password: a.Password,
		URL: a.URL, Notes: a.Notes, Tags: a.Tags, Fields: fields, GeneratePassword: a.GeneratePassword,
	})
	if err != nil {
		return d.fail(ctx, "item create", err)
	}
	ItemCreated.Log(ctx, "title", it.Title, "vault", it.Vault.Name)
	return "created\n" + render(it), false, nil
}

func (d *Dispatcher) edit(ctx context.Context, raw json.RawMessage) (string, bool, error) {
	var a struct {
		Vault            string     `json:"vault"`
		Item             string     `json:"item"`
		Title            string     `json:"title"`
		URL              string     `json:"url"`
		Tags             []string   `json:"tags"`
		GeneratePassword bool       `json:"generate_password"`
		Fields           []fieldArg `json:"fields"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "bad arguments: " + err.Error(), true, nil
	}
	if err := d.allowed(ctx, a.Vault); err != nil {
		return err.Error(), true, nil
	}
	if strings.TrimSpace(a.Item) == "" {
		return "item is required", true, nil
	}
	fields, err := fieldInputs(a.Fields)
	if err != nil {
		return err.Error(), true, nil
	}
	if a.Title == "" && a.URL == "" && a.Tags == nil && !a.GeneratePassword && len(fields) == 0 {
		return "nothing to change: give a title, url, tags, fields or generate_password", true, nil
	}
	detail := fmt.Sprintf("edit %q in vault %s", a.Item, a.Vault)
	if err := d.gateWrite(ctx, detail); err != nil {
		return err.Error(), true, nil
	}
	it, err := d.be.Edit(ctx, EditParams{Vault: a.Vault, Item: a.Item, Title: a.Title, URL: a.URL, Tags: a.Tags, Fields: fields, GeneratePassword: a.GeneratePassword})
	if err != nil {
		return d.fail(ctx, "item edit", err)
	}
	ItemEdited.Log(ctx, "item", it.Title)
	return "edited\n" + render(it), false, nil
}

func (d *Dispatcher) archive(ctx context.Context, raw json.RawMessage) (string, bool, error) {
	var a struct {
		Vault string `json:"vault"`
		Item  string `json:"item"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "bad arguments: " + err.Error(), true, nil
	}
	if err := d.allowed(ctx, a.Vault); err != nil {
		return err.Error(), true, nil
	}
	if strings.TrimSpace(a.Item) == "" {
		return "item is required", true, nil
	}
	detail := fmt.Sprintf("archive %q in vault %s", a.Item, a.Vault)
	if d.policy.Confirm == nil || !d.policy.Confirm(ctx, ActionArchive, detail) {
		return "blocked: archiving " + a.Item + " requires explicit approval and was not confirmed. This is a data-safety boundary, not a bug", true, nil
	}
	if err := d.be.Archive(ctx, a.Vault, a.Item); err != nil {
		return d.fail(ctx, "item delete", err)
	}
	ItemArchived.Log(ctx, "item", a.Item)
	return "archived " + a.Item, false, nil
}
