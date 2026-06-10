package imessage

// Cross-platform tests for the native chat.db reader. They build a
// synthetic database with Apple's schema (no Full Disk Access, no Mac
// required) and assert the reader's shape, ordering, Mac-time conversion,
// attributedBody fallback, and the watcher read primitive. These run on
// Windows/Linux/macOS because the SQL layer is build-tag-free.

import (
	"context"
	"database/sql"
	"encoding/binary"
	"path/filepath"
	"testing"
	"unicode/utf16"

	_ "modernc.org/sqlite"
)

// macNanos converts a Unix-seconds time into chat.db's nanoseconds-since-
// 2001 representation.
func macNanos(unixSec int64) int64 { return (unixSec - macEpochOffset) * 1e9 }

// mkAttrBody wraps payload bytes in a minimal streamtyped envelope of the
// form the parser expects: ...NSString <bookkeeping> 84 01 '+' <varint> <bytes>.
func mkAttrBody(payload []byte) []byte {
	out := []byte("streamtyped\x00\x00prefix-junk")
	out = append(out, []byte("NSString")...)
	out = append(out, 0x01, 0x95, 0x84, 0x01, '+') // class bookkeeping + type descriptor '+'
	n := len(payload)
	switch {
	case n < 0x80:
		out = append(out, byte(n))
	case n <= 0xFFFF:
		var two [2]byte
		binary.LittleEndian.PutUint16(two[:], uint16(n))
		out = append(out, 0x81, two[0], two[1])
	default:
		var four [4]byte
		binary.LittleEndian.PutUint32(four[:], uint32(n))
		out = append(out, 0x82, four[0], four[1], four[2], four[3])
	}
	out = append(out, payload...)
	out = append(out, []byte("trailing-junk")...)
	return out
}

func utf16leBytes(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, len(u)*2)
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[i*2:], c)
	}
	return b
}

// buildSyntheticDB creates a temp chat.db with Apple's schema + fixtures
// and returns an open handle. base is the Unix-seconds anchor for dates.
func buildSyntheticDB(t *testing.T) (*sql.DB, int64) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chat.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	schema := []string{
		`CREATE TABLE handle (ROWID INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT);`,
		`CREATE TABLE chat (ROWID INTEGER PRIMARY KEY AUTOINCREMENT, guid TEXT, chat_identifier TEXT, display_name TEXT);`,
		`CREATE TABLE message (ROWID INTEGER PRIMARY KEY AUTOINCREMENT, guid TEXT, text TEXT, attributedBody BLOB, handle_id INTEGER, is_from_me INTEGER, date INTEGER);`,
		`CREATE TABLE chat_message_join (chat_id INTEGER, message_id INTEGER);`,
		`CREATE TABLE chat_handle_join (chat_id INTEGER, handle_id INTEGER);`,
	}
	for _, s := range schema {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}

	const base = 1780000000 // ~2026
	exec := func(q string, args ...any) {
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	// handles
	exec(`INSERT INTO handle (ROWID, id) VALUES (1, '+15551230000'), (2, 'mom@example.com')`)
	// chats: 1:1 (no display name) + group
	exec(`INSERT INTO chat (ROWID, guid, chat_identifier, display_name) VALUES
		(1, 'iMessage;-;+15551230000', '+15551230000', ''),
		(2, 'iMessage;+;chat99', 'chat99', 'Team')`)
	exec(`INSERT INTO chat_handle_join (chat_id, handle_id) VALUES (1,1), (2,1), (2,2)`)

	// messages: plain text, NULL-text+attributedBody (with emoji), is_from_me mix.
	exec(`INSERT INTO message (ROWID, guid, text, attributedBody, handle_id, is_from_me, date) VALUES (?,?,?,?,?,?,?)`,
		1, "m1", "hey there", nil, 1, 0, macNanos(base+100))
	exec(`INSERT INTO message (ROWID, guid, text, attributedBody, handle_id, is_from_me, date) VALUES (?,?,?,?,?,?,?)`,
		2, "m2", nil, mkAttrBody([]byte("on my way 👋")), 0, 1, macNanos(base+200))
	exec(`INSERT INTO message (ROWID, guid, text, attributedBody, handle_id, is_from_me, date) VALUES (?,?,?,?,?,?,?)`,
		3, "m3", "dinner at 7?", nil, 2, 0, macNanos(base+300))
	exec(`INSERT INTO message (ROWID, guid, text, attributedBody, handle_id, is_from_me, date) VALUES (?,?,?,?,?,?,?)`,
		4, "m4", nil, mkAttrBody([]byte("see you then")), 1, 0, macNanos(base+400))
	exec(`INSERT INTO chat_message_join (chat_id, message_id) VALUES (1,1), (1,2), (2,3), (2,4)`)

	return db, base
}

