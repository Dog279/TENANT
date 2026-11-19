package shadow

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"filippo.io/age"

	"tenant/internal/datalock"
	"tenant/internal/fsperm"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
)

// Errors of the master's store of shadows.
var (
	// ErrInUse is an operation that replaces or removes a shadow while its
	// stores are held.
	ErrInUse = errors.New("shadow: the machine's memory is in use")
	// ErrClosed is any use of a Store after Close.
	ErrClosed = errors.New("shadow: the store is closed")
)

const (
	shadowFile = "shadow.snap" // the master's snapshot of the shadow, vectors in
	childFile  = "child.snap"  // the newest generation made for the child
	asideDir   = "aside"       // shadows that were replaced
	lockFile   = ".shadows.lock"
	// asideKeep is how long a replaced shadow is kept.
	asideKeep = 30 * 24 * time.Hour
	// busyWait is how long the background pass waits for a database whose one
	// connection is in use before leaving that shadow for the next pass. It
	// holds the shadow's lock while it waits, so it is short.
	busyWait = 100 * time.Millisecond
	// passLimit bounds the work the pass does on one shadow.
	passLimit = 2 * time.Minute
)

// Options tune a Store. The zero value is what Tenant runs with.
type Options struct {
	// FlushAfter is how often a shadow in RAM is looked at, and so how long
	// after a write of content the snapshot on disk is replaced: at most
	// this long, plus the time to write it. A crash loses the writes since.
	// Default 3 seconds.
	FlushAfter time.Duration
	// IdleAfter is how long a shadow nobody holds stays in RAM. Default 10
	// minutes.
	IdleAfter time.Duration
	// OnError is told what the background work could not do (a snapshot that
	// would not write, a shadow whose database was lost). It may be nil.
	OnError func(host string, err error)
}

// Store keeps the shadows, for the one process that owns the fleet.
//
// A shadow is loaded on its first Acquire: its snapshot is decrypted straight
// into two in-memory databases. From then on a background pass looks at it
// every FlushAfter, replaces the snapshot when its content has changed, and
// drops it from RAM once it has been idle for IdleAfter. Nothing readable is
// written to disk at any point: not by this package, and not by SQLite, whose
// journal and temporary files are kept in RAM for these databases.
type Store struct {
	dir  string
	key  *Key
	to   []age.Recipient
	opt  Options
	lock *datalock.Lock

	mu      sync.Mutex
	hosts   map[string]*shadow
	damaged map[string]bool // shadows that did not load
	closed  bool
	stop    chan struct{}
	done    chan struct{}
}

type shadow struct {
	host string

	// Guarded by Store.mu.
	refs     int
	lastUsed time.Time

	// mu is held to load, look at, snapshot, replace or drop the shadow.
	// Everything below is guarded by it.
	mu       sync.Mutex
	episodic *memDB // nil while the shadow is not in RAM
	semantic *memDB
	pair     Pair
	fresh    bool     // started empty and never written out: the machine has no memory
	gen      uint64   // the newest generation made
	minted   string   // its content hash; empty when not known
	seen     [2]int64 // the databases' change counts when last looked at
	disk     recorded // what the snapshot on disk records
	content  string   // that snapshot's content hash; empty when not known
}

// recorded is the state of a shadow as a snapshot records it: the databases'
// change counts, which move with every write, and the generation.
type recorded struct {
	changes [2]int64
	gen     uint64
	minted  string
}

// Pair is a machine's two stores while they are held. They are ordinary
// stores, each on a database with one connection: a statement waits for the
// one before it, and so does the Store's own work, so do not call the Store
// for this machine from inside a transaction or an open result of theirs.
// Do not Close them: Release the Pair, and stop using them then.
type Pair struct {
	Episodic *episodic.Store
	Semantic *semantic.Store

	release func()
}

// Release gives the stores back. A shadow nobody holds can be dropped from
// RAM, replaced or removed.
func (p *Pair) Release() {
	if p.release != nil {
		p.release()
	}
}

