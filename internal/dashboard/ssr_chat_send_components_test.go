package dashboard

// A tap on a choices button or a filled form sends the next message as the
// operator (TEN-518): the server fills it from the saved call, checks every
// value, tags it "via choices" / "via form", and refuses what it cannot
// trust: an option that does not exist, a value outside its field, a call
// it no longer holds, a second send while a turn runs, another origin.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tenant/internal/agent"
	"tenant/internal/memory/working"
	"tenant/internal/model"
)

type metaRunner struct {
	reqs  chan agent.TurnRequest
	block chan struct{} // when set, Turn waits on it
}

func (r *metaRunner) Turn(_ context.Context, req agent.TurnRequest) (*agent.TurnResult, error) {
	if r.reqs != nil {
		r.reqs <- req
	}
	if r.block != nil {
		<-r.block
	}
	return &agent.TurnResult{}, nil
}
func (r *metaRunner) Interject(string) {}

const (
	choicesArgs = `{"kind":"choices","data":{"options":[{"label":"Show by price"},{"label":"Fastest","send":"Switch to the fastest model"}]}}`
	formArgs    = `{"kind":"form","data":{"fields":[{"name":"total","label":"Bill total","type":"number","min":0},{"name":"people","type":"range","min":2,"max":10,"default":"2"},{"name":"tip","type":"select","options":["10%","15%","20%"]}],"submit":"Split it","template":"Split {total} between {people} people with a {tip} tip"}}`
	slashArgs   = `{"kind":"choices","data":{"options":[{"label":"a"},{"label":"b","send":"/disable approvals"}]}}`
)

func componentServer(t *testing.T, r *metaRunner) *Server {
	t.Helper()
	s := New(Config{}, r, &fakeTools{}, nil, nil, nil)
	s.SetTranscript(func() []working.Message {
		return []working.Message{
			{Role: "user", Content: "which model?"},
			{Role: "assistant", ToolCalls: []model.ToolCall{
				{ID: "c1", Name: "ui_render", Arguments: json.RawMessage(choicesArgs)},
				{ID: "f1", Name: "ui_render", Arguments: json.RawMessage(formArgs)},
				{ID: "s1", Name: "ui_render", Arguments: json.RawMessage(slashArgs)},
			}},
			{Role: "user", Content: "Show by price", Meta: map[string]any{"source": "ui_render", "component": "c1", "kind": "choices"}},
		}
	})
	return s
}

func postSend(s *Server, body string, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/chat/send", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec
}

func turnOrNone(t *testing.T, r *metaRunner) (agent.TurnRequest, bool) {
	t.Helper()
	select {
	case req := <-r.reqs:
		return req, true
	case <-time.After(300 * time.Millisecond):
		return agent.TurnRequest{}, false
	}
}

func TestChatSend_ChoicesTapSendsTaggedMessage(t *testing.T) {
	r := &metaRunner{reqs: make(chan agent.TurnRequest, 1)}
	s := componentServer(t, r)
	body := postSend(s, `{"text":"typed text is ignored","component":"c1","choice":1}`, "").Body.String()
	if !strings.Contains(body, "Switch to the fastest model") || !strings.Contains(body, `<div class="via dim">via choices</div>`) || strings.Contains(body, "typed text") {
		t.Fatalf("the tap's message, tagged, not the typed text:\n%s", body)
	}
	req, ok := turnOrNone(t, r)
	if !ok || req.UserQuery != "Switch to the fastest model" || req.Meta["kind"] != "choices" || req.Meta["component"] != "c1" || req.Meta["source"] != "ui_render" {
		t.Fatalf("turn = %+v (%v)", req, ok)
	}
}

func TestChatSend_FormSubmitFillsAndTags(t *testing.T) {
	r := &metaRunner{reqs: make(chan agent.TurnRequest, 1)}
	s := componentServer(t, r)
	body := postSend(s, `{"component":"f1","f_f1_total":"120.50","f_f1_people":4,"f_f1_tip":"15%"}`, "").Body.String()
	want := "Split 120.5 between 4 people with a 15% tip"
	if !strings.Contains(body, want) || !strings.Contains(body, "via form") {
		t.Fatalf("filled form:\n%s", body)
	}
	if req, ok := turnOrNone(t, r); !ok || req.UserQuery != want || req.Meta["kind"] != "form" {
		t.Fatalf("turn = %+v (%v)", req, ok)
	}
}

