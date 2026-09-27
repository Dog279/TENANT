package tenantlog

// These tests are NOT part of the copy. They pin the reference client to the
// hub it talks to: the on-disk line format byte-for-byte, source-name
// normalization, the data-dir rule, and the discovery/tailing round trip — so
// a copy taken from this folder is always compatible with the running tenant.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tenant/internal/logs"
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
		{Warn, "ansi \x1b[31mred\x1b[0m bell\a csi\u009b bad\xff\r\nend", []field{{"esc", "\x1b]0;title\a"}, {"c1", "a\u009bb"}, {"bad", "x\xffy"}}},
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
	for _, in := range []string{"deploy", "My Sidecar!", "  Sub Agents ", "..", "", ".hidden", "a/b", "UPPER_case.1", strings.Repeat("x", 60), "-x", "9lives",
		"con", "NUL.txt", "com1", "LPT9.log", "aux.x.y", "prn-x", "com10", "consul", "a."} {
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

// The file fallback lands where the hub looks, in a format it parses, so a
// hub that starts later shows the sidecar's lines with their fields and levels
// under the sidecar's own tab.
func TestFileFallbackIsDiscoveredByHub(t *testing.T) {
	data := t.TempDir()
	l, err := NewWithConfig(Config{Source: "My Sidecar", DataDir: data, FileOnly: true, Defaults: map[string]string{"host": "build-box"}})
	if err != nil {
		t.Fatal(err)
	}
	l.Info("hello from a script", "k", "v")
	l.Error("it broke", "err", os.ErrNotExist)
	if err := l.Event("tool_call", "tool call disk_check", map[string]string{"tool": "disk_check"}); err != nil {
		t.Fatal(err)
	}

	files, _ := filepath.Glob(filepath.Join(data, "logs", "my-sidecar", "*.log"))
	if len(files) != 1 {
		t.Fatalf("expected one daily file, got %v", files)
	}
	hub, err := logs.Open(filepath.Join(data, "logs"), logs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub.Start(ctx)
	all, _ := hub.Snapshot()
	if len(all) != 3 {
		t.Fatalf("hub backfilled %d entries, want 3: %+v", len(all), all)
	}
	if all[0].Source != "my-sidecar" || all[0].Msg != "hello from a script" || all[0].Field("k") != "v" || all[0].Field("host") != "build-box" {
		t.Errorf("entry 0 = %+v", all[0])
	}
	if !all[1].IsErr() || all[1].Field("err") != "file does not exist" {
		t.Errorf("entry 1 = %+v", all[1])
	}
	if all[2].Kind() != "tool_call" || all[2].Field("tool") != "disk_check" {
		t.Errorf("entry 2 = %+v", all[2])
	}
	if s := hub.Sources(); len(s) != 1 || s[0].Name != "my-sidecar" || s[0].Native {
		t.Errorf("Sources() = %+v (want one foreign my-sidecar)", s)
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
	if got.path != "/api/logs/deploy" || got.auth != "Bearer tok" {
		t.Errorf("path=%s auth=%s", got.path, got.auth)
	}
	if got.body["level"] != "WARN" || got.body["msg"] != "build failed" {
		t.Errorf("body = %v", got.body)
	}
	f, _ := got.body["fields"].(map[string]any)
	if f["step"] != "link" || f["bad_key_"] != "x" {
		t.Errorf("fields = %v (keys must be cleaned for the hub)", f)
	}
	if _, err := os.Stat(filepath.Join(data, "logs")); err == nil {
		t.Error("a delivered line must not also be written to the file")
	}
}

func TestHubDownFallsBackAndStaysOnFileForRetryWindow(t *testing.T) {
	data := t.TempDir()
	l, _ := NewWithConfig(Config{Source: "sidecar", URL: "http://127.0.0.1:1", DataDir: data, Retry: time.Hour, Timeout: 500 * time.Millisecond})
	l.Info("first")
	if l.hubOK() {
		t.Error("a connection failure should park the logger on the file for Retry")
	}
	l.Info("second")
	files, _ := filepath.Glob(filepath.Join(data, "logs", "sidecar", "*.log"))
	if len(files) != 1 {
		t.Fatalf("expected one daily file, got %v", files)
	}
	b, _ := os.ReadFile(files[0])
	if strings.Count(string(b), "[sidecar]") != 2 {
		t.Errorf("both lines should be on disk:\n%s", b)
	}
	if err := l.Ping(); err == nil {
		t.Error("Ping must report an unreachable hub")
	}
}

func TestRejectedLineIsKeptOnDiskAndReported(t *testing.T) {
	srv, _ := hubStub(t, http.StatusBadRequest)
	data := t.TempDir()
	l, _ := NewWithConfig(Config{Source: "sidecar", URL: srv.URL, DataDir: data})
	err := l.Log(Info, "x", nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("a rejection must be returned, got %v", err)
	}
	if files, _ := filepath.Glob(filepath.Join(data, "logs", "sidecar", "*.log")); len(files) != 1 {
		t.Error("a rejected line must still be kept on disk")
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
	if got.n != 1 || got.body["msg"] != "handled" || got.body["level"] != "INFO" {
		t.Fatalf("got %d posts, body %v", got.n, got.body)
	}
	f, _ := got.body["fields"].(map[string]any)
	for k, v := range map[string]string{"host": "build-box", "req.id": "7", "req.path": "/x", "req.peer.ip": "10.0.0.1", "req.err": "file does not exist"} {
		if f[k] != v {
			t.Errorf("field %s = %v, want %s (all: %v)", k, f[k], v, f)
		}
	}
	lo, _ := NewWithConfig(Config{Source: "sidecar", URL: srv.URL, DataDir: t.TempDir(), MinLevel: Debug})
	slog.New(lo.Handler()).Debug("kept")
	if got.n != 2 || got.body["level"] != "DEBUG" {
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
	if got.path != "/api/logs/deploy-audit" {
		t.Errorf("path = %s", got.path)
	}
	if f, _ := got.body["fields"].(map[string]any); f["host"] != "build-box" || f["id"] != "42" {
		t.Errorf("defaults should stamp every line: %v", got.body["fields"])
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