// OpenStore opens the shadows under dir (<data>/fleet). key is the fleet key
// and stays the caller's to close, after the Store. recovery is the public
// half of the recovery identity ("age1…"). Every snapshot is encrypted to
// both.
//
// Only one process can have the Store open: a second OpenStore fails with
// datalock.ErrBusy.
func OpenStore(dir string, key *Key, recovery string, opt Options) (*Store, error) {
	rec, err := age.ParseX25519Recipient(recovery)
	if err != nil {
		return nil, fmt.Errorf("shadow: recovery recipient: %w", err)
	}
	if opt.FlushAfter <= 0 {
		opt.FlushAfter = 3 * time.Second
	}
	if opt.IdleAfter <= 0 {
		opt.IdleAfter = 10 * time.Minute
	}
	if err := fsperm.MkdirAll(dir); err != nil {
		return nil, fmt.Errorf("shadow: %w", err)
	}
	lock, err := datalock.ExclusiveFile(filepath.Join(dir, lockFile))
	if err != nil {
		return nil, fmt.Errorf("shadow: another process holds the fleet's memory: %w", err)
	}
	s := &Store{
		dir: dir, key: key, to: []age.Recipient{key.recipient(), rec}, opt: opt, lock: lock,
		hosts: map[string]*shadow{}, damaged: map[string]bool{},
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	go s.run()
	return s, nil
}

// Close writes out every shadow in RAM, makes a generation of any that has
// content its child was never sent, and frees them. Pairs still held stop
// working.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	all := s.shadows()
	s.mu.Unlock()
	close(s.stop)
	<-s.done
	var errs []error
	for _, sh := range all {
		sh.mu.Lock()
		if sh.episodic != nil {
			// Bounded, so that a holder stuck in a transaction cannot keep
			// the process from exiting. What it was writing is lost with it.
			ctx, cancel := context.WithTimeout(context.Background(), passLimit)
			errs = append(errs, s.settle(ctx, sh))
			cancel()
			sh.unload()
		}
		sh.mu.Unlock()
	}
	return errors.Join(append(errs, s.lock.Release())...)
}

