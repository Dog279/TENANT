package selfupdate

// The project key (TEN-542). The main releases carry, beside cosign's bundle,
// an Ed25519 signature over checksums.txt made with a key the project holds
// as a repository secret; the public half is built into the release binary
// (version.ReleaseKeys). Checking it needs nothing on the machine. A build
// with no key (a fork, a development build) never looks at it.

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
)

const (
	// SignatureAsset is the release asset holding the project key's
	// signature over checksums.txt: standard base64 of the 64 bytes, one
	// line.
	SignatureAsset = "checksums.txt.sig"
	// MaxKeys is how many project keys a build carries: room to replace one
	// over two releases.
	MaxKeys = 3
	// MaxChecksums bounds the checksums.txt a signature is checked over.
	MaxChecksums = 1 << 20
	// MaxSignature is the longest signature text taken: the line and one "\n".
	MaxSignature = sigTextLen + 1

	keyTextLen = 44 // base64 of 32 bytes
	sigTextLen = 88 // base64 of 64 bytes
)

// ParseKeys reads the project keys a build carries: standard base64 of each
// 32-byte Ed25519 public key, comma-separated, at most MaxKeys, nothing else
// (no spaces, no newline, no empty entry, no key twice). "" is no key: nil,
// nil. Anything else that does not read is an error, never "no key".
func ParseKeys(s string) ([]ed25519.PublicKey, error) {
	if s == "" {
		return nil, nil
	}
	if len(s) > MaxKeys*(keyTextLen+1) {
		return nil, fmt.Errorf("project keys: %d characters, more than %d keys take", len(s), MaxKeys)
	}
	parts := strings.Split(s, ",")
	if len(parts) > MaxKeys {
		return nil, fmt.Errorf("project keys: %d of them, at most %d", len(parts), MaxKeys)
	}
	keys := make([]ed25519.PublicKey, 0, len(parts))
	for i, p := range parts {
		b, err := decodeExact(p, ed25519.PublicKeySize)
		if err != nil {
			return nil, fmt.Errorf("project key %d of %d: %w", i+1, len(parts), err)
		}
		k := ed25519.PublicKey(b)
		if slices.ContainsFunc(keys, func(o ed25519.PublicKey) bool { return o.Equal(k) }) {
			return nil, fmt.Errorf("project key %d of %d: the same key twice", i+1, len(parts))
		}
		keys = append(keys, k)
	}
	return keys, nil
}

// decodeExact is s as standard base64 of exactly n bytes, in its one
// canonical spelling.
func decodeExact(s string, n int) ([]byte, error) {
	enc := base64.StdEncoding
	if len(s) != enc.EncodedLen(n) {
		return nil, fmt.Errorf("%d characters, not the %d of %d bytes in standard base64", len(s), enc.EncodedLen(n), n)
	}
	b, err := enc.Strict().DecodeString(s)
	if err != nil || len(b) != n || enc.EncodeToString(b) != s {
		return nil, fmt.Errorf("not standard base64 of %d bytes", n)
	}
	return b, nil
}

// parseSignature is a signature asset's text: the 88 characters and at most
// one "\n" after them.
func parseSignature(text []byte) ([]byte, error) {
	if len(text) > MaxSignature {
		return nil, fmt.Errorf("%d bytes, more than a signature's %d", len(text), MaxSignature)
	}
	s := strings.TrimSuffix(string(text), "\n")
	return decodeExact(s, ed25519.SignatureSize)
}

// VerifyProject checks sig, the text of a release's checksums.txt.sig,
// over the exact bytes of checksums with each of keys, and returns the index
// of the key that verified. It fails when keys is empty, when checksums is
// over MaxChecksums, when sig does not read, and when no key verifies it.
// Both `tenant update` and a fleet child call it.
func VerifyProject(checksums, sig []byte, keys []ed25519.PublicKey) (int, error) {
	if len(keys) == 0 {
		return -1, errors.New("this build carries no project key to verify a project signature with")
	}
	if len(checksums) > MaxChecksums {
		return -1, fmt.Errorf("checksums.txt is larger than %d bytes", MaxChecksums)
	}
	s, err := parseSignature(sig)
	if err != nil {
		return -1, fmt.Errorf("the project signature (%s) is malformed: %w", SignatureAsset, err)
	}
	for i, k := range keys {
		if len(k) == ed25519.PublicKeySize && ed25519.Verify(k, checksums, s) {
			return i, nil
		}
	}
	return -1, fmt.Errorf("the project signature (%s) does not verify over checksums.txt with the project key this build carries", SignatureAsset)
}
