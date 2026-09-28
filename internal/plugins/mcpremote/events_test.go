package mcpremote

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"

	"tenant/internal/logs/event"
	"tenant/internal/logs/event/eventtest"
	"tenant/internal/model"
)

// seen lists the events recorded so far as "<log> <provider>/<id> <opcode>".
func seen(r *eventtest.Recorder) []string {
	var out []string
	for _, e := range r.Events() {
		out = append(out, e.Log+" "+e.Provider+"/"+strconv.FormatUint(uint64(e.ID), 10)+" "+e.Opcode)
	}
	return out
}

func find(r *eventtest.Recorder, id uint32) *event.Event {
	for _, e := range r.Events() {
		if e.Provider == "mcp" && e.ID == id {
			return e
		}
	}
	return nil
}

func field(e *event.Event, key string) string {
	for _, f := range e.Data {
		if f.Key == key {
			return f.Text()
		}
	}
	return ""
}

func TestServerLog(t *testing.T) {
	r := eventtest.Capture(t)
	for label, want := range map[string]string{
		"mcp:github":                     "mcp.github",
		"mcp:mcp.atlassian.com":          "mcp.mcp.atlassian.com",
		"mcp:127.0.0.1:9000/b#2":         "mcp.127.0.0.1-9000-b-2",
		"mcp:Files_Local":                "mcp.files_local",
		"mcp:" + strings.Repeat("x", 64): "mcp." + strings.Repeat("x", 36),
		"mcp:!!!":                        "mcp",
		"":                               "mcp",
	} {
		got := ServerLog(label)
		if got != want {
			t.Errorf("ServerLog(%q) = %q, want %q", label, got, want)
		}
		Connected.Log(WithServerLog(context.Background(), label))
		evs := r.Events()
		if e := evs[len(evs)-1]; e.Log != want {
			t.Errorf("%q: an event under WithServerLog went to %s, want %s", label, e.Log, want)
		}
	}
}

// A remote connection is an activity in the connector's own log: its start,
// Connected with the tools, a call that fails, and one stop however often the
// cleanup runs.
func TestRemoteConnectionIsAnActivity(t *testing.T) {
	r := eventtest.Capture(t)
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "ping"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, struct{}{}, nil
	})
	ms := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	d, cleanup, err := Open(context.Background(), Config{ServerURL: ms.URL, Label: "mcp:test", CacheDir: t.TempDir()}, true, Policy{AllowWrite: true})
	if err != nil {
		t.Fatal(err)
	}
	ms.Close()
	if out, isErr, _ := d.Dispatch(context.Background(), model.ToolCall{Name: "ping"}); !isErr || !strings.Contains(out, "failed") {
		t.Fatalf("a call to a stopped server = %q, %v", out, isErr)
	}
	cleanup()
	cleanup()
	want := []string{"mcp.test mcp/1 start", "mcp.test mcp/1001 ", "mcp.test mcp/3001 ", "mcp.test mcp/1 stop"}
	if got := seen(r); !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
	evs := r.Events()
	start, connected := evs[0], evs[1]
	if connected.Corr.ActivityID != start.Corr.ActivityID || evs[3].Corr.ActivityID != start.Corr.ActivityID ||
		field(connected, "tools") != "1" || field(connected, "names") != "ping" || field(start, "interactive") != "false" {
		t.Errorf("start %+v, connected %+v", start, connected)
	}
	if failed := evs[2]; field(failed, "tool") != "ping" || field(failed, "server") != "mcp:test" {
		t.Errorf("call failed = %+v", failed)
	}
}

// A launch reconnect with no saved sign-in stops, with a line saying so: it
// is the normal first run, not a failure.
func TestNoSavedSignInIsNotAFailure(t *testing.T) {
	r := eventtest.Capture(t)
	ms := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer ms.Close()
	if _, _, err := Open(context.Background(), Config{ServerURL: ms.URL, Label: "mcp:auth", CacheDir: t.TempDir()}, false, Policy{}); err == nil {
		t.Fatal("connected without a sign-in")
	}
	want := []string{"mcp.auth mcp/1 start", "mcp.auth mcp/1003 ", "mcp.auth mcp/1 stop"}
	if got := seen(r); !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

// A local server's connection is an activity under the server's own, and a
// server that says its tools changed is logged.
func TestStdioConnectionAndToolListChange(t *testing.T) {
	r := eventtest.Capture(t)
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "a"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
		return &mcp.CallToolResult{}, struct{}{}, nil
	})
	connect := func(ctx context.Context) (io.ReadCloser, io.WriteCloser, error) {
		clientR, serverW := io.Pipe()
		serverR, clientW := io.Pipe()
		go func() { _ = srv.Run(ctx, &mcp.IOTransport{Reader: serverR, Writer: serverW}) }()
		return clientR, clientW, nil
	}
	ctx, server := event.Start(WithServerLog(context.Background(), "mcp:fake"), Server, "server", "mcp:fake")
	_, cleanup, err := OpenStdio(ctx, StdioConfig{Label: "mcp:fake", Connect: connect}, false, Policy{})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	mcp.AddTool(srv, &mcp.Tool{Name: "b"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
		return &mcp.CallToolResult{}, struct{}{}, nil
	})
	deadline := time.Now().Add(5 * time.Second)
	for find(r, 1002) == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	server.End(nil)
	if e := find(r, 1002); e == nil || e.Log != "mcp.fake" {
		t.Errorf("the tool list change = %+v (events %q)", e, seen(r))
	}
	serverStart, conn := r.Events()[0], find(r, 1)
	if conn == nil || conn.Opcode != event.OpStart || conn.Corr.RelatedID != serverStart.Corr.ActivityID || conn.Log != "mcp.fake" {
		t.Errorf("the connection %+v is not under the server %+v", conn, serverStart)
	}
}

// A connector's sign-in is logged in its own log: a refresh, an early refresh
// that failed, its death once, with the fix, and its recovery.
func TestSignInEvents(t *testing.T) {
	r := eventtest.Capture(t)
	var count int32
	ok := rotatingTokenServer(t, &count, false)
	defer ok.Close()
	failing := rotatingTokenServer(t, &count, true)
	defer failing.Close()
	source := func(url string, expiry time.Duration) *refreshingSource {
		src, _ := newSourceFor(t, url, &oauth2.Token{AccessToken: "a", RefreshToken: "r", Expiry: time.Now().Add(expiry)})
		src.h.label, src.h.ctx = "mcp:x", WithServerLog(context.Background(), "mcp:x")
		return src
	}
	_, _ = source(ok.URL, time.Minute).Token()        // refreshed
	_, _ = source(failing.URL, 2*time.Minute).Token() // still valid
	dead := source(failing.URL, -time.Hour)
	_, _ = dead.Token()
	_, _ = dead.Token() // dead already: not logged again
	dead.h.clearReauth()
	want := []string{"mcp.x mcp/1102 ", "mcp.x mcp/2101 ", "mcp.x mcp/3101 ", "mcp.x mcp/1103 "}
	if got := seen(r); !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
	if e := find(r, 3101); field(e, "fix") != "tenant mcp connect https://srv.example/v1/mcp" || e.Level != event.Error {
		t.Errorf("needs re-auth = %+v", e)
	}
}
