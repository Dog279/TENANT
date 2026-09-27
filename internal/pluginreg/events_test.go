package pluginreg

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"tenant/internal/logs/event"
)

// A descriptor's catalog must be its own plugin's, complete.
func TestNewChecksTheEventCatalog(t *testing.T) {
	own, other := NewEvents("regtest-a", "A"), NewEvents("regtest-b", "B")
	if _, err := New(rules, desc{id: "regtest-a", events: own}, desc{id: "sql"}); err != nil {
		t.Fatalf("its own catalog, and none: %v", err)
	}
	borrowed := *own
	borrowed.Started = other.Started
	for _, tc := range []struct {
		name string
		d    desc
		want string
	}{
		{"another plugin's provider", desc{id: "regtest-b", events: own}, `own provider and log, "plugin.regtest-b"`},
		{"another plugin's event", desc{id: "regtest-a", events: &borrowed}, "plugin.regtest-b/1, which is not its provider's"},
		{"no lifecycle", desc{id: "regtest-a", events: &Events{Provider: own.Provider}}, "lacks a lifecycle event"},
	} {
		if _, err := New(rules, tc.d); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

type sink struct {
	mu  sync.Mutex
	got []*event.Event
}

func (s *sink) Emit(_ context.Context, e *event.Event) {
	s.mu.Lock()
	s.got = append(s.got, e)
	s.mu.Unlock()
}
func (s *sink) Audit(ctx context.Context, e *event.Event) error { s.Emit(ctx, e); return nil }

type failRT struct{}

func (failRT) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("dial tcp: no such host")
}

// Observe logs a spent quota, refused credentials and a server failure, and
// leaves the call's own 4xx and a success to the tool result.
func TestObserveLogsWhatTheOperatorMustActOn(t *testing.T) {
	ev := NewAPIEvents("regtest-api", "Svc")
	s := &sink{}
	event.Install(s)
	defer event.Install(nil)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/busy":
			w.WriteHeader(http.StatusTooManyRequests)
		case "/quota":
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"error":{"errors":[{"reason":"userRateLimitExceeded"}]}}`)
		case "/denied":
			w.WriteHeader(http.StatusForbidden)
		case "/down":
			w.WriteHeader(http.StatusBadGateway)
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := &http.Client{Transport: ev.Observe(nil)}
	want := map[string]uint32{"/busy": 2001, "/quota": 2001, "/denied": 3002, "/down": 3003, "/missing": 0, "/fine": 0}
	for path, id := range want {
		s.got = nil
		resp, err := c.Get(srv.URL + path + "?q=private")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if path == "/quota" && !strings.Contains(string(body), "userRateLimitExceeded") {
			t.Errorf("the 403's body was not put back: %q", body)
		}
		switch {
		case id == 0 && len(s.got) != 0:
			t.Errorf("%s: logged %q, want nothing", path, s.got[0].Msg)
		case id != 0 && (len(s.got) != 1 || s.got[0].ID != id || s.got[0].Provider != "plugin.regtest-api"):
			t.Errorf("%s: logged %+v, want one plugin.regtest-api/%d", path, s.got, id)
		case id != 0 && strings.Contains(s.got[0].Msg, "private"):
			t.Errorf("%s: the query reached the message: %q", path, s.got[0].Msg)
		}
	}

	s.got = nil
	c = &http.Client{Transport: ev.Observe(failRT{})}
	if _, err := c.Get("http://svc.invalid/x"); err == nil {
		t.Fatal("want the transport's error")
	}
	if len(s.got) != 1 || s.got[0].ID != 3003 || !strings.Contains(s.got[0].Msg, "no such host") {
		t.Errorf("an unreachable service logged %+v, want APIFailed with the error", s.got)
	}
}
