package tenantlog

// These tests are NOT part of the copy. They pin the reference client to the
// hub it talks to: its event JSON byte-for-byte against the log book's wire
// form, the text format byte-for-byte, source and log names, the data-dir
// rule, the spool protocol, and a round trip through the real API — so a
// copy taken from this folder is always compatible with the running tenant.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"tenant/internal/logs"
	"tenant/internal/logs/event"
	"tenant/internal/logs/logapi"
	"tenant/internal/logs/query"
	"tenant/internal/logs/store"
	"tenant/internal/memory/archive"
	"tenant/internal/memory/soul"
)

func TestLineMatchesHubByteForByte(t *testing.T) {
	at := time.Date(2026, 9, 22, 14, 3, 11, 123_000_000, time.Local)
	cases := []struct {
		level  slog.Level
		msg    string
		fields []field
	}{
		{Info, "build started", []field{{"commit", "abc123"}, {"host", "build-box"}}},
		{Error, "tool result web_search\tfailed\nhard", []field{{"args", `{"q":"a b"}`}, {"empty", ""}, {"eq", "a=b"}, {"uni", "héllo → wörld"}, {"multi", "l1\nl2"}}},
		{Warn, "no fields", nil},
		{Debug, "custom level buckets", []field{{"k", "v"}}},
	}
	for _, c := range cases {
		e := entry{at: at, level: c.level, msg: c.msg, fields: c.fields}
		var hf []logs.Field
		for _, f := range c.fields {
			hf = append(hf, logs.Field{Key: f.key, Value: f.value})
		}
		hub := logs.Entry{At: at, Source: "sidecar", Level: c.level, Msg: c.msg, Fields: hf}.Line()
		if got := e.lineFor("sidecar"); got != hub {
			t.Errorf("format drift:\n ref: %q\n hub: %q", got, hub)
		}
		parsed, ok := logs.ParseLine(e.lineFor("sidecar"))
		if !ok || parsed.Source != "sidecar" || parsed.Level != c.level || len(parsed.Fields) != len(c.fields) {
			t.Errorf("hub cannot parse the reference line %q: ok=%v parsed=%+v", e.lineFor("sidecar"), ok, parsed)
		}
	}
}

func TestNormalizeSourceMatchesHub(t *testing.T) {
	for _, in := range []string{"deploy", "My Sidecar!", "  Sub Agents ", "..", "", ".hidden", "a/b", "UPPER_case.1", strings.Repeat("x", 60), "-x", "9lives"} {
		if got, want := NormalizeSource(in), logs.NormalizeSource(in); got != want {
			t.Errorf("NormalizeSource(%q) = %q, hub says %q", in, got, want)
		}
	}
}

func TestDefaultDataDirMatchesTenant(t *testing.T) {
	want, err := archive.DefaultDir()
	if err != nil {
		t.Skip(err)
	}
	if got := defaultDataDir(); got != want {
		t.Errorf("defaultDataDir() = %q, tenant uses %q", got, want)
	}
}

// The client's event JSON is the log book's wire form byte for byte: the hub
// reads it into an event and writes it back unchanged.
func TestEventJSONMatchesTheWireForm(t *testing.T) {
	at := time.Date(2026, 9, 27, 14, 3, 11, 123_456_789, time.FixedZone("x", 3600))
	for _, e := range []entry{
		{at: at, log: "ext.deploy", level: Info, msg: "build started", fields: []field{{"commit", "abc123"}, {"host", "build-box"}}},
		{at: at, log: "ext.deploy", level: Error + 4, msg: `quote " <tag> & héllo`, fields: []field{{"multi", "l1\nl2"}},
			trace: "0123456789abcdef0123456789abcdef", activity: "fedcba9876543210fedcba9876543210"},
		{at: at, log: "ext.deploy", level: Debug, msg: "no fields"},
		{at: at, log: "ext.deploy", level: Warn, msg: "warn", activity: "fedcba9876543210fedcba9876543210"},
	} {
		b := e.json()
		var ev event.Event
		if err := json.Unmarshal(b, &ev); err != nil {
			t.Fatalf("the hub can't read %s: %v", b, err)
		}
		back, _ := json.Marshal(ev)
		if !bytes.Equal(b, back) {
			t.Errorf("wire drift:\n ref: %s\n hub: %s", b, back)
		}
		if ev.Level.Slog() != e.level && !(e.level == Error+4 && ev.Level == event.Critical) {
			t.Errorf("level %v read as %v", e.level, ev.Level)
		}
	}
}

