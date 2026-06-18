package sme

import "sync"

// Live is the goroutine-safe holder for the rendered SME doc. The background
// ReflectionJob writes the freshly-synthesized markdown via Set; the agent's
// turn loop reads it via String() each turn (cheap — no per-turn DB read). A
// pointer-swap-style replacement, so a turn mid-read keeps its snapshot. Mirrors
// soul.Live. The zero value is a valid empty holder (String() returns "").
type Live struct {
	mu   sync.RWMutex
	text string
}

// NewLive returns an empty live holder.
func NewLive() *Live { return &Live{} }

// Set replaces the rendered SME text.
func (l *Live) Set(text string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.text = text
	l.mu.Unlock()
}

// String returns the current rendered SME text ("" if unset). Nil-safe so a
// caller can hold a nil *Live to mean "no SME".
func (l *Live) String() string {
	if l == nil {
		return ""
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.text
}
