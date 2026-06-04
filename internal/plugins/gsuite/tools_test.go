package gsuite

// Dispatch-level tests for the 12 read/write tools wired in when the
// official google.golang.org/api clients replaced the hand-rolled REST
// (gmail_draft/labels/modify/trash, calendar_update/delete/calendars/
// freebusy, drive_create/folder/update/trash). Each test drives the full
// stack: Dispatcher → handler → blast-radius gate → plugin → official
// client → wire. fakeWorkspace serves the exact endpoint shapes the
// official clients speak and records what crossed the wire.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeWorkspace stands up the Gmail/Calendar/Drive endpoints the new
// tools hit and captures request payloads for assertions.
type fakeWorkspace struct {
	mux *http.ServeMux

	draftRaw       string         // decoded RFC822 of the saved draft
	modifyAdd      []string       // addLabelIds seen by messages.modify
	modifyRemove   []string       // removeLabelIds seen by messages.modify
	trashedMsg     string         // message id sent to messages.trash
	patchedEvent   map[string]any // body of events.patch
	patchedEventID string         // event id patched
	deletedEvent   string         // event id deleted
	freeBusyReq    map[string]any // body of freeBusy.query
	createdFolder  map[string]any // metadata body of files.create (folder)
	patchedFile    map[string]any // metadata body of files.update (trash/rename)
	patchedFileID  string         // file id patched (metadata-only)
	uploadBody     string         // raw multipart body of a media upload
	uploadUpdateID string         // file id of a media update
}

func newFakeWorkspace() *fakeWorkspace {
	fw := &fakeWorkspace{mux: http.NewServeMux()}

	// --- Gmail ---
	fw.mux.HandleFunc("/gmail/v1/users/me/drafts", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Message struct {
				Raw string `json:"raw"`
			} `json:"message"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		dec, _ := base64.RawURLEncoding.DecodeString(body.Message.Raw)
		fw.draftRaw = string(dec)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "draft1", "message": map[string]any{"id": "md1"}})
	})
	fw.mux.HandleFunc("/gmail/v1/users/me/labels", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"labels": []map[string]any{
			{"id": "INBOX", "name": "INBOX", "type": "system"},
			{"id": "Label_1", "name": "Work", "type": "user"},
		}})
	})
	fw.mux.HandleFunc("/gmail/v1/users/me/messages/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/me/messages/")
		switch {
		case strings.HasSuffix(path, "/modify"):
			var req struct {
				AddLabelIds    []string `json:"addLabelIds"`
				RemoveLabelIds []string `json:"removeLabelIds"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			fw.modifyAdd, fw.modifyRemove = req.AddLabelIds, req.RemoveLabelIds
			id := strings.TrimSuffix(path, "/modify")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "labelIds": []string{"INBOX", "STARRED"}})
		case strings.HasSuffix(path, "/trash"):
			fw.trashedMsg = strings.TrimSuffix(path, "/trash")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": fw.trashedMsg, "labelIds": []string{"TRASH"}})
		default:
			http.NotFound(w, r)
		}
	})

	// --- Calendar ---
	fw.mux.HandleFunc("/calendar/v3/calendars/primary/events/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/calendar/v3/calendars/primary/events/")
		switch r.Method {
		case http.MethodPatch:
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &fw.patchedEvent)
			fw.patchedEventID = id
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": id, "summary": "Renamed", "htmlLink": "https://cal/" + id,
				"start": map[string]string{"dateTime": "2026-05-20T15:00:00Z"},
				"end":   map[string]string{"dateTime": "2026-05-20T15:30:00Z"},
			})
		case http.MethodDelete:
			fw.deletedEvent = id
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	fw.mux.HandleFunc("/calendar/v3/users/me/calendarList", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{
			{"id": "primary", "summary": "Me", "accessRole": "owner", "primary": true},
			{"id": "cal2", "summary": "Team Cal", "accessRole": "reader"},
		}})
	})
	fw.mux.HandleFunc("/calendar/v3/freeBusy", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &fw.freeBusyReq)
		_ = json.NewEncoder(w).Encode(map[string]any{"calendars": map[string]any{
			"primary": map[string]any{"busy": []map[string]string{
				{"start": "2026-05-20T15:00:00Z", "end": "2026-05-20T15:30:00Z"},
			}},
		}})
	})

	// --- Drive ---
	// files.create without media (folder) POSTs to the metadata endpoint.
	fw.mux.HandleFunc("/drive/v3/files", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &fw.createdFolder)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "fold1", "name": "Reports", "mimeType": driveFolderMime,
			"webViewLink": "https://drive/fold1",
		})
	})
	// files.update metadata-only (trash / rename) PATCHes here.
	fw.mux.HandleFunc("/drive/v3/files/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/drive/v3/files/")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &fw.patchedFile)
		fw.patchedFileID = id
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "name": "notes.txt", "mimeType": "text/plain",
			"webViewLink": "https://drive/" + id,
		})
	})
	// Media uploads (create with content) go to the /upload/ path prefix.
	fw.mux.HandleFunc("/upload/drive/v3/files", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		fw.uploadBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "f1", "name": "notes.txt", "mimeType": "text/plain",
			"webViewLink": "https://drive/f1",
		})
	})
	// Media updates (update with content) PATCH the /upload/ path with an id.
	fw.mux.HandleFunc("/upload/drive/v3/files/", func(w http.ResponseWriter, r *http.Request) {
		fw.uploadUpdateID = strings.TrimPrefix(r.URL.Path, "/upload/drive/v3/files/")
		b, _ := io.ReadAll(r.Body)
		fw.uploadBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": fw.uploadUpdateID, "name": "renamed.txt", "mimeType": "text/plain",
			"webViewLink": "https://drive/" + fw.uploadUpdateID,
		})
	})

	return fw
}

