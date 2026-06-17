package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tenant/internal/agent"
)

// fakeApprovals is a static ApprovalControl for the REST tests.
type fakeApprovals struct {
	pending   []PendingApproval
	decideErr error
	decided   []string // "id|decision"
}

func (f *fakeApprovals) Pending() []PendingApproval { return f.pending }
func (f *fakeApprovals) Decide(id, decision string) error {
	f.decided = append(f.decided, id+"|"+decision)
	return f.decideErr
}

// fakeLivenessRunner is an AgentRunner that also reports turn liveness (the
// duck-typed dashboard.liveness the status handler reads).
type fakeLivenessRunner struct {
	active bool
	age    time.Duration
}

func (r *fakeLivenessRunner) Turn(context.Context, agent.TurnRequest) (*agent.TurnResult, error) {
	return &agent.TurnResult{}, nil
}
func (r *fakeLivenessRunner) Interject(string)                  {}
func (r *fakeLivenessRunner) ActiveTurn() (bool, time.Duration) { return r.active, r.age }

// TestRESTApprovals_ListAndDecide: GET lists pending; POST routes the decision.
func TestRESTApprovals_ListAndDecide(t *testing.T) {
	fa := &fakeApprovals{pending: []PendingApproval{
		{ID: "ap-1", Category: "exec", Action: "os_exec", Detail: "rm x", AgeSecs: 3},
	}}
	s := New(Config{}, nil, &restFakeTools{}, nil, agent.NewBroker(0), nil)
	s.SetApprovals(fa)

	// GET /api/approvals
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/approvals", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d", rec.Code)
	}
	var list []PendingApproval
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 1 || list[0].ID != "ap-1" {
		t.Fatalf("list = %+v", list)
	}

	// POST /api/approvals/ap-1
	rec = httptest.NewRecorder()
	body := strings.NewReader(`{"decision":"approve"}`)
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/approvals/ap-1", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(fa.decided) != 1 || fa.decided[0] != "ap-1|approve" {
		t.Fatalf("decided = %v", fa.decided)
	}
}

// TestRESTApprovals_NotConfigured: with no control, GET is an empty list and
// POST is 503 (never a panic, never a silent approve).
func TestRESTApprovals_NotConfigured(t *testing.T) {
	s := New(Config{}, nil, &restFakeTools{}, nil, agent.NewBroker(0), nil)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/approvals", nil))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("GET nil-control = %d %q, want 200 []", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/approvals/ap-1", strings.NewReader(`{"decision":"approve"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST nil-control = %d, want 503", rec.Code)
	}
}

// TestRESTActivity_SinceCursor: GET /api/activity?since= returns events after
// the cursor plus the new head cursor.
func TestRESTActivity_SinceCursor(t *testing.T) {
	l := agent.NewEventLog(100)
	l.Append(agent.Event{Kind: agent.EventToolCall, Tool: "os_read"})
	l.Append(agent.Event{Kind: agent.EventFinal, Text: "done"})
	s := New(Config{}, nil, &restFakeTools{}, nil, agent.NewBroker(0), nil)
	s.SetEventLog(l)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/activity", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp restActivityResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Events) != 2 || resp.Cursor != 2 {
		t.Fatalf("events=%d cursor=%d, want 2/2", len(resp.Events), resp.Cursor)
	}
	if resp.Events[0].Kind != "tool_call" || resp.Events[0].Tool != "os_read" {
		t.Fatalf("event[0] = %+v", resp.Events[0])
	}

	// since the head cursor → no new events.
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/activity?since=2", nil))
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Events) != 0 {
		t.Fatalf("since=2 should return no events, got %d", len(resp.Events))
	}
}

// TestRESTStatus_Liveness: a liveness-reporting runner + approvals control
// populate the serve fields.
func TestRESTStatus_Liveness(t *testing.T) {
	fa := &fakeApprovals{pending: []PendingApproval{{ID: "ap-1"}, {ID: "ap-2"}}}
	runner := &fakeLivenessRunner{active: true, age: 7 * time.Second}
	s := New(Config{}, runner, &restFakeTools{plugins: []string{}}, nil, agent.NewBroker(0), nil)
	s.SetApprovals(fa)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	var st restStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !st.TurnActive || st.TurnAgeSecs != 7 {
		t.Errorf("liveness = active %v age %d, want true/7", st.TurnActive, st.TurnAgeSecs)
	}
	if st.PendingApprovals != 2 {
		t.Errorf("pending_approvals = %d, want 2", st.PendingApprovals)
	}
}
