package shadow

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"

	"filippo.io/age"
)

// A snapshot is one machine's memory, both stores, as one opaque value:
//
//	tenant-fleet-snapshot/2 <key id> <generation>\n
//	an age file, encrypted to the fleet key and the recovery key, holding
//	    a gzip stream of
//	        one line of JSON: host, key id, generation, content hash, sizes
//	        the episodic database, as SQLite serializes it
//	        the semantic database
//	32 bytes: HMAC-SHA-256 of everything before them, keyed by a key
//	    derived from the fleet key's secret (Key.mac)
//
// The first line is all that is readable without a key. It is repeated
// inside, where it cannot be changed, and the two must agree. A snapshot's
// hash is the SHA-256 of all its bytes.
//
// The authenticator is what says this master made a snapshot: age to a
// public recipient vouches for nobody, so "it decrypts" proves nothing to a
// master whose public halves someone has learned. A reader that knows only
// version 1 refuses version 2 at its first line, and this one refuses
// version 1 there: a snapshot without an authenticator is damaged, and there
// is no reading of the old format to fall back to.

var (
	// ErrDamaged is a snapshot that cannot be trusted: cut short, altered,
	// not a snapshot at all, for another machine, holding a database that
	// fails SQLite's integrity check, or gone when it should be there.
	ErrDamaged = errors.New("shadow: the snapshot is damaged")
	// ErrForeignKey is a snapshot made under a fleet key that is not this
	// master's. It is not decrypted: reading it takes the recovery identity.
	ErrForeignKey = errors.New("shadow: the snapshot was made under another fleet key")
)

const (
	snapMagic = "tenant-fleet-snapshot/2"
	// MaxSnapshotBytes bounds a snapshot. Each database in it is bounded by
	// the same figure, which is also what SQLite lets an in-memory database
	// grow to.
	MaxSnapshotBytes = 1 << 30
	maxHeaderLine    = len(snapMagic) + 1 + 2*keyIDBytes + 1 + 19
	maxMetaLine      = 1024
	// maxGen keeps a generation inside what every integer type that will
	// carry it can hold.
	maxGen = math.MaxInt64
)

// Hash is a snapshot's hash: the SHA-256 of its bytes, in hex.
func Hash(blob []byte) string {
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}

// Peek reads the clear first line of a snapshot: the id of the fleet key it
// was made under, and its generation. It needs no key, which is how a child
// checks what it is asked to keep.
func Peek(blob []byte) (keyID string, gen uint64, err error) {
	line, _, found := bytes.Cut(blob[:min(len(blob), maxHeaderLine+1)], []byte("\n"))
	if found {
		if keyID, gen, ok := parseHeader(line); ok {
			return keyID, gen, nil
		}
	}
	return "", 0, fmt.Errorf("%w: not a fleet snapshot", ErrDamaged)
}

func appendHeader(dst []byte, keyID string, gen uint64) []byte {
	dst = append(dst, snapMagic+" "+keyID+" "...)
	return append(strconv.AppendUint(dst, gen, 10), '\n')
}

// parseHeader reads a first line, without its line end. Only the one
// spelling appendHeader writes is accepted.
func parseHeader(line []byte) (keyID string, gen uint64, ok bool) {
	rest, ok := bytes.CutPrefix(line, []byte(snapMagic+" "))
	if !ok || len(rest) < 2*keyIDBytes+2 || rest[2*keyIDBytes] != ' ' {
		return "", 0, false
	}
	keyID = string(rest[:2*keyIDBytes])
	gen, ok = parseGen(string(rest[2*keyIDBytes+1:]))
	return keyID, gen, ok && validKeyID(keyID)
}

