package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseInterspersed_FlagsAfterPositionals(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	level := fs.String("level", "info", "")
	list := fs.Bool("list", false, "")
	pos, err := parseInterspersed(fs, []string{"deploy", "started build", "--level", "warn", "host=build-box", "--list"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(pos, "|") != "deploy|started build|host=build-box" || *level != "warn" || !*list {
		t.Errorf("pos=%v level=%s list=%v", pos, *level, *list)
	}
}

// With a hub reachable, the line is POSTed to /api/logs/{source} with the
// bearer token, the normalized source and the fields.
func TestCmdLog_PostsToRunningHub(t *testing.T) {
	var got struct {
		path, auth string
		body       map[string]any
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.auth = r.URL.Path, r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got.body)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer ts.Close()
	data := t.TempDir()
	err := cmdLog(context.Background(), []string{"--url", ts.URL, "--token", "tok", "--data", data, "--config", data, "--level", "error",
		"My Deploy", "build failed", "host=build-box", "step=link"})
	if err != nil {
		t.Fatal(err)
	}
	if got.path != "/api/logs/my-deploy" || got.auth != "Bearer tok" {
		t.Errorf("path=%s auth=%s", got.path, got.auth)
	}
	if got.body["level"] != "ERROR" || got.body["msg"] != "build failed" {
		t.Errorf("body = %v", got.body)
	}
	if f, _ := got.body["fields"].(map[string]any); f["host"] != "build-box" || f["step"] != "link" {
		t.Errorf("fields = %v", got.body["fields"])
	}
	if _, err := os.Stat(filepath.Join(data, "logs")); err == nil {
		t.Error("a successful POST must not also write a local file")
	}
}

// With no hub reachable, the line is appended straight to the day's file in
// the log-book format, under <data>/logs/<source>/.
func TestCmdLog_FallsBackToFileWhenHubIsDown(t *testing.T) {
	data := t.TempDir()
	err := cmdLog(context.Background(), []string{"--url", "http://127.0.0.1:1", "--data", data, "--config", data,
		"sidecar", "hello from a script", "k=v"})
	if err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(data, "logs", "sidecar", "*.log"))
	if len(files) != 1 {
		t.Fatalf("expected one daily file, got %v", files)
	}
	b, _ := os.ReadFile(files[0])
	if !strings.Contains(string(b), "INFO  [sidecar] hello from a script\tk=v\n") {
		t.Errorf("file line = %q", string(b))
	}
	// A 4xx from a live hub is an error, not a fallback.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"msg required"}`))
	}))
	defer ts.Close()
	if err := cmdLog(context.Background(), []string{"--url", ts.URL, "--data", data, "--config", data, "x", "y"}); err == nil || !strings.Contains(err.Error(), "msg required") {
		t.Errorf("a rejected line must surface the hub's error, got %v", err)
	}
	// Bad input never reaches anything.
	if err := cmdLog(context.Background(), []string{"--data", data, "--config", data, "---", "y"}); err == nil {
		t.Error("an unusable source name must error")
	}
	if err := cmdLog(context.Background(), []string{"--data", data, "--config", data, "x", "y", "notakv"}); err == nil {
		t.Error("a non key=value extra arg must error")
	}
}
