package shadow

import (
	"bytes"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"filippo.io/age"
	"golang.org/x/crypto/chacha20poly1305"

	"tenant/internal/secrets"
)

// Errors of the fleet key and the recovery identity.
var (
	// ErrKeyExists is CreateKey finding a key file already there. A second
	// key would orphan every snapshot made under the first.
	ErrKeyExists = errors.New("shadow: a fleet key already exists")
	// ErrKeyFile is a key file that cannot be read as one.
	ErrKeyFile = errors.New("shadow: the fleet key file is not valid")
	// ErrNotRecovery is text that is not an age secret key, or a recovery
	// identity that does not open what was encrypted to the recovery key.
	ErrNotRecovery = errors.New("shadow: not the recovery identity")
)

const (
	keySize    = 32
	keyIDBytes = 16
	// The human-readable parts of age's X25519 keys.
	recipientHRP = "age"
	identityHRP  = "age-secret-key-"
	x25519Label  = "age-encryption.org/v1/X25519"
	fileKeySize  = 16

	// The key file: one of these two lines, then the key. With a protector
	// the key is sealed by it and written in base64; without one it is
	// written in hex and the file's owner-only mode is all that guards it.
	keyFileSealed = "tenant-fleet-key/1 sealed\n"
	keyFilePlain  = "tenant-fleet-key/1 plain\n"
	maxKeyFile    = 4096
)

// Key is an X25519 secret key held in locked memory: the fleet key, for as
// long as Tenant runs, or the recovery identity, for the length of one
// decryption. It is an age.Identity that age's own X25519 recipient encrypts
// to, and it never exists as a Go string or on the Go heap.
//
// What a decryption does put on the heap is that one file's secrets: the
// shared secret, the key that wraps the file key, and the file key, the last
// of which age itself holds for as long as it reads the file. They open that
// one file and nothing else.
type Key struct {
	buf      *secrets.LockedBuffer
	pub      [keySize]byte
	id       string
	to       *age.X25519Recipient
	recovery bool
}

// newKey takes ownership of buf, which holds the 32-byte scalar.
func newKey(buf *secrets.LockedBuffer, recovery bool) (*Key, error) {
	k := &Key{buf: buf, recovery: recovery}
	if err := buf.With(func(p []byte) error {
		if len(p) != keySize {
			return ErrKeyFile
		}
		x25519Base(&k.pub, p)
		return nil
	}); err != nil {
		_ = buf.Release()
		return nil, err
	}
	sum := sha256.Sum256(append([]byte("tenant fleet key id v1\x00"), k.pub[:]...))
	k.id = hex.EncodeToString(sum[:keyIDBytes])
	text := make([]byte, bech32Len(recipientHRP, keySize))
	bech32Encode(text, recipientHRP, k.pub[:])
	to, err := age.ParseX25519Recipient(string(text))
	if err != nil {
		_ = buf.Release()
		return nil, err
	}
	k.to = to
	return k, nil
}

// ID identifies the key without revealing it or its public half: snapshots
// carry it in the clear, on machines that must not be able to encrypt to the
// fleet.
func (k *Key) ID() string { return k.id }

// recipient is the public half, the standard age recipient snapshots are
// encrypted to. It is not exported: whoever has the fleet key's can make a
// snapshot the master will accept, so it is derived when the key is opened
// and written nowhere.
func (k *Key) recipient() *age.X25519Recipient { return k.to }

// Locked reports whether the key's memory is locked into RAM, and if not,
// why not, as secrets.LockedBuffer.Locked does.
func (k *Key) Locked() (bool, string) { return k.buf.Locked() }

// Close zeroes the key and gives its memory back. The Key decrypts nothing
// afterwards.
func (k *Key) Close() error { return k.buf.Release() }

// Unwrap makes Key an age.Identity for files encrypted to its public half.
func (k *Key) Unwrap(stanzas []*age.Stanza) ([]byte, error) {
	for _, s := range stanzas {
		fileKey, err := k.unwrap(s)
		if errors.Is(err, age.ErrIncorrectIdentity) {
			continue
		}
		return fileKey, err
	}
	return nil, age.ErrIncorrectIdentity
}

