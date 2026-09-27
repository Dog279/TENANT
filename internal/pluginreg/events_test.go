package pluginreg

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tenant/internal/logs/event/eventtest"
)

// A descriptor's catalog must be its own plugin's.
func TestNewChecksTheEventCatalog(t *testing.T) {
	own := NewEvents("regtest-a", "A")
	if _, err := New(rules, desc{id: "regtest-a", events: own}, desc{id: "sql"}); err != nil {
		t.Fatalf("its own catalog, and none: %v", err)
	}
	if _, err := New(rules, desc{id: "regtest-b", events: own}); err == nil || !strings.Contains(err.Error(), `own provider and log, "plugin.regtest-b"`) {
		t.Errorf("another plugin's provider: err = %v", err)
	}
}

type failRT struct{}

func (failRT) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("dial tcp: no such host")
}

// Observe logs a spent quota, refused credentials and a server failure, and
// leaves the call's own 4xx and a success to the tool result.
func TestObserveLogsWhatTheOperatorMustActOn(t *testing.T) {
	ev := NewAPIEvents("regtest-api", "Svc")

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
		rec := eventtest.Capture(t)
		resp, err := c.Get(srv.URL + path + "?q=private")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if path == "/quota" && !strings.Contains(string(body), "userRateLimitExceeded") {
			t.Errorf("the 403's body was not put back: %q", body)
		}
		got := rec.Events()
		switch {
		case id == 0 && len(got) != 0:
			t.Errorf("%s: logged %q, want nothing", path, got[0].Msg)
		case id != 0 && (len(got) != 1 || got[0].ID != id || got[0].Provider != "plugin.regtest-api"):
			t.Errorf("%s: logged %+v, want one plugin.regtest-api/%d", path, got, id)
		case id != 0 && strings.Contains(got[0].Msg, "private"):
			t.Errorf("%s: the query reached the message: %q", path, got[0].Msg)
		}
	}

	rec := eventtest.Capture(t)
	c = &http.Client{Transport: ev.Observe(failRT{})}
	if _, err := c.Get("http://svc.invalid/x"); err == nil {
		t.Fatal("want the transport's error")
	}
	got := rec.Events()
	if len(got) != 1 || got[0].ID != 3003 || !strings.Contains(got[0].Msg, "no such host") {
		t.Errorf("an unreachable service logged %+v, want APIFailed with the error", got)
	}
}