// openWorkspace wires a Dispatcher over a fresh fakeWorkspace; the server
// is torn down via t.Cleanup.
func openWorkspace(t *testing.T, p Policy) (*Dispatcher, *fakeWorkspace) {
	t.Helper()
	fw := newFakeWorkspace()
	srv := httptest.NewServer(fw.mux)
	t.Cleanup(srv.Close)
	return openFake(t, srv, p), fw
}

func TestGmail_DraftLabelsModifyTrash(t *testing.T) {
	d, fw := openWorkspace(t, Policy{AllowSend: true})
	ctx := context.Background()

	// gmail_draft is ungated — it stays in the mailbox.
	out, isErr, _ := d.Dispatch(ctx, call("gmail_draft", map[string]any{
		"to": "bob@x.com", "subject": "Weekly update", "body": "draft body here"}))
	if isErr || !strings.Contains(out, "draft saved: id=draft1") {
		t.Fatalf("draft: isErr=%v %q", isErr, out)
	}
	for _, want := range []string{"Subject: Weekly update", "draft body here"} {
		if !strings.Contains(fw.draftRaw, want) {
			t.Errorf("draft RFC822 missing %q in:\n%s", want, fw.draftRaw)
		}
	}

	// gmail_labels resolves names → ids.
	out, isErr, _ = d.Dispatch(ctx, call("gmail_labels", nil))
	if isErr || !strings.Contains(out, "Work") || !strings.Contains(out, "id=Label_1") {
		t.Fatalf("labels: isErr=%v %q", isErr, out)
	}

	// gmail_modify adds/removes labels and reports the resulting set.
	out, isErr, _ = d.Dispatch(ctx, call("gmail_modify", map[string]any{
		"id": "m1", "add": []string{"STARRED"}, "remove": []string{"UNREAD"}}))
	if isErr || !strings.Contains(out, "labels now: INBOX, STARRED") {
		t.Fatalf("modify: isErr=%v %q", isErr, out)
	}
	if len(fw.modifyAdd) != 1 || fw.modifyAdd[0] != "STARRED" || len(fw.modifyRemove) != 1 || fw.modifyRemove[0] != "UNREAD" {
		t.Errorf("modify payload wrong: add=%v remove=%v", fw.modifyAdd, fw.modifyRemove)
	}

	// gmail_trash moves to Trash (reversible).
	out, isErr, _ = d.Dispatch(ctx, call("gmail_trash", map[string]any{"id": "m1"}))
	if isErr || !strings.Contains(out, "trashed message m1") {
		t.Fatalf("trash: isErr=%v %q", isErr, out)
	}
	if fw.trashedMsg != "m1" {
		t.Errorf("trash did not hit the wire for m1: %q", fw.trashedMsg)
	}
}

