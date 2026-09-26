package backup

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"

	"filippo.io/age"
	"filippo.io/age/armor"
)

// ageMagic starts every binary age file.
const ageMagic = "age-encryption.org/v1"

// ErrEncrypted is returned when a backup is encrypted and no passphrase or
// identity was supplied to open it.
var ErrEncrypted = errors.New("backup: the backup is encrypted; supply its passphrase or identity file")

// ErrDecrypt is returned when the supplied passphrase or identities do not
// open an encrypted backup.
var ErrDecrypt = errors.New("backup: cannot decrypt the backup (wrong passphrase or identity?)")

// writeStream writes the manifest and then every entry, in manifest order, as
// one gzip-compressed tar stream, age-encrypted to recipients when there are
// any. open returns the staged content of an entry; its bytes are hashed again
// on the way out, so a staged file that changed after it was hashed fails the
// write instead of producing a backup that cannot verify.
func writeStream(w io.Writer, m *Manifest, open func(Entry) (*os.File, error), recipients []age.Recipient) error {
	out := w
	var enc io.WriteCloser
	if len(recipients) > 0 {
		var err error
		if enc, err = age.Encrypt(w, recipients...); err != nil {
			return fmt.Errorf("backup: encrypt: %w", err)
		}
		out = enc
	}
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	manifest, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(entryHeader(manifestName, int64(len(manifest)), m)); err != nil {
		return err
	}
	if _, err := tw.Write(manifest); err != nil {
		return err
	}
	for _, e := range m.Entries {
		if err := writeEntry(tw, e, m, open); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if enc != nil {
		if err := enc.Close(); err != nil {
			return fmt.Errorf("backup: encrypt: %w", err)
		}
	}
	return nil
}

func writeEntry(tw *tar.Writer, e Entry, m *Manifest, open func(Entry) (*os.File, error)) error {
	f, err := open(e)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := tw.WriteHeader(entryHeader(e.Path, e.Size, m)); err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(tw, io.TeeReader(io.LimitReader(f, e.Size+1), h))
	if err != nil {
		return fmt.Errorf("backup: write %s: %w", e.Path, err)
	}
	if n != e.Size || hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
		return fmt.Errorf("backup: %s changed while the backup was being written", e.Path)
	}
	return nil
}

// entryHeader is a plain owner-only regular file stamped with the backup's
// creation time, so the archive carries no per-file timestamps or modes.
func entryHeader(name string, size int64, m *Manifest) *tar.Header {
	return &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Size:     size,
		Mode:     0o600,
		ModTime:  m.CreatedAt,
	}
}

// Reader reads a backup stream and checks every entry against its manifest.
type Reader struct {
	manifest  Manifest
	encrypted bool
	tail      *bufio.Reader // the gzip input, checked for trailing bytes
	gz        *gzip.Reader
	tr        *tar.Reader
	next      int
}

// NewReader opens a backup. An age-encrypted backup, binary or armored, needs
// at least one identity; ErrEncrypted is returned when none is given. The
// manifest is read and validated before NewReader returns.
func NewReader(r io.Reader, identities []age.Identity) (*Reader, error) {
	br := bufio.NewReader(r)
	head, _ := br.Peek(len(armor.Header))
	var src io.Reader = br
	encrypted := false
	switch {
	case bytes.HasPrefix(head, []byte(armor.Header)), bytes.HasPrefix(head, []byte(ageMagic)):
		if len(identities) == 0 {
			return nil, ErrEncrypted
		}
		var in io.Reader = br
		if bytes.HasPrefix(head, []byte(armor.Header)) {
			in = armor.NewReader(br)
		}
		dec, err := age.Decrypt(in, identities...)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrDecrypt, err)
		}
		src, encrypted = dec, true
	case len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b:
	default:
		return nil, errors.New("backup: not a Tenant backup (neither gzip nor age-encrypted)")
	}
	// gzip reads byte-exactly from a bufio.Reader, so whatever follows the
	// compressed stream stays in tail and can be checked.
	tail := bufio.NewReader(src)
	gz, err := gzip.NewReader(tail)
	if err != nil {
		return nil, fmt.Errorf("backup: %w", err)
	}
	gz.Multistream(false)
	rd := &Reader{encrypted: encrypted, tail: tail, gz: gz, tr: tar.NewReader(gz)}
	if err := rd.readManifest(); err != nil {
		return nil, err
	}
	return rd, nil
}

