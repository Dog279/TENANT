package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/agent"
	"tenant/internal/logs"
)

func logsServer(t *testing.T, root string) (*Server, *logs.Hub) {
	t.Helper()
	h, err := logs.Open(root, logs.Options{RingCap: 100})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	s := New(Config{}, nil, &restFakeTools{}, nil, agent.NewBroker(0), nil)
	s.SetLogs(h)
	return s, h
}

func do(t *testing.T, s *Server, method, path, ctype, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if ctype != "" {
		r.Header.Set("Content-Type", ctype)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec
}

// POST /api/logs/{source} creates the source (folder + tab) on first write and
// files the line; the sources list and the Activity page see it at once.
func TestRESTLogs_WriteCreatesSource(t *testing.T) {
	root := t.TempDir()
	s, h := logsServer(t, root)

	rec := do(t, s, http.MethodPost, "/api/logs/My%20Sidecar", "application/json",
		`{"level":"warn","msg":"disk at 91%","fields":{"host":"build-box","pct":"91"}}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var ack map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &ack)
	if ack["source"] != "my-sidecar" || ack["seq"] != "1" {
		t.Errorf("ack = %v", ack)
	}
	all, _ := h.Snapshot()
	if len(all) != 1 || all[0].Source != "my-sidecar" || all[0].Level != slog.LevelWarn || all[0].Field("host") != "build-box" || all[0].Field("pct") != "91" {
		t.Fatalf("entry = %+v", all)
	}
	files, _ := filepath.Glob(filepath.Join(root, "my-sidecar", "*.log"))
	if len(files) != 1 {
		t.Fatalf("expected one daily file, got %v", files)
	}
	b, _ := os.ReadFile(files[0])
	if !strings.Contains(string(b), "WARN  [my-sidecar] disk at 91%\thost=build-box pct=91\n") {
		t.Errorf("file line = %q", string(b))
	}

	// A text/plain body is the message; ?level= sets the level.
	rec = do(t, s, http.MethodPost, "/api/logs/my-sidecar?level=error", "text/plain", "backup failed\n")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("text write = %d %s", rec.Code, rec.Body.String())
	}
	all, _ = h.Snapshot()
	if all[1].Msg != "backup failed" || !all[1].IsErr() {
		t.Errorf("text entry = %+v", all[1])
	}

	// Sources + tab.
	rec = do(t, s, http.MethodGet, "/api/logs", "", "")
	var srcs restLogSourcesResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &srcs)
	if srcs.Root != root || len(srcs.Sources) != 1 || srcs.Sources[0].Name != "my-sidecar" || !srcs.Sources[0].Native || srcs.Sources[0].Count != 2 {
		t.Errorf("sources = %+v", srcs)
	}
	if body := get(t, s, "/activity").Body.String(); !strings.Contains(body, `href="/activity?source=my-sidecar"`) {
		t.Error("the new source should have a tab")
	}
	// The live broker saw it too (open TUI feeds).
}

func TestRESTLogs_WriteValidation(t *testing.T) {
	s, _ := logsServer(t, t.TempDir())
	cases := []struct {
		path, ctype, body string
		want              int
	}{
		{"/api/logs/---", "application/json", `{"msg":"x"}`, http.StatusBadRequest}, // no usable name
		{"/api/logs/ok", "application/json", `{"msg":""}`, http.StatusBadRequest},   // empty msg
		{"/api/logs/ok", "application/json", `not json`, http.StatusBadRequest},     // bad json
		{"/api/logs/ok", "application/json", `{"msg":"x","level":"loud"}`, http.StatusBadRequest},
		{"/api/logs/ok", "application/json", `{"msg":"x","fields":{"bad key":"v"}}`, http.StatusBadRequest},
		{"/api/logs/ok", "application/json", `{"msg":"x","fields":{"k=v":"v"}}`, http.StatusBadRequest},
		{"/api/logs/ok", "text/plain", "   ", http.StatusBadRequest},
		{"/api/logs/ok", "application/json", `{"msg":"fine"}`, http.StatusAccepted},
	}
	for _, c := range cases {
		if rec := do(t, s, http.MethodPost, c.path, c.ctype, c.body); rec.Code != c.want {
			t.Errorf("POST %s %q → %d, want %d (%s)", c.path, c.body, rec.Code, c.want, rec.Body.String())
		}
	}
	// No hub → 503, never a panic.
	bare := New(Config{}, nil, &restFakeTools{}, nil, nil, nil)
	if rec := do(t, bare, http.MethodPost, "/api/logs/ok", "application/json", `{"msg":"x"}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no-hub write = %d", rec.Code)
	}
}

// The legacy add-on endpoint keeps working and files under the add-on's own
// name — so every add-on is its own folder/tab now.
func TestRESTAddon_CompatFilesUnderItsName(t *testing.T) {
	s, h := logsServer(t, t.TempDir())
	rec := do(t, s, http.MethodPost, "/api/addon/event", "application/json", `{"source":"Hermes Sentinel","text":"quality loop ok","level":"error"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("addon = %d %s", rec.Code, rec.Body.String())
	}
	all, _ := h.Snapshot()
	if len(all) != 1 || all[0].Source != "hermes-sentinel" || all[0].Kind() != "addon" || !all[0].IsErr() || all[0].Msg != "quality loop ok" {
		t.Fatalf("addon entry = %+v", all)
	}
	// Projection: kind stays "addon", agent is the add-on, text the line.
	rec = do(t, s, http.MethodGet, "/api/activity", "", "")
	var resp restActivityResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Events) != 1 || resp.Events[0].Kind != "addon" || resp.Events[0].Text != "quality loop ok" || resp.Events[0].Source != "hermes-sentinel" || !resp.Events[0].IsErr {
		t.Errorf("projection = %+v", resp.Events)
	}
}

// GET /api/activity honors source/level, and plain records project as kind "log".
func TestRESTActivity_FiltersAndLogKind(t *testing.T) {
	s, h := logsServer(t, t.TempDir())
	logEvent(h, agent.Event{Kind: agent.EventToolCall, Tool: "web_search", Args: `{"q":"x"}`, Iter: 3})
	h.Logf("cron", slog.LevelInfo, "job fired", "job", "nightly")
	h.Logf("cron", slog.LevelError, "job failed", "job", "nightly")

	rec := do(t, s, http.MethodGet, "/api/activity?source=cron", "", "")
	var resp restActivityResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Events) != 2 || resp.Events[0].Kind != "log" || resp.Events[0].Msg != "job fired" || resp.Events[0].Fields["job"] != "nightly" || resp.Events[0].Level != "INFO" {
		t.Errorf("cron projection = %+v", resp.Events)
	}
	rec = do(t, s, http.MethodGet, "/api/activity?level=error", "", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Events) != 1 || resp.Events[0].Msg != "job failed" {
		t.Errorf("error projection = %+v", resp.Events)
	}
	rec = do(t, s, http.MethodGet, "/api/activity?source=turns", "", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Events) != 1 || resp.Events[0].Tool != "web_search" || resp.Events[0].Iter != 3 || resp.Events[0].Text != `{"q":"x"}` {
		t.Errorf("turn projection = %+v", resp.Events)
	}
	if _, has := resp.Events[0].Fields["ev"]; has {
		t.Error("the ev field is the kind; it must not be duplicated into fields")
	}
}

// Files: list + raw view through both the page route and the API; traversal
// and foreign names are rejected.
func TestRESTLogs_FilesAndRawView(t *testing.T) {
	root := t.TempDir()
	s, h := logsServer(t, root)
	h.Logf("turns", slog.LevelInfo, "hello file")
	_ = os.WriteFile(filepath.Join(root, "secret.txt"), []byte("nope"), 0o644)

	rec := do(t, s, http.MethodGet, "/api/logs/turns", "", "")
	var files restLogFilesResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &files)
	if len(files.Files) != 1 || !strings.HasSuffix(files.Files[0].Name, ".log") || files.Files[0].Size == 0 {
		t.Fatalf("files = %+v", files)
	}
	name := files.Files[0].Name
	for _, p := range []string{"/activity/files/turns/" + name, "/api/logs/turns/" + name} {
		rec = get(t, s, p)
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") || !strings.Contains(rec.Body.String(), "[turns] hello file") {
			t.Errorf("%s → %d %q %q", p, rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
		}
	}
	rec = get(t, s, "/activity/files/turns/"+name+"?download=1")
	if !strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment") {
		t.Error("?download should attach")
	}
	for _, p := range []string{"/activity/files/turns/..%2Fsecret.txt", "/api/logs/turns/secret.txt", "/api/logs/..%2F/secret.txt", "/activity/files/turns/.hidden.log"} {
		if rec = get(t, s, p); rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "nope") {
			t.Errorf("%s leaked a file outside the folder", p)
		}
	}
	if rec = do(t, s, http.MethodGet, "/api/logs/BAD..NAME", "", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("bad source name = %d", rec.Code)
	}
}