// Acquire returns host's stores, loading its shadow into RAM if it is not
// there. A machine with no memory yet gets empty stores. Release the Pair
// when done.
//
// A shadow whose snapshot cannot be trusted is not loaded: the error wraps
// ErrDamaged, or ErrForeignKey when it was made under another fleet key (the
// machine needs the recovery key). State then says the shadow is not there,
// and Decide says what to rebuild it from.
func (s *Store) Acquire(ctx context.Context, host string) (*Pair, error) {
	sh, err := s.hold(host)
	if err != nil {
		return nil, err
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if err := s.ensure(ctx, sh); err != nil {
		s.letGo(sh)
		return nil, err
	}
	p := sh.pair
	p.release = sync.OnceFunc(func() { s.letGo(sh) })
	return &p, nil
}

// hold finds or makes host's slot and counts one more holder.
func (s *Store) hold(host string) (*shadow, error) {
	if !validHost(host) {
		return nil, fmt.Errorf("shadow: %q is not a machine id", host)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	sh := s.hosts[host]
	if sh == nil {
		sh = &shadow{host: host}
		s.hosts[host] = sh
	}
	sh.refs++
	return sh, nil
}

func (s *Store) letGo(sh *shadow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sh.refs--
	sh.lastUsed = time.Now()
}

// exclusive is hold for an operation that needs the shadow to itself: it
// returns with sh.mu held, or ErrInUse when anyone else holds the shadow.
// done releases both.
func (s *Store) exclusive(host string) (sh *shadow, done func(), err error) {
	if sh, err = s.hold(host); err != nil {
		return nil, nil, err
	}
	sh.mu.Lock()
	done = func() {
		sh.mu.Unlock()
		s.letGo(sh)
	}
	s.mu.Lock()
	busy, closed := sh.refs > 1, s.closed
	s.mu.Unlock()
	switch {
	case closed: // while this waited for the lock
		done()
		return nil, nil, ErrClosed
	case busy:
		done()
		return nil, nil, ErrInUse
	}
	return sh, done, nil
}

func (s *Store) hostDir(host string) string { return filepath.Join(s.dir, host) }

// Loaded lists the machines whose shadows are in RAM. Only these can have
// content their child was not sent: a shadow is brought up to date with Mint
// before it is dropped.
func (s *Store) Loaded() []string {
	s.mu.Lock()
	all := s.shadows()
	s.mu.Unlock()
	var hosts []string
	for _, sh := range all {
		sh.mu.Lock()
		if sh.episodic != nil {
			hosts = append(hosts, sh.host)
		}
		sh.mu.Unlock()
	}
	slices.Sort(hosts)
	return hosts
}

// shadows copies the slots out. The caller holds s.mu.
func (s *Store) shadows() []*shadow {
	all := make([]*shadow, 0, len(s.hosts))
	for _, sh := range s.hosts {
		all = append(all, sh)
	}
	return all
}

// ensure loads sh if it is not in RAM. The caller holds sh.mu.
func (s *Store) ensure(ctx context.Context, sh *shadow) error {
	if sh.episodic != nil {
		return nil
	}
	// Close may have come and gone while the caller waited for the lock; a
	// shadow loaded now would never be written out.
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return ErrClosed
	}
	return s.load(ctx, sh)
}

// load brings sh into RAM from its snapshot. The caller holds sh.mu.
func (s *Store) load(ctx context.Context, sh *shadow) error {
	dir := s.hostDir(sh.host)
	var ep, sem *memDB
	sh.gen, sh.minted, sh.fresh = 0, "", false
	f, err := os.Open(filepath.Join(dir, shadowFile))
	switch {
	case err == nil:
		snap, err := openSnapshot(ctx, f, s.key, sh.host)
		_ = f.Close()
		if err != nil {
			return s.unreadable(sh.host, err)
		}
		ep, sem = snap.episodic, snap.semantic
		sh.gen, sh.minted = snap.Gen, snap.Content
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("shadow: %s: %w", sh.host, err)
	case s.remains(sh.host):
		// There was a memory: a generation was made from it, or an older one
		// was set aside. Starting empty would send the child an empty memory
		// as the newest.
		return s.unreadable(sh.host, fmt.Errorf("%w: the master's snapshot is gone", ErrDamaged))
	default:
		if ep, err = openMem(ctx, nil, false); err != nil {
			return err
		}
		if sem, err = openMem(ctx, nil, false); err != nil {
			_ = ep.Close()
			return err
		}
		sh.fresh = true
	}
	// A generation made just before a crash is on disk without the snapshot
	// that would have recorded it. Its number is taken; what it holds is not
	// known, so the next Mint makes another.
	onDisk := recorded{gen: sh.gen, minted: sh.minted}
	if c, _, err := s.newest(sh.host); err == nil && c.Gen > sh.gen {
		sh.gen, sh.minted = c.Gen, ""
	}
	pair, err := openStores(ep, sem)
	if err != nil {
		return s.unreadable(sh.host, err)
	}
	if err := s.install(ctx, sh, ep, sem, pair); err != nil {
		return err
	}
	sh.disk.gen, sh.disk.minted = onDisk.gen, onDisk.minted
	return nil
}

// openStores opens the two stores on two databases, which are closed if it
// fails. A database that is sound and that the stores cannot use (a snapshot
// of something else) is a damaged snapshot like any other.
func openStores(ep, sem *memDB) (Pair, error) {
	es, err := episodic.OpenDB(ep.DB)
	if err != nil {
		_ = sem.Close()
		return Pair{}, fmt.Errorf("%w: %v", ErrDamaged, err)
	}
	ss, err := semantic.OpenDB(sem.DB)
	if err != nil {
		_ = ep.Close()
		return Pair{}, fmt.Errorf("%w: %v", ErrDamaged, err)
	}
	return Pair{Episodic: es, Semantic: ss}, nil
}

// install puts two databases and the stores on them in place as sh's. The
// caller holds sh.mu.
func (s *Store) install(ctx context.Context, sh *shadow, ep, sem *memDB, pair Pair) (err error) {
	sh.episodic, sh.semantic, sh.pair = ep, sem, pair
	if sh.seen, err = sh.changes(ctx); err != nil {
		sh.unload()
		return fmt.Errorf("shadow: %s: %w", sh.host, err)
	}
	sh.disk, sh.content = recorded{sh.seen, sh.gen, sh.minted}, ""
	s.mu.Lock()
	delete(s.damaged, sh.host)
	s.mu.Unlock()
	return nil
}

// unreadable records that host's shadow did not load.
func (s *Store) unreadable(host string, err error) error {
	if errors.Is(err, ErrDamaged) || errors.Is(err, ErrForeignKey) {
		s.mu.Lock()
		s.damaged[host] = true
		s.mu.Unlock()
	}
	return fmt.Errorf("shadow: %s: %w", host, err)
}

// remains reports whether host's directory shows it once had a memory.
func (s *Store) remains(host string) bool {
	if _, err := os.Lstat(filepath.Join(s.hostDir(host), childFile)); err == nil {
		return true
	}
	entries, _ := os.ReadDir(filepath.Join(s.hostDir(host), asideDir))
	return len(entries) > 0
}

// unload closes the databases. What they held is gone. The caller holds sh.mu.
func (sh *shadow) unload() {
	_ = sh.episodic.Close()
	_ = sh.semantic.Close()
	sh.episodic, sh.semantic, sh.pair = nil, nil, Pair{}
}

func (sh *shadow) changes(ctx context.Context) (n [2]int64, err error) {
	if n[0], err = sh.episodic.changes(ctx); err == nil {
		n[1], err = sh.semantic.changes(ctx)
	}
	return n, err
}

// flush replaces the snapshot on disk with the shadow as it is now. The
// caller holds sh.mu.
func (s *Store) flush(ctx context.Context, sh *shadow) error {
	content, err := contentHash(ctx, sh.episodic.DB, sh.semantic.DB)
	if err != nil {
		return err
	}
	ep, n0, err := sh.episodic.serialize(ctx)
	if err != nil {
		return err
	}
	sem, n1, err := sh.semantic.serialize(ctx)
	if err != nil {
		return err
	}
	blob, err := seal(meta{Host: sh.host, KeyID: s.key.id, Gen: sh.gen, Content: sh.minted, Embeddings: true}, ep, sem, s.to)
	if err != nil {
		return err
	}
	if err := s.write(sh.host, shadowFile, blob); err != nil {
		return err
	}
	// A write that landed between the hash and the images is in the snapshot
	// and not in content, so the next look sees a difference and writes once
	// more than it had to.
	sh.disk, sh.content, sh.fresh = recorded{[2]int64{n0, n1}, sh.gen, sh.minted}, content, false
	return nil
}

// write puts a file in host's directory: owner-only, whole or not at all. It
// is the only place the Store hands bytes to the file system.
func (s *Store) write(host, name string, data []byte) error {
	if err := fsperm.MkdirAll(s.hostDir(host)); err != nil {
		return err
	}
	// On Windows a file cannot be replaced while another program has it open
	// (a backup reading it, a virus scanner), and the rename is refused. That
	// passes in a moment, and the write that fails may be the last one before
	// the process exits, so it is tried a few times.
	var err error
	for try := range 5 {
		time.Sleep(time.Duration(try) * 20 * time.Millisecond)
		if err = replaceFile(filepath.Join(s.hostDir(host), name), data); !errors.Is(err, fs.ErrPermission) {
			break
		}
	}
	return err
}

// replaceFile is fsperm.Replace. Tests wrap it to see every byte written.
var replaceFile = fsperm.Replace

// Flush writes host's shadow to disk now, if it is in RAM and anything at all
// changed since its snapshot: after a re-embed, say, which changes no content
// and so would otherwise wait for the next Mint.
func (s *Store) Flush(ctx context.Context, host string) error {
	sh, err := s.hold(host)
	if err != nil {
		return err
	}
	defer s.letGo(sh)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.episodic == nil {
		return nil
	}
	return s.flushIfChanged(ctx, sh)
}

func (s *Store) flushIfChanged(ctx context.Context, sh *shadow) error {
	n, err := sh.changes(ctx)
	if err != nil || (recorded{n, sh.gen, sh.minted}) == sh.disk {
		return err
	}
	return s.flush(ctx, sh)
}

// Mint makes the next generation of host's memory for its child, if the
// content changed since the last one: the shadow without its vectors,
// compressed and encrypted, numbered one higher. It is kept on the master's
// disk until a newer one replaces it, whether or not the child can be
// reached, and Newest reads it back for sending. It reports the newest
// generation and whether it was made by this call.
//
// The five-minute pass calls Mint for every machine in Loaded. A shadow that
// is not in RAM has nothing new.
func (s *Store) Mint(ctx context.Context, host string) (Copy, bool, error) {
	sh, err := s.hold(host)
	if err != nil {
		return Copy{}, false, err
	}
	defer s.letGo(sh)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.episodic == nil {
		c, _, _ := s.newest(host)
		return c, false, nil
	}
	return s.mint(ctx, sh)
}

// mint is Mint for a shadow in RAM. The caller holds sh.mu.
func (s *Store) mint(ctx context.Context, sh *shadow) (Copy, bool, error) {
	content, err := contentHash(ctx, sh.episodic.DB, sh.semantic.DB)
	if err != nil {
		return Copy{}, false, err
	}
	if sh.gen > 0 && content == sh.minted {
		if c, _, err := s.newest(sh.host); err == nil && c.Gen == sh.gen {
			return c, false, s.flushIfChanged(ctx, sh)
		}
		// Nothing changed, but the generation's file is gone or is not that
		// generation: the next one is made, so that there is one to send.
	}
	if n, err := sh.changes(ctx); err != nil {
		return Copy{}, false, err
	} else if sh.fresh && n == sh.disk.changes {
		return Copy{}, false, nil // a machine with no memory has no generations
	}
	if sh.gen >= maxGen {
		return Copy{}, false, errors.New("shadow: out of generation numbers")
	}
	ep, _, err := sh.episodic.serialize(ctx)
	if err != nil {
		return Copy{}, false, err
	}
	sem, _, err := sh.semantic.serialize(ctx)
	if err != nil {
		return Copy{}, false, err
	}
	if ep, err = withoutEmbeddings(ctx, ep); err != nil {
		return Copy{}, false, err
	}
	if sem, err = withoutEmbeddings(ctx, sem); err != nil {
		return Copy{}, false, err
	}
	gen := sh.gen + 1
	blob, err := seal(meta{Host: sh.host, KeyID: s.key.id, Gen: gen, Content: content}, ep, sem, s.to)
	if err != nil {
		return Copy{}, false, err
	}
	// A generation file under another fleet key is not ours to replace.
	if _, err := os.Lstat(filepath.Join(s.hostDir(sh.host), childFile)); err == nil {
		if _, _, err := s.newest(sh.host); err != nil {
			if _, err := s.setAside(sh.host, childFile); err != nil {
				return Copy{}, false, err
			}
		}
	}
	if err := s.write(sh.host, childFile, blob); err != nil {
		return Copy{}, false, err
	}
	sh.gen, sh.minted = gen, content
	// The snapshot records the generation, so it is written again.
	return Copy{KeyID: s.key.id, Gen: gen, Hash: Hash(blob), OK: true}, true, s.flush(ctx, sh)
}

// Newest returns the newest generation made for host under this fleet key,
// and its bytes, for sending to the child. ErrNoCopy means none was made.
func (s *Store) Newest(host string) (Copy, []byte, error) {
	sh, err := s.hold(host)
	if err != nil {
		return Copy{}, nil, err
	}
	defer s.letGo(sh)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return s.newest(host)
}

// Skip makes host's next generation now, numbered above past, whether or not
// the content changed. It is for a push the child refuses with ErrStale: the
// child holds a file it takes for a newer copy, at generation past, which
// the master has found is not one (Adopt refused it, so Decide said Push). A
// child takes no generation lower than one it believes it holds intact, and
// nothing the master sends makes it drop a newer one. After Skip the
// master's newest is above it and the child takes that.
//
// Skip is never the answer to a copy that merely failed to verify: the
// master may not have the child's real memory yet, and would number an
// older or empty shadow above it.
func (s *Store) Skip(ctx context.Context, host string, past uint64) (Copy, error) {
	sh, err := s.hold(host)
	if err != nil {
		return Copy{}, err
	}
	defer s.letGo(sh)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if err := s.ensure(ctx, sh); err != nil {
		return Copy{}, err
	}
	if past >= maxGen {
		return Copy{}, errors.New("shadow: out of generation numbers")
	}
	sh.gen, sh.minted, sh.fresh = max(sh.gen, past), "", false
	c, _, err := s.mint(ctx, sh)
	return c, err
}

// newest is Newest for a caller that holds the shadow's lock, which is what
// keeps the file from being replaced while it is read.
func (s *Store) newest(host string) (Copy, []byte, error) {
	blob, err := readBounded(filepath.Join(s.hostDir(host), childFile))
	if errors.Is(err, fs.ErrNotExist) {
		return Copy{}, nil, ErrNoCopy
	}
	if err != nil {
		return Copy{}, nil, fmt.Errorf("shadow: %s: %w", host, err)
	}
	keyID, gen, err := Peek(blob)
	if err != nil || keyID != s.key.id || gen == 0 {
		return Copy{}, nil, ErrNoCopy
	}
	return Copy{KeyID: keyID, Gen: gen, Hash: Hash(blob), OK: true}, blob, nil
}

func readBounded(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	blob, err := io.ReadAll(io.LimitReader(f, MaxSnapshotBytes+1))
	if err == nil && len(blob) > MaxSnapshotBytes {
		err = fmt.Errorf("%w: over the size limit", ErrDamaged)
	}
	return blob, err
}

// State is what the master states about host for Decide: its key, the newest
// generation it made, and whether its own snapshot of the shadow is there.
// It reads the first lines of files and decrypts nothing, so a snapshot that
// is damaged further in is only found by Acquire, and State says so after.
func (s *Store) State(host string) (Master, error) {
	sh, err := s.hold(host)
	if err != nil {
		return Master{}, err
	}
	defer s.letGo(sh)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	m := Master{KeyID: s.key.id}
	if c, _, err := s.newest(host); err == nil {
		m.Gen, m.Hash = c.Gen, c.Hash
	}
	s.mu.Lock()
	damaged := s.damaged[host]
	s.mu.Unlock()
	if damaged || sh.episodic != nil {
		m.ShadowOK = !damaged
		return m, nil
	}
	f, err := os.Open(filepath.Join(s.hostDir(host), shadowFile))
	switch {
	case err == nil:
		head := make([]byte, maxHeaderLine+1)
		n, _ := io.ReadFull(f, head)
		_ = f.Close()
		keyID, _, err := Peek(head[:n])
		m.ShadowOK = err == nil && keyID == s.key.id
	case errors.Is(err, fs.ErrNotExist):
		// No snapshot is in order for a machine that never had a memory.
		m.ShadowOK = !s.remains(host)
	default:
		return Master{}, fmt.Errorf("shadow: %s: %w", host, err)
	}
	return m, nil
}

// Adopted is what Adopt did.
type Adopted struct {
	// Gen is the generation the shadow now stands at.
	Gen uint64
	// Aside is the file the shadow that was replaced is kept in, for thirty
	// days; empty when there was none. It is a snapshot like any other.
	Aside string
	// Reembed says the new shadow came without vectors: re-embed it, then
	// Flush.
	Reembed bool
}

// Adopt makes blob, a snapshot of host's memory, the shadow: the child's
// copy when the child wins, or the copy the shadow is rebuilt from. c is
// what the holder stated about it.
//
// blob is trusted only after all of this holds: its bytes hash to c.Hash,
// its first line names c's key and generation, it decrypts with the fleet
// key (or with, a recovery identity, which reads a copy made under any fleet
// key), the inside agrees with the first line and names host, both databases
// pass SQLite's integrity check, and the stores open on them. Otherwise the
// error wraps ErrDamaged, ErrForeignKey or ErrNotRecovery and nothing is
// changed.
//
// The shadow that was there is never overwritten: its snapshot is moved
// aside, readable or not. Adopt fails with ErrInUse while the stores are
// held.
//
// A copy read with a recovery identity becomes a new generation under this
// master's key, numbered after it. Send that one (Newest) to the child
// straight away: until the child holds a copy under this key that is newer
// than its old ones, Decide goes on answering NeedsRecoveryKey.
func (s *Store) Adopt(ctx context.Context, host string, c Copy, blob []byte, with *Key) (Adopted, error) {
	if len(blob) > MaxSnapshotBytes || Hash(blob) != c.Hash {
		return Adopted{}, fmt.Errorf("%w: the bytes do not match the stated hash", ErrDamaged)
	}
	if keyID, gen, err := Peek(blob); err != nil || keyID != c.KeyID || gen != c.Gen {
		return Adopted{}, fmt.Errorf("%w: the snapshot is not the generation it is stated to be", ErrDamaged)
	}
	sh, done, err := s.exclusive(host)
	if err != nil {
		return Adopted{}, err
	}
	defer done()
	k := s.key
	if with != nil {
		k = with
	}
	snap, err := openSnapshot(ctx, bytes.NewReader(blob), k, host)
	if err != nil {
		return Adopted{}, fmt.Errorf("shadow: %s: %w", host, err)
	}
	pair, err := openStores(snap.episodic, snap.semantic)
	if err != nil {
		return Adopted{}, fmt.Errorf("shadow: %s: %w", host, err)
	}
	// From here the old shadow gives way. It goes to disk as it is, and the
	// file is moved aside.
	var out Adopted
	if sh.episodic != nil {
		if err := s.flushIfChanged(ctx, sh); err != nil {
			snap.close()
			return Adopted{}, err
		}
		sh.unload()
	}
	if out.Aside, err = s.setAside(host, shadowFile); err != nil {
		snap.close()
		return Adopted{}, err
	}
	sh.gen, sh.minted, sh.fresh = snap.Gen, snap.Content, false
	if err := s.install(ctx, sh, snap.episodic, snap.semantic, pair); err != nil {
		return Adopted{}, err
	}
	sh.disk = recorded{changes: [2]int64{-1, -1}} // no snapshot of this shadow is on disk yet
	out.Reembed = !snap.Embeddings
	if snap.KeyID == s.key.id && !snap.Embeddings {
		// A generation of ours: it is the newest generation on both sides now.
		if err := s.write(host, childFile, blob); err != nil {
			return out, err
		}
		err = s.flush(ctx, sh)
	} else {
		// Read with the recovery identity, or a shadow that had been set
		// aside: the child gets it as a new generation under this key.
		sh.minted = ""
		_, _, err = s.mint(ctx, sh)
	}
	out.Gen = sh.gen
	s.sweepAside(host)
	return out, err
}

// setAside moves a file of host's out of the way, into its aside directory,
// and returns where it went; "" when there was no such file.
func (s *Store) setAside(host, name string) (string, error) {
	from := filepath.Join(s.hostDir(host), name)
	if _, err := os.Lstat(from); errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	dir := filepath.Join(s.hostDir(host), asideDir)
	if err := fsperm.MkdirAll(dir); err != nil {
		return "", err
	}
	now := time.Now()
	to := filepath.Join(dir, strconv.FormatInt(now.UnixNano(), 10)+"-"+name)
	if err := os.Rename(from, to); err != nil {
		return "", err
	}
	// The thirty days run from now, not from when the file was last written.
	_ = os.Chtimes(to, now, now)
	return to, nil
}

// sweepAside removes what has been kept aside for longer than asideKeep.
func (s *Store) sweepAside(host string) {
	dir := filepath.Join(s.hostDir(host), asideDir)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() && time.Since(info.ModTime()) > asideKeep {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// RemoveAside deletes the shadows kept aside for host. A forget needs it:
// what was set aside before the forget still holds what was forgotten.
func (s *Store) RemoveAside(host string) error {
	if !validHost(host) {
		return fmt.Errorf("shadow: %q is not a machine id", host)
	}
	return os.RemoveAll(filepath.Join(s.hostDir(host), asideDir))
}

// Remove deletes everything the master keeps of host's memory, in RAM and on
// disk, for a machine that is revoked with its memory or that starts again
// after Decide said Lost. It fails with ErrInUse while the stores are held.
func (s *Store) Remove(host string) error {
	sh, done, err := s.exclusive(host)
	if err != nil {
		return err
	}
	defer done()
	if sh.episodic != nil {
		sh.unload()
	}
	s.mu.Lock()
	delete(s.damaged, host)
	s.mu.Unlock()
	return os.RemoveAll(s.hostDir(host))
}

// run is the background pass.
func (s *Store) run() {
	defer close(s.done)
	t := time.NewTicker(s.opt.FlushAfter)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.pass()
		}
	}
}

// pass looks at every shadow in RAM once.
func (s *Store) pass() {
	s.mu.Lock()
	all := s.shadows()
	s.mu.Unlock()
	for _, sh := range all {
		if err := s.look(sh); err != nil && s.opt.OnError != nil {
			s.opt.OnError(sh.host, err)
		}
	}
}

// look writes sh out if its content changed, and drops it if it is idle.
func (s *Store) look(sh *shadow) error {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	s.mu.Lock()
	idle := sh.refs == 0 && time.Since(sh.lastUsed) >= s.opt.IdleAfter
	if idle && sh.episodic == nil {
		delete(s.hosts, sh.host)
	}
	s.mu.Unlock()
	if sh.episodic == nil {
		return nil
	}
	probe, cancel := context.WithTimeout(context.Background(), busyWait)
	n, err := sh.changes(probe)
	cancel()
	switch {
	case errors.Is(err, errMemLost):
		// The database is gone, and with it the writes since the last
		// snapshot. The next Acquire loads that snapshot.
		sh.unload()
		return err
	case errors.Is(err, context.DeadlineExceeded):
		return nil // in use: the next pass looks again
	case err != nil:
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), passLimit)
	defer cancel()
	if idle {
		// Still nobody holds it: Acquire counts itself in before it waits
		// for sh.mu, which this pass has.
		s.mu.Lock()
		idle = sh.refs == 0
		s.mu.Unlock()
	}
	if idle {
		if err := s.settle(ctx, sh); err != nil {
			return err
		}
		sh.unload()
		return nil
	}
	if n == sh.seen {
		return nil
	}
	content, err := contentHash(ctx, sh.episodic.DB, sh.semantic.DB)
	if err != nil {
		return err
	}
	// Content the same: recall counted an access, or vectors were rewritten.
	// Neither is worth a snapshot of its own; they go out with the next one.
	if content != sh.content {
		if err := s.flush(ctx, sh); err != nil {
			return err // and seen stays, so the next look tries again
		}
	}
	sh.seen = n
	return nil
}

// settle leaves nothing in RAM that is not on disk: the snapshot is written
// if anything changed, and a generation is made if the content is not the
// last generation's. The caller holds sh.mu.
func (s *Store) settle(ctx context.Context, sh *shadow) error {
	if _, _, err := s.mint(ctx, sh); err != nil {
		return err
	}
	return s.flushIfChanged(ctx, sh)
}
