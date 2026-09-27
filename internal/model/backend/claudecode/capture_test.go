package claudecode

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"tenant/internal/logs/event"
	"tenant/internal/model"
)

// TestMain doubles as a fake claude CLI: with CLAUDECODE_FAKE set it answers
// one call, and chats on stderr as the real one can.
func TestMain(m *testing.M) {
	if os.Getenv("CLAUDECODE_FAKE") != "" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		fmt.Fprintln(os.Stderr, "a newer version is available")
		fmt.Print(`{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"s","num_turns":1,"usage":{}}`)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type events struct {
	mu  sync.Mutex
	got []*event.Event
}

func (e *events) Emit(_ context.Context, ev *event.Event) {
	if ev.Log == "system" {
		return // the child's component events (internal/lifecycle)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.got = append(e.got, ev)
}
func (e *events) Audit(ctx context.Context, ev *event.Event) error { e.Emit(ctx, ev); return nil }

// A successful call's stderr and exit are filed in the model log, not thrown
// away (TEN-364).
func TestGenerateFilesStderrAndExit(t *testing.T) {
	t.Setenv("CLAUDECODE_FAKE", "1")
	em := &events{}
	event.Install(em)
	t.Cleanup(func() { event.Install(nil) })
	b := &Backend{
		profile: model.Profile{Endpoint: os.Args[0]},
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		ledger:  &ledger{path: filepath.Join(t.TempDir(), "usage.json")},
	}
	resp, err := b.Generate(context.Background(), model.GenerateRequest{Messages: []model.Message{{Role: "user", Content: "hi"}}})
	if err != nil || resp.Text != "ok" {
		t.Fatalf("Generate = %+v, %v", resp, err)
	}
	var msgs []string
	for _, e := range em.got {
		if e.Log != "model" || fieldOf(e, "child") != "claude" {
			t.Errorf("filed %s %q %v", e.Log, e.Msg, e.Data)
		}
		msgs = append(msgs, e.Msg)
	}
	if len(msgs) != 3 || msgs[0] != "process started" || msgs[1] != "a newer version is available" || msgs[2] != "process exited" {
		t.Fatalf("filed %q", msgs)
	}
	if exit := em.got[2]; fieldOf(exit, "exit") != "0" || fieldOf(exit, "duration") == "" {
		t.Errorf("exit line = %v", exit.Data)
	}
}

func fieldOf(e *event.Event, key string) string {
	for _, f := range e.Data {
		if f.Key == key {
			return f.Text()
		}
	}
	return ""
}
