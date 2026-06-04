package improve_test

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tenant/internal/improve"
	"tenant/internal/memory/distill"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/model"
	"tenant/internal/model/testllm"
)

// --- Meta tests ---

func TestMeta_GetSetRoundtrip(t *testing.T) {
	m, err := improve.OpenMeta(":memory:")
	if err != nil {
		t.Fatalf("OpenMeta: %v", err)
	}
	defer m.Close()
	ctx := context.Background()

	if _, ok, _ := m.Get(ctx, "missing"); ok {
		t.Fatal("Get on missing key returned ok=true")
	}
	if err := m.Set(ctx, "k", "v"); err != nil {
		t.Fatal(err)
	}
	v, ok, err := m.Get(ctx, "k")
	if err != nil || !ok || v != "v" {
		t.Fatalf("Get = %q,%v,%v; want v,true,nil", v, ok, err)
	}
}

func TestMeta_SetOverwrites(t *testing.T) {
	m, _ := improve.OpenMeta(":memory:")
	defer m.Close()
	ctx := context.Background()
	_ = m.Set(ctx, "k", "first")
	_ = m.Set(ctx, "k", "second")
	v, _, _ := m.Get(ctx, "k")
	if v != "second" {
		t.Fatalf("v = %q, want second", v)
	}
}

func TestMeta_Int64Helpers(t *testing.T) {
	m, _ := improve.OpenMeta(":memory:")
	defer m.Close()
	ctx := context.Background()

	if n, ok, _ := m.GetInt64(ctx, "cursor"); ok || n != 0 {
		t.Fatalf("GetInt64 missing = %d,%v; want 0,false", n, ok)
	}
	if err := m.SetInt64(ctx, "cursor", 42); err != nil {
		t.Fatal(err)
	}
	n, ok, err := m.GetInt64(ctx, "cursor")
	if err != nil || !ok || n != 42 {
		t.Fatalf("GetInt64 = %d,%v,%v; want 42,true,nil", n, ok, err)
	}
}

func TestMeta_PersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.db")
	m1, err := improve.OpenMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = m1.SetInt64(context.Background(), "cursor", 99)
	_ = m1.Close()

	m2, err := improve.OpenMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	n, ok, _ := m2.GetInt64(context.Background(), "cursor")
	if !ok || n != 99 {
		t.Fatalf("after reopen GetInt64 = %d,%v; want 99,true", n, ok)
	}
}

func TestMeta_CloseIdempotent(t *testing.T) {
	m, _ := improve.OpenMeta(":memory:")
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
}

// --- Scheduler tests ---

// fakeJob is a programmable Job for scheduler tests.
type fakeJob struct {
	name string
	runs atomic.Int32
	fn   func(ctx context.Context) (improve.JobResult, error)
}

func (j *fakeJob) Name() string { return j.name }
func (j *fakeJob) Run(ctx context.Context) (improve.JobResult, error) {
	j.runs.Add(1)
	if j.fn != nil {
		return j.fn(ctx)
	}
	return improve.JobResult{Summary: "ok"}, nil
}

func TestScheduler_RunAllForcesEveryJob(t *testing.T) {
	s := improve.NewScheduler(slog.Default(), 8)
	a := &fakeJob{name: "a"}
	b := &fakeJob{name: "b"}
	s.Register(a, time.Hour) // long interval — RunDue wouldn't fire it
	s.Register(b, time.Hour)

	recs := s.RunAll(context.Background())
	if len(recs) != 2 {
		t.Fatalf("RunAll records = %d, want 2", len(recs))
	}
	if a.runs.Load() != 1 || b.runs.Load() != 1 {
		t.Fatalf("runs a=%d b=%d, want 1,1", a.runs.Load(), b.runs.Load())
	}
}

func TestScheduler_RunDueRespectsInterval(t *testing.T) {
	s := improve.NewScheduler(slog.Default(), 8)
	j := &fakeJob{name: "j"}
	s.Register(j, 50*time.Millisecond)

	// First RunDue: never run → fires.
	if recs := s.RunDue(context.Background()); len(recs) != 1 {
		t.Fatalf("first RunDue records = %d, want 1", len(recs))
	}
	// Immediate second RunDue: interval not elapsed → skipped.
	if recs := s.RunDue(context.Background()); len(recs) != 0 {
		t.Fatalf("second RunDue records = %d, want 0 (interval not elapsed)", len(recs))
	}
	// After interval: fires again.
	time.Sleep(60 * time.Millisecond)
	if recs := s.RunDue(context.Background()); len(recs) != 1 {
		t.Fatalf("third RunDue records = %d, want 1 (interval elapsed)", len(recs))
	}
	if j.runs.Load() != 2 {
		t.Fatalf("job ran %d times, want 2", j.runs.Load())
	}
}