func (k *Key) unwrap(s *age.Stanza) ([]byte, error) {
	if s.Type != "X25519" {
		return nil, age.ErrIncorrectIdentity
	}
	if len(s.Args) != 1 {
		return nil, errors.New("invalid X25519 recipient block")
	}
	var theirs [keySize]byte
	if len(s.Args[0]) != base64.RawStdEncoding.EncodedLen(keySize) {
		return nil, errors.New("invalid X25519 recipient block")
	}
	if n, err := base64.RawStdEncoding.Strict().Decode(theirs[:], []byte(s.Args[0])); err != nil || n != keySize {
		return nil, errors.New("invalid X25519 recipient block")
	}
	var shared [keySize]byte
	defer clear(shared[:])
	var ok bool
	if err := k.buf.With(func(p []byte) error {
		ok = x25519(&shared, p, &theirs)
		return nil
	}); err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("invalid X25519 recipient")
	}
	salt := append(theirs[:len(theirs):len(theirs)], k.pub[:]...)
	wrap, err := hkdf.Key(sha256.New, shared[:], salt, x25519Label, chacha20poly1305.KeySize)
	if err != nil {
		return nil, err
	}
	defer clear(wrap)
	aead, err := chacha20poly1305.New(wrap)
	if err != nil {
		return nil, err
	}
	if len(s.Body) != fileKeySize+aead.Overhead() {
		return nil, errors.New("invalid X25519 recipient block: incorrect file key size")
	}
	fileKey, err := aead.Open(nil, make([]byte, chacha20poly1305.NonceSize), s.Body, nil)
	if err != nil {
		return nil, age.ErrIncorrectIdentity
	}
	return fileKey, nil
}

// CreateKey makes the fleet key and stores it at path: sealed by the
// operating system's secret protector where there is one (Windows), and as an
// owner-only file otherwise. The key is made in locked memory and is written
// from there. It fails with ErrKeyExists, and changes nothing, when path
// already holds a key.
func CreateKey(path string) (*Key, error) {
	buf, err := secrets.NewLockedBuffer(make([]byte, keySize))
	if err != nil {
		return nil, err
	}
	if err := buf.With(func(p []byte) error {
		_, err := rand.Read(p)
		return err
	}); err != nil {
		_ = buf.Release()
		return nil, err
	}
	k, err := newKey(buf, false)
	if err != nil {
		return nil, err
	}
	if err := k.store(path); err != nil {
		_ = k.Close()
		return nil, err
	}
	return k, nil
}

// protector is the operating system's, replaced in tests.
var protector = secrets.NewUserProtector

func (k *Key) store(path string) error {
	p, err := protector()
	if err != nil && !errors.Is(err, secrets.ErrProtectorUnavailable) {
		return err
	}
	// The file is put together in locked memory as well: without a protector
	// it holds the key itself.
	size := len(keyFilePlain) + hex.EncodedLen(keySize) + 1
	file, err := secrets.NewLockedBuffer(make([]byte, size))
	if err != nil {
		return err
	}
	defer file.Release()
	var created bool
	err = file.With(func(f []byte) error {
		return k.buf.With(func(scalar []byte) error {
			if p != nil {
				sealed, err := p.Protect(scalar)
				if err != nil {
					return fmt.Errorf("shadow: seal the fleet key: %w", err)
				}
				text := append([]byte(keyFileSealed), base64.StdEncoding.AppendEncode(nil, sealed)...)
				created, err = secrets.CreateExclusive(path, append(text, '\n'))
				return err
			}
			n := copy(f, keyFilePlain)
			hex.Encode(f[n:], scalar)
			f[len(f)-1] = '\n'
			created, err = secrets.CreateExclusive(path, f)
			return err
		})
	})
	if err != nil {
		return err
	}
	if !created {
		return ErrKeyExists
	}
	return nil
}

// OpenKey unlocks the fleet key at path into locked memory.
func OpenKey(path string) (*Key, error) {
	if err := secrets.Harden(path); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// The file is read straight into locked memory, never through the heap:
	// without a protector it holds the key itself.
	file, err := secrets.NewLockedBuffer(make([]byte, maxKeyFile))
	if err != nil {
		return nil, err
	}
	defer file.Release()
	var buf *secrets.LockedBuffer
	err = file.With(func(p []byte) error {
		n, err := io.ReadFull(f, p)
		if err == nil {
			return ErrKeyFile // longer than any key file
		}
		if err != io.ErrUnexpectedEOF && err != io.EOF {
			return err
		}
		buf, err = parseKeyFile(p[:n])
		return err
	})
	if err != nil {
		return nil, err
	}
	return newKey(buf, false)
}

