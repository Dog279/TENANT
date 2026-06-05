package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"tenant/internal/memory/episodic"
	"tenant/internal/memory/working"
)

func resInsert(t *testing.T, s *episodic.Store, agent, prompt, resp string, ts time.Time) {
	t.Helper()
	if _, err := s.Insert(context.Background(), &episodic.Episode{
		AgentID: agent, Prompt: prompt, Response: resp,
		EmbedderID: "fake", Embedding: []float32{1}, Timestamp: ts,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
}

func TestSeedResumeBridge(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 4, 18, 0, 0, 0, time.UTC)
	ep, err := episodic.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	resInsert(t, ep, "main", "set up wiki linking", "done, see wiki:notes/x.md", now.Add(-2*time.Hour))
	resInsert(t, ep, "main", "recover the facts db", "salvaged 11 facts", now.Add(-30*time.Minute))

	// Cold start + recent → seeds a single assistant/resume bridge message.
	w := working.New()
	if n := seedResumeBridge(ctx, w, ep, "main", now); n != 2 {
		t.Fatalf("cold start: want 2 episodes recapped, got %d", n)
	}
	if w.Len() != 1 {
		t.Fatalf("want 1 bridge message, got %d", w.Len())
	}
	m := w.Messages()[0]
	if m.Role != "assistant" || m.Kind != "resume" {
		t.Errorf("framing wrong: role=%q kind=%q (want assistant/resume)", m.Role, m.Kind)
	}
	for _, want := range []string{"Resuming", "recover the facts db", "wiki:notes/x.md"} {
		if !strings.Contains(m.Content, want) {
			t.Errorf("bridge missing %q:\n%s", want, m.Content)
		}
	}

	// Warm start (non-empty working set) → never seeds.
	if n := seedResumeBridge(ctx, w, ep, "main", now); n != 0 {
		t.Errorf("warm start seeded (%d); must no-op", n)
	}

	// Stale session (newest beyond resumeMaxAge) → no auto-resume.
	if n := seedResumeBridge(ctx, working.New(), ep, "main", now.Add(resumeMaxAge+time.Hour)); n != 0 {
		t.Errorf("stale session resumed (%d); must no-op", n)
	}

	// Empty store + nil guards → no-op.
	empty, _ := episodic.Open(":memory:")
	if seedResumeBridge(ctx, working.New(), empty, "main", now) != 0 {
		t.Error("empty store should not seed")
	}
	if seedResumeBridge(ctx, nil, ep, "main", now) != 0 || seedResumeBridge(ctx, working.New(), nil, "main", now) != 0 {
		t.Error("nil working/store should no-op")
	}

	// No recursion: seeding must NOT create an episode.
	before, _ := ep.Count(ctx, true)
	_ = seedResumeBridge(ctx, working.New(), ep, "main", now)
	after, _ := ep.Count(ctx, true)
	if before != after {
		t.Errorf("seeding changed episode count %d->%d (recursion!)", before, after)
	}
}

func TestRenderSessionBridge_CapAndEmpty(t *testing.T) {
	if renderSessionBridge(nil) != "" {
		t.Error("nil episodes should render empty")
	}
	big := strings.Repeat("x", 6000)
	out := renderSessionBridge([]*episodic.Episode{{Prompt: "p", Response: big, Timestamp: time.Now()}})
	if got := len([]rune(out)); got > resumeMaxChars+24 {
		t.Errorf("bridge not capped: %d runes (cap %d)", got, resumeMaxChars)
	}
}
