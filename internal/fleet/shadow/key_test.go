package shadow

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"filippo.io/age"

	"tenant/internal/secrets"
)

// xorProtector stands in for an operating system's secret protector, so both
// forms of the key file are tested on every system.
type xorProtector struct{}

func (xorProtector) Name() string { return "test" }
func (xorProtector) Protect(p []byte) ([]byte, error) {
	out := append([]byte("sealed:"), p...)
	for i := 7; i < len(out); i++ {
		out[i] ^= 0xa5
	}
	return out, nil
}
func (xorProtector) Unprotect(c []byte) ([]byte, error) {
	body, ok := bytes.CutPrefix(c, []byte("sealed:"))
	if !ok {
		return nil, errors.New("not sealed by this protector")
	}
	out := bytes.Clone(body)
	for i := range out {
		out[i] ^= 0xa5
	}
	return out, nil
}

// useProtector swaps the protector for one test: nil for a system without one.
func useProtector(t testing.TB, p secrets.Protector) {
	t.Helper()
	old := protector
	protector = func() (secrets.Protector, error) {
		if p == nil {
			return nil, secrets.ErrProtectorUnavailable
		}
		return p, nil
	}
	t.Cleanup(func() { protector = old })
}

func locked(t testing.TB, b []byte) *secrets.LockedBuffer {
	t.Helper()
	buf, err := secrets.NewLockedBuffer(bytes.Clone(b))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = buf.Release() })
	return buf
}

func testKey(t testing.TB) *Key {
	t.Helper()
	k, err := CreateKey(filepath.Join(t.TempDir(), "fleet.key"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k.Close() })
	return k
}

// testRecovery is a recovery identity's text and its public half.
func testRecovery(t testing.TB) (*secrets.LockedBuffer, string) {
	t.Helper()
	text, recipient, err := NewRecovery()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = text.Release() })
	return text, recipient
}

func ageEncrypt(t testing.TB, plain []byte, to ...age.Recipient) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := age.Encrypt(&out, to...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func ageDecrypt(sealed []byte, id age.Identity) ([]byte, error) {
	r, err := age.Decrypt(bytes.NewReader(sealed), id)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func TestKeyFile_RoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name   string
		set    func(*testing.T)
		header string
	}{
		{"this system's protector", func(*testing.T) {}, map[bool]string{true: keyFileSealed, false: keyFilePlain}[runtime.GOOS == "windows"]},
		{"no protector", func(t *testing.T) { useProtector(t, nil) }, keyFilePlain},
		{"a protector", func(t *testing.T) { useProtector(t, xorProtector{}) }, keyFileSealed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.set(t)
			path := filepath.Join(t.TempDir(), "sub", "fleet.key")
			k, err := CreateKey(path)
			if err != nil {
				t.Fatal(err)
			}
			defer k.Close()
			if id, err := hex.DecodeString(k.ID()); err != nil || len(id) != keyIDBytes || strings.ToLower(k.ID()) != k.ID() {
				t.Errorf("key id %q", k.ID())
			}
			if isLocked, reason := k.Locked(); !isLocked {
				t.Logf("the key's memory is not locked on this system: %s", reason)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasPrefix(raw, []byte(tc.header)) {
				t.Fatalf("the file starts %q, want %q", raw[:min(len(raw), 30)], tc.header)
			}
			if p := secrets.Probe(path); !p.Protected {
				t.Errorf("the key file is not owner-only: %s", p.Detail)
			}
			if strings.Contains(string(raw), k.recipient().String()) || strings.Contains(string(raw), k.ID()) {
				t.Error("the key file names the key's public half or id")
			}

			again, err := OpenKey(path)
			if err != nil {
				t.Fatal(err)
			}
			defer again.Close()
			if again.ID() != k.ID() || again.recipient().String() != k.recipient().String() {
				t.Fatal("the key read back is another key")
			}
			sealed := ageEncrypt(t, []byte("to the first"), k.recipient())
			if got, err := ageDecrypt(sealed, again); err != nil || string(got) != "to the first" {
				t.Fatalf("the key read back does not decrypt: %q, %v", got, err)
			}

			// A second key would orphan everything made under the first.
			if _, err := CreateKey(path); !errors.Is(err, ErrKeyExists) {
				t.Fatalf("a second CreateKey: %v, want ErrKeyExists", err)
			}
			if now, _ := os.ReadFile(path); !bytes.Equal(now, raw) {
				t.Fatal("the refused CreateKey changed the key file")
			}
		})
	}
}