// Every refusal is a row in the chat and no turn.
func TestChatSend_RefusalsStartNothing(t *testing.T) {
	cases := map[string]string{
		`{"component":"c1","choice":99}`:                                          "option 100 does not exist (there are 2)",
		`{"component":"c1","choice":-1}`:                                          "does not exist",
		`{"component":"f1","f_f1_total":"abc","f_f1_tip":"15%"}`:                  "Bill total must be a number",
		`{"component":"f1","f_f1_total":"5","f_f1_people":"40","f_f1_tip":"15%"}`: "people must be between 2 and 10",
		`{"component":"f1","f_f1_total":"5","f_f1_tip":"50%"}`:                    "tip must be one of 10%, 15%, 20%",
		`{"component":"f1","f_f1_people":"4","f_f1_tip":"15%"}`:                   "Bill total is missing",
		`{"component":"nope","choice":0}`:                                         "That form is gone",
		`{"component":"s1","choice":1}`:                                           "can&#39;t be used: choices: option 2&#39;s message must not start with /",
	}
	for body, want := range cases {
		r := &metaRunner{reqs: make(chan agent.TurnRequest, 1)}
		s := componentServer(t, r)
		rec := postSend(s, body, "")
		if got := rec.Body.String(); !strings.Contains(got, `class="msg sys"`) || !strings.Contains(got, want) {
			t.Errorf("%s: want a system row saying %q, got:\n%s", body, want, got)
		}
		if req, ok := turnOrNone(t, r); ok {
			t.Errorf("%s started a turn: %q", body, req.UserQuery)
		}
	}
}

func TestChatSend_SecondSendWhileAnsweringIsRefused(t *testing.T) {
	r := &metaRunner{reqs: make(chan agent.TurnRequest, 2), block: make(chan struct{})}
	defer close(r.block)
	s := componentServer(t, r)
	if body := postSend(s, `{"component":"c1","choice":0}`, "").Body.String(); !strings.Contains(body, "via choices") {
		t.Fatalf("first tap: %s", body)
	}
	turnOrNone(t, r)
	body := postSend(s, `{"component":"c1","choice":1}`, "").Body.String()
	if !strings.Contains(body, "still answering") || strings.Contains(body, "via choices") {
		t.Fatalf("a second tap during a turn is refused:\n%s", body)
	}
}

func TestChatSend_OtherOriginIsRejected(t *testing.T) {
	r := &metaRunner{reqs: make(chan agent.TurnRequest, 1)}
	s := withSession(New(Config{Addr: "example.com", Auth: "secret", AllowedHosts: []string{"dash.local:9770"}}, r, &fakeTools{}, nil, nil, nil))
	s.SetTranscript(func() []working.Message {
		return []working.Message{{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "c1", Name: "ui_render", Arguments: json.RawMessage(choicesArgs)}}}}
	})
	send := func(origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/chat/send", strings.NewReader(`{"component":"c1","choice":0}`))
		req.Host = "dash.local:9770"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer secret")
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		s.secure(s.Handler()).ServeHTTP(rec, req) // secure wraps the whole mux at serve time (Run, served)
		return rec
	}
	if rec := send("http://evil.example:9770"); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin tap = %d, want 403:\n%s", rec.Code, rec.Body.String())
	}
	if _, ok := turnOrNone(t, r); ok {
		t.Fatal("a cross-origin tap started a turn")
	}
	if rec := send("http://dash.local:9770"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "via choices") {
		t.Fatalf("same-origin tap = %d:\n%s", rec.Code, rec.Body.String())
	}
}

// The transcript keeps the tag and draws the interactive kinds.
func TestTranscript_ViaTagAndInteractiveKinds(t *testing.T) {
	s := componentServer(t, &metaRunner{})
	var all strings.Builder
	for _, h := range s.transcriptHTML(20) {
		all.WriteString(string(h))
	}
	got := all.String()
	for _, want := range []string{`<div class="via dim">via choices</div>`, `$component='c1';$choice=0;`, `<form class="comp comp-form"`, `data-bind:f_f1_tip`, `Split it</button>`} {
		if !strings.Contains(got, want) {
			t.Errorf("transcript missing %q:\n%s", want, got)
		}
	}
}
