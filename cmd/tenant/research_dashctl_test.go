package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tenant/internal/agent"
	"tenant/internal/dashboard"
	"tenant/internal/lifecycle"
	"tenant/internal/memory/working"
	"tenant/internal/model"
	"tenant/internal/model/testllm"
	"tenant/internal/orchestra"
	"tenant/internal/plugins/osys"
	"tenant/internal/plugins/web"
	"tenant/internal/research"
	"tenant/internal/tui"
)

type runCall struct {
	q, replayOf string
	skip        bool
}

type runOut struct {
	id, report string
	err        error
}

// stubbedDash is the adapter with its run stubbed: each run reports its
// arguments on calls and waits for an answer on outs (or its context).
func stubbedDash(t *testing.T, withStore bool) (d *dashResearch, rc *researchControl, calls <-chan runCall, outs chan<- runOut) {
	t.Helper()
	rc = &researchControl{}
	if withStore {
		s, err := research.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		rc.store = s
	}
	d = dashResearchOf(context.Background(), rc, true, nil)
	c, o := make(chan runCall, 4), make(chan runOut)
	d.run = func(ctx context.Context, q, replayOf string, skip bool) (string, string, error) {
		c <- runCall{q, replayOf, skip}
		select {
		case out := <-o:
			return out.id, out.report, out.err
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
	}
	return d, rc, c, o
}

// awaitPhase waits for the live view to reach phase.
func awaitPhase(t *testing.T, d *dashResearch, phase string) dashboard.ResearchLiveView {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		v, changed := d.Watch()
		if v.Phase == phase {
			return v
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("phase %q, want %q", v.Phase, phase)
		}
	}
}

func nextCall(t *testing.T, calls <-chan runCall) runCall {
	t.Helper()
	select {
	case c := <-calls:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no run started")
	}
	return runCall{}
}

// The pipeline's timeline updates become the page's phase, round and
// questions, and each one wakes the page (a helper's row doesn't).
func TestDashResearch_Timeline(t *testing.T) {
	d, _, _, _ := stubbedDash(t, false)
	steps := []struct {
		u                  tui.ResearchTimelineUpdate
		phase, progress    string
		round, rounds, nqs int
		wakes              bool
	}{
		{tui.ResearchTimelineUpdate{Kind: "started", Total: 2}, "planning", "", 0, 0, 0, true},
		{tui.ResearchTimelineUpdate{Kind: "plan", Cycle: 1, Total: 2, Plan: &tui.ResearchPlanData{SubQuestions: []string{"a", "b", "c"}}}, "researching", "", 1, 2, 3, true},
		{tui.ResearchTimelineUpdate{Kind: "wave", Cycle: 1, Total: 2, Wave: &tui.ResearchWaveData{From: 1, To: 3, Total: 3}}, "researching", "Questions 1-3 are in progress.", 1, 2, 3, true},
		{tui.ResearchTimelineUpdate{Kind: "agent_status", Agent: &tui.ResearchAgentRow{ID: "r1", Status: "running"}}, "researching", "Questions 1-3 are in progress.", 1, 2, 3, false},
		{tui.ResearchTimelineUpdate{Kind: "reflect_done", Cycle: 1, Total: 2, Reflect: &tui.ResearchReflectData{}}, "reflecting", "Questions 1-3 are in progress.", 1, 2, 3, true},
		{tui.ResearchTimelineUpdate{Kind: "plan", Cycle: 2, Total: 2, Plan: &tui.ResearchPlanData{SubQuestions: []string{"d", "e"}}}, "researching", "", 2, 2, 2, true},
		{tui.ResearchTimelineUpdate{Kind: "wave", Cycle: 2, Total: 2, Wave: &tui.ResearchWaveData{From: 2, To: 2, Total: 2}}, "researching", "Question 2 is in progress.", 2, 2, 2, true},
		{tui.ResearchTimelineUpdate{Kind: "synth", Synth: &tui.ResearchSynthData{Starting: true}}, "writing", "Question 2 is in progress.", 2, 2, 2, true},
		{tui.ResearchTimelineUpdate{Kind: "done", Done: &tui.ResearchDoneData{Status: "done"}}, "saving", "Question 2 is in progress.", 2, 2, 2, true},
	}
	for i, s := range steps {
		_, changed := d.Watch()
		d.rc.emitTimeline(s.u) // what the pipeline calls
		v, _ := d.Watch()
		woke := false
		select {
		case <-changed:
			woke = true
		default:
		}
		if v.Phase != s.phase || v.Progress != s.progress || v.Round != s.round || v.Rounds != s.rounds || len(v.Questions) != s.nqs || woke != s.wakes {
			t.Errorf("step %d (%s): phase %q progress %q round %d/%d questions %d woke %v", i, s.u.Kind, v.Phase, v.Progress, v.Round, v.Rounds, len(v.Questions), woke)
		}
	}
}