func (r *Reader) readManifest() error {
	hdr, err := r.tr.Next()
	if err != nil {
		return fmt.Errorf("backup: read manifest: %w", err)
	}
	if hdr.Name != manifestName || hdr.Typeflag != tar.TypeReg {
		return fmt.Errorf("backup: first entry is %q, not %s", hdr.Name, manifestName)
	}
	if hdr.Size > maxManifestBytes {
		return fmt.Errorf("backup: manifest is %d bytes, over the %d limit", hdr.Size, maxManifestBytes)
	}
	data, err := io.ReadAll(r.tr)
	if err != nil {
		return fmt.Errorf("backup: read manifest: %w", err)
	}
	if err := json.Unmarshal(data, &r.manifest); err != nil {
		return fmt.Errorf("backup: decode manifest: %w", err)
	}
	if err := r.manifest.validate(); err != nil {
		return err
	}
	if r.manifest.Secrets && !r.encrypted {
		return errors.New("backup: the manifest lists secrets but the backup is not encrypted; Tenant never writes secrets in the clear")
	}
	return nil
}

// Manifest returns the backup's validated manifest.
func (r *Reader) Manifest() *Manifest { return &r.manifest }

// Encrypted reports whether the backup was age-encrypted.
func (r *Reader) Encrypted() bool { return r.encrypted }

// Each calls fn for every entry in manifest order with a reader over its
// content. Whatever fn leaves unread is drained, and the entry's size and
// SHA-256 are checked before the next one starts. After the last entry Each
// checks that the stream ends there: no extra tar entries, an intact gzip
// trailer, no trailing bytes, and for an encrypted backup a complete final
// age chunk. fn's own error stops the walk and is returned as is.
func (r *Reader) Each(fn func(Entry, io.Reader) error) error {
	for ; r.next < len(r.manifest.Entries); r.next++ {
		e := r.manifest.Entries[r.next]
		hdr, err := r.tr.Next()
		if err == io.EOF {
			return fmt.Errorf("backup: stream ends before %s; the backup is truncated", e.Path)
		}
		if err != nil {
			return fmt.Errorf("backup: read %s: %w", e.Path, err)
		}
		if hdr.Name != e.Path {
			return fmt.Errorf("backup: expected %s next but found %q", e.Path, hdr.Name)
		}
		if hdr.Typeflag != tar.TypeReg {
			return fmt.Errorf("backup: %s is not a regular file in the archive", e.Path)
		}
		if hdr.Size != e.Size {
			return fmt.Errorf("backup: %s is %d bytes in the archive, the manifest says %d", e.Path, hdr.Size, e.Size)
		}
		v := &verifier{r: r.tr, h: sha256.New()}
		if err := fn(e, v); err != nil {
			return err
		}
		if _, err := io.Copy(io.Discard, v); err != nil {
			return fmt.Errorf("backup: read %s: %w", e.Path, err)
		}
		if v.n != e.Size {
			return fmt.Errorf("backup: %s is truncated (%d of %d bytes)", e.Path, v.n, e.Size)
		}
		if got := hex.EncodeToString(v.h.Sum(nil)); got != e.SHA256 {
			return fmt.Errorf("backup: %s does not match its checksum; the backup is damaged or was altered", e.Path)
		}
	}
	if hdr, err := r.tr.Next(); err != io.EOF {
		if err != nil {
			return fmt.Errorf("backup: after the last entry: %w", err)
		}
		return fmt.Errorf("backup: unexpected extra entry %q after the last listed file", hdr.Name)
	}
	if _, err := io.Copy(io.Discard, r.gz); err != nil {
		return fmt.Errorf("backup: compressed stream: %w", err)
	}
	if n, err := io.Copy(io.Discard, r.tail); err != nil {
		return fmt.Errorf("backup: after the compressed stream: %w", err)
	} else if n > 0 {
		return fmt.Errorf("backup: %d unexpected bytes after the end of the backup", n)
	}
	return nil
}

type verifier struct {
	r io.Reader
	h hash.Hash
	n int64
}

func (v *verifier) Read(p []byte) (int, error) {
	n, err := v.r.Read(p)
	v.h.Write(p[:n])
	v.n += int64(n)
	return n, err
}