// parseKeyFile returns the key a key file holds, in a locked buffer. p is
// locked memory.
func parseKeyFile(p []byte) (*secrets.LockedBuffer, error) {
	if body, ok := bytes.CutPrefix(p, []byte(keyFilePlain)); ok {
		body = bytes.TrimSuffix(bytes.TrimSuffix(body, []byte("\n")), []byte("\r"))
		if len(body) != hex.EncodedLen(keySize) {
			return nil, ErrKeyFile
		}
		buf, err := secrets.NewLockedBuffer(make([]byte, keySize))
		if err != nil {
			return nil, err
		}
		if err := buf.With(func(scalar []byte) error {
			if _, err := hex.Decode(scalar, body); err != nil {
				return ErrKeyFile // not hex.Decode's error: it quotes the byte it choked on
			}
			return nil
		}); err != nil {
			_ = buf.Release()
			return nil, err
		}
		return buf, nil
	}
	body, ok := bytes.CutPrefix(p, []byte(keyFileSealed))
	if !ok {
		return nil, ErrKeyFile
	}
	sealed, err := base64.StdEncoding.AppendDecode(nil, bytes.TrimSpace(body))
	if err != nil {
		return nil, ErrKeyFile
	}
	prot, err := protector()
	if err != nil {
		return nil, fmt.Errorf("shadow: the fleet key was sealed on a system with a secret protector: %w", err)
	}
	// The protector hands the key back on the heap. NewLockedBuffer moves it
	// into locked memory and zeroes that copy at once.
	scalar, err := prot.Unprotect(sealed)
	if err != nil {
		return nil, fmt.Errorf("shadow: unseal the fleet key: %w", err)
	}
	if len(scalar) != keySize {
		clear(scalar)
		return nil, ErrKeyFile
	}
	return secrets.NewLockedBuffer(scalar)
}

// NewRecovery makes the recovery identity. It returns the identity in a
// locked buffer as the line of text age itself reads ("AGE-SECRET-KEY-1…",
// no newline), for the caller to show the operator once and then release,
// and its public half ("age1…"), which is what to keep: snapshots are
// encrypted to it, and ProveRecovery checks the operator's copy against it.
// The identity is not stored anywhere by Tenant.
func NewRecovery() (identity *secrets.LockedBuffer, recipient string, err error) {
	scalar, err := secrets.NewLockedBuffer(make([]byte, keySize))
	if err != nil {
		return nil, "", err
	}
	if err := scalar.With(func(p []byte) error {
		_, err := rand.Read(p)
		return err
	}); err != nil {
		_ = scalar.Release()
		return nil, "", err
	}
	identity, err = secrets.NewLockedBuffer(make([]byte, bech32Len(identityHRP, keySize)))
	if err != nil {
		_ = scalar.Release()
		return nil, "", err
	}
	_ = identity.With(func(text []byte) error {
		return scalar.With(func(p []byte) error {
			bech32Encode(text, identityHRP, p)
			for i, c := range text {
				if 'a' <= c && c <= 'z' {
					text[i] = c - 'a' + 'A'
				}
			}
			return nil
		})
	})
	k, err := newKey(scalar, true)
	if err != nil {
		_ = identity.Release()
		return nil, "", err
	}
	defer k.Close()
	return identity, k.to.String(), nil
}

// ParseRecovery reads a recovery identity from the text NewRecovery made, as
// the operator gives it back: spaces and line ends around it are ignored.
// text is not kept and is the caller's to release. The Key it returns opens
// a snapshot made under any fleet key, as long as it was encrypted to this
// recovery key.
func ParseRecovery(text *secrets.LockedBuffer) (*Key, error) {
	scalar, err := secrets.NewLockedBuffer(make([]byte, keySize))
	if err != nil {
		return nil, err
	}
	if err := text.With(func(t []byte) error {
		t = bytes.TrimSpace(t)
		return scalar.With(func(p []byte) error {
			if !bech32Decode(p, identityHRP, t) {
				return ErrNotRecovery
			}
			return nil
		})
	}); err != nil {
		_ = scalar.Release()
		return nil, err
	}
	return newKey(scalar, true)
}

// ProveRecovery is the test decryption that shows the operator saved the
// recovery identity: a fresh random message is encrypted to recipient, the
// public half NewRecovery returned, and must decrypt with the identity in
// text. It returns ErrNotRecovery when text is not an identity or is the
// wrong one.
func ProveRecovery(text *secrets.LockedBuffer, recipient string) error {
	to, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		return fmt.Errorf("shadow: recovery recipient: %w", err)
	}
	k, err := ParseRecovery(text)
	if err != nil {
		return err
	}
	defer k.Close()
	probe := make([]byte, 32)
	_, _ = rand.Read(probe)
	var sealed bytes.Buffer
	w, err := age.Encrypt(&sealed, to)
	if err != nil {
		return err
	}
	if _, err := w.Write(probe); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	r, err := age.Decrypt(&sealed, k)
	if err != nil {
		return ErrNotRecovery
	}
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, probe) {
		return ErrNotRecovery
	}
	return nil
}
