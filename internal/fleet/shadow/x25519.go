package shadow

// X25519 (RFC 7748) with the secret scalar read where it lies.
//
// crypto/ecdh, and golang.org/x/crypto/curve25519 on top of it, copy the
// scalar into a private key object on the Go heap, which cannot be wiped and
// can be swapped out. A key that lives in a locked buffer would leave a copy
// behind at every decryption. This ladder reads the scalar one bit at a time
// from the caller's slice and keeps no copy of it: what it holds on the stack
// are field elements, which are multiples of the peer's point.
//
// The arithmetic is TweetNaCl's (public domain): sixteen 16-bit limbs in
// int64, no branch and no table lookup on secret data. It is about ten times
// slower than the standard library's, which is a millisecond per decryption.
// The tests compare it with crypto/ecdh on the RFC's vectors and on random
// and fuzzed inputs.

type fe [16]int64

var fe121665 = fe{0xDB41, 1}

func feCarry(o *fe) {
	for i := range o {
		o[i] += 1 << 16
		c := o[i] >> 16
		if i < 15 {
			o[i+1] += c - 1
		} else {
			o[0] += 38 * (c - 1)
		}
		o[i] -= c << 16
	}
}

// feSwap exchanges p and q when b is 1 and leaves them when b is 0.
func feSwap(p, q *fe, b int64) {
	c := ^(b - 1)
	for i := range p {
		t := c & (p[i] ^ q[i])
		p[i] ^= t
		q[i] ^= t
	}
}

func fePack(o []byte, n *fe) {
	var m fe
	t := *n
	feCarry(&t)
	feCarry(&t)
	feCarry(&t)
	for range 2 {
		m[0] = t[0] - 0xffed
		for i := 1; i < 15; i++ {
			m[i] = t[i] - 0xffff - ((m[i-1] >> 16) & 1)
			m[i-1] &= 0xffff
		}
		m[15] = t[15] - 0x7fff - ((m[14] >> 16) & 1)
		b := (m[15] >> 16) & 1
		m[14] &= 0xffff
		feSwap(&t, &m, 1-b)
	}
	for i := range t {
		o[2*i] = byte(t[i])
		o[2*i+1] = byte(t[i] >> 8)
	}
}

func feUnpack(o *fe, n []byte) {
	for i := range o {
		o[i] = int64(n[2*i]) + int64(n[2*i+1])<<8
	}
	o[15] &= 0x7fff
}

func feAdd(o, a, b *fe) {
	for i := range o {
		o[i] = a[i] + b[i]
	}
}

func feSub(o, a, b *fe) {
	for i := range o {
		o[i] = a[i] - b[i]
	}
}

func feMul(o, a, b *fe) {
	var t [31]int64
	for i := range a {
		for j := range b {
			t[i+j] += a[i] * b[j]
		}
	}
	for i := range 15 {
		t[i] += 38 * t[i+16]
	}
	copy(o[:], t[:16])
	feCarry(o)
	feCarry(o)
}

func feInvert(o, in *fe) {
	c := *in
	for a := 253; a >= 0; a-- {
		feMul(&c, &c, &c)
		if a != 2 && a != 4 {
			feMul(&c, &c, in)
		}
	}
	*o = c
}

// x25519 writes scalar times point to out. scalar is 32 bytes, read in place
// and clamped as it is read. It reports false when the result is all zero,
// which a point of low order produces and no honest peer sends.
func x25519(out *[32]byte, scalar []byte, point *[32]byte) bool {
	var x, a, b, c, d, e, f fe
	feUnpack(&x, point[:])
	b = x
	a[0], d[0] = 1, 1
	for i := 254; i >= 0; i-- {
		r := int64(scalar[i>>3]>>(uint(i)&7)) & 1
		// The clamp: the three low bits are clear and bit 254 is set. i is
		// the loop counter, not secret.
		if i < 3 {
			r = 0
		} else if i == 254 {
			r = 1
		}
		feSwap(&a, &b, r)
		feSwap(&c, &d, r)
		feAdd(&e, &a, &c)
		feSub(&a, &a, &c)
		feAdd(&c, &b, &d)
		feSub(&b, &b, &d)
		feMul(&d, &e, &e)
		feMul(&f, &a, &a)
		feMul(&a, &c, &a)
		feMul(&c, &b, &e)
		feAdd(&e, &a, &c)
		feSub(&a, &a, &c)
		feMul(&b, &a, &a)
		feSub(&c, &d, &f)
		feMul(&a, &c, &fe121665)
		feAdd(&a, &a, &d)
		feMul(&c, &c, &a)
		feMul(&a, &d, &f)
		feMul(&d, &b, &x)
		feMul(&b, &e, &e)
		feSwap(&a, &b, r)
		feSwap(&c, &d, r)
	}
	feInvert(&c, &c)
	feMul(&a, &a, &c)
	fePack(out[:], &a)
	var nonzero byte
	for _, v := range out {
		nonzero |= v
	}
	return nonzero != 0
}

// x25519Base is the public key of a scalar.
func x25519Base(out *[32]byte, scalar []byte) {
	x25519(out, scalar, &[32]byte{9})
}
