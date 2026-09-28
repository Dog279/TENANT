package privacy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tenant/internal/memory/archive"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/memory/skills"
	"tenant/internal/memory/sme"
	"tenant/internal/memory/usage"
	"tenant/internal/runtrace"
)

// now is the fixture's present.
var now = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

// fixture is a small install with one of everything a forget touches:
//
//	sess_a (May):  e1 "my address is 221B Baker Street" (run-1), e2 weather (run-2)
//	sess_b (June): e3 "I like Go" (run-3)
//	sess_c (Sep):  e4 by agent "researcher"
//	facts: f1 address [e1], f2 likes Go [e3] (pinned), f3 weather+London [e1 e2], f4 imported, no sources
//	sme: one section citing f1 and f2
//	skills: weather-check (run-2), address-lookup (mentions Baker)
type fixture struct {
	d  Data
	ep map[string]int64
	f  map[string]int64
}

func vec() []float32 { return []float32{0.1, 0.2, 0.3} }

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	fx := &fixture{
		d:  Data{DataDir: filepath.Join(root, "data"), ConfigDir: filepath.Join(root, "config"), Now: func() time.Time { return now }},
		ep: map[string]int64{}, f: map[string]int64{},
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(fx.d.DataDir, 0o700))
	may10 := time.Date(2026, 5, 10, 9, 0, 0, 0, time.UTC)
	jun15 := time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC)
	sep20 := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	type turn struct {
		name, agent, session, run, prompt, response string
		at                                          time.Time
	}
	turns := []turn{
		{"e1", "main", "sess_a", "run-1", "my address is 221B Baker Street", "noted", may10},
		{"e2", "main", "sess_a", "run-2", "what's the weather", "sunny", may10.Add(time.Hour)},
		{"e3", "main", "sess_b", "run-3", "I like Go", "great", jun15},
		{"e4", "researcher", "sess_c", "run-4", "research the Go memory model", "done", sep20},
	}
	eps, err := episodic.Open(fx.d.path(episodesDB))
	must(err)
	aw := archive.NewWriter(fx.d.DataDir)
	runs, err := runtrace.Open(fx.d.path(runsDB), runtrace.Config{MaxAge: -1, MaxCount: -1})
	must(err)
	for _, tr := range turns {
		// The archive gets the turn's events, a tool call in the middle.
		must(aw.Append(archive.Event{Timestamp: tr.at, AgentID: tr.agent, SessionID: tr.session, Role: "user", Content: tr.prompt}))
		output := "tool output for " + tr.prompt
		if tr.name == "e2" {
			// A tool result elsewhere in the session repeats the address.
			output = "weather near 221B Baker Street: sunny"
		}
		must(aw.Append(archive.Event{Timestamp: tr.at.Add(time.Second), AgentID: tr.agent, SessionID: tr.session, Role: "tool",
			ToolResult: &archive.ToolResult{CallID: "c-" + tr.name, Content: output}}))
		must(aw.Append(archive.Event{Timestamp: tr.at.Add(2 * time.Second), AgentID: tr.agent, SessionID: tr.session, Role: "assistant", Content: tr.response}))
		id, err := eps.Insert(ctx, &episodic.Episode{AgentID: tr.agent, SessionID: tr.session, SourceRunID: tr.run,
			Timestamp: tr.at.Add(3 * time.Second), Prompt: tr.prompt, Response: tr.response, EmbedderID: "t/3", Embedding: vec()})
		must(err)
		fx.ep[tr.name] = id
		run := runtrace.Run{TraceID: tr.run, AgentID: tr.agent, SessionID: tr.session, Source: "test", StartedAt: tr.at, Outcome: "ok"}
		must(runs.Start(ctx, run))
		if tr.name == "e4" {
			// Only the run record knows this; the episode never says it.
			run.ToolCalls = []runtrace.ToolCall{{Name: "web_search", Arguments: `{"q":"secret-project roadmap"}`, Outcome: "ok"}}
			must(runs.Finish(ctx, run))
		}
	}
	must(aw.Flush())
	eps.Close()
	runs.Close()

	facts, err := semantic.Open(fx.d.path(factsDB))
	must(err)
	addFact := func(name, text string, sources []int64, runs []string) {
		id, err := facts.Insert(ctx, &semantic.Fact{AgentID: "main", Fact: text, SourceEpisodes: sources, SourceRunIDs: runs,
			FirstSeen: may10, LastConfirmed: may10, EmbedderID: "t/3", Embedding: vec()})
		must(err)
		fx.f[name] = id
	}
	addFact("f1", "User lives at 221B Baker Street", []int64{fx.ep["e1"]}, []string{"run-1"})
	addFact("f2", "User likes Go", []int64{fx.ep["e3"]}, nil)
	addFact("f3", "User asks about the weather in London", []int64{fx.ep["e1"], fx.ep["e2"]}, []string{"run-1", "run-2"})
	addFact("f4", "User's name is Ada", nil, nil)
	must(facts.SetPinned(ctx, fx.f["f2"], true))
	smeStore, err := sme.New(facts.DB())
	must(err)
	_, err = smeStore.UpsertSection(ctx, sme.Section{AgentID: "main", Section: "About the user", Body: "Lives in London; likes Go.",
		SourceFactIDs: []int64{fx.f["f1"], fx.f["f2"]}})
	must(err)
	facts.Close()

	sk, err := skills.Open(fx.d.path(skillsDB))
	must(err)
	// weather-check's first version mentioned the address; its history keeps it.
	_, err = sk.Upsert(ctx, &skills.Skill{AgentID: "main", Name: "weather-check", Description: "check the weather", Recipe: "look out of 221B Baker Street",
		Status: "live", Enabled: true, EmbedderID: "t/3", Embedding: vec(), SourceRunIDs: []string{"run-2"}})
	must(err)
	_, err = sk.Upsert(ctx, &skills.Skill{AgentID: "main", Name: "weather-check", Description: "check the weather", Recipe: "call the weather tool",
		Status: "live", Enabled: true, EmbedderID: "t/3", Embedding: vec(), SourceRunIDs: []string{"run-2"}})
	must(err)
	_, err = sk.Upsert(ctx, &skills.Skill{AgentID: "main", Name: "address-lookup", Description: "find the user's home", Recipe: "it is 221B Baker Street",
		Status: "live", Enabled: true, EmbedderID: "t/3", Embedding: vec()})
	must(err)
	sk.Close()

	u, err := usage.Open(fx.d.path(usageDB))
	must(err)
	must(u.Record(ctx, "main", "m", 10, 20, ""))
	u.Close()

	must(os.WriteFile(fx.d.path("profile.main.md"), []byte("# Profile\n\n## Recently Remembered\n- lives at 221B Baker Street\n- likes Go\n"), 0o600))
	must(os.MkdirAll(filepath.Join(fx.d.ConfigDir, "soul"), 0o700))
	must(os.WriteFile(filepath.Join(fx.d.ConfigDir, "soul", "main.toml"), []byte("[identity]\nnote = \"lives at 221B Baker Street\"\n"), 0o600))
	must(os.MkdirAll(fx.d.path("research", "20260501-120000-old"), 0o700))
	must(os.WriteFile(fx.d.path("research", "20260501-120000-old", "report.md"), []byte("# Old report\n"), 0o600))
	must(os.MkdirAll(fx.d.path("screenshots"), 0o700))
	old := fx.d.path("screenshots", "web-1.png")
	must(os.WriteFile(old, []byte("png"), 0o600))
	must(os.Chtimes(old, may10, may10))
	must(os.MkdirAll(fx.d.path("eval-artifacts"), 0o700))
	for _, f := range []string{"eval-20260501.json", "baseline.smoke.json", "trend.jsonl"} {
		p := fx.d.path("eval-artifacts", f)
		must(os.WriteFile(p, []byte("{}\n"), 0o600))
		must(os.Chtimes(p, may10, may10))
	}
	return fx
}
