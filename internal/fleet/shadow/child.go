package shadow

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"tenant/internal/fsperm"
)

// Errors of the child's copy store.
var (
	// ErrStale is a copy older than one the child already keeps under the
	// same key.
	ErrStale = errors.New("shadow: the child keeps a newer generation")
	// ErrConflict is a copy with the generation of one the child keeps and
	// different content. The child's stands.
	ErrConflict = errors.New("shadow: the child keeps this generation with different content")
	// ErrNoCopy is a generation the child does not keep, or keeps damaged.
	ErrNoCopy = errors.New("shadow: no such copy")
)

const (
	// keepCopies is how many generations a child keeps under one key.
	keepCopies = 3
	// maxKeys is how many different fleet keys a child keeps copies under.
	// Copies under a key that is no longer the master's are never removed
	// here, so this is what stops them from filling the disk.
	maxKeys = 8
	copyExt = ".snap"
)

// Copy is one generation of a machine's memory as its holder states it.
type Copy struct {
	KeyID string `json:"key"`  // the fleet key it was made under
	Gen   uint64 `json:"gen"`  // its generation, from 1
	Hash  string `json:"hash"` // the SHA-256 of its bytes
	// OK says the bytes the holder has still match Hash.
	OK bool `json:"ok"`
}

// valid reports whether a copy is one a master could have made.
func (c Copy) valid() bool {
	return validKeyID(c.KeyID) && c.Gen >= 1 && c.Gen <= maxGen && validHash(c.Hash)
}

// fileName is the one name a copy is kept under.
func (c Copy) fileName() string {
	return c.KeyID + "." + strconv.FormatUint(c.Gen, 10) + "." + c.Hash + copyExt
}

// parseCopyName reads a file name as fileName wrote it. Any other name is not
// a copy.
func parseCopyName(name string) (Copy, bool) {
	base, ok := strings.CutSuffix(name, copyExt)
	if !ok {
		return Copy{}, false
	}
	parts := strings.Split(base, ".")
	if len(parts) != 3 {
		return Copy{}, false
	}
	gen, ok := parseGen(parts[1])
	c := Copy{KeyID: parts[0], Gen: gen, Hash: parts[2]}
	return c, ok && c.valid()
}

// ChildStore is a child's side of its memory: the last three snapshots of
// its shadow, as the master sent them. The child has no key. What it keeps
// is opaque to it, and all it can check is that the bytes are the bytes it
// was given.
//
// It deletes in two cases only: a fourth generation arrives under a key (the
// oldest goes), or the master sends a purge. Copies under a key other than
// the one a call names are never touched.
type ChildStore struct {
	dir string
	mu  sync.Mutex
}

// OpenChildStore opens the store in dir, an owner-only directory it creates.
func OpenChildStore(dir string) (*ChildStore, error) {
	if err := fsperm.MkdirAll(dir); err != nil {
		return nil, fmt.Errorf("shadow: child store: %w", err)
	}
	return &ChildStore{dir: dir}, nil
}

// List states every copy the child keeps, newest first. A copy whose bytes no
// longer match its hash, or whose size no snapshot has, is listed with OK
// false. Anything else in the directory (a file not named as a copy is, a
// link, a directory) is left alone and left out.
func (s *ChildStore) List() ([]Copy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.list()
}

func (s *ChildStore) list() ([]Copy, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("shadow: child store: %w", err)
	}
	var copies []Copy
	for _, e := range entries {
		// A link or a directory is not a copy, whatever it is called.
		c, ok := parseCopyName(e.Name())
		if !ok || !e.Type().IsRegular() {
			continue
		}
		c.OK = s.intact(c)
		copies = append(copies, c)
	}
	slices.SortFunc(copies, func(a, b Copy) int {
		return cmp.Or(cmp.Compare(b.Gen, a.Gen), cmp.Compare(a.KeyID, b.KeyID), cmp.Compare(a.Hash, b.Hash))
	})
	return copies, nil
}

// open opens a copy's file if it is a plain file of a believable size. A
// link or a directory under a copy's name is not followed or entered.
func (s *ChildStore) open(c Copy) (*os.File, int64, error) {
	path := filepath.Join(s.dir, c.fileName())
	info, err := os.Lstat(path)
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > MaxSnapshotBytes {
		return nil, 0, ErrDamaged
	}
	f, err := os.Open(path)
	return f, info.Size(), err
}

// intact reports whether a copy's file is what its name says: the hash of
// its bytes, and a first line with the same key id and generation. It reads
// the file through, bounded by the size just checked, and keeps none of it.
func (s *ChildStore) intact(c Copy) bool {
	f, size, err := s.open(c)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	r := io.TeeReader(io.LimitReader(f, size+1), h)
	head := make([]byte, maxHeaderLine+1)
	n, _ := io.ReadFull(r, head)
	keyID, gen, err := Peek(head[:n])
	if err != nil || keyID != c.KeyID || gen != c.Gen {
		return false
	}
	rest, err := io.Copy(io.Discard, r)
	return err == nil && int64(n)+rest == size && hex.EncodeToString(h.Sum(nil)) == c.Hash
}

