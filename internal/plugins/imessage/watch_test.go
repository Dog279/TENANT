package imessage

import (
	"context"
	"testing"
	"time"
)

// fakeSource serves canned rows with ROWID > after, ascending.
type fakeSource struct{ rows []InboundMessage }

func (f *fakeSource) MessagesSince(_ context.Context, after int64, limit int) ([]InboundMessage, error) {
	var out []InboundMessage
	for _, m := range f.rows {
		if m.RowID > after {
			out = append(out, m)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// memStore is an in-memory cursorStore.
type memStore struct{ m map[string]int64 }

func newMemStore() *memStore { return &memStore{m: map[string]int64{}} }
func (s *memStore) GetInt64(_ context.Context, key string) (int64, bool, error) {
	v, ok := s.m[key]
	return v, ok, nil
}
func (s *memStore) SetInt64(_ context.Context, key string, n int64) error {
	s.m[key] = n
	return nil
}

func inbound(rowID int64, chat, from, text string, fromMe bool) InboundMessage {
	return InboundMessage{
		Message:  Message{From: from, Text: text, IsFromMe: fromMe},
		RowID:    rowID,
		ChatGUID: chat,
	}
}

func sampleRows() []InboundMessage {
	return []InboundMessage{
		inbound(1, "c1", "+15551112222", "hi", false),
		inbound(2, "c1", "me", "yo", true), // is_from_me → must be skipped
		inbound(3, "c2", "+15559998888", "sup", false),
	}
}

func TestWatcher_IsFromMeAndCursor(t *testing.T) {
	src := &fakeSource{rows: sampleRows()}
	store := newMemStore()
	w, err := NewWatcher(WatchConfig{Source: src, Store: store, Account: "acct"})
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	got, err := w.Poll(context.Background(), 100)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	// row 2 (is_from_me) dropped → only 1 and 3 surface.
	if len(got) != 2 || got[0].RowID != 1 || got[1].RowID != 3 {
		t.Fatalf("is_from_me filter wrong: %+v", got)
	}
	// cursor advanced past the highest ROWID seen, and persisted.
	if w.Cursor() != 3 {
		t.Errorf("cursor = %d, want 3", w.Cursor())
	}
	if store.m["imessage_cursor:acct"] != 3 {
		t.Errorf("cursor not persisted: %v", store.m)
	}
	// second poll: nothing new (monotonic, no replay).
	if got2, _ := w.Poll(context.Background(), 100); len(got2) != 0 {
		t.Errorf("second poll should be empty, got %+v", got2)
	}
}

func TestWatcher_PersistedCursorResumes(t *testing.T) {
	src := &fakeSource{rows: sampleRows()}
	store := newMemStore()
	store.m["imessage_cursor:acct"] = 2 // pretend we already processed up to ROWID 2
	w, _ := NewWatcher(WatchConfig{Source: src, Store: store, Account: "acct"})
	got, _ := w.Poll(context.Background(), 100)
	if len(got) != 1 || got[0].RowID != 3 {
		t.Fatalf("should resume after persisted cursor: %+v", got)
	}
}

func TestWatcher_Allowlist(t *testing.T) {
	src := &fakeSource{rows: sampleRows()}
	w, _ := NewWatcher(WatchConfig{
		Source:    src,
		AllowFrom: []string{"+1 (555) 111-2222"}, // normalized to +15551112222
	})
	got, _ := w.Poll(context.Background(), 100)
	if len(got) != 1 || got[0].From != "+15551112222" {
		t.Fatalf("allowlist should pass only the listed sender: %+v", got)
	}
}

func TestWatcher_EchoDedup(t *testing.T) {
	src := &fakeSource{rows: sampleRows()}
	w, _ := NewWatcher(WatchConfig{Source: src})
	// We "sent" exactly the text that inbound row 1 carries on chat c1;
	// its echo must be dropped even though is_from_me is 0.
	w.RecordSent("c1", "hi")
	got, _ := w.Poll(context.Background(), 100)
	if len(got) != 1 || got[0].RowID != 3 {
		t.Fatalf("echo of our own send should be dropped: %+v", got)
	}
}

func TestEchoCache_TTL(t *testing.T) {
	c := newEchoCache(time.Minute)
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	c.record("c1", "hi")
	if !c.seen("c1", "hi") {
		t.Fatal("should be seen immediately")
	}
	now = now.Add(2 * time.Minute) // past TTL
	if c.seen("c1", "hi") {
		t.Fatal("should expire after TTL")
	}
}

func TestNewWatcher_RequiresSource(t *testing.T) {
	if _, err := NewWatcher(WatchConfig{}); err == nil {
		t.Fatal("nil source must error")
	}
}
