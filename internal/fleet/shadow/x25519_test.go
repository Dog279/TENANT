package shadow

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"

	"filippo.io/age"
)

func unhex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// stdX25519 is the standard library's answer, and whether it gave one.
func stdX25519(scalar, point []byte) ([]byte, bool) {
	priv, err := ecdh.X25519().NewPrivateKey(scalar)
	if err != nil {
		return nil, false
	}
	pub, err := ecdh.X25519().NewPublicKey(point)
	if err != nil {
		return nil, false
	}
	out, err := priv.ECDH(pub)
	return out, err == nil
}

func TestX25519_RFC7748Vectors(t *testing.T) {
	for _, v := range []struct{ scalar, point, want string }{
		{"a546e36bf0527c9d3b16154b82465edd62144c0ac1fc5a18506a2244ba449ac4",
			"e6db6867583030db3594c1a424b15f7c726624ec26b3353b10a903a6d0ab1c4c",
			"c3da55379de9c6908e94ea4df28d084f32eccf03491c71f754b4075577a28552"},
		{"4b66e9d4d1b4673c5ad22691957d6af5c11b6421e0ea01d42ca4169e7918ba0d",
			"e5210f12786811d3f4b7959d0538ae2c31dbe7106fc03c3efc4cd549c715a493",
			"95cbde9476e8907d7aade45cb4b873f88b595a68799fa152e6f8f7647aac7957"},
		// Alice's and Bob's keys from section 6.1.
		{"77076d0a7318a57d3c16c17251b26645df4c2f87ebc0992ab177fba51db92c2a",
			"de9edb7d7b7dc1b4d35b61c2ece435373f8343c85b78674dadfc7e146f882b4f",
			"4a5d9d5ba4ce2de1728e3bf480350f25e07e21c947d19e3376f09b3c1e161742"},
	} {
		var out, point [32]byte
		copy(point[:], unhex(t, v.point))
		if !x25519(&out, unhex(t, v.scalar), &point) {
			t.Fatalf("scalar %s: reported a zero result", v.scalar)
		}
		if got := hex.EncodeToString(out[:]); got != v.want {
			t.Errorf("scalar %s:\n got %s\nwant %s", v.scalar, got, v.want)
		}
	}
}

// The RFC's iterated test: the result of each round is the next scalar.
func TestX25519_RFC7748Iterated(t *testing.T) {
	k, u := [32]byte{9}, [32]byte{9}
	for i := 1; i <= 1000; i++ {
		var out [32]byte
		x25519(&out, k[:], &u)
		u, k = k, out
		switch i {
		case 1:
			if got := hex.EncodeToString(k[:]); got != "422c8e7a6227d7bca1350b3e2bb7279f7897b87bb6854b783c60e80311ae3079" {
				t.Fatalf("after 1 round: %s", got)
			}
		case 1000:
			if got := hex.EncodeToString(k[:]); got != "684cf59ba83309552800ef566f2f4d3c1c3887c49360e3875f2eb94d99532c51" {
				t.Fatalf("after 1000 rounds: %s", got)
			}
		}
	}
}

func TestX25519_MatchesStandardLibrary(t *testing.T) {
	for i := range 300 {
		scalar, point := make([]byte, 32), make([]byte, 32)
		_, _ = rand.Read(scalar)
		_, _ = rand.Read(point)
		switch i % 4 {
		case 1: // a point with the top bit set, which X25519 ignores
			point[31] |= 0x80
		case 2: // a coordinate at or above the field's prime
			for j := range point {
				point[j] = 0xff
			}
			point[0] -= byte(i % 19)
		}
		var out, p [32]byte
		copy(p[:], point)
		ok := x25519(&out, scalar, &p)
		want, wantOK := stdX25519(scalar, point)
		if ok != wantOK || (ok && !bytes.Equal(out[:], want)) {
			t.Fatalf("scalar %x point %x:\n got %x (%v)\nwant %x (%v)", scalar, point, out, ok, want, wantOK)
		}
	}
}

// Points of low order give the all-zero result, which is refused.
func TestX25519_RefusesLowOrderPoints(t *testing.T) {
	scalar := make([]byte, 32)
	_, _ = rand.Read(scalar)
	for _, p := range []string{
		"0000000000000000000000000000000000000000000000000000000000000000",
		"0100000000000000000000000000000000000000000000000000000000000000",
		"e0eb7a7c3b41b8ae1656e3faf19fc46ada098deb9c32b1fd866205165f49b800",
		"5f9c95bca3508c24b1d0b1559c83ef5b04445cc4581c8e86d8224eddd09f1157",
		"ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
	} {
		var out, point [32]byte
		copy(point[:], unhex(t, p))
		if x25519(&out, scalar, &point) {
			t.Errorf("point %s: accepted, result %x", p, out)
		}
		if _, ok := stdX25519(scalar, point[:]); ok {
			t.Errorf("point %s: the standard library accepts it; this is not a low-order point", p)
		}
	}
}

func FuzzX25519(f *testing.F) {
	f.Add(make([]byte, 32), make([]byte, 32))
	f.Add(bytes.Repeat([]byte{0xff}, 32), bytes.Repeat([]byte{0xff}, 32))
	f.Add(unhex(f, "a546e36bf0527c9d3b16154b82465edd62144c0ac1fc5a18506a2244ba449ac4"),
		unhex(f, "e6db6867583030db3594c1a424b15f7c726624ec26b3353b10a903a6d0ab1c4c"))
	f.Add(bytes.Repeat([]byte{1}, 32), unhex(f, "e0eb7a7c3b41b8ae1656e3faf19fc46ada098deb9c32b1fd866205165f49b800"))
	f.Fuzz(func(t *testing.T, scalar, point []byte) {
		if len(scalar) != 32 || len(point) != 32 {
			return
		}
		var out, p [32]byte
		copy(p[:], point)
		ok := x25519(&out, scalar, &p)
		want, wantOK := stdX25519(scalar, point)
		if ok != wantOK || (ok && !bytes.Equal(out[:], want)) {
			t.Fatalf("scalar %x point %x:\n got %x (%v)\nwant %x (%v)", scalar, point, out, ok, want, wantOK)
		}
	})
}

