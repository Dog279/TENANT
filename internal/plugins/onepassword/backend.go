// Package onepassword gives the agent typed access to 1Password: the vaults a
// service account can see, the items in them, one secret at a time through
// an op:// reference, and new or changed items. A Backend does the talking;
// the op command-line tool is the one that ships (opcli.go). The Dispatcher
// (dispatch.go) is where the rules live: which vaults, which tools are
// gated, and that a concealed value reaches the model only through op_read.
package onepassword

import (
	"context"
	"errors"
	"strings"
)

// Vault is one vault the service account can see.
type Vault struct {
	ID   string
	Name string
}

// ItemOverview is an item as a listing shows it: no field values.
type ItemOverview struct {
	ID        string
	Title     string
	Category  string
	Vault     Vault
	Tags      []string
	UpdatedAt string
}

// Field is one field of an item. Reference is the op://vault/item/field
// form 1Password reports for it, which op_read resolves.
type Field struct {
	ID        string
	Label     string
	Type      string // STRING, CONCEALED, URL, EMAIL, OTP, DATE, ...
	Purpose   string // USERNAME, PASSWORD, NOTES, or ""
	Value     string
	Reference string
	Section   string
}

// URL is one website on an item.
type URL struct {
	Label   string
	Href    string
	Primary bool
}

// Item is one item with its fields.
type Item struct {
	ID        string
	Title     string
	Category  string
	Vault     Vault
	Tags      []string
	Fields    []Field
	URLs      []URL
	UpdatedAt string
}

// FieldInput is a field the agent sets on create or edit.
type FieldInput struct {
	Label     string
	Value     string
	Concealed bool
}

// CreateParams describes a new item.
type CreateParams struct {
	Vault    string // name or id
	Title    string
	Category string // login, password, api_credential, secure_note
	Username string
	Password string
	URL      string
	Notes    string
	Tags     []string
	Fields   []FieldInput
	// GeneratePassword asks 1Password for a strong password, so the value
	// never passes through the model or the process arguments.
	GeneratePassword bool
}

// EditParams describes changes to an item.
type EditParams struct {
	Vault            string
	Item             string // id or title
	Title            string
	URL              string
	Tags             []string // nil: unchanged
	Fields           []FieldInput
	GeneratePassword bool
}

// Identity is who the token is, as `op whoami` reports it.
type Identity struct {
	URL   string
	Email string
	Type  string // SERVICE_ACCOUNT for a service account
	ID    string
}

// Backend is what the dispatcher needs from 1Password.
type Backend interface {
	WhoAmI(ctx context.Context) (Identity, error)
	Vaults(ctx context.Context) ([]Vault, error)
	Items(ctx context.Context, vault string, categories, tags []string) ([]ItemOverview, error)
	Item(ctx context.Context, vault, item string) (Item, error)
	Read(ctx context.Context, reference string) (string, error)
	Create(ctx context.Context, p CreateParams) (Item, error)
	Edit(ctx context.Context, p EditParams) (Item, error)
	Archive(ctx context.Context, vault, item string) error
}

// The errors a backend classifies, so the dispatcher can tell the model (and
// the log) what happened without quoting op's stderr.
var (
	ErrCLINotFound = errors.New("the 1Password CLI (op) was not found")
	ErrAuth        = errors.New("1Password refused the service account token")
	ErrRateLimited = errors.New("1Password's rate limit is reached; wait for the window to reset")
	ErrNotFound    = errors.New("not found in 1Password")
)

// Concealed reports whether a field holds a value the model must not see
// in an item listing: passwords and anything 1Password marks concealed, plus
// one-time codes, SSH keys and card numbers.
func Concealed(f Field) bool {
	switch strings.ToUpper(f.Type) {
	case "CONCEALED", "OTP", "SSHKEY", "CREDIT_CARD_NUMBER":
		return true
	}
	return strings.ToUpper(f.Purpose) == "PASSWORD"
}

// ParseReference splits an op://vault/item/field reference into its parts.
// A section may precede the field (op://vault/item/section/field); it is
// kept with the field.
func ParseReference(ref string) (vault, item, field string, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(ref), "op://")
	if !found {
		return "", "", "", false
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}