// Without a protector the file holds the key in hex and nothing else.
func TestKeyFile_PlainIsTheKeyInHex(t *testing.T) {
	useProtector(t, nil)
	path := filepath.Join(t.TempDir(), "fleet.key")
	k, err := CreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	raw, _ := os.ReadFile(path)
	body, ok := strings.CutPrefix(string(raw), keyFilePlain)
	if !ok || len(body) != 65 || body[64] != '\n' {
		t.Fatalf("file is %q", raw)
	}
	scalar := unhex(t, body[:64])
	var pub [32]byte
	x25519Base(&pub, scalar)
	if pub != k.pub {
		t.Fatal("the hex in the file is not the key")
	}
}

func TestOpenKey_Refusals(t *testing.T) {
	useProtector(t, xorProtector{})
	dir := t.TempDir()
	if _, err := OpenKey(filepath.Join(dir, "none")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a missing file: %v, want fs.ErrNotExist", err)
	}
	seal := func(p []byte) string {
		c, _ := xorProtector{}.Protect(p)
		return keyFileSealed + base64.StdEncoding.EncodeToString(c) + "\n"
	}
	goodHex := strings.Repeat("0123456789abcdef", 4)
	for name, content := range map[string]string{
		"empty":                    "",
		"no header":                goodHex + "\n",
		"another version":          "tenant-fleet-key/2 plain\n" + goodHex + "\n",
		"header only":              keyFilePlain,
		"hex too short":            keyFilePlain + goodHex[:62] + "\n",
		"hex too long":             keyFilePlain + goodHex + "00\n",
		"not hex":                  keyFilePlain + goodHex[:63] + "g\n",
		"two lines of hex":         keyFilePlain + goodHex + "\n" + goodHex + "\n",
		"a space before the hex":   keyFilePlain + " " + goodHex[:63] + "\n",
		"sealed, not base64":       keyFileSealed + "!!!!\n",
		"sealed by something else": keyFileSealed + base64.StdEncoding.EncodeToString([]byte("who knows")) + "\n",
		"sealed, 31 bytes":         seal(make([]byte, 31)),
		"sealed, 33 bytes":         seal(make([]byte, 33)),
		"sealed, nothing":          keyFileSealed,
		"longer than any key file": keyFilePlain + strings.Repeat("a", maxKeyFile),
	} {
		path := filepath.Join(dir, "k")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		k, err := OpenKey(path)
		if err == nil {
			_ = k.Close()
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), goodHex[:20]) {
			t.Errorf("%s: the error quotes the file: %v", name, err)
		}
	}
	// And the two shapes that are right are read.
	for name, content := range map[string]string{
		"plain":          keyFilePlain + goodHex + "\n",
		"plain, no end":  keyFilePlain + goodHex,
		"plain, CR LF":   keyFilePlain + goodHex + "\r\n",
		"sealed":         seal(unhex(t, goodHex)),
		"sealed, no end": strings.TrimSuffix(seal(unhex(t, goodHex)), "\n"),
		"sealed, CR LF":  strings.TrimSuffix(seal(unhex(t, goodHex)), "\n") + "\r\n",
	} {
		path := filepath.Join(dir, "k")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		k, err := OpenKey(path)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		_ = k.Close()
	}
}

// A key sealed where there was a protector cannot be opened where there is
// none, and says so.
func TestOpenKey_SealedWithoutProtector(t *testing.T) {
	useProtector(t, xorProtector{})
	path := filepath.Join(t.TempDir(), "fleet.key")
	k, err := CreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = k.Close()
	useProtector(t, nil)
	if _, err := OpenKey(path); !errors.Is(err, secrets.ErrProtectorUnavailable) {
		t.Fatalf("got %v, want ErrProtectorUnavailable", err)
	}
}

// keyFromAge is a Key holding the same secret as a standard age identity.
func keyFromAge(t testing.TB, id *age.X25519Identity) *Key {
	t.Helper()
	k, err := ParseRecovery(locked(t, []byte(id.String())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k.Close() })
	return k
}

// Both ways round: what standard age encrypts to the public key, Key
// decrypts; what is encrypted to Key's recipient, the standard identity
// decrypts.
func TestKey_InteroperatesWithAge(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	k := keyFromAge(t, id)
	if k.recipient().String() != id.Recipient().String() {
		t.Fatalf("recipient %s, age says %s", k.recipient(), id.Recipient())
	}
	other, _ := age.GenerateX25519Identity()
	plain := bytes.Repeat([]byte("a machine's memory. "), 10_000) // several age chunks

	theirs := ageEncrypt(t, plain, id.Recipient())
	if got, err := ageDecrypt(theirs, k); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("Key did not decrypt what age encrypted: %v", err)
	}
	mine := ageEncrypt(t, plain, k.recipient())
	if got, err := ageDecrypt(mine, id); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("age did not decrypt what was encrypted to Key's recipient: %v", err)
	}
	// Second of two recipients, as the fleet key and the recovery key are.
	both := ageEncrypt(t, plain, other.Recipient(), k.recipient())
	if got, err := ageDecrypt(both, k); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("Key did not find its own stanza: %v", err)
	}
	// Not for this key at all.
	var none *age.NoIdentityMatchError
	if _, err := ageDecrypt(ageEncrypt(t, plain, other.Recipient()), k); !errors.As(err, &none) {
		t.Fatalf("a file for someone else: %v, want NoIdentityMatchError", err)
	}
	pass, _ := age.NewScryptRecipient("a passphrase")
	pass.SetWorkFactor(10)
	if _, err := ageDecrypt(ageEncrypt(t, plain, pass), k); !errors.As(err, &none) {
		t.Fatalf("a passphrase file: %v, want NoIdentityMatchError", err)
	}
}

