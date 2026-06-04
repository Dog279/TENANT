package imessage

// White-box tests. No live BlueBubbles/Mac here and no server password
// solicited: the BlueBubbles REST API (envelope + endpoints) is an
// httptest server, which is exactly the wire contract a real server
// speaks. The blast-radius gate is proven with "nothing was sent"
// assertions.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tenant/internal/model"
)

// fakeBB mimics a BlueBubbles server. sendBody/newBody capture the
// last write payloads so request shape can be asserted.
func fakeBB(t *testing.T, sendBody, newBody *map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	wrap := func(w http.ResponseWriter, data any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": 200, "message": "Success", "data": data})
	}
	checkPW := func(t *testing.T, r *http.Request) {
		if r.URL.Query().Get("password") != "pw" {
			t.Errorf("%s missing/wrong password param", r.URL.Path)
		}
	}

	mux.HandleFunc("/api/v1/ping", func(w http.ResponseWriter, r *http.Request) {
		checkPW(t, r)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": 200, "message": "pong"})
	})
	mux.HandleFunc("/api/v1/chat/query", func(w http.ResponseWriter, r *http.Request) {
		checkPW(t, r)
		wrap(w, []map[string]any{{
			"guid": "iMessage;-;+15551230000", "displayName": "",
			"participants": []map[string]string{{"address": "+15551230000"}},
			"lastMessage":  map[string]any{"text": "see you then"},
		}, {
			"guid": "iMessage;+;group1", "displayName": "Team",
			"participants": []map[string]string{{"address": "a@b.c"}, {"address": "d@e.f"}},
		}})
	})
	mux.HandleFunc("/api/v1/chat/new", func(w http.ResponseWriter, r *http.Request) {
		checkPW(t, r)
		b, _ := io.ReadAll(r.Body)
		if newBody != nil {
			_ = json.Unmarshal(b, newBody)
		}
		wrap(w, map[string]any{"guid": "newchat-1"})
	})
	mux.HandleFunc("/api/v1/chat/", func(w http.ResponseWriter, r *http.Request) { // /chat/{guid}/message
		checkPW(t, r)
		if !strings.HasSuffix(r.URL.Path, "/message") {
			http.NotFound(w, r)
			return
		}
		wrap(w, []map[string]any{
			{"guid": "m1", "text": "hey there", "dateCreated": int64(1779100000000), "isFromMe": false,
				"handle": map[string]string{"address": "+15551230000"}},
			{"guid": "m2", "text": "on my way", "dateCreated": int64(1779100100000), "isFromMe": true},
		})
	})
	mux.HandleFunc("/api/v1/message/query", func(w http.ResponseWriter, r *http.Request) {
		checkPW(t, r)
		wrap(w, []map[string]any{
			{"guid": "s1", "text": "dinner at 7?", "dateCreated": int64(1779100200000), "isFromMe": false,
				"handle": map[string]string{"address": "mom"}},
		})
	})
	mux.HandleFunc("/api/v1/message/text", func(w http.ResponseWriter, r *http.Request) {
		checkPW(t, r)
		b, _ := io.ReadAll(r.Body)
		if sendBody != nil {
			_ = json.Unmarshal(b, sendBody)
		}
		wrap(w, map[string]any{"guid": "sent-1"})
	})
	return httptest.NewServer(mux)
}