func TestChatReader_ListChats(t *testing.T) {
	db, _ := buildSyntheticDB(t)
	r := &chatReader{db: db}
	chats, err := r.ListChats(context.Background(), 25)
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	if len(chats) != 2 {
		t.Fatalf("want 2 chats, got %d: %+v", len(chats), chats)
	}
	// chat2 (Team) is most recent (m4 base+400) → first.
	if chats[0].GUID != "iMessage;+;chat99" || chats[0].Name != "Team" {
		t.Errorf("chat[0] wrong: %+v", chats[0])
	}
	if chats[0].LastMessage != "see you then" {
		t.Errorf("chat[0] last message (attributedBody) wrong: %q", chats[0].LastMessage)
	}
	// chat1 is 1:1 with no display name → name falls back to participant.
	if chats[1].GUID != "iMessage;-;+15551230000" || chats[1].Name != "+15551230000" {
		t.Errorf("chat[1] name fallback wrong: %+v", chats[1])
	}
	if chats[1].LastMessage != "on my way 👋" {
		t.Errorf("chat[1] last message (emoji) wrong: %q", chats[1].LastMessage)
	}
	// group participants present
	if len(chats[0].Participants) != 2 {
		t.Errorf("group participants wrong: %+v", chats[0].Participants)
	}
}

func TestChatReader_ChatMessages(t *testing.T) {
	db, _ := buildSyntheticDB(t)
	r := &chatReader{db: db}
	msgs, err := r.ChatMessages(context.Background(), "iMessage;-;+15551230000", 25)
	if err != nil {
		t.Fatalf("ChatMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 msgs, got %d", len(msgs))
	}
	// newest first: m2 (from me, attributedBody emoji) then m1 (plain text).
	if !msgs[0].IsFromMe || msgs[0].From != "me" || msgs[0].Text != "on my way 👋" {
		t.Errorf("msg[0] wrong: %+v", msgs[0])
	}
	if msgs[1].IsFromMe || msgs[1].From != "+15551230000" || msgs[1].Text != "hey there" {
		t.Errorf("msg[1] wrong: %+v", msgs[1])
	}
	if msgs[0].Date == "" {
		t.Errorf("date should be formatted, got empty")
	}
}

func TestChatReader_Search(t *testing.T) {
	db, _ := buildSyntheticDB(t)
	r := &chatReader{db: db}
	ctx := context.Background()

	// plain-text match
	got, err := r.SearchMessages(ctx, "dinner", 25)
	if err != nil || len(got) != 1 || got[0].Text != "dinner at 7?" || got[0].From != "mom@example.com" {
		t.Fatalf("search dinner: %v %+v", err, got)
	}
	// attributedBody (emoji) match — proves search decodes NULL-text rows
	got, err = r.SearchMessages(ctx, "👋", 25)
	if err != nil || len(got) != 1 || got[0].Text != "on my way 👋" {
		t.Fatalf("search emoji: %v %+v", err, got)
	}
	// no match
	if got, _ := r.SearchMessages(ctx, "zzz-nope", 25); len(got) != 0 {
		t.Errorf("search miss should be empty: %+v", got)
	}
}

func TestChatReader_MessagesSince(t *testing.T) {
	db, _ := buildSyntheticDB(t)
	r := &chatReader{db: db}
	got, err := r.MessagesSince(context.Background(), 2, 100)
	if err != nil {
		t.Fatalf("MessagesSince: %v", err)
	}
	// ROWID > 2 → m3, m4, ascending by ROWID.
	if len(got) != 2 || got[0].RowID != 3 || got[1].RowID != 4 {
		t.Fatalf("since cursor wrong: %+v", got)
	}
	if got[0].ChatGUID != "iMessage;+;chat99" {
		t.Errorf("chat guid not tagged: %+v", got[0])
	}
	if got[1].Text != "see you then" {
		t.Errorf("attributedBody not decoded in since: %q", got[1].Text)
	}
}

func TestMacTime(t *testing.T) {
	if got := macTime(0); got != "" {
		t.Errorf("zero date should be empty, got %q", got)
	}
	// nanoseconds path
	ns := macNanos(1700000000)
	if got := macTime(ns); got == "" {
		t.Errorf("nanoseconds date should format, got empty")
	}
	// legacy seconds path (value below 1e11)
	if got := macTime(1700000000 - macEpochOffset); got == "" {
		t.Errorf("legacy seconds date should format, got empty")
	}
	// both representations of the same instant should format identically
	sec := int64(1700000000)
	if a, b := macTime(macNanos(sec)), macTime(sec-macEpochOffset); a != b {
		t.Errorf("ns vs seconds mismatch: %q vs %q", a, b)
	}
}

func TestNormalizeHandle(t *testing.T) {
	cases := map[string]string{
		"+1 (555) 123-0000": "+15551230000",
		"555-123-0000":      "5551230000",
		"Mom@Example.com":   "mom@example.com",
		"  ":                "",
	}
	for in, want := range cases {
		if got := normalizeHandle(in); got != want {
			t.Errorf("normalizeHandle(%q) = %q, want %q", in, got, want)
		}
	}
}