// Stanza by stanza, Key answers as age's own identity does: the same file
// key, the same "not mine", an error for the same malformed ones.
func TestKey_UnwrapMatchesAge(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	k := keyFromAge(t, id)
	good, err := id.Recipient().Wrap(bytes.Repeat([]byte{7}, fileKeySize))
	if err != nil {
		t.Fatal(err)
	}
	g := good[0]
	lowOrder := base64.RawStdEncoding.EncodeToString(unhex(t, "e0eb7a7c3b41b8ae1656e3faf19fc46ada098deb9c32b1fd866205165f49b800"))
	for name, s := range map[string]*age.Stanza{
		"good":                 g,
		"another type":         {Type: "scrypt", Args: g.Args, Body: g.Body},
		"no argument":          {Type: "X25519", Body: g.Body},
		"two arguments":        {Type: "X25519", Args: []string{g.Args[0], g.Args[0]}, Body: g.Body},
		"argument not base64":  {Type: "X25519", Args: []string{"!" + g.Args[0][1:]}, Body: g.Body},
		"argument padded":      {Type: "X25519", Args: []string{g.Args[0] + "="}, Body: g.Body},
		"argument short":       {Type: "X25519", Args: []string{g.Args[0][:42]}, Body: g.Body},
		"argument long":        {Type: "X25519", Args: []string{g.Args[0] + "AA"}, Body: g.Body},
		"argument with a line": {Type: "X25519", Args: []string{g.Args[0][:20] + "\n" + g.Args[0][20:]}, Body: g.Body},
		"a low-order point":    {Type: "X25519", Args: []string{lowOrder}, Body: g.Body},
		"body short":           {Type: "X25519", Args: g.Args, Body: g.Body[:31]},
		"body long":            {Type: "X25519", Args: g.Args, Body: append(bytes.Clone(g.Body), 0)},
		"body empty":           {Type: "X25519", Args: g.Args},
		"body altered":         {Type: "X25519", Args: g.Args, Body: append([]byte{g.Body[0] ^ 1}, g.Body[1:]...)},
		"for another key":      {Type: "X25519", Args: []string{base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))}, Body: g.Body},
	} {
		want, wantErr := id.Unwrap([]*age.Stanza{s})
		got, err := k.Unwrap([]*age.Stanza{s})
		if !bytes.Equal(got, want) || (err == nil) != (wantErr == nil) ||
			errors.Is(err, age.ErrIncorrectIdentity) != errors.Is(wantErr, age.ErrIncorrectIdentity) {
			t.Errorf("%s:\n Key: %x, %v\n age: %x, %v", name, got, err, want, wantErr)
		}
	}
	// A malformed stanza for this key stops the search, as in age; a stanza
	// that is simply not ours does not.
	if got, err := k.Unwrap([]*age.Stanza{{Type: "scrypt"}, g}); err != nil || len(got) != fileKeySize {
		t.Errorf("a foreign stanza before ours: %x, %v", got, err)
	}
	if _, err := k.Unwrap(nil); !errors.Is(err, age.ErrIncorrectIdentity) {
		t.Errorf("no stanzas: %v", err)
	}
}

func TestKey_ClosedDecryptsNothing(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	k := keyFromAge(t, id)
	sealed := ageEncrypt(t, []byte("x"), id.Recipient())
	if err := k.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ageDecrypt(sealed, k); !errors.Is(err, secrets.ErrBufferReleased) {
		t.Fatalf("after Close: %v, want ErrBufferReleased", err)
	}
	if isLocked, reason := k.Locked(); isLocked || reason != "released" {
		t.Fatalf("Locked after Close: %v %q", isLocked, reason)
	}
	if err := k.Close(); err != nil {
		t.Fatalf("a second Close: %v", err)
	}
}

