package fleet

// memory.go is the link's part in a machine's memory (TEN-532,
// docs/plans/gap6-fleet-plan.md section 6). The master asks a child which
// copies of its memory it keeps, sends it one, fetches one back, and tells it
// to drop the ones older than a copy it keeps. A copy is a snapshot the child
// cannot read (internal/fleet/shadow): all the child checks is that the bytes
// are the bytes it was sent.
//
// A snapshot can be larger than a frame, so put and get move it in pieces of
// at most memoryChunk, in order, and the whole is checked against its
// SHA-256 at the end, as fleet/update does with an archive. Only the master
// asks; a child asks its master nothing about memory.
//
// Everything a child says here is a machine's word: its list is bounded and
// parsed by shadow.ParseCopies, every piece it sends is bounded and in order,
// and what it sends is used only when the whole hashes to what was asked for.
// The caller (the master's memory) still opens nothing it has not verified.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"tenant/internal/fleet/shadow"
	"tenant/internal/mcp"
)

const (
	methodMemoryList  = "fleet/memory/list"
	methodMemoryPut   = "fleet/memory/put"
	methodMemoryGet   = "fleet/memory/get"
	methodMemoryPurge = "fleet/memory/purge"

	codeStale    = -32007 // fleet/memory/put: the child keeps a newer generation under that key
	codeConflict = -32008 // fleet/memory/put: the child keeps that generation with other content
	codeNoCopy   = -32009 // fleet/memory/get, purge: the child does not keep that copy intact

	// memoryChunk is the most snapshot bytes one put or get carries: a frame
	// takes it once it is base64.
	memoryChunk = MaxUpdateChunk
	// memoryTransferTime bounds one whole put or get; each piece has
	// requestTime of its own.
	memoryTransferTime = 10 * time.Minute
	// memoryIdle is how long a child keeps a copy half sent or half fetched
	// with nothing more asked of it.
	memoryIdle = 2 * time.Minute
)

// maxMemoryBytes bounds a snapshot on the link, both ways: the most the
// shadow package itself takes.
const maxMemoryBytes = shadow.MaxSnapshotBytes

var (
	// ErrNoMemory is a machine whose child does not know the memory methods:
	// a Tenant older than its master's, or one that keeps no copy. It keeps no
	// copy of its memory yet. The link is otherwise unharmed.
	ErrNoMemory = errors.New("fleet: this machine keeps no copy of its memory (its Tenant does not know how yet)")
	// errNotServed is a request the child answered with "no such method".
	errNotServed = errors.New("fleet: the machine does not serve that method")
)

// memoryCopy names one copy on the wire.
type memoryCopy struct {
	Key  string `json:"key"`
	Gen  uint64 `json:"gen"`
	Hash string `json:"hash"`
}

func wireCopy(c shadow.Copy) memoryCopy { return memoryCopy{Key: c.KeyID, Gen: c.Gen, Hash: c.Hash} }

func (w memoryCopy) copy() shadow.Copy { return shadow.Copy{KeyID: w.Key, Gen: w.Gen, Hash: w.Hash} }

// valid accepts a name a master could have given a copy: the key id and hash
// in lowercase hex, a generation from 1.
func (w memoryCopy) valid() bool {
	return len(w.Key) == 32 && lowerHex(w.Key) && len(w.Hash) == 64 && lowerHex(w.Hash) && w.Gen >= 1 && w.Gen <= 1<<63-1
}

func lowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

type memoryPut struct {
	memoryCopy
	Size   int64  `json:"size"`
	Offset int64  `json:"offset"`
	Data   []byte `json:"data"`
}

type memoryPutAck struct {
	Received int64 `json:"received"`
}

type memoryGet struct {
	memoryCopy
	Offset int64 `json:"offset"`
}

type memoryPiece struct {
	Size int64  `json:"size"`
	Data []byte `json:"data"`
}

// --- the master's end ---

// memoryLink is the link to a machine for its memory, or why there is none.
func (m *Manager) memoryLink(id string) (*link, error) {
	return m.live(id, true)
}

// memoryErr turns a child's "no such method" into ErrNoMemory.
func memoryErr(err error) error {
	if errors.Is(err, errNotServed) {
		return fmt.Errorf("%w (%v)", ErrNoMemory, err)
	}
	return err
}

// MemoryList asks a machine which copies of its memory it keeps. What comes
// back has been through shadow.ParseCopies. A machine whose child does not
// know the method fails with ErrNoMemory.
func (m *Manager) MemoryList(ctx context.Context, id string) ([]shadow.Copy, error) {
	l, err := m.memoryLink(id)
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if err := l.request(ctx, requestTime, methodMemoryList, nil, &raw); err != nil {
		return nil, memoryErr(err)
	}
	copies, err := shadow.ParseCopies(raw)
	if err != nil {
		return nil, fmt.Errorf("fleet: machine %s: %w", id, err)
	}
	return copies, nil
}