func TestCalendar_UpdateDeleteCalendarsFreeBusy(t *testing.T) {
	d, fw := openWorkspace(t, Policy{AllowSend: true})
	ctx := context.Background()

	// calendar_update with only a summary must PATCH summary and leave
	// start/end untouched (partial-update semantics).
	out, isErr, _ := d.Dispatch(ctx, call("calendar_update", map[string]any{
		"id": "ev1", "summary": "Renamed Standup"}))
	if isErr || !strings.Contains(out, "updated:") || !strings.Contains(out, "ev1") {
		t.Fatalf("update: isErr=%v %q", isErr, out)
	}
	if fw.patchedEventID != "ev1" {
		t.Errorf("patched wrong event: %q", fw.patchedEventID)
	}
	if fw.patchedEvent["summary"] != "Renamed Standup" {
		t.Errorf("patch summary wrong: %v", fw.patchedEvent)
	}
	if _, hasStart := fw.patchedEvent["start"]; hasStart {
		t.Errorf("partial update must not send start: %v", fw.patchedEvent)
	}

	// calendar_delete cancels the event.
	out, isErr, _ = d.Dispatch(ctx, call("calendar_delete", map[string]any{"id": "ev1"}))
	if isErr || !strings.Contains(out, "deleted event ev1") {
		t.Fatalf("delete: isErr=%v %q", isErr, out)
	}
	if fw.deletedEvent != "ev1" {
		t.Errorf("delete did not hit the wire: %q", fw.deletedEvent)
	}

	// calendar_calendars is ungated metadata.
	out, isErr, _ = d.Dispatch(ctx, call("calendar_calendars", nil))
	if isErr || !strings.Contains(out, "Team Cal") || !strings.Contains(out, "id=cal2") || !strings.Contains(out, "(primary)") {
		t.Fatalf("calendars: isErr=%v %q", isErr, out)
	}

	// calendar_freebusy returns busy blocks.
	out, isErr, _ = d.Dispatch(ctx, call("calendar_freebusy", map[string]any{"days": 7}))
	if isErr || !strings.Contains(out, "busy block") {
		t.Fatalf("freebusy: isErr=%v %q", isErr, out)
	}
	if fw.freeBusyReq["timeMin"] == nil || fw.freeBusyReq["timeMax"] == nil {
		t.Errorf("freebusy request missing time window: %v", fw.freeBusyReq)
	}
}

func TestDrive_CreateFolderUpdateTrash(t *testing.T) {
	d, fw := openWorkspace(t, Policy{AllowSend: true})
	ctx := context.Background()

	// drive_create uploads content via the media path.
	out, isErr, _ := d.Dispatch(ctx, call("drive_create", map[string]any{
		"name": "notes.txt", "content": "hello from drive"}))
	if isErr || !strings.Contains(out, "created:") || !strings.Contains(out, "id=f1") {
		t.Fatalf("create: isErr=%v %q", isErr, out)
	}
	for _, want := range []string{"notes.txt", "hello from drive"} {
		if !strings.Contains(fw.uploadBody, want) {
			t.Errorf("upload body missing %q", want)
		}
	}

	// drive_folder creates a folder (no media, folder mimeType).
	out, isErr, _ = d.Dispatch(ctx, call("drive_folder", map[string]any{"name": "Reports"}))
	if isErr || !strings.Contains(out, "folder created:") || !strings.Contains(out, "id=fold1") {
		t.Fatalf("folder: isErr=%v %q", isErr, out)
	}
	if fw.createdFolder["mimeType"] != driveFolderMime {
		t.Errorf("folder mimeType wrong: %v", fw.createdFolder)
	}

	// drive_update with new content takes the media-update path.
	out, isErr, _ = d.Dispatch(ctx, call("drive_update", map[string]any{
		"id": "f9", "content": "replacement body"}))
	if isErr || !strings.Contains(out, "updated:") {
		t.Fatalf("update: isErr=%v %q", isErr, out)
	}
	if fw.uploadUpdateID != "f9" || !strings.Contains(fw.uploadBody, "replacement body") {
		t.Errorf("update did not upload to f9: id=%q body=%q", fw.uploadUpdateID, fw.uploadBody)
	}

	// drive_trash flips Trashed via a metadata-only PATCH.
	out, isErr, _ = d.Dispatch(ctx, call("drive_trash", map[string]any{"id": "f1"}))
	if isErr || !strings.Contains(out, "trashed file f1") {
		t.Fatalf("trash: isErr=%v %q", isErr, out)
	}
	if fw.patchedFileID != "f1" || fw.patchedFile["trashed"] != true {
		t.Errorf("trash PATCH wrong: id=%q body=%v", fw.patchedFileID, fw.patchedFile)
	}
}