// The terminal app's control keeps its own progress sinks: a dashboard run
// never opens the terminal's timeline pane. Its feed gets the lines.
func TestDashResearch_KeepsTheTerminalTimeline(t *testing.T) {
	var timeline, said, fed int
	rc := &researchControl{say: func(string, ...any) { said++ }, emitTimeline: func(tui.ResearchTimelineUpdate) { timeline++ }}
	life := lifecycle.New(testLog())
	t.Cleanup(func() { _ = life.Shutdown() })
	d := newDashResearch(context.Background(), life, TeamConfig{}, newTeamRuntime(TeamConfig{}), rc, true, func(string, ...any) { fed++ }).(*dashResearch)
	d.rc.emitTimeline(tui.ResearchTimelineUpdate{Kind: "started"})
	d.rc.say("plan: %d sub-question(s)", 3)
	if timeline != 0 || said != 0 || fed != 1 {
		t.Errorf("terminal timeline %d, terminal say %d, feed %d; want 0, 0, 1", timeline, said, fed)
	}
	if v, _ := d.Watch(); v.Phase != "planning" {
		t.Errorf("the page's phase = %q", v.Phase)
	}
	if rc.busy == nil || rc.busy != d.rc.busy {
		t.Error("the two sides don't share the one-run flag")
	}
}

// C-1: in the terminal app the page's research has its own runtime, as
// serve's does. On the orchestrator's, a run's new window cancelled the
// helpers a chat turn had spawned; a run from the page is in the background,
// so the two overlap.
func TestDashResearch_TerminalRunLeavesChatHelpersAlone(t *testing.T) {
	reg := model.NewEmptyRegistry()
	if err := reg.Add(model.Profile{ID: "orch", Role: model.RolePlanner, Backend: "orch", ProviderKind: "vllm", Endpoint: "http://orch", ContextLength: 32000}); err != nil {
		t.Fatal(err)
	}
	router := model.NewRouter(reg, nil)
	planner := testllm.New()
	planner.GenerateFn = func(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
		return nil, errors.New("planner down")
	}
	router.RegisterBackend("orch", func(context.Context, model.Profile, *slog.Logger) (any, error) { return planner, nil })
	ag, err := agent.New(agent.Config{AgentID: "main", Router: router, Working: working.New(), Tools: agent.NewStaticRegistry(), Dispatcher: newToolMux()})
	if err != nil {
		t.Fatal(err)
	}
	cfg := TeamConfig{Bus: orchestra.NewBus(), OrchID: "main"}
	rt := newTeamRuntime(cfg) // cmdTUI's: the orchestrator's and /research's
	rt.budget.Ensure(context.Background())
	helperCtx, release, err := rt.budget.AcquireAgent() // a chat turn's spawn_agent
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	life := lifecycle.New(testLog())
	t.Cleanup(func() { _ = life.Shutdown() })
	s, _ := research.New(t.TempDir())
	rc := &researchControl{ag: ag, rt: rt, store: s, opts: defaultResearchOpts()}
	d := newDashResearch(context.Background(), life, cfg, rt, rc, true, nil).(*dashResearch)
	if d.rc.rt == rt || rc.busy == nil || d.rc.busy != rc.busy {
		t.Fatal("the page's research must have its own runtime and share the terminal's one-run flag")
	}
	if err := d.Start("what is fakeness?", true); err != nil {
		t.Fatal(err)
	}
	awaitPhase(t, d, "failed")
	if helperCtx.Err() != nil {
		t.Fatalf("the chat's helper was cancelled by a dashboard research start: %v", context.Cause(helperCtx))
	}
}