// A Key that ends up in a log line or an error prints no part of the secret.
func TestKey_PrintsNoSecret(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	k := keyFromAge(t, id)
	scalar := make([]byte, 32)
	bech32Decode(scalar, identityHRP, []byte(id.String()))
	verbs := "%v|%+v|%#v|%s|%x|%q" // not a constant: vet would object to %s of a struct
	out := fmt.Sprintf(verbs, k, k, k, k, k, k) + fmt.Sprintf(verbs, *k, *k, *k, *k, *k, *k)
	for _, secret := range []string{
		id.String(), strings.ToLower(id.String()), id.String()[16:],
		hex.EncodeToString(scalar), string(scalar), fmt.Sprint(scalar), fmt.Sprintf("%#v", scalar),
	} {
		if strings.Contains(out, secret) {
			t.Fatalf("a printed Key shows the secret: %s", out)
		}
	}
}

func TestRecovery(t *testing.T) {
	text, recipient := testRecovery(t)
	var shown string // the test's own copy, as an operator would write it down
	_ = text.With(func(p []byte) error { shown = string(p); return nil })
	if len(shown) != 74 || !strings.HasPrefix(shown, "AGE-SECRET-KEY-1") || strings.ToUpper(shown) != shown {
		t.Fatalf("the identity is not a line age writes: %q", shown)
	}
	// It is a standard age identity, and recipient is its public half.
	std, err := age.ParseX25519Identity(shown)
	if err != nil {
		t.Fatalf("age does not read the identity: %v", err)
	}
	if std.Recipient().String() != recipient {
		t.Fatalf("recipient %s, age derives %s", recipient, std.Recipient())
	}
	for name, typed := range map[string]string{
		"as shown":       shown,
		"with a newline": shown + "\n",
		"from Windows":   "  " + shown + "\r\n",
		"in lowercase":   strings.ToLower(shown),
	} {
		if err := ProveRecovery(locked(t, []byte(typed)), recipient); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	other, otherRecipient := testRecovery(t)
	if recipient == otherRecipient {
		t.Fatal("two recovery identities are the same")
	}
	for name, typed := range map[string]*secrets.LockedBuffer{
		"another identity":   other,
		"nothing":            locked(t, nil),
		"the public half":    locked(t, []byte(recipient)),
		"one character off":  locked(t, []byte(shown[:40]+map[bool]string{true: "P", false: "Q"}[shown[40] == 'Q']+shown[41:])),
		"cut short":          locked(t, []byte(shown[:70])),
		"two on one line":    locked(t, []byte(shown+" "+shown)),
		"with a comment":     locked(t, []byte("# recovery\n"+shown)),
		"a passphrase":       locked(t, []byte("correct horse battery staple")),
		"a released buffer":  func() *secrets.LockedBuffer { b := locked(t, []byte(shown)); _ = b.Release(); return b }(),
		"mixed case":         locked(t, []byte(strings.ToLower(shown[:30])+shown[30:])),
		"the fleet key file": locked(t, []byte(keyFilePlain+strings.Repeat("ab", 32))),
	} {
		err := ProveRecovery(typed, recipient)
		if name == "a released buffer" {
			if !errors.Is(err, secrets.ErrBufferReleased) {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if !errors.Is(err, ErrNotRecovery) {
			t.Errorf("%s: %v, want ErrNotRecovery", name, err)
		}
		if err != nil && strings.Contains(err.Error(), shown[20:40]) {
			t.Errorf("%s: the error quotes the identity: %v", name, err)
		}
	}
	if err := ProveRecovery(text, "age1notarecipient"); err == nil || errors.Is(err, ErrNotRecovery) {
		t.Errorf("a recipient that is not one: %v", err)
	}
	// The buffer the caller was handed is still whole after all that.
	_ = text.With(func(p []byte) error {
		if string(p) != shown {
			t.Error("proving changed the caller's buffer")
		}
		return nil
	})
}

func FuzzKeyFile(f *testing.F) {
	useProtector(f, xorProtector{})
	sealed, _ := xorProtector{}.Protect(bytes.Repeat([]byte{3}, 32))
	f.Add([]byte(keyFilePlain + strings.Repeat("ab", 32) + "\n"))
	f.Add([]byte(keyFileSealed + base64.StdEncoding.EncodeToString(sealed) + "\n"))
	f.Add([]byte(keyFilePlain))
	f.Add([]byte(keyFileSealed + "AAAA"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		buf, err := parseKeyFile(data)
		if err != nil {
			if buf != nil {
				t.Fatal("a buffer came back with an error")
			}
			return
		}
		defer buf.Release()
		_ = buf.With(func(p []byte) error {
			if len(p) != keySize {
				t.Fatalf("a key of %d bytes was accepted from %q", len(p), data)
			}
			return nil
		})
	})
}
