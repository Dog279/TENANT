package onepassword

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// CLI is the Backend over the op command-line tool. Every call is one op
// process with the service account token in its environment, never in its
// arguments, and --format json. A value the agent sets on an item travels
// on stdin as a JSON template, so it is not in the process list either.
type CLI struct {
	path    string
	token   string
	timeout time.Duration
	// run starts the process; tests replace it.
	run func(ctx context.Context, path string, env []string, stdin string, args ...string) (stdout, stderr []byte, err error)
}

// DefaultTimeout bounds one op call. op talks to 1Password's servers on
// every call; a stall there must not hang the agent's turn.
const DefaultTimeout = 20 * time.Second

// NewCLI finds the op binary at path (empty: on PATH) and keeps the token.
// A missing binary is ErrCLINotFound, so a misconfigured install fails at
// launch, not on the first tool call.
func NewCLI(path, token string) (*CLI, error) {
	if strings.TrimSpace(path) == "" {
		path = "op"
	}
	resolved, err := exec.LookPath(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrCLINotFound, path)
	}
	return &CLI{path: resolved, token: token, timeout: DefaultTimeout, run: runProcess}, nil
}

// Path is the binary the CLI runs.
func (c *CLI) Path() string { return c.path }

func runProcess(ctx context.Context, path string, env []string, stdin string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = env
	noWindow(cmd)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}

// env is the child's environment: the parent's without any op session or
// token variable, plus this token. OP_FORMAT is not used; --format json is
// passed per call so a reader of the arguments sees what was asked.
func (c *CLI) env() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(name) {
		case "OP_SERVICE_ACCOUNT_TOKEN", "OP_SESSION", "OP_CONNECT_HOST", "OP_CONNECT_TOKEN":
			continue
		}
		if strings.HasPrefix(strings.ToUpper(name), "OP_SESSION_") {
			continue
		}
		env = append(env, kv)
	}
	if c.token != "" {
		env = append(env, "OP_SERVICE_ACCOUNT_TOKEN="+c.token)
	}
	return env
}

// call runs op with args and classifies a failure. stdin is sent when set.
func (c *CLI) call(ctx context.Context, stdin string, args ...string) ([]byte, error) {
	timeout := c.timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, stderr, err := c.run(ctx, c.path, c.env(), stdin, args...)
	if err == nil {
		return out, nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("op %s timed out after %s", opName(args), timeout)
	}
	var ee *exec.Error
	if errors.As(err, &ee) {
		return nil, fmt.Errorf("%w: %v", ErrCLINotFound, err)
	}
	return nil, classify(opName(args), string(stderr), err)
}

// opName is the command the arguments name, for messages and events: the
// group and verb of a two-word command (vault list, item get), or the one
// word of read, whoami and the like.
func opName(args []string) string {
	if len(args) == 0 {
		return ""
	}
	switch args[0] {
	case "vault", "item", "account", "user", "group", "document", "connect", "service-account", "events-api":
		if len(args) > 1 && !strings.HasPrefix(args[1], "-") {
			return args[0] + " " + args[1]
		}
	}
	return args[0]
}

// errPrefix is op's stderr line shape: "[ERROR] 2024/01/02 15:04:05 message".
var errPrefix = regexp.MustCompile(`^\[ERROR\]\s+\S+\s+\S+\s*`)

// classify turns op's stderr into one of the package errors, or a plain
// message with the prefix and timestamps gone.
func classify(op, stderr string, err error) error {
	msg := firstLine(stderr)
	msg = errPrefix.ReplaceAllString(msg, "")
	if msg == "" {
		msg = err.Error()
	}
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "(429)") || strings.Contains(lower, "too many requests") || strings.Contains(lower, "rate limit"):
		return fmt.Errorf("%w (%s): %s", ErrRateLimited, op, msg)
	case strings.Contains(lower, "(401)") || strings.Contains(lower, "unauthorized") || strings.Contains(lower, "token") && (strings.Contains(lower, "invalid") || strings.Contains(lower, "expired") || strings.Contains(lower, "revoked")) ||
		strings.Contains(lower, "not currently signed in") || strings.Contains(lower, "session expired") || strings.Contains(lower, "authentication"):
		return fmt.Errorf("%w (%s): %s", ErrAuth, op, msg)
	case strings.Contains(lower, "(404)") || strings.Contains(lower, "isn't a vault") || strings.Contains(lower, "isn't an item") ||
		strings.Contains(lower, "not found") || strings.Contains(lower, "could not find") || strings.Contains(lower, "no vault matches") ||
		strings.Contains(lower, "no item matches") || strings.Contains(lower, "doesn't have access") || strings.Contains(lower, "does not have access"):
		return fmt.Errorf("%w (%s): %s", ErrNotFound, op, msg)
	}
	return fmt.Errorf("op %s failed: %s", op, msg)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// --- the Backend ---

