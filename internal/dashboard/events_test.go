package dashboard

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/logs"
	"tenant/internal/logs/catalog"
	"tenant/internal/logs/event"
)

// capture keeps the dashboard events written while it is installed.
type capture struct {
	mu     sync.Mutex
	events []*event.Event
}

func (c *capture) Emit(_ context.Context, e *event.Event) {
	c.mu.Lock()
	c.events = append(c.events, e)
	c.mu.Unlock()
}

func (c *capture) Audit(ctx context.Context, e *event.Event) error { c.Emit(ctx, e); return nil }

// lines renders the dashboard events as "id opcode msg k=v…", one each.
func (c *capture) lines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, e := range c.events {
		if e.Provider != Events.Name {
			continue
		}
		l := fmt.Sprintf("%d %s %s", e.ID, e.Opcode, e.Msg)
		for _, f := range e.Data {
			if f.Key != "duration" {
				l += " " + f.Key + "=" + f.Text()
			}
		}
		out = append(out, l)
	}
	return out
}

// has reports whether some line contains every part.
func (c *capture) has(parts ...string) bool {
	for _, l := range c.lines() {
		all := true
		for _, p := range parts {
			all = all && strings.Contains(l, p)
		}
		if all {
			return true
		}
	}
	return false
}

// installCapture makes a capture the event emitter for the test. Not for
// parallel tests: the emitter is process-wide.
func installCapture(t *testing.T) *capture {
	c := &capture{}
	event.Install(c)
	t.Cleanup(func() { event.Install(nil) })
	return c
}

// The server's lifecycle, each request (method, route, status: never the
// query), its own error log and a rejection are all catalogued.
func TestServeRecordsLifecycleRequestsAndErrors(t *testing.T) {
	if g := strings.Join(catalog.Group(Events.Log), "/"); g != "Subsystems/Dashboard" {
		t.Errorf("the dashboard log sits in %q of the viewer's tree", g)
	}
	c := installCapture(t)
	s := New(Config{Addr: "127.0.0.1:0", Auth: "tok"}, nil, nil, nil, nil, nil)
	s.mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) { panic("boom-marker") })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	base := "http://" + ln.Addr().String()

	get := func(path, token string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0 // the panicking handler drops the connection
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get("/healthz?token=query-secret", ""); code != http.StatusOK {
		t.Fatalf("healthz = %d", code)
	}
	if code := get("/tools", ""); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /tools = %d", code)
	}
	get("/boom", "tok")
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	for _, want := range [][]string{
		{"1 start Dashboard on 127.0.0.1:"},
		{"1 stop "},
		{"9001 start GET /healthz", "method=GET route=/healthz"},
		{"9001 stop GET /healthz", "status=200"},
		{"2103  Request without a valid token rejected: GET /tools", "credential=missing"},
		{"9001 stop GET /tools", "status=401"},
		{"9001 fail GET /boom", "status=500", "error=the handler panicked"},
		{"2001  HTTP server: http: panic serving", "boom-marker"},
	} {
		if !c.has(want...) {
			t.Errorf("no event with %q in:\n%s", want, strings.Join(c.lines(), "\n"))
		}
	}
	if strings.Contains(strings.Join(c.lines(), "\n"), "query-secret") {
		t.Errorf("a request's query reached the log book:\n%s", strings.Join(c.lines(), "\n"))
	}
}

// Every rejection the secure envelope makes, and a failed sign-in, is
// catalogued with its reason.
func TestRejectionsAreCatalogued(t *testing.T) {
	c := installCapture(t)
	s := withSession(New(Config{Addr: "127.0.0.1:8770", Auth: "tok"}, nil, nil, nil, nil, nil))
	h := s.secure(s.Handler())
	req := func(method, host, path string, hdr ...string) *http.Request {
		r := httptest.NewRequest(method, "http://"+host+path, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		return r
	}
	for _, r := range []*http.Request{
		req("GET", "evil.example", "/tools"),
		req("GET", "127.0.0.1:8770", "/tools", "Origin", "http://evil.example"),
		req("GET", "127.0.0.1:8770", "/tools", "Authorization", "Bearer wrong"),
		req("POST", "127.0.0.1:8770", "/tools/x/toggle", "Cookie", dashCookieName+"="+testSession),
	} {
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	login := httptest.NewRequest("POST", "http://127.0.0.1:8770/auth/login", strings.NewReader(url.Values{"token": {"nope"}}.Encode()))
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	login.Header.Set("Origin", "http://127.0.0.1:8770")
	h.ServeHTTP(httptest.NewRecorder(), login)

	for _, want := range [][]string{
		{"2101 ", "unrecognized host evil.example", "route=/tools"},
		{"2102 ", "from http://evil.example"},
		{"2103 ", "credential=invalid"},
		{"2104 ", "POST /tools/{name}/toggle"},
		{"2105 ", "Sign-in with a wrong token"},
	} {
		if !c.has(want...) {
			t.Errorf("no event with %q in:\n%s", want, strings.Join(c.lines(), "\n"))
		}
	}
	if strings.Contains(strings.Join(c.lines(), "\n"), "nope") {
		t.Error("a presented token reached the log book")
	}
}

// A live stream is an activity from connect to disconnect, and a page that
// fell behind the ring is reported with what it missed.
func TestStreamsConnectDisconnectAndLag(t *testing.T) {
	c := installCapture(t)
	l := logs.NewMemory(2)
	for i := range 5 {
		l.Logf("cron", slog.LevelInfo, "line", "i", i)
	}
	s := New(Config{}, nil, nil, nil, nil, nil)
	s.SetLogs(l)
	r := httptest.NewRequest("GET", "/activity/events?cursor=1", nil)
	ctx, cancel := context.WithCancel(r.Context())
	done := make(chan struct{})
	go func() { s.handleActivitySSE(httptest.NewRecorder(), r.WithContext(ctx)); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for !c.has("2002 ") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	for _, want := range [][]string{
		{"1001 start Live activity stream"},
		{"2002  Live activity stream", "fell behind", "missed=2"},
		{"1001 stop Live activity stream"},
	} {
		if !c.has(want...) {
			t.Errorf("no event with %q in:\n%s", want, strings.Join(c.lines(), "\n"))
		}
	}
}
