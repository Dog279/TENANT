package shadow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Master is what the master states when a child reconnects.
type Master struct {
	// KeyID is the fleet key the master holds.
	KeyID string
	// Gen and Hash name the newest generation the master made under that
	// key; Gen is 0 when it has made none.
	Gen  uint64
	Hash string
	// ShadowOK is false when the master's own snapshot of the shadow is
	// damaged or gone, or cannot be read with this key.
	ShadowOK bool
	// ShadowForeign says the master's own snapshot, or the generation file
	// beside it, was made under another fleet key: this master's key file
	// was replaced. Reading them takes the recovery identity.
	ShadowForeign bool
}

// Action is what to do about a machine's memory when its child reconnects.
type Action int

const (
	// Nothing: the two sides agree, or neither has anything.
	Nothing Action = iota
	// Push: send the master's newest generation to the child.
	Push
	// Pull: the child's copy wins. Fetch it, verify it, swap it in as the
	// shadow and re-embed; the shadow it replaces is kept aside.
	Pull
	// Rebuild: the master's shadow is damaged or gone. Rebuild it from the
	// copy named, and re-embed.
	Rebuild
	// NeedsRecoveryKey: the child's memory, or the master's own snapshot of
	// it, is under another fleet key. The machine is marked, and nothing is
	// sent, pulled or overwritten until the operator brings the recovery
	// identity.
	NeedsRecoveryKey
	// Lost: the master's shadow is damaged or gone and no copy of it is left
	// on either side. Nothing is done; the operator decides whether the
	// machine starts again with an empty memory.
	Lost
)

func (a Action) String() string {
	switch a {
	case Nothing:
		return "nothing"
	case Push:
		return "push"
	case Pull:
		return "pull"
	case Rebuild:
		return "rebuild"
	case NeedsRecoveryKey:
		return "needs recovery key"
	case Lost:
		return "lost"
	}
	return fmt.Sprintf("Action(%d)", int(a))
}

// Decision is Decide's answer.
type Decision struct {
	Action Action
	// Copy is the child's copy to fetch, for Pull and for Rebuild.
	Copy Copy
	// Own is set on a Rebuild from the master's own newest generation file,
	// when the child holds nothing as new: nothing is fetched.
	Own bool
	// Diverged is set on a Pull of the generation the master also made, with
	// other content. That only happens after the master was restored from an
	// older backup and then wrote. The operator is told, and the master's
	// side is what is kept aside.
	Diverged bool
}

// Decide is the "who wins" table (gap6-fleet-plan.md, section 6): the child
// wins, and nothing is overwritten that the master cannot read.
//
// child is what the child states it keeps, in any order. Only its intact
// copies count, so a damaged newest one gives way to the next older. A copy
// whose fields are not ones a master could have written counts as damaged.
// A caller that fetches a copy and finds it does not verify after all (Adopt
// refuses it) calls again with that copy's OK cleared. If a later Push is
// then refused by the child as stale, because the child still takes that
// copy for a newer one, Store.Skip numbers the master's generation past it.
func Decide(m Master, child []Copy) Decision {
	var newest *Copy // the child's newest intact copy
	for i := range child {
		if c := &child[i]; c.OK && c.valid() && (newest == nil || decidesBefore(m, c, newest)) {
			newest = c
		}
	}
	switch {
	case m.ShadowForeign:
		// Whatever the child has, the master holds a memory of this machine
		// it cannot read. Rebuilding from a copy, or calling it lost, would
		// be acting as if that memory were not there.
		return Decision{Action: NeedsRecoveryKey}
	case newest == nil && !m.ShadowOK && m.Gen > 0:
		return Decision{Action: Rebuild, Own: true}
	case newest == nil && m.ShadowOK && m.Gen > 0:
		return Decision{Action: Push}
	case newest == nil && !m.ShadowOK:
		return Decision{Action: Lost}
	case newest == nil:
		return Decision{Action: Nothing}
	case newest.KeyID != m.KeyID:
		return Decision{Action: NeedsRecoveryKey}
	case !m.ShadowOK && m.Gen > newest.Gen:
		return Decision{Action: Rebuild, Own: true}
	case !m.ShadowOK:
		return Decision{Action: Rebuild, Copy: *newest}
	case m.Gen > newest.Gen:
		return Decision{Action: Push}
	case m.Gen < newest.Gen:
		return Decision{Action: Pull, Copy: *newest}
	case m.Hash != newest.Hash:
		return Decision{Action: Pull, Copy: *newest, Diverged: true}
	}
	return Decision{Action: Nothing}
}

// decidesBefore orders the child's intact copies: the higher generation; of
// one generation, a copy under another key first, so that it is never passed
// over; then one that differs from the master's, because the child's stands;
// then by hash and key id, so that the answer does not depend on the order
// the child listed them in.
func decidesBefore(m Master, a, b *Copy) bool {
	if a.Gen != b.Gen {
		return a.Gen > b.Gen
	}
	if x, y := a.KeyID != m.KeyID, b.KeyID != m.KeyID; x != y {
		return x
	}
	if x, y := a.Hash != m.Hash, b.Hash != m.Hash; x != y {
		return x
	}
	if a.Hash != b.Hash {
		return a.Hash < b.Hash
	}
	return a.KeyID < b.KeyID
}

// maxStatement bounds what a child may send as its statement: the copies
// under maxKeys keys, with room to spare.
const (
	maxStatement       = 64 << 10
	maxStatementCopies = 4 * maxKeys * keepCopies
)

// ParseCopies reads a child's statement of the copies it keeps, the JSON
// array json.Marshal makes of ChildStore.List's result. It comes from
// another machine: its size and length are bounded, unknown fields are
// refused, and a copy with a field no master could have written is refused
// rather than passed on.
func ParseCopies(data []byte) ([]Copy, error) {
	if len(data) > maxStatement {
		return nil, errors.New("shadow: the child's statement is too large")
	}
	var copies []Copy
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&copies); err != nil {
		return nil, fmt.Errorf("shadow: the child's statement: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("shadow: the child's statement: something follows the list")
	}
	if len(copies) > maxStatementCopies {
		return nil, errors.New("shadow: the child's statement lists too many copies")
	}
	for _, c := range copies {
		if !c.valid() {
			return nil, errors.New("shadow: the child's statement: not a valid key id, generation or hash")
		}
	}
	return copies, nil
}
