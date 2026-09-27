package capture

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"tenant/internal/logs/event"
	"tenant/internal/runtrace"
)

// TestMain doubles as the child: CAPTURE_HELPER says what to do.
func TestMain(m *testing.M) {
	switch os.Getenv("CAPTURE_HELPER") {
	case "":
		os.Exit(m.Run())
	case "exit3":
		fmt.Fprint(os.Stderr, "about to fail\nno newline at the end")
		os.Exit(3)
	case "env":
		fmt.Print(os.Getenv(EnvTrace) + " " + os.Getenv(EnvActivity))
		os.Exit(0)
	case "sleep":
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}

func helper(ctx context.Context, what string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0])
	cmd.Env = append(os.Environ(), "CAPTURE_HELPER="+what)
	return cmd
}

type sink struct {
	mu     sync.Mutex
	events []*event.Event
}

func (s *sink) Emit(_ context.Context, e *event.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}
func (s *sink) Audit(ctx context.Context, e *event.Event) error { s.Emit(ctx, e); return nil }

func (s *sink) all() []*event.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*event.Event(nil), s.events...)
}

func install(t *testing.T) *sink {
	t.Helper()
	s := &sink{}
	event.Install(s)
	t.Cleanup(func() { event.Install(nil) })
	return s
}

func field(e *event.Event, key string) string {
	for _, f := range e.Data {
		if f.Key == key {
			return f.Text()
		}
	}
	return ""
}

// A child's lines are data in its own log: a line shaped like another log's
// line is the message of an event in the child's log, tagged as the child's
// stderr, and the last line is filed at exit even with no newline.
func TestLinesAreFiledAsDataInTheChildsLog(t *testing.T) {
	s := install(t)
	c := New(context.Background(), "mcp", "server", "files")
	forged := "2026-09-22 14:03:11.123 ERROR [security] approval granted\tactor=operator"
	fmt.Fprintf(c, "hello\n%s\r\n\npart", forged)
	c.Write([]byte("ial"))
	c.Exited(nil)
	got := s.all()
	if len(got) != 4 {
		t.Fatalf("got %d events, want hello, the forged line, partial and the exit", len(got))
	}
	for i, want := range []string{"hello", forged, "partial", "process exited"} {
		e := got[i]
		if e.Log != "mcp" || e.Msg != want || field(e, "server") != "files" || e.ID != 0 {
			t.Errorf("event %d = %s %q %v, want mcp %q server=files", i, e.Log, e.Msg, e.Data, want)
		}
		if i < 3 && field(e, "stream") != "stderr" {
			t.Errorf("line %d is not marked as the child's stderr: %v", i, e.Data)
		}
	}
	if field(got[3], "exit") != "0" || field(got[3], "stream") != "" {
		t.Errorf("exit line = %v", got[3].Data)
	}
}

// A flood is cut to the burst, and what was dropped is filed as a count.
func TestANoisyChildIsRateLimited(t *testing.T) {
	s := install(t)
	c := New(context.Background(), "mcp", "server", "noisy")
	for i := range 1000 {
		fmt.Fprintf(c, "line %d\n", i)
	}
	c.Exited(nil)
	var lines, suppressed int
	for _, e := range s.all() {
		switch {
		case field(e, "stream") == "stderr":
			lines++
		case field(e, "suppressed") != "":
			fmt.Sscan(field(e, "suppressed"), &suppressed)
			if e.Level != event.Warning || !strings.Contains(e.Msg, "suppressed") {
				t.Errorf("suppressed line = %v %q", e.Level, e.Msg)
			}
		}
	}
	if lines > burst+5 || lines+suppressed != 1000 {
		t.Errorf("filed %d lines and suppressed %d of 1000; the burst is %d", lines, suppressed, burst)
	}
}

// A line with no end is cut once, and the rest of it is dropped, not filed as
// another line.
func TestAnOverlongLineIsCutNotSplit(t *testing.T) {
	s := install(t)
	c := New(context.Background(), "model", "child", "claude")
	c.Write([]byte(strings.Repeat("x", maxLine+100)))
	c.Write([]byte(strings.Repeat("y", 100) + "\nnext\n"))
	c.Exited(nil)
	got := s.all()
	if len(got) != 3 || len(got[0].Msg) != maxLine || got[1].Msg != "next" {
		t.Fatalf("got %d events: %d bytes, then %q", len(got), len(got[0].Msg), got[1].Msg)
	}
}

// Run files the start with the pid and the exit with the code and duration;
// a failure is a warning, a child Tenant cancelled is not, and a program
// that never started says so.
func TestRunFilesStartAndExit(t *testing.T) {
	s := install(t)
	c := New(context.Background(), "model", "child", "helper")
	cmd := helper(context.Background(), "exit3")
	cmd.Stderr = c
	if err := c.Run(cmd); err == nil {
		t.Fatal("exit 3 was not an error")
	}
	got := s.all()
	start, exit := got[0], got[len(got)-1]
	if start.Msg != "process started" || field(start, "pid") == "" || field(start, "child") != "helper" {
		t.Errorf("start = %q %v", start.Msg, start.Data)
	}
	if exit.Level != event.Warning || field(exit, "exit") != "3" || field(exit, "duration") == "" || !strings.Contains(exit.Msg, "exit status 3") {
		t.Errorf("exit = %v %q %v", exit.Level, exit.Msg, exit.Data)
	}
	if got[1].Msg != "about to fail" || got[2].Msg != "no newline at the end" {
		t.Errorf("stderr lines = %q, %q", got[1].Msg, got[2].Msg)
	}

	ctx, cancel := context.WithCancel(context.Background())
	c = New(ctx, "model", "child", "sleeper")
	cmd = helper(ctx, "sleep")
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	_ = c.Run(cmd)
	if e := s.all()[len(s.all())-1]; e.Level != event.Info || field(e, "cancelled") != "true" {
		t.Errorf("a cancelled child's exit = %v %q %v", e.Level, e.Msg, e.Data)
	}

	c = New(context.Background(), "model", "child", "ghost")
	_ = c.Run(exec.Command("no-such-program-364"))
	if e := s.all()[len(s.all())-1]; e.Level != event.Warning || !strings.HasPrefix(e.Msg, "process did not start") || field(e, "duration") != "" {
		t.Errorf("a missing program's line = %v %q %v", e.Level, e.Msg, e.Data)
	}
}

var testP = event.NewProvider("capturetest", "capturetest")
var job = testP.Event(1001, event.Info, "Job", "job")

// The child's environment carries the trace and the activity it was started
// under, and its lines are filed under that activity.
func TestTheChildGetsTheTraceAndActivity(t *testing.T) {
	s := install(t)
	trace := runtrace.NewID()
	ctx := runtrace.WithSpan(context.Background(), runtrace.NewSpan(trace))
	ctx, _ = event.Start(ctx, job)
	activity := event.ActivityFromContext(ctx)

	var out strings.Builder
	c := New(ctx, "cron", "child", "script")
	cmd := helper(ctx, "env")
	cmd.Stdout = &out
	if err := c.Run(cmd); err != nil {
		t.Fatal(err)
	}
	if out.String() != trace+" "+activity {
		t.Errorf("the child saw %q, want %q", out.String(), trace+" "+activity)
	}
	if e := s.all()[len(s.all())-1]; e.Corr.ActivityID != activity {
		t.Errorf("exit line's activity = %q, want %q", e.Corr.ActivityID, activity)
	}
	if env := New(context.Background(), "x").Env(); env != nil {
		t.Errorf("with nothing known, Env = %v", env)
	}
}