// The new write tools must be blocked by default (read-only policy), and
// nothing destructive may cross the wire when blocked. A per-action
// Confirm approves a single call without the blanket flag.
func TestGate_NewWriteToolsBlockedByDefault(t *testing.T) {
	ctx := context.Background()
	d, fw := openWorkspace(t, Policy{}) // read-only

	gated := []struct {
		name string
		args map[string]any
	}{
		{"gmail_modify", map[string]any{"id": "m1", "add": []string{"STARRED"}}},
		{"gmail_trash", map[string]any{"id": "m1"}},
		{"calendar_update", map[string]any{"id": "ev1", "summary": "x"}},
		{"calendar_delete", map[string]any{"id": "ev1"}},
		{"drive_create", map[string]any{"name": "n", "content": "c"}},
		{"drive_folder", map[string]any{"name": "Reports"}},
		{"drive_update", map[string]any{"id": "f1", "content": "c"}},
		{"drive_trash", map[string]any{"id": "f1"}},
	}
	for _, g := range gated {
		out, isErr, _ := d.Dispatch(ctx, call(g.name, g.args))
		if !isErr || !strings.Contains(out, "blocked") {
			t.Errorf("%s must be blocked read-only: isErr=%v %q", g.name, isErr, out)
		}
	}
	// Destructive calls must not have reached the wire.
	if fw.trashedMsg != "" || fw.deletedEvent != "" || fw.patchedFileID != "" || fw.uploadBody != "" {
		t.Errorf("a blocked write still crossed the wire: msg=%q ev=%q file=%q upload=%q",
			fw.trashedMsg, fw.deletedEvent, fw.patchedFileID, fw.uploadBody)
	}

	// Ungated write-adjacent tools stay allowed read-only (draft stays in
	// the mailbox; labels/calendars/freebusy are reads).
	for _, ok := range []struct {
		name string
		args map[string]any
	}{
		{"gmail_draft", map[string]any{"subject": "s", "body": "b"}},
		{"gmail_labels", nil},
		{"calendar_calendars", nil},
		{"calendar_freebusy", map[string]any{"days": 3}},
	} {
		if out, isErr, _ := d.Dispatch(ctx, call(ok.name, ok.args)); isErr {
			t.Errorf("%s must be allowed read-only: %q", ok.name, out)
		}
	}

	// Per-action Confirm approves a single trash without the blanket flag.
	var sawDetail string
	cf, fw2 := openWorkspace(t, Policy{Confirm: func(_ context.Context, _, det string) bool {
		sawDetail = det
		return true
	}})
	if out, isErr, _ := cf.Dispatch(ctx, call("drive_trash", map[string]any{"id": "f7"})); isErr || !strings.Contains(out, "trashed file f7") {
		t.Fatalf("Confirm should allow the trash: isErr=%v %q", isErr, out)
	}
	if fw2.patchedFileID != "f7" {
		t.Errorf("confirmed trash did not reach the wire: %q", fw2.patchedFileID)
	}
	if !strings.Contains(sawDetail, "f7") {
		t.Errorf("Confirm detail should describe the action: %q", sawDetail)
	}
}