func TestScheduler_ZeroIntervalNeverAutoFires(t *testing.T) {
	s := improve.NewScheduler(slog.Default(), 8)
	j := &fakeJob{name: "manual"}
	s.Register(j, 0) // manual-only

	if recs := s.RunDue(context.Background()); len(recs) != 0 {
		t.Fatalf("RunDue ran a zero-interval job: %d records", len(recs))
	}
	// RunAll still forces it.
	s.RunAll(context.Background())
	if j.runs.Load() != 1 {
		t.Fatalf("RunAll didn't run manual job; runs=%d", j.runs.Load())
	}
}

func TestScheduler_JobErrorDoesNotStopOthers(t *testing.T) {
	s := improve.NewScheduler(slog.Default(), 8)
	bad := &fakeJob{name: "bad", fn: func(context.Context) (improve.JobResult, error) {
		return improve.JobResult{}, errors.New("boom")
	}}
	good := &fakeJob{name: "good"}
	s.Register(bad, time.Hour)
	s.Register(good, time.Hour)

	recs := s.RunAll(context.Background())
	if len(recs) != 2 {
		t.Fatalf("records = %d, want 2", len(recs))
	}
	if good.runs.Load() != 1 {
		t.Fatal("good job did not run after bad job errored")
	}
	// History should record the error.
	var sawErr bool
	for _, r := range s.History() {
		if r.JobName == "bad" && r.Err != nil {
			sawErr = true
		}
	}
	if !sawErr {
		t.Fatal("history missing the failed job's error")
	}
}

func TestScheduler_HistoryRingBounded(t *testing.T) {
	s := improve.NewScheduler(slog.Default(), 3)
	j := &fakeJob{name: "j"}
	s.Register(j, 0)
	for i := 0; i < 10; i++ {
		s.RunAll(context.Background())
	}
	h := s.History()
	if len(h) != 3 {
		t.Fatalf("history len = %d, want 3 (capped)", len(h))
	}
}

func TestScheduler_StartStopBackgroundLoop(t *testing.T) {
	s := improve.NewScheduler(slog.Default(), 16)
	j := &fakeJob{name: "bg"}
	s.Register(j, 10*time.Millisecond)

	if err := s.Start(context.Background(), 5*time.Millisecond); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Double-start is rejected.
	if err := s.Start(context.Background(), 5*time.Millisecond); err == nil {
		t.Fatal("expected error on double Start")
	}
	time.Sleep(80 * time.Millisecond)
	s.Stop()
	ran := j.runs.Load()
	if ran < 2 {
		t.Fatalf("background loop ran job %d times, want >= 2", ran)
	}
	// After Stop, no more runs.
	time.Sleep(40 * time.Millisecond)
	if j.runs.Load() != ran {
		t.Fatalf("job ran after Stop: %d -> %d", ran, j.runs.Load())
	}
}

func TestScheduler_StopBeforeStartIsSafe(t *testing.T) {
	s := improve.NewScheduler(slog.Default(), 8)
	s.Stop() // must not panic / hang
}

func TestScheduler_StartRejectsBadTick(t *testing.T) {
	s := improve.NewScheduler(slog.Default(), 8)
	if err := s.Start(context.Background(), 0); err == nil {
		t.Fatal("expected error for tick <= 0")
	}
}