// MemoryPut sends a machine one copy of its memory, in pieces, and returns
// once the child has it whole and on disk. It fails with an error wrapping
// shadow.ErrStale or shadow.ErrConflict when the child refuses it for those
// reasons, and with ErrNoMemory for a child that keeps no copies.
func (m *Manager) MemoryPut(ctx context.Context, id string, c shadow.Copy, blob []byte) error {
	w := wireCopy(c)
	if !w.valid() || len(blob) == 0 || len(blob) > maxMemoryBytes || shadow.Hash(blob) != c.Hash {
		return errors.New("fleet: not a copy that can be sent: its name, size or hash is wrong")
	}
	l, err := m.memoryLink(id)
	if err != nil {
		return err
	}
	ctx, cancel := withDeadline(ctx, memoryTransferTime)
	defer cancel()
	size := int64(len(blob))
	for off := int64(0); ; {
		end := min(off+memoryChunk, size)
		var ack memoryPutAck
		if err := l.request(ctx, requestTime, methodMemoryPut, memoryPut{memoryCopy: w, Size: size, Offset: off, Data: blob[off:end]}, &ack); err != nil {
			return memoryErr(err)
		}
		if ack.Received != end {
			return fmt.Errorf("fleet: machine %s says it holds %d bytes of the copy, not %d", id, ack.Received, end)
		}
		if end == size {
			return nil
		}
		off = end
	}
}

// MemoryGet fetches one copy of its memory from a machine, in pieces. It
// returns the bytes only when they are whole, no larger than a snapshot can
// be, and hash to c.Hash; the caller still decrypts and checks them before
// anything is made of them. A copy the child does not keep intact fails with
// an error wrapping shadow.ErrNoCopy.
func (m *Manager) MemoryGet(ctx context.Context, id string, c shadow.Copy) ([]byte, error) {
	w := wireCopy(c)
	if !w.valid() {
		return nil, errors.New("fleet: not the name of a copy")
	}
	l, err := m.memoryLink(id)
	if err != nil {
		return nil, err
	}
	ctx, cancel := withDeadline(ctx, memoryTransferTime)
	defer cancel()
	var blob []byte
	size := int64(-1)
	for size < 0 || int64(len(blob)) < size {
		var raw json.RawMessage
		if err := l.request(ctx, requestTime, methodMemoryGet, memoryGet{memoryCopy: w, Offset: int64(len(blob))}, &raw); err != nil {
			return nil, memoryErr(err)
		}
		piece, err := parseMemoryPiece(raw, size, int64(len(blob)))
		if err != nil {
			return nil, fmt.Errorf("fleet: machine %s, copy %d: %w", id, c.Gen, err)
		}
		size = piece.Size
		blob = append(blob, piece.Data...)
	}
	if shadow.Hash(blob) != c.Hash {
		return nil, fmt.Errorf("fleet: machine %s sent a copy whose bytes do not hash to what it listed", id)
	}
	if key, gen, err := shadow.Peek(blob); err != nil || key != c.KeyID || gen != c.Gen {
		return nil, fmt.Errorf("fleet: machine %s sent a copy that is not the generation it listed", id)
	}
	return blob, nil
}

// parseMemoryPiece reads one piece of a copy a child sent: exactly the
// fields of memoryPiece, a size that is a snapshot's and the same as the
// pieces before it (size, or -1 for the first), and at least one byte that
// fits in a frame and in the copy after the have bytes already taken.
func parseMemoryPiece(raw []byte, size, have int64) (memoryPiece, error) {
	var p memoryPiece
	if len(raw) > 2*memoryChunk {
		return p, errors.New("the piece is too large")
	}
	if err := decodeStrict(raw, &p); err != nil {
		return memoryPiece{}, fmt.Errorf("the piece cannot be read: %w", err)
	}
	switch {
	case p.Size < 1 || p.Size > maxMemoryBytes:
		return memoryPiece{}, fmt.Errorf("a copy of %d bytes is refused", p.Size)
	case size >= 0 && p.Size != size:
		return memoryPiece{}, errors.New("the copy changed its size while it was sent")
	case len(p.Data) == 0 || len(p.Data) > memoryChunk || have+int64(len(p.Data)) > p.Size:
		return memoryPiece{}, errors.New("a piece is empty, too large, or runs past the end of the copy")
	}
	return p, nil
}

// MemoryPurge tells a machine to drop every copy under keep's key older than
// keep, which it must hold intact: after a forget. The child refuses, with an
// error wrapping shadow.ErrNoCopy, when it does not hold keep.
func (m *Manager) MemoryPurge(ctx context.Context, id string, keep shadow.Copy) error {
	w := wireCopy(keep)
	if !w.valid() {
		return errors.New("fleet: not the name of a copy")
	}
	l, err := m.memoryLink(id)
	if err != nil {
		return err
	}
	return memoryErr(l.request(ctx, requestTime, methodMemoryPurge, w, nil))
}

// --- the child's end ---