func (c *CLI) WhoAmI(ctx context.Context) (Identity, error) {
	out, err := c.call(ctx, "", "whoami", "--format", "json")
	if err != nil {
		return Identity{}, err
	}
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		return Identity{}, fmt.Errorf("op whoami: unreadable answer: %w", err)
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := raw[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	return Identity{
		URL:   pick("url", "URL"),
		Email: pick("email", "Email"),
		Type:  pick("user_type", "UserType", "account_type"),
		ID:    pick("user_uuid", "integration_id", "UserUUID"),
	}, nil
}

type opVault struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (c *CLI) Vaults(ctx context.Context) ([]Vault, error) {
	out, err := c.call(ctx, "", "vault", "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	var raw []opVault
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("op vault list: unreadable answer: %w", err)
	}
	vaults := make([]Vault, 0, len(raw))
	for _, v := range raw {
		vaults = append(vaults, Vault{ID: v.ID, Name: v.Name})
	}
	return vaults, nil
}

type opItem struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Category  string   `json:"category"`
	Vault     opVault  `json:"vault"`
	Tags      []string `json:"tags"`
	UpdatedAt string   `json:"updated_at"`
	Fields    []struct {
		ID        string `json:"id"`
		Label     string `json:"label"`
		Type      string `json:"type"`
		Purpose   string `json:"purpose"`
		Value     string `json:"value"`
		Reference string `json:"reference"`
		Section   *struct {
			ID    string `json:"id"`
			Label string `json:"label"`
		} `json:"section"`
	} `json:"fields"`
	URLs []struct {
		Label   string `json:"label"`
		Href    string `json:"href"`
		Primary bool   `json:"primary"`
	} `json:"urls"`
}

func (o opItem) overview() ItemOverview {
	return ItemOverview{ID: o.ID, Title: o.Title, Category: o.Category, Vault: Vault{ID: o.Vault.ID, Name: o.Vault.Name}, Tags: o.Tags, UpdatedAt: o.UpdatedAt}
}

func (o opItem) item() Item {
	it := Item{ID: o.ID, Title: o.Title, Category: o.Category, Vault: Vault{ID: o.Vault.ID, Name: o.Vault.Name}, Tags: o.Tags, UpdatedAt: o.UpdatedAt}
	for _, f := range o.Fields {
		field := Field{ID: f.ID, Label: f.Label, Type: f.Type, Purpose: f.Purpose, Value: f.Value, Reference: f.Reference}
		if f.Section != nil {
			field.Section = f.Section.Label
			if field.Section == "" {
				field.Section = f.Section.ID
			}
		}
		it.Fields = append(it.Fields, field)
	}
	for _, u := range o.URLs {
		it.URLs = append(it.URLs, URL{Label: u.Label, Href: u.Href, Primary: u.Primary})
	}
	return it
}

func (c *CLI) Items(ctx context.Context, vault string, categories, tags []string) ([]ItemOverview, error) {
	args := []string{"item", "list", "--vault", vault, "--format", "json"}
	if len(categories) > 0 {
		args = append(args, "--categories", strings.Join(categories, ","))
	}
	if len(tags) > 0 {
		args = append(args, "--tags", strings.Join(tags, ","))
	}
	out, err := c.call(ctx, "", args...)
	if err != nil {
		return nil, err
	}
	var raw []opItem
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("op item list: unreadable answer: %w", err)
	}
	items := make([]ItemOverview, 0, len(raw))
	for _, o := range raw {
		items = append(items, o.overview())
	}
	return items, nil
}

func (c *CLI) Item(ctx context.Context, vault, item string) (Item, error) {
	out, err := c.call(ctx, "", "item", "get", item, "--vault", vault, "--format", "json")
	if err != nil {
		return Item{}, err
	}
	var raw opItem
	if err := json.Unmarshal(out, &raw); err != nil {
		return Item{}, fmt.Errorf("op item get: unreadable answer: %w", err)
	}
	return raw.item(), nil
}

func (c *CLI) Read(ctx context.Context, reference string) (string, error) {
	out, err := c.call(ctx, "", "read", reference, "--no-newline")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// categoryName maps the tool's category word to 1Password's.
func categoryName(c string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(c)) {
	case "", "login":
		return "LOGIN", nil
	case "password":
		return "PASSWORD", nil
	case "api_credential", "api credential", "api-credential":
		return "API_CREDENTIAL", nil
	case "secure_note", "secure note", "secure-note", "note":
		return "SECURE_NOTE", nil
	}
	return "", fmt.Errorf("category %q: use login, password, api_credential or secure_note", c)
}