func TestScheduler_ContextCancelStopsRunDue(t *testing.T) {
	s := improve.NewScheduler(slog.Default(), 8)
	var ran sync.Map
	for _, name := range []string{"a", "b", "c"} {
		n := name
		s.Register(&fakeJob{name: n, fn: func(context.Context) (improve.JobResult, error) {
			ran.Store(n, true)
			return improve.JobResult{}, nil
		}}, time.Nanosecond) // all due immediately
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before RunDue
	recs := s.RunDue(ctx)
	if len(recs) != 0 {
		t.Fatalf("RunDue ran %d jobs under cancelled ctx, want 0", len(recs))
	}
}

// --- DistillJob integration ---

func TestDistillJob_AdvancesCursorAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	estore, err := episodic.Open(filepath.Join(dir, "ep.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer estore.Close()
	sstore, err := semantic.Open(filepath.Join(dir, "fact.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sstore.Close()
	meta, err := improve.OpenMeta(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer meta.Close()

	reg, _ := model.NewRegistry("")
	router := model.NewRouter(reg, slog.Default())
	fakeLLM := testllm.New()
	fakeEmb := testllm.New()
	router.RegisterBackend("vllm", func(_ context.Context, p model.Profile, _ *slog.Logger) (any, error) {
		if p.Role == model.RoleEmbedder {
			return fakeEmb, nil
		}
		return fakeLLM, nil
	})

	ctx := context.Background()
	// Two episodes.
	id1, _ := estore.Insert(ctx, &episodic.Episode{
		AgentID: "main", Prompt: "I use Go", Response: "noted",
		EmbedderID: "e", Embedding: []float32{1, 0},
	})
	_, _ = estore.Insert(ctx, &episodic.Episode{
		AgentID: "main", Prompt: "I work on Tenant", Response: "ok",
		EmbedderID: "e", Embedding: []float32{0, 1},
	})

	fakeLLM.GenerateFn = func(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
		return &model.GenerateResponse{Text: `{"facts":[]}`, FinishReason: "stop"}, nil
	}
	fakeEmb.EmbedFn = func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 0}
		}
		return out, nil
	}

	d := &distill.Distiller{
		Router: router, Episodic: estore, Semantic: sstore,
		AgentID: "main", BatchSize: 10,
	}
	job := improve.NewDistillJob(d, meta, "main")

	// First run: cursor 0 → id2. Processes both episodes.
	rec := mustRunJob(t, job, ctx)
	if !contains(rec.Summary, "processed 2 episodes") {
		t.Errorf("first run summary = %q", rec.Summary)
	}
	cur, ok, _ := meta.GetInt64(ctx, "distill_cursor:main")
	if !ok || cur == 0 {
		t.Fatalf("cursor not persisted: %d, %v", cur, ok)
	}

	// Second run with NO new episodes: should process 0.
	rec2 := mustRunJob(t, job, ctx)
	if !contains(rec2.Summary, "processed 0 episodes") {
		t.Errorf("second run summary = %q, want 0 episodes", rec2.Summary)
	}

	// Add a third episode → next run picks up only that one.
	_, _ = estore.Insert(ctx, &episodic.Episode{
		AgentID: "main", Prompt: "third", Response: "r",
		EmbedderID: "e", Embedding: []float32{1, 0},
	})
	rec3 := mustRunJob(t, job, ctx)
	if !contains(rec3.Summary, "processed 1 episodes") {
		t.Errorf("third run summary = %q, want 1 episode", rec3.Summary)
	}
	_ = id1
}

func TestDistillJob_NilDepsError(t *testing.T) {
	j := improve.NewDistillJob(nil, nil, "main")
	if _, err := j.Run(context.Background()); err == nil {
		t.Fatal("expected error with nil Distiller")
	}
}

func TestDistillJob_NameStable(t *testing.T) {
	j := improve.NewDistillJob(nil, nil, "main")
	if j.Name() != "distill" {
		t.Fatalf("Name() = %q, want distill", j.Name())
	}
}

// The background loop `tenant serve` relies on: Start ticks due jobs;
// Stop drains an in-flight job and blocks until the loop has exited.
func TestScheduler_StartTicksAndStopDrains(t *testing.T) {
	s := improve.NewScheduler(slog.Default(), 8)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	j := &fakeJob{name: "slow", fn: func(ctx context.Context) (improve.JobResult, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release // hold the job "in flight"
		return improve.JobResult{Summary: "done"}, nil
	}}
	s.Register(j, time.Hour) // first tick fires it (lastRun zero)

	if err := s.Start(context.Background(), 10*time.Millisecond); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.Start(context.Background(), 10*time.Millisecond); err == nil {
		t.Error("second Start should error (already running)")
	}
	// Wait until the job is actually executing.
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("job never started ticking")
	}

	// Stop must block until the in-flight job returns — prove it by
	// releasing from another goroutine after a beat and timing the join.
	stopped := make(chan struct{})
	go func() { s.Stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("Stop returned before the in-flight job was released (didn't drain)")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return after job released")
	}
	if j.runs.Load() != 1 {
		t.Fatalf("job runs=%d, want 1", j.runs.Load())
	}
}

// OnRun fires after each job run (the TUI uses it to stream
// self-improvement into the live feed).
func TestScheduler_OnRunHookFires(t *testing.T) {
	s := improve.NewScheduler(slog.Default(), 8)
	var mu sync.Mutex
	var seen []string
	s.OnRun = func(rec improve.JobRunRecord) {
		mu.Lock()
		seen = append(seen, rec.JobName+":"+rec.Result.Summary)
		mu.Unlock()
	}
	s.Register(&fakeJob{name: "distill", fn: func(context.Context) (improve.JobResult, error) {
		return improve.JobResult{Summary: "did stuff"}, nil
	}}, 0)
	s.RunAll(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != "distill:did stuff" {
		t.Fatalf("OnRun not fired correctly: %v", seen)
	}
}

// --- helpers ---

func mustRunJob(t *testing.T, j improve.Job, ctx context.Context) improve.JobResult {
	t.Helper()
	res, err := j.Run(ctx)
	if err != nil {
		t.Fatalf("job %s Run: %v", j.Name(), err)
	}
	return res
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
