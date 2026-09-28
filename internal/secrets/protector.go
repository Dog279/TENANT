package secrets

import "errors"

// ErrProtectorUnavailable reports that this platform has no supported
// per-user data-at-rest encryption provider.
var ErrProtectorUnavailable = errors.New("per-user secret protector unavailable")

// Protector is the optional encryption boundary for especially high-value
// secret blobs. File and directory protection is always applied separately.
type Protector interface {
	Protect(plaintext []byte) ([]byte, error)
	Unprotect(ciphertext []byte) ([]byte, error)
	Name() string
}

// NewUserProtector returns the operating system's per-user data protector.
// On a Windows machine install (a service account or an elevated
// administrator) it seals to the machine instead, so the service and its
// operator can open each other's blobs.
func NewUserProtector() (Protector, error) { return newUserProtector() }