// One run at a time, shared with the terminal app.
func TestDashResearch_StartAndBusy(t *testing.T) {
	d, rc, calls, outs := stubbedDash(t, false)
	if err := d.Start("What is fakeness?", false); err != nil {
		t.Fatal(err)
	}
	if c := nextCall(t, calls); c != (runCall{"What is fakeness?", "", false}) {
		t.Errorf("run(%+v)", c)
	}
	if v, _ := d.Watch(); !v.Running || v.Phase != "checking" || v.Question != "What is fakeness?" || v.Elsewhere {
		t.Errorf("after Start: %+v", v)
	}
	var pe dashboard.PlainError
	if err := d.Start("again", true); !errors.As(err, &pe) || !strings.Contains(string(pe), "already running") {
		t.Errorf("a second Start = %v, want the plain busy error", err)
	}
	outs <- runOut{report: "# R\nAnswer [1].\n\n## References\n[1] https://example.com/a"}
	v := awaitPhase(t, d, "done")
	if v.Running || v.Report == nil || v.Report.Body != "# R\nAnswer [1]." || len(v.Report.Refs) != 1 || v.Report.Refs[0].URL != "https://example.com/a" {
		t.Errorf("done without a store: %+v %+v", v, v.Report)
	}

	// The terminal app's run holds the shared flag: the page says so and
	// won't start another.
	if rc.busy == nil || rc.busy != d.rc.busy {
		t.Fatal("the terminal app's control doesn't share the adapter's one-run flag")
	}
	rc.busy.Store(true)
	if v, _ := d.Watch(); !v.Elsewhere {
		t.Error("a terminal run doesn't show as running elsewhere")
	}
	if err := d.Start("mine", false); !errors.As(err, &pe) || !strings.Contains(string(pe), "from the terminal") {
		t.Errorf("Start during a terminal run = %v", err)
	}
	// runOne itself refuses before it touches the agent (ag is nil here).
	if _, _, err := rc.runOne(context.Background(), "q", "", true); !errors.Is(err, errResearchBusy) {
		t.Errorf("runOne while busy = %v", err)
	}
	rc.busy.Store(false)
}

func TestDashResearch_Clarify(t *testing.T) {
	d, _, calls, outs := stubbedDash(t, false)
	var pe dashboard.PlainError
	if err := d.Clarify("x"); !errors.As(err, &pe) {
		t.Errorf("Clarify with nothing asked = %v", err)
	}
	for _, answer := range []string{"2024 only", ""} {
		if err := d.Start("rust", false); err != nil {
			t.Fatal(err)
		}
		nextCall(t, calls)
		outs <- runOut{err: &ClarifyNeededError{Question: "rust", Questions: []string{"The language or the metal?"}}}
		if v := awaitPhase(t, d, "clarify"); v.Question != "rust" || !slices.Equal(v.Clarify, []string{"The language or the metal?"}) || v.Running {
			t.Errorf("clarify: %+v", v)
		}
		if err := d.Clarify(answer); err != nil {
			t.Fatal(err)
		}
		if c := nextCall(t, calls); c != (runCall{EnrichClarified("rust", answer), "", true}) {
			t.Errorf("Clarify(%q) ran %+v", answer, c)
		}
		outs <- runOut{err: errors.New("research: plan: down")}
		awaitPhase(t, d, "failed")
	}
}

// Stop cancels the run's context, which the helpers run under.
func TestDashResearch_Stop(t *testing.T) {
	d, _, calls, _ := stubbedDash(t, false)
	if d.Stop() {
		t.Error("Stop with nothing running = true")
	}
	if err := d.Start("q", true); err != nil {
		t.Fatal(err)
	}
	nextCall(t, calls)
	if !d.Stop() {
		t.Fatal("Stop = false while running")
	}
	if v := awaitPhase(t, d, "stopped"); v.Running {
		t.Errorf("stopped but running: %+v", v)
	}
}

