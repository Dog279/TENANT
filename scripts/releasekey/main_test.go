package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/selfupdate"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// A key from keygen signs in the workflow, and its signature verifies the
// way a release binary verifies it; keygen never overwrites.
func TestKeygenCheckSign(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "release.key")
	var out bytes.Buffer
	if err := run([]string{"keygen", "-out", keyFile}, envOf(nil), &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	pub := lines[len(lines)-1]
	seed, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), strings.TrimSpace(string(seed))) {
		t.Fatal("keygen printed the private key")
	}
	if err := run([]string{"keygen", "-out", keyFile}, envOf(nil), &out); err == nil {
		t.Fatal("keygen overwrote an existing key file")
	}
	if b, _ := os.ReadFile(keyFile); !bytes.Equal(b, seed) {
		t.Fatal("the refused keygen changed the key file")
	}
	otherKey, _, _ := ed25519.GenerateKey(nil)
	otherPub := base64.StdEncoding.EncodeToString(otherKey)
	// The secret as GitHub may hand it back: with its newline.
	env := map[string]string{envSecret: string(seed), envPublic: otherPub + "," + pub}

	out.Reset()
	if err := run([]string{"check"}, envOf(env), &out); err != nil || out.String() != "sign=true\n" {
		t.Fatalf("check: %q, %v", out.String(), err)
	}
	out.Reset()
	if err := run([]string{"check"}, envOf(nil), &out); err != nil || out.String() != "sign=false\n" {
		t.Fatalf("check with neither: %q, %v", out.String(), err)
	}

	sums := filepath.Join(dir, "checksums.txt")
	body := []byte(strings.Repeat("ab", 32) + "  tenant_1.0.0_linux_x86_64.tar.gz\n")
	if err := os.WriteFile(sums, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"sign", sums}, envOf(env), &out); err != nil {
		t.Fatal(err)
	}
	sig, err := os.ReadFile(filepath.Join(dir, selfupdate.SignatureAsset))
	if err != nil {
		t.Fatal(err)
	}
	keys, _ := selfupdate.ParseKeys(otherPub + "," + pub)
	if i, err := selfupdate.VerifyProject(body, sig, keys); err != nil || i != 1 {
		t.Fatalf("the signature: key %d, %v", i, err)
	}
}

func TestCheckRefusals(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	seed := base64.StdEncoding.EncodeToString(priv.Seed())
	pub := base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	_, other, _ := ed25519.GenerateKey(nil)
	otherPub := base64.StdEncoding.EncodeToString(other.Public().(ed25519.PublicKey))
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"the variable without the secret": {map[string]string{envPublic: pub}, "is not: this release's binaries would refuse"},
		"the secret without the variable": {map[string]string{envSecret: seed}, "would carry no key"},
		"a variable that does not parse":  {map[string]string{envSecret: seed, envPublic: pub + ","}, envPublic},
		"a secret that does not parse":    {map[string]string{envSecret: seed[:20], envPublic: pub}, "not standard base64"},
		"a secret not among the keys":     {map[string]string{envSecret: seed, envPublic: otherPub}, "not among the keys"},
	} {
		var out bytes.Buffer
		err := run([]string{"check"}, envOf(tc.env), &out)
		if err == nil || !strings.Contains(err.Error(), tc.want) || out.Len() != 0 {
			t.Errorf("%s: %q, %v; want a failure saying %q", name, out.String(), err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), seed) {
			t.Errorf("%s: the error carries the secret", name)
		}
	}
	if err := run([]string{"sign", filepath.Join(t.TempDir(), "x")}, envOf(nil), &bytes.Buffer{}); err == nil {
		t.Error("sign with no key")
	}
	if err := run([]string{"keygen"}, envOf(nil), &bytes.Buffer{}); err == nil {
		t.Error("keygen with no -out")
	}
}