func openFake(t *testing.T, srv *httptest.Server, p Policy, privateAPI bool) *Dispatcher {
	t.Helper()
	svc, err := Open(Config{URL: srv.URL, Password: "pw", PrivateAPI: privateAPI, HTTP: srv.Client()})
	if err != nil {
		t.Fatalf("Open: %v", err)
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

func TestReads_ListReadSearch(t *testing.T) {
	srv := fakeBB(t, nil, nil)
	defer srv.Close()
	d := openFake(t, srv, Policy{}, false)
	ctx := context.Background()

	out, isErr, _ := d.Dispatch(ctx, call("imessage_list_chats", nil))
	if isErr || !strings.Contains(out, "iMessage;-;+15551230000") || !strings.Contains(out, "see you then") {
		t.Fatalf("list_chats: isErr=%v %q", isErr, out)
	}
	if !strings.Contains(out, "+15551230000") || !strings.Contains(out, "Team") {
		t.Errorf("list_chats name fallback / group name wrong: %q", out)
	}
	out, isErr, _ = d.Dispatch(ctx, call("imessage_read_chat", map[string]any{"chat_guid": "iMessage;-;+15551230000"}))
	if isErr || !strings.Contains(out, "hey there") || !strings.Contains(out, "me: on my way") {
		t.Fatalf("read_chat: %q", out)
	}
	out, isErr, _ = d.Dispatch(ctx, call("imessage_search", map[string]any{"text": "dinner"}))
	if isErr || !strings.Contains(out, "dinner at 7?") || !strings.Contains(out, "mom") {
		t.Fatalf("search: %q", out)
	}
}

func TestSend_ShapeAndMethod(t *testing.T) {
	var sendBody, newBody map[string]any
	srv := fakeBB(t, &sendBody, &newBody)
	defer srv.Close()
	d := openFake(t, srv, Policy{AllowSend: true}, false)
	ctx := context.Background()

	out, isErr, _ := d.Dispatch(ctx, call("imessage_send", map[string]any{
		"chat_guid": "chatX", "text": "running late"}))
	if isErr || !strings.Contains(out, "sent message sent-1") {
		t.Fatalf("send: isErr=%v %q", isErr, out)
	}
	if sendBody["chatGuid"] != "chatX" || sendBody["message"] != "running late" {
		t.Errorf("send body wrong: %v", sendBody)
	}
	if sendBody["method"] != "apple-script" {
		t.Errorf("default method should be apple-script, got %v", sendBody["method"])
	}
	if tg, _ := sendBody["tempGuid"].(string); !strings.HasPrefix(tg, "tenant-") {
		t.Errorf("tempGuid missing/wrong: %v", sendBody["tempGuid"])
	}

	out, isErr, _ = d.Dispatch(ctx, call("imessage_new_chat", map[string]any{
		"address": "+15559999999", "text": "hi!"}))
	if isErr || !strings.Contains(out, "+15559999999") {
		t.Fatalf("new_chat: %q", out)
	}
	addrs, _ := newBody["addresses"].([]any)
	if len(addrs) != 1 || addrs[0] != "+15559999999" {
		t.Errorf("new_chat addresses wrong: %v", newBody)
	}
}

func TestSend_PrivateAPIMethod(t *testing.T) {
	var sendBody map[string]any
	srv := fakeBB(t, &sendBody, nil)
	defer srv.Close()
	d := openFake(t, srv, Policy{AllowSend: true}, true) // --private-api
	if _, isErr, _ := d.Dispatch(context.Background(), call("imessage_send", map[string]any{
		"chat_guid": "c", "text": "x"})); isErr {
		t.Fatal("send failed")
	}
	if sendBody["method"] != "private-api" {
		t.Errorf("private-api method not used: %v", sendBody["method"])
	}
}

// Blast-radius boundary: send/new_chat blocked by default; allowed via
// flag; allowed per-action via Confirm; nothing sent when blocked.
func TestGate_SendBlockedByDefault(t *testing.T) {
	var sendBody, newBody map[string]any
	srv := fakeBB(t, &sendBody, &newBody)
	defer srv.Close()
	ctx := context.Background()

	ro := openFake(t, srv, Policy{}, false)
	if out, _, _ := ro.Dispatch(ctx, call("imessage_list_chats", nil)); !strings.Contains(out, "see you then") {
		t.Fatalf("read must work: %q", out)
	}
	out, isErr, _ := ro.Dispatch(ctx, call("imessage_send", map[string]any{"chat_guid": "c", "text": "no"}))
	if !isErr || !strings.Contains(out, "blocked") {
		t.Fatalf("send must be blocked: isErr=%v %q", isErr, out)
	}
	if sendBody != nil {
		t.Fatalf("send was blocked but a message still left: %v", sendBody)
	}
	if out, isErr, _ := ro.Dispatch(ctx, call("imessage_new_chat", map[string]any{"address": "+1", "text": "no"})); !isErr || !strings.Contains(out, "blocked") {
		t.Fatalf("new_chat must be blocked: %q", out)
	}
	if newBody != nil {
		t.Fatalf("new_chat blocked but a chat still started: %v", newBody)
	}

	var sawDetail string
	cf := openFake(t, srv, Policy{Confirm: func(_ context.Context, _, det string) bool {
		sawDetail = det
		return true
	}}, false)
	if out, isErr, _ := cf.Dispatch(ctx, call("imessage_send", map[string]any{
		"chat_guid": "chatX", "text": "ok now"})); isErr || !strings.Contains(out, "sent-1") {
		t.Fatalf("Confirm should allow the send: isErr=%v %q", isErr, out)
	}
	if !strings.Contains(sawDetail, "ok now") {
		t.Errorf("Confirm detail should describe the send: %q", sawDetail)
	}
}

func TestOpen_Validation(t *testing.T) {
	if _, err := Open(Config{Password: "p"}); err == nil {
		t.Error("missing URL must error")
	}
	if _, err := Open(Config{URL: "://bad", Password: "p"}); err == nil {
		t.Error("invalid URL must error")
	}
	if _, err := Open(Config{URL: "http://localhost:1234"}); err == nil {
		t.Error("missing password must error")
	}
}

func TestDispatch_BadArgsAndUnknown(t *testing.T) {
	srv := fakeBB(t, nil, nil)
	defer srv.Close()
	d := openFake(t, srv, Policy{AllowSend: true}, false)
	ctx := context.Background()
	if out, isErr, _ := d.Dispatch(ctx, model.ToolCall{Name: "imessage_search", Arguments: json.RawMessage(`{bad`)}); !isErr || !strings.Contains(out, "invalid arguments") {
		t.Errorf("bad json: %q", out)
	}
	if out, isErr, _ := d.Dispatch(ctx, call("imessage_search", map[string]any{"text": " "})); !isErr || !strings.Contains(out, "text is required") {
		t.Errorf("empty text: %q", out)
	}
	if out, isErr, _ := d.Dispatch(ctx, call("nope", nil)); !isErr || !strings.Contains(out, "unknown imessage tool") {
		t.Errorf("unknown tool: %q", out)
	}
}

func TestPingAndErrorEnvelope(t *testing.T) {
	// A server that 401s with a BlueBubbles error envelope.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": 401, "message": "Invalid password!"})
	}))
	defer srv.Close()
	svc, _ := Open(Config{URL: srv.URL, Password: "wrong", HTTP: srv.Client()})
	err := svc.Ping(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Invalid password") {
		t.Fatalf("ping should surface the envelope message, got %v", err)
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
	for _, w := range []string{"imessage_list_chats", "imessage_read_chat", "imessage_search", "imessage_send", "imessage_new_chat"} {
		if !names[w] {
			t.Errorf("missing tool %s", w)
		}
	}
}