// parseGen reads a generation number: decimal digits with no sign, space or
// leading zero, at most maxGen.
func parseGen(s string) (uint64, bool) {
	if s == "" || len(s) > 19 || (s[0] == '0' && len(s) > 1) {
		return 0, false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	gen, err := strconv.ParseUint(s, 10, 64)
	return gen, err == nil && gen <= maxGen
}

func validKeyID(s string) bool { return len(s) == 2*keyIDBytes && lowerHex(s) }
func validHash(s string) bool  { return len(s) == 2*sha256.Size && lowerHex(s) }

func lowerHex(s string) bool {
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// validHost reports whether id can name a machine's directory: lowercase
// letters, digits, "-" and "_", starting with a letter or a digit, at most 64
// long, and not a name Windows reserves for a device.
func validHost(id string) bool {
	if id == "" || len(id) > 64 || id[0] == '-' || id[0] == '_' {
		return false
	}
	for i := range len(id) {
		if c := id[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	switch id {
	case "con", "prn", "aux", "nul":
		return false
	}
	if len(id) == 4 && (id[:3] == "com" || id[:3] == "lpt") && id[3] >= '0' && id[3] <= '9' {
		return false
	}
	return true
}

// meta is the line inside the encryption.
type meta struct {
	Host  string `json:"host"`
	KeyID string `json:"key"`
	Gen   uint64 `json:"gen"`
	// Content is the content hash of generation Gen. In the master's own
	// snapshot of a shadow, which may have moved on since Gen was made, it
	// is how the next look tells whether there is anything new to send.
	Content string `json:"content,omitempty"`
	// Embeddings says the vectors were left in: the master's own snapshot.
	Embeddings bool  `json:"embeddings"`
	Episodic   int64 `json:"episodic"` // the databases' sizes in bytes
	Semantic   int64 `json:"semantic"`
}

func parseMeta(line []byte) (meta, error) {
	var m meta
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return meta{}, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return meta{}, errors.New("something follows the value")
	}
	if err := m.validate(); err != nil {
		return meta{}, err
	}
	return m, nil
}

func (m meta) validate() error {
	switch {
	case !validHost(m.Host):
		return errors.New("invalid machine id")
	case !validKeyID(m.KeyID):
		return errors.New("invalid key id")
	case m.Gen > maxGen:
		return errors.New("generation out of range")
	case m.Content != "" && !validHash(m.Content):
		return errors.New("invalid content hash")
	case m.Episodic <= 0 || m.Episodic > MaxSnapshotBytes || m.Semantic <= 0 || m.Semantic > MaxSnapshotBytes:
		return errors.New("database size out of range")
	}
	return nil
}

// seal makes a snapshot of the two serialized databases, authenticated by
// by, the fleet key whose id m names.
func seal(m meta, episodic, semantic []byte, to []age.Recipient, by *Key) ([]byte, error) {
	m.Episodic, m.Semantic = int64(len(episodic)), int64(len(semantic))
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("shadow: snapshot: %w", err)
	}
	if by == nil || by.id != m.KeyID {
		return nil, errors.New("shadow: snapshot: it is authenticated by the key it names")
	}
	mac, done, err := by.mac()
	if err != nil {
		return nil, err
	}
	defer done()
	line, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	out := bytes.NewBuffer(make([]byte, 0, len(episodic)+len(semantic)+4096))
	out.Write(appendHeader(nil, m.KeyID, m.Gen))
	enc, err := age.Encrypt(out, to...)
	if err != nil {
		return nil, fmt.Errorf("shadow: encrypt: %w", err)
	}
	// With the vectors in, two thirds of the bytes are noise that does not
	// compress, and inflating the rest would cost more than everything else
	// a load does. Those are stored, in the same gzip framing.
	level := gzip.DefaultCompression
	if m.Embeddings {
		level = gzip.NoCompression
	}
	gz, err := gzip.NewWriterLevel(enc, level)
	if err != nil {
		return nil, err
	}
	for _, part := range [][]byte{line, {'\n'}, episodic, semantic} {
		if _, err := gz.Write(part); err != nil {
			return nil, fmt.Errorf("shadow: encrypt: %w", err)
		}
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("shadow: encrypt: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("shadow: encrypt: %w", err)
	}
	mac.Write(out.Bytes())
	out.Write(mac.Sum(nil))
	if out.Len() > MaxSnapshotBytes {
		return nil, fmt.Errorf("shadow: the snapshot is %d bytes, over the %d limit", out.Len(), MaxSnapshotBytes)
	}
	return out.Bytes(), nil
}

// snapshot is an opened snapshot: its two databases, in memory, checked.
type snapshot struct {
	meta
	episodic, semantic *memDB
	// authentic says this master's fleet key made it (its authenticator
	// holds). Only a copy read with a recovery identity can be without.
	authentic bool
}

func (s *snapshot) close() {
	_ = s.episodic.Close()
	_ = s.semantic.Close()
}

// openSnapshot is readSnapshot for a caller whose key is also the one that
// authenticates: the fleet key, or a recovery identity alone.
func openSnapshot(ctx context.Context, r io.ReadSeeker, k *Key, host string) (*snapshot, error) {
	return readSnapshot(ctx, r, k, k, host)
}

// readSnapshot decrypts host's snapshot with k into two in-memory databases,
// and returns them only when all of it holds up. In this order:
//
//  1. Its authenticator, checked with own, this master's fleet key, over
//     every byte before it, before anything of the snapshot is parsed or any
//     memory is sized by it.
//  2. Its first line. A snapshot that names own's key id and does not
//     authenticate is damaged, whoever reads it. One that names another key
//     is ErrForeignKey, unless k is a recovery identity: that is the one
//     reading of a snapshot this master did not make (authentic false), and
//     nothing but the recovery key's public half vouches for it.
//  3. The encryption, to its last byte; the inside agreeing with the first
//     line and naming host; both databases passing SQLite's integrity check.
//
// The plaintext exists in RAM alone.
func readSnapshot(ctx context.Context, r io.ReadSeeker, k, own *Key, host string) (_ *snapshot, err error) {
	size, err := r.Seek(0, io.SeekEnd)
	if err != nil || size <= macSize || size > MaxSnapshotBytes {
		return nil, fmt.Errorf("%w: not a fleet snapshot", ErrDamaged)
	}
	authentic := own.authentic(r, size)
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDamaged, err)
	}
	br := bufio.NewReaderSize(io.LimitReader(r, size-macSize), 64<<10)
	line, err := readLine(br, maxHeaderLine)
	if err != nil {
		return nil, fmt.Errorf("%w: not a fleet snapshot", ErrDamaged)
	}
	keyID, gen, ok := parseHeader(line)
	switch {
	case !ok:
		return nil, fmt.Errorf("%w: not a fleet snapshot", ErrDamaged)
	case authentic && keyID != own.id:
		return nil, fmt.Errorf("%w: it names another key than the one that made it", ErrDamaged)
	case !authentic && keyID == own.id:
		return nil, fmt.Errorf("%w: its authenticator does not hold: this master did not make it", ErrDamaged)
	case !k.recovery && keyID != k.id:
		return nil, fmt.Errorf("%w (key %s)", ErrForeignKey, keyID)
	}
	damaged := func(err error) error { return fmt.Errorf("%w: %v", ErrDamaged, err) }
	dec, err := age.Decrypt(br, k)
	if err != nil {
		var none *age.NoIdentityMatchError
		if k.recovery && errors.As(err, &none) {
			return nil, ErrNotRecovery
		}
		return nil, damaged(err)
	}
	tail := bufio.NewReader(dec)
	gz, err := gzip.NewReader(tail)
	if err != nil {
		return nil, damaged(err)
	}
	gz.Multistream(false)
	zr := bufio.NewReader(gz)
	line, err = readLine(zr, maxMetaLine)
	if err != nil {
		return nil, damaged(err)
	}
	m, err := parseMeta(line)
	if err != nil {
		return nil, damaged(err)
	}
	if m.KeyID != keyID || m.Gen != gen {
		return nil, damaged(errors.New("its first line was changed"))
	}
	if m.Host != host {
		return nil, damaged(fmt.Errorf("it belongs to machine %s", m.Host))
	}
	s := &snapshot{meta: m, authentic: authentic}
	defer func() {
		if err != nil {
			for _, db := range []*memDB{s.episodic, s.semantic} {
				if db != nil {
					_ = db.Close()
				}
			}
		}
	}()
	// One database at a time through one buffer, so that only one is in RAM
	// twice, and only while SQLite takes its copy. The sizes are the
	// snapshot's own word, so no more than a set amount is allocated on the
	// strength of them. bytes.MinRead of room past the end keeps the buffer
	// from doubling to read the last few bytes.
	image := bytes.NewBuffer(make([]byte, 0, min(max(m.Episodic, m.Semantic), 64<<20)+bytes.MinRead))
	for _, part := range []struct {
		size int64
		db   **memDB
	}{{m.Episodic, &s.episodic}, {m.Semantic, &s.semantic}} {
		image.Reset()
		if _, err := io.CopyN(image, zr, part.size); err != nil {
			return nil, damaged(err)
		}
		if *part.db, err = openMem(ctx, image.Bytes(), true); err != nil {
			if errors.Is(err, ErrDamaged) {
				return nil, err
			}
			return nil, damaged(err)
		}
	}
	// Nothing may follow, and reading to the end is also what checks gzip's
	// checksum and that age's last chunk is the real last chunk.
	if _, err := zr.ReadByte(); err != io.EOF {
		return nil, damaged(errors.New("data after the databases"))
	}
	if _, err := tail.ReadByte(); err != io.EOF {
		return nil, damaged(errors.New("data after the compressed stream"))
	}
	return s, nil
}

func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for len(line) <= max {
		c, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if c == '\n' {
			return line, nil
		}
		line = append(line, c)
	}
	return nil, errors.New("line too long")
}

// withoutEmbeddings returns a serialized database with its vector column
// emptied and the space given back: the child's copy of it. It works on a
// copy in RAM.
func withoutEmbeddings(ctx context.Context, image []byte) ([]byte, error) {
	db, err := openMem(ctx, image, false)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	for table, column := range embeddingColumn {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 {
			continue
		}
		if _, err := db.ExecContext(ctx, "UPDATE "+quoteIdents([]string{table})+" SET "+quoteIdents([]string{column})+" = x''"); err != nil {
			return nil, err
		}
	}
	if _, err := db.ExecContext(ctx, "VACUUM"); err != nil {
		return nil, err
	}
	out, _, err := db.serialize(ctx)
	return out, err
}
