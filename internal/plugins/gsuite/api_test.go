package gsuite

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tenant/internal/model"
)

// fakeGoogle stands up the Gmail + Calendar wire endpoints with canned
// JSON — the exact contract the real services speak.
func fakeGoogle(t *testing.T, sentRaw *string, createdBody *map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/gmail/v1/users/me/messages", func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query().Get("q"); q == "" {
			t.Errorf("search missing q")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]string{{"id": "m1"}, {"id": "m2"}},
		})
	})
	mux.HandleFunc("/gmail/v1/users/me/messages/send", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Raw string `json:"raw"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		dec, _ := base64.RawURLEncoding.DecodeString(body.Raw)
		if sentRaw != nil {
			*sentRaw = string(dec)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "sent1"})
	})
	mux.HandleFunc("/gmail/v1/users/me/messages/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/me/messages/")
		full := r.URL.Query().Get("format") == "full"
		msg := map[string]any{
			"snippet": "snippet for " + id,
			"payload": map[string]any{
				"mimeType": "multipart/alternative",
				"headers": []map[string]string{
					{"name": "From", "value": "alice@example.com"},
					{"name": "Subject", "value": "Hello " + id},
					{"name": "Date", "value": "Mon, 18 May 2026 10:00:00 +0000"},
					{"name": "To", "value": "me@example.com"},
				},
			},
		}
		if full {
			msg["payload"].(map[string]any)["parts"] = []map[string]any{{
				"mimeType": "text/plain",
				"body":     map[string]string{"data": base64.RawURLEncoding.EncodeToString([]byte("the body of " + id))},
			}}
		}
		_ = json.NewEncoder(w).Encode(msg)
	})

	mux.HandleFunc("/calendar/v3/calendars/primary/events", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			b, _ := io.ReadAll(r.Body)
			if createdBody != nil {
				_ = json.Unmarshal(b, createdBody)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "ev-new", "summary": "Sync", "htmlLink": "https://cal/ev-new",
				"start": map[string]string{"dateTime": "2026-05-20T15:00:00Z"},
				"end":   map[string]string{"dateTime": "2026-05-20T15:30:00Z"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{
			{"id": "e1", "summary": "Standup",
				"start":     map[string]string{"dateTime": "2026-05-19T09:00:00Z"},
				"end":       map[string]string{"dateTime": "2026-05-19T09:15:00Z"},
				"attendees": []map[string]string{{"email": "bob@example.com"}}},
			{"id": "e2", "summary": "Company Holiday", "location": "—",
				"start": map[string]string{"date": "2026-05-25"},
				"end":   map[string]string{"date": "2026-05-26"}},
		}})
	})
	return httptest.NewServer(mux)
}

// openFake wires a Service at the fake server. The official clients carry
// auth on their transport in prod (see Open), so tests skip auth entirely
// and inject a transport that rewrites *.googleapis.com → the httptest
// server via newService.
func openFake(t *testing.T, srv *httptest.Server, p Policy) *Dispatcher {
	t.Helper()
	svc, err := newService(&http.Client{Transport: rewriteToServer(srv)})
	if err != nil {
		t.Fatalf("newService: %v", err)
	}
	return NewDispatcher(svc, p)
}

func call(name string, args map[string]any) model.ToolCall {
	b, _ := json.Marshal(args)
	if len(args) == 0 {
		b = []byte(`{}`)
	}
	return model.ToolCall{Name: name, Arguments: b}
}

func TestGmail_SearchReadSend(t *testing.T) {
	var sent string
	srv := fakeGoogle(t, &sent, nil)
	defer srv.Close()
	d := openFake(t, srv, Policy{AllowSend: true})
	ctx := context.Background()

	out, isErr, _ := d.Dispatch(ctx, call("gmail_search", map[string]any{"query": "is:unread"}))
	if isErr || !strings.Contains(out, "id=m1") || !strings.Contains(out, "Hello m1") {
		t.Fatalf("search: isErr=%v %q", isErr, out)
	}
	out, isErr, _ = d.Dispatch(ctx, call("gmail_read", map[string]any{"id": "m1"}))
	if isErr || !strings.Contains(out, "the body of m1") || !strings.Contains(out, "alice@example.com") {
		t.Fatalf("read: %q", out)
	}
	out, isErr, _ = d.Dispatch(ctx, call("gmail_send", map[string]any{
		"to": "bob@example.com", "subject": "Hi", "body": "hello there"}))
	if isErr || !strings.Contains(out, "message id sent1") {
		t.Fatalf("send: isErr=%v %q", isErr, out)
	}
	for _, want := range []string{"To: bob@example.com", "Subject: Hi", "hello there"} {
		if !strings.Contains(sent, want) {
			t.Errorf("sent RFC822 missing %q in:\n%s", want, sent)
		}
	}
}

func TestCalendar_ListAndCreate(t *testing.T) {
	var created map[string]any
	srv := fakeGoogle(t, nil, &created)
	defer srv.Close()
	d := openFake(t, srv, Policy{AllowSend: true})
	ctx := context.Background()

	out, isErr, _ := d.Dispatch(ctx, call("calendar_list", map[string]any{"days": 14}))
	if isErr || !strings.Contains(out, "Standup") || !strings.Contains(out, "all day") {
		t.Fatalf("list: isErr=%v %q", isErr, out)
	}
	out, isErr, _ = d.Dispatch(ctx, call("calendar_create", map[string]any{
		"summary": "Sync", "start": "2026-05-20T15:00:00Z", "end": "2026-05-20T15:30:00Z",
		"attendees": []string{"bob@example.com"}}))
	if isErr || !strings.Contains(out, "ev-new") {
		t.Fatalf("create: isErr=%v %q", isErr, out)
	}
	if created["summary"] != "Sync" {
		t.Errorf("create body summary wrong: %v", created)
	}
	at, _ := created["attendees"].([]any)
	if len(at) != 1 {
		t.Errorf("attendees not sent: %v", created["attendees"])
	}
}

// The blast-radius boundary: send/create blocked by default; allowed
// with the flag; allowed per-action via Confirm; and nothing actually
// left the building when blocked.
func TestGate_SendBlockedByDefault(t *testing.T) {
	var sent string
	srv := fakeGoogle(t, &sent, nil)
	defer srv.Close()
	ctx := context.Background()

	ro := openFake(t, srv, Policy{}) // read-only
	if out, _, _ := ro.Dispatch(ctx, call("gmail_search", map[string]any{"query": "x"})); !strings.Contains(out, "id=m1") {
		t.Fatalf("read must be allowed read-only: %q", out)
	}
	out, isErr, _ := ro.Dispatch(ctx, call("gmail_send", map[string]any{"to": "a@b.c", "subject": "s", "body": "b"}))
	if !isErr || !strings.Contains(out, "blocked") {
		t.Fatalf("send must be blocked read-only: isErr=%v %q", isErr, out)
	}
	if sent != "" {
		t.Fatalf("send was blocked but a message still left: %q", sent)
	}
	out, isErr, _ = ro.Dispatch(ctx, call("calendar_create", map[string]any{
		"summary": "x", "start": "2026-05-20T15:00:00Z", "end": "2026-05-20T15:30:00Z"}))
	if !isErr || !strings.Contains(out, "blocked") {
		t.Fatalf("create must be blocked read-only: %q", out)
	}

	// Per-action Confirm approves a single send without the blanket flag.
	var sawDetail string
	cf := openFake(t, srv, Policy{Confirm: func(_ context.Context, _, det string) bool {
		sawDetail = det
		return true
	}})
	if out, isErr, _ := cf.Dispatch(ctx, call("gmail_send", map[string]any{
		"to": "bob@x.com", "subject": "Hi", "body": "yo"})); isErr || !strings.Contains(out, "sent1") {
		t.Fatalf("Confirm should allow the send: isErr=%v %q", isErr, out)
	}
	if !strings.Contains(sawDetail, "bob@x.com") {
		t.Errorf("Confirm detail should describe the send: %q", sawDetail)
	}
}

func TestDispatch_BadArgsAndOpen(t *testing.T) {
	srv := fakeGoogle(t, nil, nil)
	defer srv.Close()
	d := openFake(t, srv, Policy{AllowSend: true})
	ctx := context.Background()

	if out, isErr, _ := d.Dispatch(ctx, model.ToolCall{Name: "gmail_search", Arguments: json.RawMessage(`{bad`)}); !isErr || !strings.Contains(out, "invalid arguments") {
		t.Errorf("bad json: %q", out)
	}
	if out, isErr, _ := d.Dispatch(ctx, call("gmail_search", map[string]any{"query": "  "})); !isErr || !strings.Contains(out, "query is required") {
		t.Errorf("empty query: %q", out)
	}
	if out, isErr, _ := d.Dispatch(ctx, call("calendar_create", map[string]any{
		"summary": "x", "start": "not-a-time", "end": "2026-05-20T15:30:00Z"})); !isErr || !strings.Contains(out, "invalid start") {
		t.Errorf("bad time: %q", out)
	}
	if out, isErr, _ := d.Dispatch(ctx, call("nope", nil)); !isErr || !strings.Contains(out, "unknown gsuite tool") {
		t.Errorf("unknown tool: %q", out)
	}

	// Open auth-path validation.
	if _, err := Open(Config{Auth: "martian"}); err == nil {
		t.Error("unknown auth must error")
	}
	if _, err := Open(Config{Auth: "sa"}); err == nil {
		t.Error("auth=sa without SAJSON must error")
	}
}

func TestTools(t *testing.T) {
	d := NewDispatcher(nil, Policy{})
	names := map[string]bool{}
	for _, sp := range d.Tools() {
		names[sp.Name] = true
		if !json.Valid(sp.Parameters) {
			t.Errorf("%s invalid params schema", sp.Name)
		}
	}
	for _, w := range []string{"gmail_search", "gmail_read", "gmail_send", "calendar_list", "calendar_create"} {
		if !names[w] {
			t.Errorf("missing tool %s", w)
		}
	}
}