// template is the JSON `op item create -` reads on stdin: the same shape op
// item get prints. Values travel here, not in the arguments.
type template struct {
	Title    string          `json:"title"`
	Category string          `json:"category"`
	Tags     []string        `json:"tags,omitempty"`
	Fields   []templateField `json:"fields"`
	URLs     []templateURL   `json:"urls,omitempty"`
}

type templateField struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Purpose string `json:"purpose,omitempty"`
	Label   string `json:"label"`
	Value   string `json:"value"`
}

type templateURL struct {
	Label   string `json:"label"`
	Primary bool   `json:"primary"`
	Href    string `json:"href"`
}

func fieldID(label string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(label) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	id := strings.Trim(b.String(), "_")
	if id == "" {
		id = "field"
	}
	return id
}

func fieldType(concealed bool) string {
	if concealed {
		return "CONCEALED"
	}
	return "STRING"
}

func (c *CLI) Create(ctx context.Context, p CreateParams) (Item, error) {
	cat, err := categoryName(p.Category)
	if err != nil {
		return Item{}, err
	}
	t := template{Title: p.Title, Category: cat, Tags: p.Tags, Fields: []templateField{}}
	switch cat {
	case "LOGIN":
		t.Fields = append(t.Fields, templateField{ID: "username", Type: "STRING", Purpose: "USERNAME", Label: "username", Value: p.Username})
		if !p.GeneratePassword {
			t.Fields = append(t.Fields, templateField{ID: "password", Type: "CONCEALED", Purpose: "PASSWORD", Label: "password", Value: p.Password})
		}
	case "PASSWORD":
		if !p.GeneratePassword {
			t.Fields = append(t.Fields, templateField{ID: "password", Type: "CONCEALED", Purpose: "PASSWORD", Label: "password", Value: p.Password})
		}
	case "API_CREDENTIAL":
		if p.Username != "" {
			t.Fields = append(t.Fields, templateField{ID: "username", Type: "STRING", Label: "username", Value: p.Username})
		}
		if p.Password != "" {
			t.Fields = append(t.Fields, templateField{ID: "credential", Type: "CONCEALED", Label: "credential", Value: p.Password})
		}
	}
	if p.Notes != "" {
		t.Fields = append(t.Fields, templateField{ID: "notesPlain", Type: "STRING", Purpose: "NOTES", Label: "notesPlain", Value: p.Notes})
	}
	for _, f := range p.Fields {
		t.Fields = append(t.Fields, templateField{ID: fieldID(f.Label), Type: fieldType(f.Concealed), Label: f.Label, Value: f.Value})
	}
	if p.URL != "" {
		t.URLs = []templateURL{{Label: "website", Primary: true, Href: p.URL}}
	}
	body, err := json.Marshal(t)
	if err != nil {
		return Item{}, err
	}
	args := []string{"item", "create", "-", "--vault", p.Vault, "--format", "json"}
	if p.GeneratePassword {
		args = append(args, "--generate-password=letters,digits,symbols,32")
	}
	out, err := c.call(ctx, string(body), args...)
	if err != nil {
		return Item{}, err
	}
	var raw opItem
	if err := json.Unmarshal(out, &raw); err != nil {
		return Item{}, fmt.Errorf("op item create: unreadable answer: %w", err)
	}
	return raw.item(), nil
}

// Edit changes an item with op's field assignments. A value set here is an
// argument of the op process; the tool's description says to prefer
// generate_password for a secret.
func (c *CLI) Edit(ctx context.Context, p EditParams) (Item, error) {
	args := []string{"item", "edit", p.Item, "--vault", p.Vault, "--format", "json"}
	if p.Title != "" {
		args = append(args, "--title", p.Title)
	}
	if p.URL != "" {
		args = append(args, "--url", p.URL)
	}
	if p.Tags != nil {
		args = append(args, "--tags", strings.Join(p.Tags, ","))
	}
	if p.GeneratePassword {
		args = append(args, "--generate-password=letters,digits,symbols,32")
	}
	for _, f := range p.Fields {
		kind := "text"
		if f.Concealed {
			kind = "password"
		}
		args = append(args, assignment(f.Label)+"["+kind+"]="+f.Value)
	}
	out, err := c.call(ctx, "", args...)
	if err != nil {
		return Item{}, err
	}
	var raw opItem
	if err := json.Unmarshal(out, &raw); err != nil {
		return Item{}, fmt.Errorf("op item edit: unreadable answer: %w", err)
	}
	return raw.item(), nil
}

// assignment escapes a field label for op's [section.]label[type]=value
// syntax: a period, an equals sign and a backslash in the label are escaped.
func assignment(label string) string {
	r := strings.NewReplacer(`\`, `\\`, `.`, `\.`, `=`, `\=`)
	return r.Replace(label)
}

func (c *CLI) Archive(ctx context.Context, vault, item string) error {
	_, err := c.call(ctx, "", "item", "delete", item, "--vault", vault, "--archive")
	return err
}
