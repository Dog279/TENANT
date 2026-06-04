package soul

import "sync/atomic"

// Live is a concurrency-safe holder for the agent's in-use soul. The
// agent reads it once per turn (Load) while the operator edits it from
// the dashboard/TUI (Store) — without this, an unsynchronized struct
// copy raced the assembler's Render mid-turn (a torn read).
//
// The discipline is pointer-swap, not in-place mutation: a published
// *Soul is treated as IMMUTABLE. An edit clones it, mutates the clone,
// and Stores the new pointer. A turn that already Loaded the old pointer
// finishes against a consistent snapshot; the next turn sees the new one.
// Mirrors the agent's atomic.Pointer[model.Router] live-swap pattern.
type Live struct {
	ptr atomic.Pointer[Soul]
}

// NewLive wraps s as the initial live soul. s may be nil.
func NewLive(s *Soul) *Live {
	l := &Live{}
	l.ptr.Store(s)
	return l
}

// Load returns the current soul snapshot. Callers must not mutate it —
// it is shared with concurrent readers. Use Edit to change the soul.
func (l *Live) Load() *Soul {
	if l == nil {
		return nil
	}
	return l.ptr.Load()
}

// Store swaps in a new soul snapshot. The caller hands off ownership —
// it must not mutate sl after Store.
func (l *Live) Store(sl *Soul) {
	if l == nil {
		return
	}
	l.ptr.Store(sl)
}

// Edit clones the current soul, runs mutate on the clone, Stores it, and
// returns the new soul. The clone deep-copies the slice fields so the
// mutation never aliases the backing arrays of the still-readable
// published soul. mutate sees a private copy; a nil current soul yields a
// zero-value clone for mutate to populate.
func (l *Live) Edit(mutate func(*Soul)) *Soul {
	if l == nil {
		return nil
	}
	next := l.ptr.Load().Clone()
	mutate(next)
	l.ptr.Store(next)
	return next
}

// Clone returns a deep copy safe to mutate without affecting the
// receiver: the slice fields (Values/Preferences/Facts/Instructions) get
// fresh backing arrays. A nil receiver yields a fresh zero-value *Soul.
func (s *Soul) Clone() *Soul {
	if s == nil {
		return &Soul{}
	}
	c := *s // scalar fields + Meta copy directly
	c.Values.Items = cloneStrings(s.Values.Items)
	c.User.Preferences = cloneStrings(s.User.Preferences)
	c.User.Facts = cloneStrings(s.User.Facts)
	c.Instructions.Items = cloneStrings(s.Instructions.Items)
	return &c
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}