// How a run ends, in words the page can show: never the raw error.
func TestDashResearch_FinishMapping(t *testing.T) {
	for raw, want := range map[string]string{
		"resolve planner: " + `provider "cloud" is not configured`: "Your agent's model didn't answer. Check the Model page, then try again.",
		"research: plan: connection refused":                       "Your agent's model didn't answer. Check the Model page, then try again.",
		"research: no usable findings — too thin":                  "The helpers didn't find enough to write a report. What they found is listed with the run.",
		errResearchBusy.Error():                                    "Research was already running from the terminal. Start another when it finishes.",
		"research: synthesize: sql: database is closed":            "Research stopped because of an error. The full log has the details.",
	} {
		d, _, calls, outs := stubbedDash(t, false)
		if err := d.Start("q", true); err != nil {
			t.Fatal(err)
		}
		nextCall(t, calls)
		outs <- runOut{err: errors.New(raw)}
		if v := awaitPhase(t, d, "failed"); v.Err != want {
			t.Errorf("%q → %q, want %q", raw, v.Err, want)
		}
	}

	// A stored run: the report comes back from the store, without its
	// reference block, with its sources parsed.
	d, rc, calls, outs := stubbedDash(t, true)
	report := "# Fake report\nAnswer [1].\n\n## References\n[1] https://example.com/a\n[2] wiki:notes/x.md"
	run, err := rc.store.Create(research.Manifest{Question: "What is fakeness?", Model: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Finalize(report, parseReferencesURIs(report), research.StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	if err := d.Start("What is fakeness?", true); err != nil {
		t.Fatal(err)
	}
	nextCall(t, calls)
	outs <- runOut{id: run.ID(), report: report}
	v := awaitPhase(t, d, "done")
	r := v.Report
	if r == nil || r.ID != run.ID() || r.Body != "# Fake report\nAnswer [1]." || r.StateText != "Finished" || r.Model != "fake" ||
		!slices.Equal(r.Refs, []dashboard.ResearchRef{{N: 1, URL: "https://example.com/a"}, {N: 2, URL: "wiki:notes/x.md"}}) {
		t.Errorf("stored report = %+v", r)
	}
	if v.RunID != run.ID() {
		t.Errorf("RunID = %q", v.RunID)
	}
}

// The run id comes from its progress line; the live run can't be deleted;
// the page keeps only the lines worth showing, in plain words, and the
// terminal feed gets every line as it was.
func TestDashResearch_RunIDLinesAndDelete(t *testing.T) {
	var fed []string
	rc := &researchControl{}
	s, err := research.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rc.store = s
	d := dashResearchOf(context.Background(), rc, true, func(f string, a ...any) { fed = append(fed, f) })
	release := make(chan struct{})
	d.run = func(ctx context.Context, q, _ string, _ bool) (string, string, error) {
		<-release
		return "", "", errors.New("research: plan: down")
	}
	run, _ := s.Create(research.Manifest{Question: "q"})
	if err := d.Start("q", true); err != nil {
		t.Fatal(err)
	}
	d.say(sayRunID, run.ID())
	d.say("plan: %d sub-question(s)", 3)
	d.say("📄 saved to wiki: %s", "research/what-is-fakeness.md")
	d.say("  ! spawn failed: %v", errors.New("sql: database is closed"))
	d.say("  · [%s] %s — %d chars: %s", "r1", "error", 12, "error: boom")
	d.say("  · [%s] %s — %d chars: %s", "r2", "done", 12, "Fakeness is")
	v, _ := d.Watch()
	if v.RunID != run.ID() {
		t.Errorf("RunID = %q, want %q", v.RunID, run.ID())
	}
	want := []string{"Saved to your agent's notes as research/what-is-fakeness.md.", "A helper couldn't start.", "A helper didn't finish.", "A helper finished: Fakeness is"}
	if !slices.Equal(v.Lines, want) {
		t.Errorf("Lines = %q, want %q", v.Lines, want)
	}
	if len(fed) != 6 || fed[0] != sayRunID {
		t.Errorf("the terminal feed got %q", fed)
	}
	rows, _ := d.History(10)
	if len(rows) != 1 || !rows[0].Live || rows[0].StateText != "Running now" {
		t.Errorf("the live run's row = %+v", rows)
	}
	var pe dashboard.PlainError
	if err := d.Delete(run.ID()); !errors.As(err, &pe) || !strings.Contains(string(pe), "still running") {
		t.Errorf("Delete of the live run = %v", err)
	}
	close(release)
	awaitPhase(t, d, "failed")
	if err := d.Delete(run.ID()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(run.ID()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("after Delete, Get = %v", err)
	}
	if err := d.Delete(run.ID()); err != nil {
		t.Errorf("deleting again = %v (idempotent)", err)
	}
}

func TestDashResearch_HistoryLabels(t *testing.T) {
	d, rc, _, _ := stubbedDash(t, true)
	mk := func(q string, status research.Status, errMsg string) string {
		r, err := rc.store.Create(research.Manifest{Question: q, Model: "fake", Started: d.since.Add(-time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		if status != research.StatusRunning {
			if err := r.Finalize("body", []string{"https://a"}, status, errMsg); err != nil {
				t.Fatal(err)
			}
		}
		return r.ID()
	}
	mk("done", research.StatusDone, "")
	mk("partial", research.StatusPartial, "research: no usable findings — x")
	mk("error", research.StatusError, "research: plan: down")
	mk("clarify", research.StatusError, "research: clarification needed (1 question)")
	mk("interrupted", research.StatusRunning, "")
	stopped := mk("stopped", research.StatusError, "research: plan: context canceled")
	rows, err := d.History(10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.Question] = r.StateText
		if r.Live || r.When == "" || r.Model != "fake" {
			t.Errorf("row %+v", r)
		}
	}
	want := map[string]string{"done": "Finished", "partial": "Partly finished", "error": "Didn't finish", "clarify": "Asked for more detail",
		"interrupted": "Interrupted (Tenant restarted)", "stopped": "Stopped"}
	for q, w := range want {
		if got[q] != w {
			t.Errorf("%s: %q, want %q", q, got[q], w)
		}
	}
	if v, err := d.Report(stopped); err != nil || v.StateText != "Stopped" || v.Err != "" {
		t.Errorf("a stopped run's page: %+v, %v (no error banner)", v, err)
	}

	// A run the terminal app is doing: live, never "interrupted", and it
	// can't be deleted from under it. The older "running" row stays
	// interrupted.
	term, _ := rc.store.Create(research.Manifest{Question: "terminal"})
	rc.busy.Store(true)
	rows, _ = d.History(10)
	for _, r := range rows {
		if (r.Question == "terminal") != r.Live || r.Question == "terminal" && r.StateText != "Running from the terminal" || r.Question == "interrupted" && r.StateText != want["interrupted"] {
			t.Errorf("while the terminal runs: %+v", r)
		}
	}
	if v, _ := d.Report(term.ID()); !v.Live {
		t.Errorf("the terminal run's page offers Delete: %+v", v)
	}
	var pe dashboard.PlainError
	if err := d.Delete(term.ID()); !errors.As(err, &pe) {
		t.Errorf("Delete of the terminal's run = %v", err)
	}
	rc.busy.Store(false)
	if rows, _ := d.History(10); rows[0].Question != "terminal" || rows[0].Live || rows[0].StateText != want["interrupted"] {
		t.Errorf("after the terminal run: %+v", rows[0])
	}
	// No store: plain errors, never the raw ones.
	d2, _, _, _ := stubbedDash(t, false)
	for name, err := range map[string]error{"History": second(d2.History(1)), "Report": second(d2.Report("x")), "Replay": d2.Replay("x"), "Delete": d2.Delete("x")} {
		if !errors.Is(err, errNoResearchHistory) {
			t.Errorf("%s without a store = %v", name, err)
		}
	}
	if _, err := d.Report("missing"); !errors.As(err, &pe) {
		t.Errorf("Report of a missing run = %v", err)
	}
}

func second[T any](_ T, err error) error { return err }

// A report lists each helper's question and finding; an error finding's
// text (an error message) stays off the page.
func TestDashResearch_ReportFindings(t *testing.T) {
	d, rc, _, _ := stubbedDash(t, true)
	r, _ := rc.store.Create(research.Manifest{Question: "q"})
	_ = r.AppendFinding(research.Finding{AgentID: "a1", Task: researcherTask("q", "What is A?"), Status: "done", Result: "A is a letter."})
	_ = r.AppendFinding(research.Finding{AgentID: "a2", Task: researcherTask("q", "What is B?"), Status: "error", Result: "error: sql: database is closed"})
	_ = r.Finalize("", nil, research.StatusError, "research: no usable findings — x")
	v, err := d.Report(r.ID())
	if err != nil {
		t.Fatal(err)
	}
	want := []dashboard.ResearchFinding{{Question: "What is A?", State: "Finished", Body: "A is a letter."}, {Question: "What is B?", State: "Didn't finish"}}
	if !slices.Equal(v.Findings, want) || v.StateText != "Didn't finish" || !strings.HasPrefix(v.Err, "The helpers didn't find enough") {
		t.Errorf("report = %+v", v)
	}
}

func TestSubQuestionOf(t *testing.T) {
	for task, want := range map[string]string{
		researcherTask("g", "what is X?"): "what is X?",
		"  malformed\nsecond line":        "malformed",
		"":                                "",
	} {
		if got := subQuestionOf(task); got != want {
			t.Errorf("subQuestionOf(%q) = %q, want %q", task, got, want)
		}
	}
}

func TestDashResearch_Replay(t *testing.T) {
	d, rc, calls, outs := stubbedDash(t, true)
	orig, _ := rc.store.Create(research.Manifest{Question: "graphiti"})
	_ = orig.Finalize("body", nil, research.StatusDone, "")
	if err := d.Replay(orig.ID()); err != nil {
		t.Fatal(err)
	}
	if c := nextCall(t, calls); c != (runCall{"graphiti", orig.ID(), true}) {
		t.Errorf("Replay ran %+v", c)
	}
	if v, _ := d.Watch(); v.ReplayOf != orig.ID() || v.Phase != "planning" { // a re-run skips the clarifier
		t.Errorf("ReplayOf = %q, phase = %q", v.ReplayOf, v.Phase)
	}
	outs <- runOut{err: errors.New("x")}
	awaitPhase(t, d, "failed")
	var pe dashboard.PlainError
	if err := d.Replay("missing"); !errors.As(err, &pe) {
		t.Errorf("Replay of a missing run = %v", err)
	}
}

// Replay goes the real way: runOne resolves the current planner, and the new
// run is stored as a re-run of the old one.
func TestDashResearch_ReplayRunsThePipeline(t *testing.T) {
	reg := model.NewEmptyRegistry()
	if err := reg.Add(model.Profile{ID: "orch", Role: model.RolePlanner, Backend: "orch", ProviderKind: "vllm", Endpoint: "http://orch", ContextLength: 32000}); err != nil {
		t.Fatal(err)
	}
	router := model.NewRouter(reg, nil)
	planner := testllm.New()
	var asked atomic.Int32
	planner.GenerateFn = func(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
		asked.Add(1)
		return nil, errors.New("planner down")
	}
	router.RegisterBackend("orch", func(context.Context, model.Profile, *slog.Logger) (any, error) { return planner, nil })
	ag, err := agent.New(agent.Config{AgentID: "main", Router: router, Working: working.New(), Tools: agent.NewStaticRegistry(), Dispatcher: newToolMux()})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := research.New(t.TempDir())
	orig, _ := s.Create(research.Manifest{Question: "graphiti"})
	_ = orig.Finalize("body", nil, research.StatusDone, "")
	d := dashResearchOf(context.Background(), &researchControl{ag: ag, rt: newTeamRuntime(TeamConfig{}), store: s, opts: defaultResearchOpts()}, true, nil)
	if err := d.Replay(orig.ID()); err != nil {
		t.Fatal(err)
	}
	if v := awaitPhase(t, d, "failed"); !strings.HasPrefix(v.Err, "Your agent's model didn't answer") || asked.Load() == 0 {
		t.Errorf("failed with %q after %d planner calls", v.Err, asked.Load())
	}
	ms, _ := s.List(0)
	if len(ms) != 2 || ms[0].ReplayOf != orig.ID() || ms[0].Question != "graphiti" || ms[0].Status != research.StatusError {
		t.Errorf("runs = %+v", ms)
	}
}

// Serve's research runtime is its own (bus, runtime, no tool-switch saving)
// but keeps serve's tools, approval gate and web policy: a helper's gated
// call waits for the operator like any other. Web browsing goes off only
// for a service account with Chrome's sandbox off (D19).
func TestServeDashResearch_KeepsGates(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := &commonFlags{backend: "echo", agent: "main", dataDir: t.TempDir(), cfgDir: t.TempDir()}
	st, closeStores, err := openStores(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeStores)
	broker := newApprovalBroker(log) // everything asks
	t.Cleanup(broker.DenyAll)
	egress := originConfirm(broker.Confirm)
	shared := newToolMux()
	shared.Add("os", osys.NewDispatcher(nil, osys.Policy{Confirm: egress})) // serve's gated shell

	// Helpers run on a scripted model: run a command, then answer.
	reg := model.NewEmptyRegistry()
	for _, p := range []model.Profile{
		{ID: "orch", Role: model.RolePlanner, Backend: "orch", ProviderKind: "vllm", Endpoint: "http://orch", ContextLength: 32000},
		{ID: "embed", Role: model.RoleEmbedder, Backend: "embed", ProviderKind: "vllm", Endpoint: "http://embed", ContextLength: 8000},
	} {
		if err := reg.Add(p); err != nil {
			t.Fatal(err)
		}
	}
	router := model.NewRouter(reg, nil)
	helper := testllm.New()
	var calls atomic.Int32
	helper.GenerateFn = func(context.Context, model.GenerateRequest) (*model.GenerateResponse, error) {
		if calls.Add(1) == 1 {
			return &model.GenerateResponse{ToolCalls: []model.ToolCall{teamCall("x", "os_exec", map[string]any{"command": "ls"})}}, nil
		}
		return &model.GenerateResponse{Text: "done", FinishReason: "stop"}, nil
	}
	router.RegisterBackend("orch", func(context.Context, model.Profile, *slog.Logger) (any, error) { return helper, nil })
	router.RegisterBackend("embed", func(context.Context, model.Profile, *slog.Logger) (any, error) { return testllm.New(), nil })

	serveBus := orchestra.NewBus()
	serveBus.Register("main")
	cfg := TeamConfig{
		Bus: serveBus, Router: router, Stores: st, Shared: shared, OrchID: "main", Log: log,
		ConfirmSensitiveEgress: egress, WebConfig: web.Config{NoSandbox: true}, WebPolicy: web.Policy{AllowInteract: true},
		LocalRestore: map[string]bool{"web_read": false}, LocalOnChange: func(map[string]bool) {},
	}
	serveRT := newTeamRuntime(cfg)
	life := lifecycle.New(log)
	t.Cleanup(func() { _ = life.Shutdown() })
	build := func(service, web, noSandbox bool) *dashResearch {
		prev := runsAsService
		runsAsService = func() bool { return service }
		defer func() { runsAsService = prev }()
		c := cfg
		c.WebConfig.NoSandbox = noSandbox
		return newServeDashResearch(context.Background(), life, c, serveRT, researchControl{}, t.TempDir(), web).(*dashResearch)
	}

	for _, tc := range []struct {
		service, web, noSandbox, webOn bool
		why                            string
	}{
		{service: true, web: true, noSandbox: true, webOn: false, why: "the browser would have full control of the computer"},
		{service: true, web: true, noSandbox: false, webOn: true}, // Chrome's sandbox holds
		{service: false, web: true, noSandbox: true, webOn: true},
		{service: false, web: false, noSandbox: true, webOn: false},
	} {
		d := build(tc.service, tc.web, tc.noSandbox)
		v, _ := d.Watch()
		if d.rc.rt.cfg.Web != tc.webOn || v.WebOff == tc.webOn || !strings.Contains(v.WebOffWhy, tc.why) || (tc.why == "") != (v.WebOffWhy == "") {
			t.Errorf("service %v, web %v, no sandbox %v: runtime web %v, page %+v", tc.service, tc.web, tc.noSandbox, d.rc.rt.cfg.Web, v)
		}
	}

	d := build(false, true, true)
	rt := d.rc.rt
	switch {
	case rt == serveRT || rt.cfg.Bus == serveBus:
		t.Error("research shares serve's runtime or bus")
	case rt.cfg.Shared != shared:
		t.Error("research lost serve's tools")
	case rt.cfg.ConfirmSensitiveEgress == nil || !rt.cfg.WebPolicy.AllowInteract:
		t.Error("research lost serve's approval gate or web policy")
	case rt.cfg.LocalRestore != nil || rt.cfg.LocalOnChange != nil:
		t.Error("research would save serve's tool switches")
	case d.rc.store == nil || d.rc.busy == nil:
		t.Error("research has no store or no one-run flag")
	}

	// A helper added on the Helpers page reaches research at its next start.
	serveRT.SetAgentProfiles(map[string]*agentProfile{"checker": {Soul: "Check facts."}})
	d.run = func(context.Context, string, string, bool) (string, string, error) { return "", "", errors.New("x") }
	if err := d.Start("q", true); err != nil {
		t.Fatal(err)
	}
	if _, ok := rt.AgentProfiles()["checker"]; !ok {
		t.Error("the research runtime didn't pick up the helper profiles at start")
	}

	// A helper's command waits for the operator's OK.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := rt.Spawn(ctx, "researcher", "list the files"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(broker.Pending()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	p := broker.Pending()
	if len(p) != 1 || p[0].Action != "os_exec" {
		t.Fatalf("pending = %+v, want the helper's os_exec", p)
	}
	if err := broker.Decide(p[0].ID, "deny"); err != nil {
		t.Fatal(err)
	}
	rt.Await(ctx, 20*time.Second)
	if r := rt.Results(); len(r) != 1 || r[0].Status != "done" {
		t.Errorf("helper results = %+v", r)
	}
}