// memoryEnd is a child's side of the memory methods: one copy coming in and
// one going out at a time, each dropped when the master leaves it for
// memoryIdle.
type memoryEnd struct {
	mu  sync.Mutex // held across the store's disk work: one memory request at a time
	in  *transfer
	out *transfer
}

type transfer struct {
	c     shadow.Copy
	size  int64
	buf   []byte
	timer *time.Timer
}

// drop forgets a transfer. The caller holds e.mu.
func (e *memoryEnd) drop(t **transfer) {
	if *t != nil && (*t).timer != nil {
		(*t).timer.Stop()
	}
	*t = nil
}

// keep (re)starts a transfer's idle clock. The caller holds e.mu.
func (e *memoryEnd) keep(t **transfer) {
	tr := *t
	if tr.timer != nil {
		tr.timer.Stop()
	}
	tr.timer = time.AfterFunc(memoryIdle, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if *t == tr {
			*t = nil
		}
	})
}

// memory answers the memory methods from c.cfg.Memory.
func (c *Client) memory(method string, params json.RawMessage) (json.RawMessage, error) {
	e, store := &c.mem, c.cfg.Memory
	e.mu.Lock()
	defer e.mu.Unlock()
	switch method {
	case methodMemoryList:
		copies, err := store.List()
		if err != nil {
			return nil, mcp.NewError(codeUnavailable, "fleet: this machine cannot list its copies: "+err.Error())
		}
		if copies == nil {
			copies = []shadow.Copy{}
		}
		return encode(copies)
	case methodMemoryPut:
		return e.put(store, params)
	case methodMemoryGet:
		return e.get(store, params)
	}
	var w memoryCopy
	if err := json.Unmarshal(params, &w); err != nil || !w.valid() {
		return nil, mcp.NewError(mcp.ErrInvalidParams, "fleet: fleet/memory/purge needs the name of a copy")
	}
	if err := store.Purge(w.copy()); err != nil {
		return nil, memoryRefusal(err)
	}
	return emptyObject, nil
}

// memoryRefusal is the child's answer for a refusal of its store.
func memoryRefusal(err error) error {
	code := codeUnavailable
	switch {
	case errors.Is(err, shadow.ErrStale):
		code = codeStale
	case errors.Is(err, shadow.ErrConflict):
		code = codeConflict
	case errors.Is(err, shadow.ErrNoCopy):
		code = codeNoCopy
	}
	return mcp.NewError(code, "fleet: "+err.Error())
}

func (e *memoryEnd) put(store *shadow.ChildStore, params json.RawMessage) (json.RawMessage, error) {
	var p memoryPut
	if err := json.Unmarshal(params, &p); err != nil || !p.valid() || p.Size < 1 || p.Size > maxMemoryBytes ||
		len(p.Data) == 0 || len(p.Data) > memoryChunk || p.Offset < 0 || p.Offset+int64(len(p.Data)) > p.Size {
		return nil, mcp.NewError(mcp.ErrInvalidParams, "fleet: not a valid piece of a copy")
	}
	c := p.copy()
	if p.Offset == 0 {
		e.drop(&e.in)
		e.in = &transfer{c: c, size: p.Size}
	}
	in := e.in
	if in == nil || in.c != c || in.size != p.Size || int64(len(in.buf)) != p.Offset {
		e.drop(&e.in)
		return nil, mcp.NewError(mcp.ErrInvalidParams, "fleet: that piece does not follow the copy being received; start again from 0")
	}
	in.buf = append(in.buf, p.Data...)
	if int64(len(in.buf)) < in.size {
		e.keep(&e.in)
		return encode(memoryPutAck{Received: int64(len(in.buf))})
	}
	blob := in.buf
	e.drop(&e.in)
	if err := store.Put(c, blob); err != nil {
		return nil, memoryRefusal(err)
	}
	return encode(memoryPutAck{Received: int64(len(blob))})
}

func (e *memoryEnd) get(store *shadow.ChildStore, params json.RawMessage) (json.RawMessage, error) {
	var p memoryGet
	if err := json.Unmarshal(params, &p); err != nil || !p.valid() || p.Offset < 0 || p.Offset > maxMemoryBytes {
		return nil, mcp.NewError(mcp.ErrInvalidParams, "fleet: fleet/memory/get needs the name of a copy and an offset")
	}
	c := p.copy()
	if out := e.out; p.Offset == 0 || out == nil || out.c != c {
		e.drop(&e.out)
		blob, err := store.Get(c) // read whole and checked against its hash once per fetch
		if err != nil {
			return nil, memoryRefusal(err)
		}
		e.out = &transfer{c: c, size: int64(len(blob)), buf: blob}
	}
	out := e.out
	if p.Offset >= out.size {
		return nil, mcp.NewError(mcp.ErrInvalidParams, "fleet: that offset is past the end of the copy")
	}
	end := min(p.Offset+memoryChunk, out.size)
	raw, err := encode(memoryPiece{Size: out.size, Data: out.buf[p.Offset:end]})
	if end == out.size {
		e.drop(&e.out)
	} else {
		e.keep(&e.out)
	}
	return raw, err
}