func TestBech32_MatchesAge(t *testing.T) {
	for range 50 {
		id, err := age.GenerateX25519Identity()
		if err != nil {
			t.Fatal(err)
		}
		secret := id.String() // AGE-SECRET-KEY-1…, uppercase
		scalar := make([]byte, 32)
		if !bech32Decode(scalar, identityHRP, []byte(secret)) {
			t.Fatalf("did not read age's own identity %q", secret)
		}
		back := make([]byte, bech32Len(identityHRP, 32))
		bech32Encode(back, identityHRP, scalar)
		if got := strings.ToUpper(string(back)); got != secret {
			t.Fatalf("identity written as %q, age writes %q", got, secret)
		}
		if lower := []byte(strings.ToLower(secret)); !bech32Decode(make([]byte, 32), identityHRP, lower) {
			t.Fatal("the lowercase spelling was refused")
		}
		var pub [32]byte
		x25519Base(&pub, scalar)
		text := make([]byte, bech32Len(recipientHRP, 32))
		bech32Encode(text, recipientHRP, pub[:])
		if string(text) != id.Recipient().String() {
			t.Fatalf("recipient written as %q, age writes %q", text, id.Recipient())
		}
	}
}

func TestBech32_Refusals(t *testing.T) {
	id, _ := age.GenerateX25519Identity()
	good := id.String()
	if !bech32Decode(make([]byte, 32), identityHRP, []byte(good)) {
		t.Fatal("the unchanged identity was refused")
	}
	flip := func(i int) string { // another character of the alphabet at i
		b := []byte(good)
		if b[i] == 'Q' {
			b[i] = 'P'
		} else {
			b[i] = 'Q'
		}
		return string(b)
	}
	for name, text := range map[string]string{
		"empty":              "",
		"one short":          good[:len(good)-1],
		"one long":           good + "Q",
		"mixed case":         strings.ToLower(good[:20]) + good[20:],
		"a recipient":        id.Recipient().String(),
		"another prefix":     "AGE-SECRET-KEX-" + good[15:],
		"no separator":       strings.Replace(good, "KEY-1", "KEY-Q", 1),
		"data changed":       flip(30),
		"checksum changed":   flip(len(good) - 1),
		"outside alphabet":   good[:30] + "B" + good[31:],
		"not ASCII":          good[:30] + "\xc3" + good[31:],
		"a space inside":     good[:30] + " " + good[31:],
		"line end kept":      good[:len(good)-1] + "\n",
		"swapped characters": good[:30] + string(good[31]) + string(good[30]) + good[32:],
	} {
		if text == good {
			continue // the two swapped characters were the same one
		}
		if bech32Decode(make([]byte, 32), identityHRP, []byte(text)) {
			t.Errorf("%s: %q was accepted", name, text)
		}
	}
}

// The last data character of a 32-byte key carries four bits of padding,
// which must be zero. A checksum made over non-zero padding is still refused.
func TestBech32_RefusesNonZeroPadding(t *testing.T) {
	scalar := bytes.Repeat([]byte{0x5a}, 32)
	text := make([]byte, bech32Len(identityHRP, 32))
	bech32Encode(text, identityHRP, scalar)
	last := len(text) - 7 // the last data character
	v := bech32Value[text[last]]
	body := text[len(identityHRP)+1 : last]
	for pad := byte(1); pad < 16; pad++ {
		chk := bech32Start(identityHRP)
		for _, c := range body {
			chk = bech32Step(chk, byte(bech32Value[c]))
		}
		chk = bech32Step(chk, byte(v)|pad)
		for range 6 {
			chk = bech32Step(chk, 0)
		}
		chk ^= 1
		forged := append([]byte{}, text...)
		forged[last] = bech32Alphabet[byte(v)|pad]
		for i := range 6 {
			forged[last+1+i] = bech32Alphabet[chk>>(5*(5-i))&31]
		}
		if bech32Decode(make([]byte, 32), identityHRP, forged) {
			t.Fatalf("padding %04b was accepted", pad)
		}
	}
}

func FuzzBech32Decode(f *testing.F) {
	id, _ := age.GenerateX25519Identity()
	f.Add([]byte(id.String()))
	f.Add([]byte(strings.ToLower(id.String())))
	f.Add([]byte(id.Recipient().String()))
	f.Add([]byte("AGE-SECRET-KEY-1"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, text []byte) {
		scalar := make([]byte, 32)
		if !bech32Decode(scalar, identityHRP, text) {
			return
		}
		// What is accepted is one spelling of one key, and age reads it the same.
		back := make([]byte, bech32Len(identityHRP, 32))
		bech32Encode(back, identityHRP, scalar)
		if !strings.EqualFold(string(back), string(text)) {
			t.Fatalf("%q was read as the key that is written %q", text, back)
		}
		theirs, err := age.ParseX25519Identity(strings.ToUpper(string(text)))
		if err != nil {
			t.Fatalf("%q was accepted and age refuses it: %v", text, err)
		}
		var pub [32]byte
		x25519Base(&pub, scalar)
		mine := make([]byte, bech32Len(recipientHRP, 32))
		bech32Encode(mine, recipientHRP, pub[:])
		if string(mine) != theirs.Recipient().String() {
			t.Fatalf("%q: public key %s, age says %s", text, mine, theirs.Recipient())
		}
	})
}
