package peering

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// invitePrefix tags a Tenant peering invite code so a mis-paste is rejected
// with a clear error rather than a confusing decode failure.
const invitePrefix = "tnt1_"

// Invite is the payload encoded in a one-time invite code. The inviter mints
// it; the joiner parses it and dials back. Short JSON keys keep the code
// copy-pasteable.
type Invite struct {
	Name        string `json:"n"`           // inviter's name for ITSELF (the joiner stores the peer under this)
	URL         string `json:"u"`           // where the joiner dials the inviter
	Secret      string `json:"s"`           // the shared bearer
	InstanceID  string `json:"i"`           // inviter's stable instance_id
	Fingerprint string `json:"f,omitempty"` // inviter's TLS cert pin (TEN-185); empty under overlay
	Expiry      int64  `json:"e"`           // unix seconds; the code is rejected after this
}

// base64URL encodes without padding (URL/clipboard safe).
func base64URL(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// Encode renders the invite as a single copy-pasteable code.
func (iv Invite) Encode() (string, error) {
	b, err := json.Marshal(iv)
	if err != nil {
		return "", fmt.Errorf("peering: encode invite: %w", err)
	}
	return invitePrefix + base64URL(b), nil
}

// ParseInvite decodes and validates an invite code: prefix, structure,
// required fields, and expiry. An expired or malformed code is rejected.
func ParseInvite(code string) (*Invite, error) {
	code = strings.TrimSpace(code)
	if !strings.HasPrefix(code, invitePrefix) {
		return nil, fmt.Errorf("not a Tenant peer invite code (expected %q… prefix)", invitePrefix)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(code, invitePrefix))
	if err != nil {
		return nil, fmt.Errorf("peering: invite code is corrupt: %w", err)
	}
	var iv Invite
	if err := json.Unmarshal(raw, &iv); err != nil {
		return nil, fmt.Errorf("peering: invite code is corrupt: %w", err)
	}
	if iv.Name == "" || iv.URL == "" || iv.Secret == "" || iv.InstanceID == "" {
		return nil, fmt.Errorf("peering: invite code is missing required fields")
	}
	if iv.Expiry != 0 && time.Now().Unix() > iv.Expiry {
		return nil, fmt.Errorf("peering: invite code expired %s ago — ask for a fresh one",
			time.Since(time.Unix(iv.Expiry, 0)).Round(time.Second))
	}
	return &iv, nil
}
