package backup

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

func TestKeysPassphraseRoundTrip(t *testing.T) {
	k := Keys{Passphrase: "pw", scryptWorkFactor: 10}
	if !k.Encrypts() {
		t.Fatal("a passphrase should encrypt")
	}
	rs, err := k.recipients()
	if err != nil {
		t.Fatal(err)
	}
	ids, err := k.identities()
	if err != nil {
		t.Fatal(err)
	}
	m, open := stage(t)
	var buf bytes.Buffer
	if err := writeStream(&buf, m, open, rs); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readAll(t, buf.Bytes(), ids...); err != nil {
		t.Fatal(err)
	}
}

func TestKeysRecipientAndIdentityFiles(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	rf := filepath.Join(dir, "recipients.txt")
	if err := os.WriteFile(rf, []byte("# backup key\n\n"+id.Recipient().String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	idf := filepath.Join(dir, "key.txt")
	if err := os.WriteFile(idf, []byte("# created: now\n"+id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	enc := Keys{RecipientFiles: []string{rf}}
	rs, err := enc.recipients()
	if err != nil || len(rs) != 1 {
		t.Fatalf("recipients = %v, %v", rs, err)
	}
	ids, err := Keys{IdentityFiles: []string{idf}}.identities()
	if err != nil || len(ids) != 1 {
		t.Fatalf("identities = %v, %v", ids, err)
	}
	m, open := stage(t)
	var buf bytes.Buffer
	if err := writeStream(&buf, m, open, rs); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readAll(t, buf.Bytes(), ids...); err != nil {
		t.Fatal(err)
	}
}

func TestKeysRejectMisuse(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (Keys{Passphrase: "pw", Recipients: []string{id.Recipient().String()}}).recipients(); err == nil {
		t.Fatal("mixed passphrase and public keys accepted")
	}
	_, err = (Keys{Recipients: []string{id.String()}}).recipients()
	if err == nil || strings.Contains(err.Error(), id.String()[15:]) {
		t.Fatalf("a private key given as a recipient: err = %v (must refuse without echoing it)", err)
	}
	if _, err := (Keys{Recipients: []string{"age1notakey"}}).recipients(); err == nil {
		t.Fatal("malformed recipient accepted")
	}
	if (Keys{IdentityFiles: []string{"x"}}).Encrypts() {
		t.Fatal("identity files alone do not encrypt")
	}
}
