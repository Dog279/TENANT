package backup

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"filippo.io/age"
)

// Keys says how a backup is encrypted or decrypted. A passphrase and public
// keys cannot be mixed when encrypting: age allows a passphrase only as the
// sole recipient.
type Keys struct {
	// Passphrase encrypts with age's scrypt recipient, or decrypts with it.
	Passphrase string
	// Recipients are age X25519 public keys ("age1…") to encrypt to.
	Recipients []string
	// RecipientFiles hold public keys, one per line; blank lines and lines
	// starting with # are ignored.
	RecipientFiles []string
	// IdentityFiles hold age private keys ("AGE-SECRET-KEY-1…") to decrypt
	// with, in the format age-keygen writes.
	IdentityFiles []string
	// scryptWorkFactor overrides age's default cost; tests lower it.
	scryptWorkFactor int
}

// Encrypts reports whether Keys asks for an encrypted backup.
func (k Keys) Encrypts() bool {
	return k.Passphrase != "" || len(k.Recipients) > 0 || len(k.RecipientFiles) > 0
}

func (k Keys) recipients() ([]age.Recipient, error) {
	keys := append([]string(nil), k.Recipients...)
	for _, path := range k.RecipientFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("backup: recipients file: %w", err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			keys = append(keys, line)
		}
	}
	if k.Passphrase != "" && len(keys) > 0 {
		return nil, errors.New("backup: use a passphrase or public-key recipients, not both")
	}
	if k.Passphrase != "" {
		r, err := age.NewScryptRecipient(k.Passphrase)
		if err != nil {
			return nil, fmt.Errorf("backup: %w", err)
		}
		if k.scryptWorkFactor > 0 {
			r.SetWorkFactor(k.scryptWorkFactor)
		}
		return []age.Recipient{r}, nil
	}
	var out []age.Recipient
	for _, key := range keys {
		if strings.HasPrefix(strings.ToUpper(key), "AGE-SECRET-KEY-") {
			return nil, errors.New("backup: a recipient must be a public key (age1…), and that one is a private key; keep it out of recipient lists")
		}
		r, err := age.ParseX25519Recipient(key)
		if err != nil {
			return nil, fmt.Errorf("backup: recipient %q: %w", shorten(key), err)
		}
		out = append(out, r)
	}
	return out, nil
}

func (k Keys) identities() ([]age.Identity, error) {
	var out []age.Identity
	if k.Passphrase != "" {
		id, err := age.NewScryptIdentity(k.Passphrase)
		if err != nil {
			return nil, fmt.Errorf("backup: %w", err)
		}
		out = append(out, id)
	}
	for _, path := range k.IdentityFiles {
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("backup: identity file: %w", err)
		}
		ids, err := age.ParseIdentities(f)
		_ = f.Close()
		if err != nil {
			return nil, fmt.Errorf("backup: identity file %s: %w", path, err)
		}
		out = append(out, ids...)
	}
	return out, nil
}

// shorten keeps an error from echoing a long pasted value in full.
func shorten(s string) string {
	if len(s) > 24 {
		return s[:20] + "…"
	}
	return s
}
