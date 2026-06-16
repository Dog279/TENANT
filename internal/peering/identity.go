package peering

import (
	"crypto/rand"
	"fmt"
)

// NewInstanceID mints a stable per-installation identity (a random UUIDv4
// string). It is generated once at first launch, persisted in config.json by
// the caller, exchanged at pairing, and later reused as the bus Origin
// (TEN-189). Pure stdlib (crypto/rand) — no new dependency.
func NewInstanceID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("peering: mint instance_id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// newSecret mints a high-entropy URL-safe peering token (32 bytes → 43 chars
// base64url, no padding). Used as the bearer a dialing peer presents and the
// serving peer accepts.
func newSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("peering: mint secret: %w", err)
	}
	return base64URL(b[:]), nil
}