// read returns a copy's bytes if they are what its name says.
func (s *ChildStore) read(c Copy) ([]byte, error) {
	f, size, err := s.open(c)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	blob, err := io.ReadAll(io.LimitReader(f, size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(blob)) != size || Hash(blob) != c.Hash {
		return nil, ErrDamaged
	}
	if keyID, gen, err := Peek(blob); err != nil || keyID != c.KeyID || gen != c.Gen {
		return nil, ErrDamaged
	}
	return blob, nil
}

// Get returns the bytes of a copy the child keeps, checked against hash once
// more. ErrNoCopy means it is not there or no longer intact.
func (s *ChildStore) Get(c Copy) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !c.valid() {
		return nil, ErrNoCopy
	}
	blob, err := s.read(c)
	if err != nil {
		return nil, fmt.Errorf("%w: generation %d", ErrNoCopy, c.Gen)
	}
	return blob, nil
}

// Put keeps blob, a snapshot the master sent with the key id, generation and
// hash it states for it. The child checks what it can without a key: the
// hash is the hash of the bytes, and the snapshot's own first line says the
// same key id and generation.
//
// A copy already kept is accepted again and changes nothing. A generation
// older than the newest intact one under that key is refused with ErrStale,
// and the same generation with other content with ErrConflict. Once the new
// copy is safely on disk, copies under that key beyond three are removed:
// damaged ones first, then the oldest.
func (s *ChildStore) Put(c Copy, blob []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !c.valid() {
		return errors.New("shadow: child store: not a valid key id, generation or hash")
	}
	if len(blob) == 0 || len(blob) > MaxSnapshotBytes {
		return fmt.Errorf("shadow: child store: a snapshot of %d bytes is refused", len(blob))
	}
	if Hash(blob) != c.Hash {
		return fmt.Errorf("%w: the bytes do not match the stated hash", ErrDamaged)
	}
	if keyID, gen, err := Peek(blob); err != nil || keyID != c.KeyID || gen != c.Gen {
		return fmt.Errorf("%w: the snapshot is not the generation it is stated to be", ErrDamaged)
	}
	copies, err := s.list()
	if err != nil {
		return err
	}
	keys := map[string]bool{c.KeyID: true}
	var conflict bool
	var newer uint64
	for _, have := range copies {
		keys[have.KeyID] = true
		if have.KeyID != c.KeyID || !have.OK {
			continue
		}
		switch {
		case have.Gen == c.Gen && have.Hash == c.Hash:
			return nil
		case have.Gen == c.Gen:
			conflict = true
		case have.Gen > c.Gen:
			newer = max(newer, have.Gen)
		}
	}
	if conflict {
		return ErrConflict
	}
	if newer > 0 {
		return fmt.Errorf("%w (%d, and this is %d)", ErrStale, newer, c.Gen)
	}
	if len(keys) > maxKeys {
		return fmt.Errorf("shadow: child store: copies under %d other fleet keys are kept; the operator must remove some first", maxKeys)
	}
	if err := fsperm.Replace(filepath.Join(s.dir, c.fileName()), blob); err != nil {
		return fmt.Errorf("shadow: child store: %w", err)
	}
	// The new copy is on disk. Now, and only now, the surplus goes.
	c.OK = true
	mine := []Copy{c}
	for _, have := range copies {
		if have.KeyID == c.KeyID && have.fileName() != c.fileName() {
			mine = append(mine, have)
		}
	}
	// Intact before damaged, then newest first: what is past the third goes.
	slices.SortStableFunc(mine, func(a, b Copy) int {
		if a.OK != b.OK {
			if a.OK {
				return -1
			}
			return 1
		}
		return cmp.Compare(b.Gen, a.Gen)
	})
	for _, old := range mine[min(len(mine), keepCopies):] {
		if err := os.Remove(filepath.Join(s.dir, old.fileName())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("shadow: child store: %w", err)
		}
	}
	return nil
}

// Purge is the master's order, after a forget, to drop what came before
// keep: every copy under keep's key with a lower generation is removed, and
// any damaged copy under that key. It is refused unless the child holds keep
// intact, so a purge can never leave a machine with nothing. Copies under
// other keys are not touched.
func (s *ChildStore) Purge(keep Copy) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !keep.valid() {
		return ErrNoCopy
	}
	if _, err := s.read(keep); err != nil {
		return fmt.Errorf("%w: generation %d, so nothing is purged", ErrNoCopy, keep.Gen)
	}
	copies, err := s.list()
	if err != nil {
		return err
	}
	for _, have := range copies {
		if have.KeyID != keep.KeyID || (have.OK && have.Gen >= keep.Gen) {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, have.fileName())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("shadow: child store: %w", err)
		}
	}
	return nil
}