// The client names its log as the API files it.
func TestLogNameMatchesTheAPI(t *testing.T) {
	for _, in := range []string{"deploy", "ext.deploy", "My Sidecar!", strings.Repeat("x", 60), "UPPER_case.1"} {
		want, _ := logapi.ExternalLog(in)
		if got := LogName(in); got != want {
			t.Errorf("LogName(%q) = %q, the API files it as %q", in, got, want)
		}
	}
}

// readSpool reads the spool's finished files, in name order, as events; no
// .tmp file is left behind.
func readSpool(t *testing.T, data string) []event.Event {
	t.Helper()
	if tmp, _ := filepath.Glob(filepath.Join(data, "logs", "spool", "*.tmp")); len(tmp) != 0 {
		t.Errorf("unfinished spool files left: %v", tmp)
	}
	files, _ := filepath.Glob(filepath.Join(data, "logs", "spool", "*.ndjson"))
	var out []event.Event
	for _, f := range files {
		b, _ := os.ReadFile(f)
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			var e event.Event
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			out = append(out, e)
		}
	}
	slices.SortFunc(out, func(a, b event.Event) int { return a.Time.Compare(b.Time) })
	return out
}

// With no hub, each line is dropped in the spool as a finished file of its
// own, an event Tenant reads, correlated with the trace and activity Tenant
// started the program under.
func TestSpoolFallback(t *testing.T) {
	data := t.TempDir()
	t.Setenv("TENANT_TRACE_ID", "0123456789abcdef0123456789abcdef")
	t.Setenv("TENANT_ACTIVITY_ID", "fedcba9876543210fedcba9876543210")
	l, err := NewWithConfig(Config{Source: "My Sidecar", DataDir: data, FileOnly: true, Defaults: map[string]string{"host": "build-box"}})
	if err != nil {
		t.Fatal(err)
	}
	l.Info("hello from a script", "k", "v")
	l.Error("it broke", "err", os.ErrNotExist)
	if err := l.Event("tool_call", "tool call disk_check", map[string]string{"tool": "disk_check"}); err != nil {
		t.Fatal(err)
	}
	evs := readSpool(t, data)
	if len(evs) != 3 {
		t.Fatalf("spool holds %d events, want 3", len(evs))
	}
	e := evs[0]
	if e.Log != "ext.my-sidecar" || e.Msg != "hello from a script" || e.Level != event.Info ||
		e.Corr.TraceID != "0123456789abcdef0123456789abcdef" || e.Corr.ActivityID != "fedcba9876543210fedcba9876543210" {
		t.Errorf("event 0 = %+v", e)
	}
	if !slices.Equal(e.Data, []event.Field{{Key: "host", Value: "build-box"}, {Key: "k", Value: "v"}}) {
		t.Errorf("data = %+v", e.Data)
	}
	if evs[1].Level != event.Error || evs[1].Data[0] != (event.Field{Key: "err", Value: "file does not exist"}) {
		t.Errorf("event 1 = %+v", evs[1])
	}
	if i := slices.IndexFunc(evs[2].Data, func(f event.Field) bool { return f.Key == "kind" }); i < 0 || evs[2].Data[i].Value != "tool_call" {
		t.Errorf("event 2 = %+v", evs[2])
	}
}

