package dashboard

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"tenant/internal/logs"
	"tenant/internal/logs/event"
	"tenant/internal/logs/store"
)

// The log book's API sits behind the dashboard's bearer auth.
func TestLogbookAPIRequiresAuth(t *testing.T) {
	s, _, _ := logsBook(t)
	h := s.secure(s.Handler())
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/events", ""}, {"GET", "/api/events/stream", ""}, {"POST", "/api/events", `{"log":"x","msg":"m"}`},
		{"GET", "/api/logbook/logs", ""}, {"PATCH", "/api/logbook/logs/x", `{}`}, {"POST", "/api/logbook/logs/x/clear", ""},
		{"GET", "/api/logbook/views", ""}, {"GET", "/api/logbook/catalog", ""}, {"GET", "/api/logbook/components", ""},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, strings.NewReader(c.body)))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a token = %d", c.method, c.path, rec.Code)
		}
	}
	r := httptest.NewRequest("GET", "/api/events", nil)
	r.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Errorf("with the token = %d %s", rec.Code, rec.Body.String())
	}
	if !isDashboardStream(httptest.NewRequest("GET", "/api/events/stream", nil)) || !isDashboardStream(httptest.NewRequest("GET", "/api/events/export", nil)) {
		t.Error("the live tail and exports must be exempt from the write timeout")
	}
}

// With a store, GET /api/activity reads it: the same projection and
// filters, the store's seq as the cursor and its ID as the epoch, so a
// cursor survives a restart; diagnostic events stay out, as they are out of
// the text files.
func TestActivityShimOverTheStore(t *testing.T) {
	s, _, st := logsBook(t)
	ctx := context.Background()
	trace := "0123456789abcdef0123456789abcdef"
	evs := []event.Event{
		logs.EventFromEntry(logs.Entry{Source: "cron", Level: slog.LevelInfo, Msg: "job fired", Fields: []logs.Field{{Key: "job", Value: "nightly"}}}),
		logs.EventFromEntry(logs.Entry{Source: "cron", Level: slog.LevelError, Msg: "job failed"}),
		{Log: "cron", Provider: "cron", Level: event.Info, Channel: event.Diagnostic, Msg: "diagnostic detail"},
		logs.EventFromEntry(logs.Entry{Source: "turns", Level: slog.LevelInfo, Msg: "tool call web_search", Fields: []logs.Field{
			{Key: "ev", Value: "tool_call"}, {Key: "tool", Value: "web_search"}, {Key: "iter", Value: "3"}, {Key: "args", Value: `{"q":"x"}`}, {Key: "trace", Value: trace}}}),
	}
	for i := range evs {
		evs[i].Time = time.Now().UTC()
	}
	if err := st.Commit(ctx, evs); err != nil {
		t.Fatal(err)
	}
	get := func(q string) restActivityResponse {
		t.Helper()
		var resp restActivityResponse
		rec := do(t, s, http.MethodGet, "/api/activity?"+q, "", "")
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != 200 {
			t.Fatalf("%s: %d %s", q, rec.Code, rec.Body.String())
		}
		return resp
	}
	id := st.StoreID()
	epoch := hex.EncodeToString(id[:])
	r := get("source=cron")
	if len(r.Events) != 2 || r.Events[0].Kind != "log" || r.Events[0].Fields["job"] != "nightly" || r.Epoch != epoch || r.Cursor != evs[3].Seq {
		t.Fatalf("cron = %+v cursor=%d epoch=%s", r.Events, r.Cursor, r.Epoch)
	}
	if r = get("level=error"); len(r.Events) != 1 || r.Events[0].Msg != "job failed" {
		t.Errorf("errors = %+v", r.Events)
	}
	r = get("trace=" + trace)
	if len(r.Events) != 1 || r.Events[0].Kind != "tool_call" || r.Events[0].Tool != "web_search" || r.Events[0].Iter != 3 || r.Events[0].Text != `{"q":"x"}` {
		t.Errorf("trace = %+v", r.Events)
	}
	if r = get("limit=1"); len(r.Events) != 1 || r.Cursor != evs[0].Seq {
		t.Errorf("limit = %+v cursor=%d", r.Events, r.Cursor)
	}
	if r = get("since=1&epoch=" + epoch); len(r.Events) != 2 || r.Events[0].Msg != "job failed" {
		t.Errorf("since = %+v", r.Events)
	}
	if r = get("since=3&epoch=0ld3p0ch"); len(r.Events) != 3 {
		t.Errorf("a cursor from another epoch replays: %+v", r.Events)
	}
}

// v1 writes through a hub with a store are filed as external.
func TestV1WritesAreExternal(t *testing.T) {
	s, _, st := logsBook(t)
	if rec := do(t, s, http.MethodPost, "/api/logs/sidecar", "application/json", `{"msg":"hello","fields":{"event":"system/3101","k":"v"}}`); rec.Code != http.StatusAccepted {
		t.Fatalf("v1 write = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, s, http.MethodPost, "/api/addon/event", "application/json", `{"source":"Sentinel","text":"loop ok"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("addon = %d %s", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		evs, _, _, _ := st.Since(context.Background(), store.Cursor{}, 0)
		if len(evs) == 2 {
			for _, e := range evs {
				if !slices.Equal(e.Keywords, []string{"external"}) || e.ID != 0 {
					t.Errorf("%s: %+v", e.Log, e)
				}
			}
			if evs[1].Log != "ext.sentinel" || !slices.ContainsFunc(evs[1].Data, func(f event.Field) bool { return f.Key == "ev" && f.Value == "addon" }) {
				t.Errorf("addon event = %+v", evs[1])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("store holds %+v", evs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
