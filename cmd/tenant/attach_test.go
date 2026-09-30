package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeHub serves the subset of the hub API the attach client reads, with a
// bearer-token gate and cursor-based /api/activity paging.
func fakeHub(t *testing.T, token string, events []attachEvent) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if token == "" {
			return true
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode(attachStatus{Status: "ok", ToolsEnabled: 3, ToolsTotal: 5, PendingApprovals: 1})
	})
	mux.HandleFunc("/api/activity", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
		out := attachActivityResp{Events: []attachEvent{}, Cursor: since}
		for _, e := range events {
			if e.Seq > since {
				out.Events = append(out.Events, e)
				if e.Seq > out.Cursor {
					out.Cursor = e.Seq
				}
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestAttach_FetchActivityCursorPaging(t *testing.T) {
	ctx := context.Background()
	evs := []attachEvent{
		{Seq: 1, At: "2026-06-18T10:00:00Z", Kind: "turn_start", Text: "hi"},
		{Seq: 2, At: "2026-06-18T10:00:01Z", Kind: "tool_call", Tool: "os_read"},
		{Seq: 3, At: "2026-06-18T10:00:02Z", Kind: "final", Text: "done"},
	}
	srv := fakeHub(t, "", evs)
	httpc := srv.Client()

	got, cursor, err := attachFetchActivity(ctx, httpc, srv.URL, "", 0)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 3 || cursor != 3 {
		t.Fatalf("first fetch: %d events cursor=%d, want 3/3", len(got), cursor)
	}
	// Tail from the cursor → only NEW events (none yet).
	got2, cursor2, _ := attachFetchActivity(ctx, httpc, srv.URL, "", cursor)
	if len(got2) != 0 || cursor2 != 3 {
		t.Errorf("tail from cursor 3: %d events cursor=%d, want 0/3", len(got2), cursor2)
	}
}

// --trace narrows the tail to one turn's lines (TEN-362); flags placed after
// the URL are honored (TEN-367).
func TestAttach_TraceFilter(t *testing.T) {
	sent := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(attachStatus{Status: "ok"})
	})
	mux.HandleFunc("/api/activity", func(w http.ResponseWriter, r *http.Request) {
		select {
		case sent <- r.URL.Query().Get("trace"):
		default:
		}
		_ = json.NewEncoder(w).Encode(attachActivityResp{Events: []attachEvent{}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	dir := t.TempDir()
	if err := cmdAttach(context.Background(), []string{srv.URL, "--config", dir, "--data", dir, "--token", "x", "--trace", "4f2a"}); err != nil {
		t.Fatal(err)
	}
	if got := <-sent; got != "4f2a" {
		t.Fatalf("trace sent = %q, want 4f2a", got)
	}
}

// A restarted hub numbers from 1 again under a new epoch: the next page takes
// its lower cursor and replays what it kept, instead of going quiet until the
// new head passes the old cursor. Within one epoch the cursor never regresses
// (TEN-367).
func TestAttach_ResyncsAcrossRestart(t *testing.T) {
	var mu sync.Mutex
	epoch, head, reported := "e1", uint64(5), uint64(5)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		q := r.URL.Query()
		since, _ := strconv.ParseUint(q.Get("since"), 10, 64)
		if e := q.Get("epoch"); e != "" && e != epoch {
			since = 0
		}
		out := attachActivityResp{Events: []attachEvent{}, Cursor: reported, Epoch: epoch}
		for s := since + 1; s <= head; s++ {
			out.Events = append(out.Events, attachEvent{Seq: s, Kind: "log"})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()
	ctx, httpc := context.Background(), srv.Client()
	set := func(e string, h, rep uint64) { mu.Lock(); epoch, head, reported = e, h, rep; mu.Unlock() }

	_, cur, ep, err := attachFetchActivityFiltered(ctx, httpc, srv.URL, "", 0, "", "")
	if err != nil || cur != 5 || ep != "e1" {
		t.Fatalf("first page: cursor=%d epoch=%q err=%v", cur, ep, err)
	}
	set("e1", 5, 3) // same epoch, a lower cursor: malformed, ignored
	if _, cur, _, _ = attachFetchActivityFiltered(ctx, httpc, srv.URL, "", cur, ep, ""); cur != 5 {
		t.Fatalf("within one epoch the cursor regressed to %d", cur)
	}
	set("e2", 2, 2) // restarted
	evs, cur, ep, err := attachFetchActivityFiltered(ctx, httpc, srv.URL, "", cur, ep, "")
	if err != nil || len(evs) != 2 || cur != 2 || ep != "e2" {
		t.Fatalf("after restart: %d events cursor=%d epoch=%q err=%v, want 2/2/e2", len(evs), cur, ep, err)
	}
}

func TestAttach_BearerTokenEnforced(t *testing.T) {
	ctx := context.Background()
	srv := fakeHub(t, "sekret", []attachEvent{{Seq: 1, At: "2026-06-18T10:00:00Z", Kind: "final"}})
	httpc := srv.Client()

	// Wrong/missing token → 401 surfaced as an error.
	if _, _, err := attachFetchActivity(ctx, httpc, srv.URL, "", 0); err == nil {
		t.Error("missing token should 401")
	}
	if _, _, err := attachFetchActivity(ctx, httpc, srv.URL, "wrong", 0); err == nil {
		t.Error("wrong token should 401")
	}
	// Correct token → ok.
	got, _, err := attachFetchActivity(ctx, httpc, srv.URL, "sekret", 0)
	if err != nil || len(got) != 1 {
		t.Errorf("correct token: %d events err=%v, want 1/nil", len(got), err)
	}
	// Status probe honors the token too.
	if _, err := attachFetchStatus(ctx, httpc, srv.URL, "sekret"); err != nil {
		t.Errorf("status with token: %v", err)
	}
}

func TestAttach_BaseURLResolution(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:9000":           "http://127.0.0.1:9000",
		"http://host:1/":           "http://host:1",
		"https://hub.example:8443": "https://hub.example:8443",
	}
	for in, want := range cases {
		if got := attachBaseURL(in, &launchConfig{}); got != want {
			t.Errorf("attachBaseURL(%q)=%q want %q", in, got, want)
		}
	}
	// Empty arg → falls back to the configured/default dashboard addr.
	if got := attachBaseURL("", &launchConfig{}); !strings.HasPrefix(got, "http://") {
		t.Errorf("empty arg should default to the local dashboard addr, got %q", got)
	}
}

func TestAttach_FormatEvent(t *testing.T) {
	line := formatAttachEvent(attachEvent{At: "2026-06-18T10:00:00Z", Kind: "tool_result", Tool: "os_exec", IsErr: true, Text: "line1\n  line2"})
	for _, want := range []string{"tool_result", "[os_exec]", "✗", "line1 line2"} {
		if !strings.Contains(line, want) {
			t.Errorf("format missing %q: %q", want, line)
		}
	}
	// A scheduled job's tool is named cron:<tool>, as the TUI names it.
	if line := formatAttachEvent(attachEvent{Kind: "tool_call", Source: "cron", Agent: "tenant-cron", Tool: "web_search"}); !strings.Contains(line, "[cron:web_search]") {
		t.Errorf("cron tool line = %q", line)
	}
}