// Through the real API: a line is accepted, filed under ext.<source> with
// keyword external and its correlation, and nothing reaches the spool.
func TestPostsThroughTheRealAPI(t *testing.T) {
	root := t.TempDir()
	hub, err := logs.Open(root, logs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(root, "logbook.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	hub.AttachStore(st)
	srv := httptest.NewServer((&logapi.API{Hub: func() *logs.Hub { return hub }}).Handler())
	defer srv.Close()
	data := t.TempDir()
	l, _ := NewWithConfig(Config{Source: "deploy", URL: srv.URL, DataDir: data, Trace: "0123456789abcdef0123456789abcdef"})
	if err := l.Log(Warn, "build failed", map[string]string{"step": "link"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Ping(); err != nil {
		t.Errorf("Ping = %v", err)
	}
	r, err := store.Open(filepath.Join(root, "logbook.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := hub.Close(); err != nil { // flushes the pipeline
		t.Fatal(err)
	}
	f, _ := query.Parse("log:ext.deploy")
	evs, _, _, err := r.Query(context.Background(), f, store.Cursor{}, 10, false)
	if err != nil || len(evs) != 1 {
		t.Fatalf("filed = %+v, %v", evs, err)
	}
	e := evs[0]
	if e.Msg != "build failed" || e.Level != event.Warning || e.Corr.TraceID != "0123456789abcdef0123456789abcdef" ||
		!slices.Equal(e.Keywords, []string{"external"}) || e.Data[0] != (event.Field{Key: "step", Value: "link"}) {
		t.Errorf("filed as %+v", e)
	}
	if _, err := os.Stat(filepath.Join(data, "logs", "spool")); err == nil {
		t.Error("a delivered line must not also be spooled")
	}
}

type capture struct {
	path, auth string
	body       map[string]any
	n          int
}

func hubStub(t *testing.T, status int) (*httptest.Server, *capture) {
	t.Helper()
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.n++
		c.path, c.auth = r.URL.Path, r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &c.body)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func TestPostsToHubWithToken(t *testing.T) {
	srv, got := hubStub(t, http.StatusAccepted)
	data := t.TempDir()
	l, _ := NewWithConfig(Config{Source: "deploy", URL: srv.URL, Token: "tok", DataDir: data})
	l.Warn("build failed", "step", "link", "bad key=", "x")
	if got.path != "/api/events" || got.auth != "Bearer tok" {
		t.Errorf("path=%s auth=%s", got.path, got.auth)
	}
	if got.body["log"] != "ext.deploy" || got.body["level"] != "warning" || got.body["msg"] != "build failed" {
		t.Errorf("body = %v", got.body)
	}
	f, _ := got.body["data"].(map[string]any)
	if f["step"] != "link" || f["bad_key_"] != "x" {
		t.Errorf("data = %v (keys must be cleaned for the hub)", f)
	}
	if _, err := os.Stat(filepath.Join(data, "logs")); err == nil {
		t.Error("a delivered line must not also be spooled")
	}
}

func TestHubDownFallsBackAndStaysOnTheSpoolForRetryWindow(t *testing.T) {
	data := t.TempDir()
	l, _ := NewWithConfig(Config{Source: "sidecar", URL: "http://127.0.0.1:1", DataDir: data, Retry: time.Hour, Timeout: 500 * time.Millisecond})
	l.Info("first")
	if l.hubOK() {
		t.Error("a connection failure should park the logger on the spool for Retry")
	}
	l.Info("second")
	if evs := readSpool(t, data); len(evs) != 2 || evs[0].Msg != "first" || evs[1].Msg != "second" {
		t.Errorf("both lines should be in the spool: %+v", evs)
	}
	if err := l.Ping(); err == nil {
		t.Error("Ping must report an unreachable hub")
	}
}

func TestRejectedLineIsSpooledAndReported(t *testing.T) {
	srv, _ := hubStub(t, http.StatusBadRequest)
	data := t.TempDir()
	l, _ := NewWithConfig(Config{Source: "sidecar", URL: srv.URL, DataDir: data})
	err := l.Log(Info, "x", nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("a rejection must be returned, got %v", err)
	}
	if evs := readSpool(t, data); len(evs) != 1 {
		t.Error("a rejected line must still be kept in the spool")
	}
	if !l.hubOK() {
		t.Error("a rejection is not an outage — keep trying the hub")
	}
}

func TestHandlerRoutesSlogRecords(t *testing.T) {
	srv, got := hubStub(t, http.StatusAccepted)
	l, _ := NewWithConfig(Config{Source: "sidecar", URL: srv.URL, DataDir: t.TempDir()})
	log := slog.New(l.Handler()).With("host", "build-box").WithGroup("req").With("id", 7)
	log.Debug("dropped at the default floor")
	if got.n != 0 {
		t.Fatal("DEBUG should not be sent by default")
	}
	log.Info("handled", "path", "/x", slog.Group("peer", "ip", "10.0.0.1"), "err", os.ErrNotExist)
	if got.n != 1 || got.body["msg"] != "handled" || got.body["level"] != "info" {
		t.Fatalf("got %d posts, body %v", got.n, got.body)
	}
	f, _ := got.body["data"].(map[string]any)
	for k, v := range map[string]string{"host": "build-box", "req.id": "7", "req.path": "/x", "req.peer.ip": "10.0.0.1", "req.err": "file does not exist"} {
		if f[k] != v {
			t.Errorf("field %s = %v, want %s (all: %v)", k, f[k], v, f)
		}
	}
	lo, _ := NewWithConfig(Config{Source: "sidecar", URL: srv.URL, DataDir: t.TempDir(), MinLevel: Debug})
	slog.New(lo.Handler()).Debug("kept")
	if got.n != 2 || got.body["level"] != "verbose" {
		t.Errorf("MinLevel Debug should send DEBUG: n=%d body=%v", got.n, got.body)
	}
}

func TestSubAndDefaultsAndStderr(t *testing.T) {
	srv, got := hubStub(t, http.StatusAccepted)
	var errOut strings.Builder
	l, _ := NewWithConfig(Config{Source: "deploy", URL: srv.URL, DataDir: t.TempDir(), Defaults: map[string]string{"host": "build-box"}, Stderr: &errOut})
	audit := l.Sub("Deploy Audit")
	if audit.Source() != "deploy-audit" {
		t.Errorf("Sub source = %q", audit.Source())
	}
	audit.Info("verdict recorded", "id", "42")
	if got.path != "/api/events" || got.body["log"] != "ext.deploy-audit" {
		t.Errorf("path = %s, log = %v", got.path, got.body["log"])
	}
	if f, _ := got.body["data"].(map[string]any); f["host"] != "build-box" || f["id"] != "42" {
		t.Errorf("defaults should stamp every line: %v", got.body["data"])
	}
	if !strings.Contains(errOut.String(), "INFO  [deploy-audit] verdict recorded\thost=build-box id=42\n") {
		t.Errorf("stderr mirror = %q", errOut.String())
	}
}

func TestNewPanicsOnUnusableSource(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("New(\"..\") should panic at startup")
		}
	}()
	t.Setenv("TENANT_URL", "http://127.0.0.1:1")
	New("..")
}

// With no TENANT_TOKEN and no dashboard.auth, the token comes from the file a
// default install keeps it in, in Tenant's own config dir (TEN-367).
func TestDiscoveryReadsTokenFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("AppData", filepath.Join(home, "AppData"))
	t.Setenv("TENANT_TOKEN", "")
	t.Setenv("TENANT_URL", "")
	cfgDir, err := soul.DefaultDir() // Tenant's config dir rule
	if err != nil {
		t.Skip(err)
	}
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(`{"dashboard":{"addr":"127.0.0.1:9123"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "dashboard-auth-token"), []byte("filetok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if d := discover(); d.token != "filetok" || d.url != "127.0.0.1:9123" {
		t.Errorf("discovered = %+v, want the token file's token", d)
	}
}

func TestDiscoveryHonorsEnv(t *testing.T) {
	t.Setenv("TENANT_URL", "10.0.0.5:9000")
	t.Setenv("TENANT_TOKEN", "envtok")
	t.Setenv("TENANT_DATA", "/tmp/tenant-test-data")
	l := New("x")
	if l.cfg.URL != "http://10.0.0.5:9000" || l.cfg.Token != "envtok" || l.cfg.LogDir != filepath.Join("/tmp/tenant-test-data", "logs") {
		t.Errorf("discovered cfg = %+v", l.cfg)
	}
}
