package selfupdate

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
)

// testKey is a throwaway project key made from a fixed seed.
func testKey(seed byte) (ed25519.PrivateKey, string) {
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, ed25519.SeedSize))
	return priv, base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
}

func sign(priv ed25519.PrivateKey, msg []byte) []byte {
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg)) + "\n")
}

func TestParseKeysRefusals(t *testing.T) {
	_, a := testKey(1)
	_, b := testKey(2)
	_, c := testKey(3)
	_, d := testKey(4)
	raw, _ := base64.StdEncoding.DecodeString(a)
	for name, in := range map[string]string{
		"four keys":                  a + "," + b + "," + c + "," + d,
		"an empty entry":             a + ",," + b,
		"a trailing comma":           a + ",",
		"a leading comma":            "," + a,
		"a space after the comma":    a + ", " + b,
		"a trailing newline":         a + "\n",
		"a newline inside":           a[:20] + "\n" + a[20:],
		"no padding":                 strings.TrimRight(a, "="),
		"URL-safe alphabet":          base64.URLEncoding.EncodeToString(append([]byte{0xfb, 0xff}, raw[2:]...)),
		"31 bytes":                   base64.StdEncoding.EncodeToString(raw[:31]),
		"33 bytes":                   base64.StdEncoding.EncodeToString(append(raw, 0)),
		"64 bytes (a private key)":   base64.StdEncoding.EncodeToString(append(raw, raw...)),
		"hex, not base64":            strings.Repeat("ab", 32),
		"non-canonical padding bits": a[:42] + "B=",
		"the same key twice":         a + "," + a,
		"very long":                  strings.Repeat("A", 1<<20),
		"a NUL":                      a[:43] + "\x00",
	} {
		if keys, err := ParseKeys(in); err == nil {
			t.Errorf("%s: parsed %d keys", name, len(keys))
		}
	}
	if keys, err := ParseKeys(""); err != nil || keys != nil {
		t.Errorf("no key: %v, %v", keys, err)
	}
	if keys, err := ParseKeys(a + "," + b + "," + c); err != nil || len(keys) != 3 {
		t.Errorf("three keys: %d, %v", len(keys), err)
	}
}

func TestVerifyProjectRefusals(t *testing.T) {
	priv1, pub1 := testKey(1)
	priv2, pub2 := testKey(2)
	priv3, _ := testKey(3)
	one, _ := ParseKeys(pub1)
	two, _ := ParseKeys(pub1 + "," + pub2)
	sums := []byte(strings.Repeat("ab", 32) + "  tenant_1.0.0_linux_x86_64.tar.gz\n")
	good := sign(priv1, sums)
	flipped := bytes.Clone(good)
	flipped[10] ^= 'A' ^ 'B'
	if flipped[10] == good[10] {
		flipped[10] = 'C'
	}

	for name, tc := range map[string]struct {
		sums, sig []byte
		keys      []ed25519.PublicKey
		want      string
	}{
		"tampered checksums":           {append(bytes.Clone(sums), "00  extra\n"...), good, one, "does not verify"},
		"a signature by another key":   {sums, sign(priv3, sums), two, "does not verify"},
		"a signature for another file": {sums, sign(priv1, []byte("another file")), one, "does not verify"},
		"one character changed":        {sums, flipped, one, ""},
		"no signature":                 {sums, nil, one, "malformed"},
		"no newline but CRLF":          {sums, append(bytes.Clone(good[:len(good)-1]), "\r\n"...), one, "malformed"},
		"two newlines":                 {sums, append(bytes.Clone(good), '\n'), one, "malformed"},
		"trailing junk":                {sums, append(bytes.Clone(good[:len(good)-1]), " x"...), one, "malformed"},
		"leading space":                {sums, append([]byte(" "), good...), one, "malformed"},
		"too long":                     {sums, bytes.Repeat([]byte("A"), 4096), one, "malformed"},
		"too short":                    {sums, good[:40], one, "malformed"},
		"raw bytes, not base64":        {sums, ed25519.Sign(priv1, sums), one, "malformed"},
		"no key at all":                {sums, good, nil, "no project key"},
		"a key of the wrong length":    {sums, good, []ed25519.PublicKey{one[0][:31]}, "does not verify"},
		"checksums too large":          {make([]byte, MaxChecksums+1), good, one, "larger than"},
	} {
		if i, err := VerifyProject(tc.sums, tc.sig, tc.keys); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: key %d, %v; want a refusal saying %q", name, i, err, tc.want)
		}
	}

	if i, err := VerifyProject(sums, good, one); err != nil || i != 0 {
		t.Errorf("a good signature: key %d, %v", i, err)
	}
	if i, err := VerifyProject(sums, bytes.TrimSuffix(good, []byte("\n")), one); err != nil || i != 0 {
		t.Errorf("without its newline: key %d, %v", i, err)
	}
	// A key being replaced: release N carries both, signed with the new one.
	if i, err := VerifyProject(sums, sign(priv2, sums), two); err != nil || i != 1 {
		t.Errorf("signed by the second key of two: key %d, %v", i, err)
	}
}

// FuzzProjectKeyText: the key list a build carries and the signature text a
// release (or a master) supplies. Whatever parses is exactly what canonical
// encoding would print: no junk, no alternative spellings.
func FuzzProjectKeyText(f *testing.F) {
	priv, pub := testKey(1)
	_, pub2 := testKey(2)
	sig := sign(priv, []byte("x"))
	for _, s := range []struct{ keys, sig string }{
		{pub, string(sig)},
		{pub + "," + pub2, string(sig[:len(sig)-1])},
		{pub + ",", string(sig) + "\n"},
		{pub + "\n", "\r\n" + string(sig)},
		{strings.TrimRight(pub, "="), strings.ReplaceAll(string(sig), "+", "-")},
		{pub + "," + pub + "," + pub + "," + pub, strings.Repeat("A", 89)},
		{" " + pub, strings.Repeat("=", 88)},
		{"", "\x00"},
		{"Ａ" + pub[3:], string(sig[:87]) + "é"},
		{strings.Repeat(",", 200), strings.Repeat("A", 1<<12)},
	} {
		f.Add(s.keys, []byte(s.sig))
	}
	f.Fuzz(func(t *testing.T, keys string, sigText []byte) {
		if ks, err := ParseKeys(keys); err == nil {
			if len(ks) > MaxKeys || (keys == "") != (len(ks) == 0) {
				t.Fatalf("%d keys from %q", len(ks), keys)
			}
			var enc []string
			for _, k := range ks {
				if len(k) != ed25519.PublicKeySize {
					t.Fatalf("a key of %d bytes", len(k))
				}
				enc = append(enc, base64.StdEncoding.EncodeToString(k))
			}
			if strings.Join(enc, ",") != keys {
				t.Fatalf("%q parsed, but is not the canonical %q", keys, strings.Join(enc, ","))
			}
		}
		if s, err := parseSignature(sigText); err == nil {
			if len(s) != ed25519.SignatureSize || base64.StdEncoding.EncodeToString(s) != string(bytes.TrimSuffix(sigText, []byte("\n"))) {
				t.Fatalf("%q parsed as %d bytes", sigText, len(s))
			}
		}
	})
}
