package imessage

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// chat.db attachments come back on MessagesSince (a NULL mime_type and a file
// not downloaded yet included), a symlink is refused, and a chat.db without
// the attachment tables still reads its messages.
func TestChatReader_Attachments(t *testing.T) {
	db, base := buildSyntheticDB(t)
	r := &chatReader{db: db}
	if got, err := r.MessagesSince(context.Background(), 0, 100); err != nil || len(got) != 4 {
		t.Fatalf("no attachment tables: %d messages, %v", len(got), err)
	}
	dir := t.TempDir()
	photo := filepath.Join(dir, "IMG_1.jpeg")
	if err := os.WriteFile(photo, []byte("jpeg bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.jpeg")
	if err := os.Symlink(photo, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	for _, q := range []string{
		`CREATE TABLE attachment (ROWID INTEGER PRIMARY KEY AUTOINCREMENT, guid TEXT, filename TEXT, mime_type TEXT, transfer_name TEXT)`,
		`CREATE TABLE message_attachment_join (message_id INTEGER, attachment_id INTEGER)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(`INSERT INTO message (ROWID, guid, text, handle_id, is_from_me, date) VALUES (5, 'm5', ?, 1, 0, ?)`, "\uFFFC", macNanos(base+500))
	mustExec(`INSERT INTO chat_message_join (chat_id, message_id) VALUES (1, 5)`)
	mustExec(`INSERT INTO attachment (ROWID, guid, filename, mime_type, transfer_name) VALUES
		(1, 'a1', ?, NULL, 'IMG_1.jpeg'), (2, 'a2', ?, 'image/heic', 'IMG_2.HEIC'), (3, 'a3', ?, 'image/jpeg', 'link.jpeg')`,
		photo, filepath.Join(dir, "missing.heic"), link)
	mustExec(`INSERT INTO message_attachment_join (message_id, attachment_id) VALUES (5, 1), (5, 2), (5, 3)`)

	got, err := r.MessagesSince(context.Background(), 4, 100)
	if err != nil || len(got) != 1 || len(got[0].Attachments) != 3 {
		t.Fatalf("MessagesSince = %+v, %v; want one message with 3 attachments", got, err)
	}
	a := got[0].Attachments
	if a[0].GUID != "a1" || a[0].Path != photo || a[0].MIME != "" || !a[0].IsImage() {
		t.Errorf("first attachment = %+v", a[0])
	}
	if b, err := r.AttachmentBytes(context.Background(), a[0]); err != nil || string(b) != "jpeg bytes" {
		t.Errorf("read = %q, %v", b, err)
	}
	if _, err := r.AttachmentBytes(context.Background(), a[1]); !errors.Is(err, ErrAttachmentMissing) {
		t.Errorf("missing file: err = %v, want ErrAttachmentMissing", err)
	}
	if _, err := r.AttachmentBytes(context.Background(), a[2]); err == nil {
		t.Error("a symlinked attachment was read")
	}
}

// BlueBubbles asks for attachments with the messages and downloads one by its
// (escaped) GUID.
func TestService_Attachments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/message/query":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": 200, "message": "Success", "data": []map[string]any{{
				"originalROWID": 9, "guid": "p1", "text": "",
				"handle":      map[string]string{"address": "+15551230000", "service": "iMessage"},
				"chats":       []map[string]string{{"guid": "iMessage;-;+15551230000"}},
				"attachments": []map[string]string{{"guid": "at/1 x", "mimeType": "image/png", "transferName": "shot.png"}},
			}}})
		case r.URL.RawPath == "/api/v1/attachment/at%2F1%20x/download" && r.URL.Query().Get("password") == "pw":
			_, _ = w.Write([]byte("png bytes"))
		default:
			http.Error(w, "unexpected "+r.URL.String(), http.StatusNotFound)
		}
	}))
	defer srv.Close()
	svc, err := Open(Config{URL: srv.URL, Password: "pw", HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.MessagesSince(context.Background(), 0, 10)
	if err != nil || len(got) != 1 || len(got[0].Attachments) != 1 {
		t.Fatalf("MessagesSince = %+v, %v", got, err)
	}
	b, err := svc.AttachmentBytes(context.Background(), got[0].Attachments[0])
	if err != nil || string(b) != "png bytes" {
		t.Fatalf("download = %q, %v", b, err)
	}
}

// A photo whose caption matches something Tenant just sent isn't taken for
// an echo: Tenant never sends attachments.
func TestWatcher_PhotoIsNeverAnEcho(t *testing.T) {
	m := inbound(1, "c1", "+15551112222", "ok", false)
	m.Attachments = []Attachment{{GUID: "a1", MIME: "image/jpeg"}}
	w, _ := NewWatcher(WatchConfig{Source: &fakeSource{rows: []InboundMessage{m}}})
	w.RecordSent("c1", "ok")
	if got, _ := w.Poll(context.Background(), 100); len(got) != 1 {
		t.Fatalf("got %d messages, want the photo", len(got))
	}
}
