// Package working implements T1 of Tenant's memory architecture: the
// in-process sliding window of the current conversation. The working
// set is what the agent "remembers" within a session, mirrored to
// the T5 archive on every append for crash safety.
//
// Concurrency: a Set is safe for concurrent Append + Messages calls
// (one writer, many readers via copy-out). Compaction (replacing old
// turns with summaries) is not implemented in v1 — Trim drops the
// oldest N turns wholesale. Hierarchical summarization is a v1.1 job
// once we have a summarizer LLM available.
package working

import (
	"sync"
	"time"

	"tenant/internal/model"
)

// Message is one turn in the working set. Mirrors model.Message but adds
// Timestamp (archive provenance) plus optional Kind/Meta used by compaction.
// Kind tags non-ordinary messages (a compaction summary, an elided tool
// result); Meta carries structured provenance (source ranges, elision byte
// counts, call ids). Zero values mean "ordinary turn" — fully backward
// compatible with every existing producer.
type Message struct {
	Role       string           // user | assistant | tool | system
	Content    string           // text content
	ToolCalls  []model.ToolCall // emitted by assistant
	ToolCallID string           // for role=tool, references the call this responds to
	Timestamp  time.Time
	Kind       string         // optional compaction tag (e.g. "tool-elided"); "" = ordinary turn
	Meta       map[string]any // optional structured provenance; nil = none
}

// Set is the working memory. Bounded growth is the caller's job
// (call Trim or Compact when the assembler reports overflow).
type Set struct {
	mu       sync.Mutex
	messages []Message
}

// New returns an empty Set.
func New() *Set { return &Set{} }

// Append adds m to the end of the set. Timestamp defaults to now if
// zero. Safe for concurrent use.
func (s *Set) Append(m Message) {
	if m.Timestamp.IsZero() {
		m.Timestamp = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, m)
}

// Messages returns a copy of the current message slice. Safe to
// retain — callers can't mutate internal state.
func (s *Set) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Message, len(s.messages))
	copy(out, s.messages)
	return out
}

// Len returns the current message count.
func (s *Set) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.messages)
}

// Trim keeps only the most recent n messages. If n >= current length,
// no change. Use this when the assembler reports working-set overflow.
// Returns the number of messages dropped.
func (s *Set) Trim(n int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n < 0 {
		n = 0
	}
	if n >= len(s.messages) {
		return 0
	}
	dropped := len(s.messages) - n
	s.messages = s.messages[dropped:]
	return dropped
}

// Replace swaps the entire message slice — used by context compaction to
// substitute a run of old turns with a single summary message followed by
// the protected recent tail. Copies the input so the caller can't mutate
// internal state afterward.
func (s *Set) Replace(msgs []Message) {
	cp := make([]Message, len(msgs))
	copy(cp, msgs)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = cp
}

// Reset clears the set. Used between sessions or after the user asks
// the agent to "start over".
func (s *Set) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = nil
}
