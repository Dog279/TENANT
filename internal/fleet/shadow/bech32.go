package shadow

// Bech32 (BIP 173), the encoding age writes its keys in, over byte slices.
// age's own encoder takes and returns Go strings, and a string that holds a
// secret key cannot be wiped. These two functions work between slices the
// caller owns (locked buffers, for a secret) and keep the data in no table
// or slice of their own: a few bits at a time pass through local variables.

const bech32Alphabet = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

// bech32Value maps a lowercase character to its five bits, or -1.
var bech32Value = func() (t [128]int8) {
	for i := range t {
		t[i] = -1
	}
	for i := range len(bech32Alphabet) {
		t[bech32Alphabet[i]] = int8(i)
	}
	return t
}()

func bech32Step(chk uint32, v byte) uint32 {
	top := chk >> 25
	chk = (chk&0x1ffffff)<<5 ^ uint32(v)
	for i, g := range [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3} {
		chk ^= g & -(top >> i & 1)
	}
	return chk
}

// bech32Start is the checksum state after the human-readable part, which is
// lowercase.
func bech32Start(hrp string) uint32 {
	chk := uint32(1)
	for i := range len(hrp) {
		chk = bech32Step(chk, hrp[i]>>5)
	}
	chk = bech32Step(chk, 0)
	for i := range len(hrp) {
		chk = bech32Step(chk, hrp[i]&31)
	}
	return chk
}

// bech32Len is the length of the encoding of n bytes under hrp.
func bech32Len(hrp string, n int) int { return len(hrp) + 1 + (n*8+4)/5 + 6 }

// bech32Encode writes the lowercase encoding of data under hrp to dst, which
// is bech32Len(hrp, len(data)) long.
func bech32Encode(dst []byte, hrp string, data []byte) {
	n := copy(dst, hrp)
	dst[n] = '1'
	n++
	chk := bech32Start(hrp)
	var acc uint32
	var bits uint
	for _, b := range data {
		acc = (acc<<8 | uint32(b)) & 0xfff
		bits += 8
		for bits >= 5 {
			bits -= 5
			v := byte(acc>>bits) & 31
			dst[n] = bech32Alphabet[v]
			n++
			chk = bech32Step(chk, v)
		}
	}
	if bits > 0 {
		v := byte(acc<<(5-bits)) & 31
		dst[n] = bech32Alphabet[v]
		n++
		chk = bech32Step(chk, v)
	}
	for range 6 {
		chk = bech32Step(chk, 0)
	}
	chk ^= 1
	for i := range 6 {
		dst[n+i] = bech32Alphabet[chk>>(5*(5-i))&31]
	}
}

// bech32Decode reads text as the encoding of len(dst) bytes under hrp and
// writes them to dst. text is all lowercase or all uppercase. It reports
// false, with dst in an unknown state, for anything else: another prefix,
// another length, a character outside the alphabet, a bad checksum, padding
// that is not zero.
func bech32Decode(dst []byte, hrp string, text []byte) bool {
	if len(text) != bech32Len(hrp, len(dst)) {
		return false
	}
	var upper, lower bool
	for _, c := range text {
		upper = upper || 'A' <= c && c <= 'Z'
		lower = lower || 'a' <= c && c <= 'z'
	}
	if upper && lower {
		return false
	}
	fold := func(c byte) byte {
		if 'A' <= c && c <= 'Z' {
			return c + 'a' - 'A'
		}
		return c
	}
	for i := range len(hrp) {
		if fold(text[i]) != hrp[i] {
			return false
		}
	}
	if text[len(hrp)] != '1' {
		return false
	}
	body := text[len(hrp)+1:]
	chk := bech32Start(hrp)
	var acc uint32
	var bits uint
	n := 0
	for i, c := range body {
		c = fold(c)
		if c >= 128 || bech32Value[c] < 0 {
			return false
		}
		v := byte(bech32Value[c])
		chk = bech32Step(chk, v)
		if i >= len(body)-6 {
			continue // the checksum itself
		}
		acc = (acc<<5 | uint32(v)) & 0xfff
		bits += 5
		if bits >= 8 {
			bits -= 8
			dst[n] = byte(acc >> bits)
			n++
		}
	}
	// What is left over is padding: less than one character's worth, all zero.
	return chk == 1 && n == len(dst) && bits < 5 && acc&(1<<bits-1) == 0
}
