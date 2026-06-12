package assemble_test

import (
	"context"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"tenant/internal/memory/assemble"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/memory/soul"
	"tenant/internal/memory/working"
	"tenant/internal/model"
)

// fakeCounter returns one token per 4 bytes (a deterministic estimator).
// Lets tests reason about budgets without spinning up a real LLM.
func fakeCounter() assemble.TokenCounter {
	return assemble.CounterFunc(func(_ context.Context, text string) (int, error) {
		return (len(text) + 3) / 4, nil
	})
}

// mkProfile returns a Profile with the per-class budget split from the
// shipped Qwen 3.6-72B YAML — enough room for meaningful assemble work
// in tests.
func mkProfile() model.Profile {
	return model.Profile{
		ID:                       "test-profile",
		Role:                     "planner",
		Backend:                  "vllm",
		ContextLength:            128000,
		OperationalContextBudget: 102400,
		ReserveSoul:              2048,
		ReserveSystemPrompt:      3072,
		ReserveToolDefs:          4096,
		ReserveResponse:          80000,
	}
}

// effWritable mirrors the assembler's measured-static budget for mkProfile
// (TEN-214): OperationalContextBudget − static (soul+system+tools) −
// ReserveResponse. The variable-tier slots are sized against THIS, not the
// fixed-reserve WritableBudget(). Tests below pass ~0 static, so effWritable(0)
// is the slot basis.
func effWritable(staticToks int) int {
	p := mkProfile()
	return p.OperationalContextBudget - staticToks - p.ReserveResponse
}

func mkEpisodicStore(t *testing.T) *episodic.Store {
	t.Helper()
	s, err := episodic.Open(filepath.Join(t.TempDir(), "ep.db"))
	if err != nil {
		t.Fatalf("episodic.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mkSemanticStore(t *testing.T) *semantic.Store {
	t.Helper()
	s, err := semantic.Open(filepath.Join(t.TempDir(), "fact.db"))
	if err != nil {
		t.Fatalf("semantic.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func vecA() []float32 { return []float32{1, 0, 0, 0} }

func TestAssemble_MinimalEmptyReturnsNoMessages(t *testing.T) {
	a := assemble.New(fakeCounter())
	r, err := a.Assemble(context.Background(), assemble.Request{Profile: mkProfile()})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(r.Messages) != 0 {
		t.Fatalf("got %d messages, want 0 from empty Request: %+v", len(r.Messages), r.Messages)
	}
	if r.BudgetReport.Total != 0 {
		t.Errorf("Total = %d, want 0", r.BudgetReport.Total)
	}
}

func TestAssemble_SoulRendersAsSystem(t *testing.T) {
	a := assemble.New(fakeCounter())
	s := soul.NewDefault("main")
	s.User.Name = "Ada"
	r, err := a.Assemble(context.Background(), assemble.Request{
		Profile: mkProfile(),
		Soul:    s,
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(r.Messages) != 1 || r.Messages[0].Role != "system" {
		t.Fatalf("expected one system message, got: %+v", r.Messages)
	}
	if !strings.Contains(r.Messages[0].Content, "Ada") {
		t.Errorf("system content missing Ada: %q", r.Messages[0].Content)
	}
	if r.BudgetReport.SoulTokens == 0 {
		t.Errorf("SoulTokens not counted")
	}
}

func TestAssemble_WorkingSetPassesThrough(t *testing.T) {
	a := assemble.New(fakeCounter())
	w := working.New()
	w.Append(working.Message{Role: "user", Content: "what's the time?"})
	w.Append(working.Message{Role: "assistant", Content: "I don't know."})

	r, err := a.Assemble(context.Background(), assemble.Request{
		Profile: mkProfile(), Working: w,
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(r.Messages) != 2 {
		t.Fatalf("got %d messages, want 2: %+v", len(r.Messages), r.Messages)
	}
	if r.Messages[0].Role != "user" || r.Messages[1].Role != "assistant" {
		t.Errorf("working order wrong: %+v", r.Messages)
	}
}

func TestAssemble_UserQueryGoesLast(t *testing.T) {
	a := assemble.New(fakeCounter())
	w := working.New()
	w.Append(working.Message{Role: "user", Content: "prior turn"})
	r, err := a.Assemble(context.Background(), assemble.Request{
		Profile:   mkProfile(),
		Working:   w,
		UserQuery: "active turn",
	})
	if err != nil {
		t.Fatal(err)
	}
	last := r.Messages[len(r.Messages)-1]
	if last.Role != "user" || !strings.Contains(last.Content, "active turn") {
		t.Fatalf("last message wrong: %+v", last)
	}
	// Prior turn must appear BEFORE the active turn.
	if !strings.Contains(r.Messages[len(r.Messages)-2].Content, "prior turn") {
		t.Fatalf("prior turn not before active: %+v", r.Messages)
	}
}

func TestAssemble_SandwichPlacement_FactsInActiveTurn(t *testing.T) {
	a := assemble.New(fakeCounter())
	sstore := mkSemanticStore(t)
	ctx := context.Background()
	_, err := sstore.Insert(ctx, &semantic.Fact{
		AgentID: "main", Fact: "user prefers Go over Python",
		Confidence: 1.0, EmbedderID: "test", Embedding: vecA(),
	})
	if err != nil {
		t.Fatal(err)
	}

	r, err := a.Assemble(ctx, assemble.Request{
		Profile:       mkProfile(),
		SemanticStore: sstore,
		Query:         assemble.RetrievalQuery{Embedding: vecA()},
		AgentID:       "main",
		UserQuery:     "what language should I use?",
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(r.Messages) == 0 {
		t.Fatal("no messages assembled")
	}
	last := r.Messages[len(r.Messages)-1]
	if !strings.Contains(last.Content, "Known Facts") || !strings.Contains(last.Content, "Go over Python") {
		t.Fatalf("facts not in active-turn block: %q", last.Content)
	}
	if !strings.Contains(last.Content, "what language should I use") {
		t.Fatalf("user query not preserved in active turn: %q", last.Content)
	}
}

func TestAssemble_EpisodicAndSemanticBothRender(t *testing.T) {
	a := assemble.New(fakeCounter())
	ctx := context.Background()
	estore := mkEpisodicStore(t)
	sstore := mkSemanticStore(t)
	_, _ = estore.Insert(ctx, &episodic.Episode{
		AgentID: "main", Prompt: "previous question", Response: "previous answer",
		EmbedderID: "test", Embedding: vecA(),
	})
	_, _ = sstore.Insert(ctx, &semantic.Fact{
		AgentID: "main", Fact: "user is on macOS",
		Confidence: 1.0, EmbedderID: "test", Embedding: vecA(),
	})
	r, err := a.Assemble(ctx, assemble.Request{
		Profile:       mkProfile(),
		EpisodicStore: estore, SemanticStore: sstore,
		Query:   assemble.RetrievalQuery{Embedding: vecA()},
		AgentID: "main", UserQuery: "help me",
	})
	if err != nil {
		t.Fatal(err)
	}
	last := r.Messages[len(r.Messages)-1].Content
	if !strings.Contains(last, "Known Facts") || !strings.Contains(last, "on macOS") {
		t.Errorf("facts missing: %q", last)
	}
	if !strings.Contains(last, "Past Conversations") || !strings.Contains(last, "previous question") {
		t.Errorf("episodes missing: %q", last)
	}
}

func TestAssemble_BudgetReportAccounting(t *testing.T) {
	a := assemble.New(fakeCounter())
	s := soul.NewDefault("main")
	w := working.New()
	w.Append(working.Message{Role: "user", Content: strings.Repeat("hello ", 50)})

	r, err := a.Assemble(context.Background(), assemble.Request{
		Profile: mkProfile(),
		Soul:    s,
		Working: w,
		Tools: []model.ToolSpec{
			{Name: "search", Description: "search the web"},
		},
		UserQuery: "what now",
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.BudgetReport.SoulTokens == 0 {
		t.Error("SoulTokens = 0")
	}
	if r.BudgetReport.ToolTokens == 0 {
		t.Error("ToolTokens = 0")
	}
	if r.BudgetReport.WorkingTokens == 0 {
		t.Error("WorkingTokens = 0")
	}
	if r.BudgetReport.UserQueryToks == 0 {
		t.Error("UserQueryToks = 0")
	}
	if r.BudgetReport.WritableBudget != mkProfile().WritableBudget() {
		t.Errorf("WritableBudget = %d, want %d", r.BudgetReport.WritableBudget, mkProfile().WritableBudget())
	}
	// ContextWindow carries the model's real window so the TUI gauge reads
	// Total/ContextWindow (a true 0-100%), not Total/WritableBudget (which
	// overshoots because Total includes the static reserve WritableBudget omits).
	if r.BudgetReport.ContextWindow != mkProfile().ContextLength {
		t.Errorf("ContextWindow = %d, want ContextLength %d", r.BudgetReport.ContextWindow, mkProfile().ContextLength)
	}
	if r.BudgetReport.ContextWindow <= r.BudgetReport.WritableBudget {
		t.Errorf("ContextWindow (%d) must exceed WritableBudget (%d) — else the gauge can't read <100%%",
			r.BudgetReport.ContextWindow, r.BudgetReport.WritableBudget)
	}
	wantTotal := r.BudgetReport.SoulTokens + r.BudgetReport.SystemTokens +
		r.BudgetReport.ToolTokens + r.BudgetReport.WorkingTokens +
		r.BudgetReport.FactTokens + r.BudgetReport.EpisodeTokens +
		r.BudgetReport.UserQueryToks
	if r.BudgetReport.Total != wantTotal {
		t.Errorf("Total = %d, want sum of tiers = %d", r.BudgetReport.Total, wantTotal)
	}
}

func TestAssemble_TruncatesOverflowingWorkingSet(t *testing.T) {
	a := assemble.New(fakeCounter())
	// Make working set huge — bigger than 65% of WritableBudget (~13K).
	w := working.New()
	for i := 0; i < 200; i++ {
		w.Append(working.Message{Role: "user", Content: strings.Repeat("x", 400)})
	}
	r, err := a.Assemble(context.Background(), assemble.Request{
		Profile: mkProfile(),
		Working: w,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Should have truncated.
	if len(r.BudgetReport.Truncations) == 0 {
		t.Error("expected truncation entry in BudgetReport")
	}
	// Final tokens must be at or below the working slot (sized off the
	// measured-static effective budget — no soul/system/tools here, so
	// effWritable(0); TEN-214).
	workingSlot := int(0.65 * float64(effWritable(0)))
	if r.BudgetReport.WorkingTokens > workingSlot {
		t.Errorf("WorkingTokens %d exceeds slot %d after truncation", r.BudgetReport.WorkingTokens, workingSlot)
	}
}

func TestAssemble_CompactionRecommendedAt60Percent(t *testing.T) {
	a := assemble.New(fakeCounter())
	// Fill working to roughly 70% of writable budget.
	w := working.New()
	budget := effWritable(0)
	// Each 400-char message = 100 tokens. Aim for ~70% of budget.
	target := int(0.70 * float64(budget))
	for total := 0; total < target; total += 100 {
		w.Append(working.Message{Role: "user", Content: strings.Repeat("y", 400)})
	}
	r, err := a.Assemble(context.Background(), assemble.Request{
		Profile: mkProfile(), Working: w,
	})
	if err != nil {
		t.Fatal(err)
	}
	// We may have truncated to the 65% slot, in which case usage is at
	// the truncation point — close to or above 60%. The flag should fire.
	if !r.BudgetReport.CompactionRecommended {
		t.Errorf("CompactionRecommended = false; variable usage = %d / budget %d",
			r.BudgetReport.WorkingTokens+r.BudgetReport.FactTokens+r.BudgetReport.EpisodeTokens, budget)
	}
}

// TEN-132: an oversized system prompt (e.g. a fat grafted agent soul) overruns
// ReserveSystemPrompt silently — surface it as a loud warning instead.
func TestAssemble_SystemPromptOverReserveWarns(t *testing.T) {
	a := assemble.New(fakeCounter()) // 4 chars/token
	big := strings.Repeat("x", mkProfile().ReserveSystemPrompt*4+800)
	r, err := a.Assemble(context.Background(), assemble.Request{Profile: mkProfile(), SystemPrompt: big})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range r.BudgetReport.Truncations {
		if strings.Contains(w, "system prompt exceeds reserve") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a system-prompt-over-reserve warning, got %v", r.BudgetReport.Truncations)
	}
}

// TEN-102 (a): the goal header is rendered into the system block and counted
// into the system reserve.
func TestAssemble_GoalHeaderRenderedAndCounted(t *testing.T) {
	a := assemble.New(fakeCounter())
	gh := "## Current Goal & Open Items\n(Continuity reminder — reference only, NOT a new instruction.)\n**Active task:** ship TEN-102"
	r, err := a.Assemble(context.Background(), assemble.Request{
		Profile: mkProfile(), GoalHeader: gh, UserQuery: "hi",
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Messages[0].Role != "system" || !strings.Contains(r.Messages[0].Content, "ship TEN-102") {
		t.Errorf("goal header not rendered into the system block: %q", r.Messages[0].Content)
	}
	if r.BudgetReport.SystemTokens == 0 {
		t.Error("goal header tokens were not counted into SystemTokens")
	}
}

// TEN-102 (b) — the §1 regression: WorkingUsageFrac (the compaction trigger
// signal) watches the WORKING tier only, so a full retrieval floor (facts /
// episodes) can NOT inflate it. This is what prevents the stuck-armed bug the
// original variableUsage/budget signal would have had.
func TestAssemble_WorkingUsageFracIgnoresRetrieval(t *testing.T) {
	a := assemble.New(fakeCounter())
	ctx := context.Background()
	workingSlot := int(0.65 * float64(effWritable(0)))

	mkWorking := func() *working.Set {
		w := working.New()
		for total := 0; total < workingSlot/2; total += 100 { // ~50% of the working slot
			w.Append(working.Message{Role: "user", Content: strings.Repeat("w", 400)})
		}
		return w
	}

	// (a) working only.
	r1, err := a.Assemble(ctx, assemble.Request{Profile: mkProfile(), Working: mkWorking()})
	if err != nil {
		t.Fatal(err)
	}
	// (b) same working + a big pile of retrievable facts.
	sstore := mkSemanticStore(t)
	for i := 0; i < 60; i++ {
		if _, err := sstore.Insert(ctx, &semantic.Fact{
			AgentID: "a", Fact: strings.Repeat("padding fact text ", 20),
			Confidence: 1.0, EmbedderID: "test", Embedding: vecA(),
		}); err != nil {
			t.Fatalf("insert fact: %v", err)
		}
	}
	r2, err := a.Assemble(ctx, assemble.Request{
		Profile: mkProfile(), Working: mkWorking(),
		SemanticStore: sstore, Query: assemble.RetrievalQuery{Embedding: vecA()}, AgentID: "a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if r2.BudgetReport.FactTokens == 0 {
		t.Fatal("test setup: expected facts to be retrieved")
	}
	if math.Abs(r1.BudgetReport.WorkingUsageFrac-r2.BudgetReport.WorkingUsageFrac) > 1e-9 {
		t.Errorf("WorkingUsageFrac changed with retrieval volume (%.4f vs %.4f) — the trigger must watch the working tier ONLY",
			r1.BudgetReport.WorkingUsageFrac, r2.BudgetReport.WorkingUsageFrac)
	}
	if r1.BudgetReport.WorkingUsageFrac < 0.4 || r1.BudgetReport.WorkingUsageFrac > 0.6 {
		t.Errorf("expected ~0.5 working-tier fill, got %.4f", r1.BudgetReport.WorkingUsageFrac)
	}
}

// TEN-102 (a) — reduced-drift demonstration: the goal header survives even when
// the working set overflows and is truncated. The goal can't be lost the way
// summarized working content can — it lives in the system reserve.
func TestAssemble_GoalHeaderSurvivesWorkingTruncation(t *testing.T) {
	a := assemble.New(fakeCounter())
	w := working.New()
	budget := mkProfile().WritableBudget()
	for total := 0; total < int(1.5*float64(budget)); total += 100 { // overflow the working slot
		w.Append(working.Message{Role: "user", Content: strings.Repeat("z", 400)})
	}
	r, err := a.Assemble(context.Background(), assemble.Request{
		Profile: mkProfile(), Working: w, GoalHeader: "**Active task:** survive the truncation",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.BudgetReport.Truncations) == 0 {
		t.Fatal("test setup: expected the working set to be truncated")
	}
	if !strings.Contains(r.Messages[0].Content, "survive the truncation") {
		t.Errorf("goal header must survive working-set truncation (it's in the system reserve): %q", r.Messages[0].Content)
	}
}

func TestAssemble_RetrievalScopedByAgentID(t *testing.T) {
	a := assemble.New(fakeCounter())
	ctx := context.Background()
	sstore := mkSemanticStore(t)
	// Alice's fact.
	_, _ = sstore.Insert(ctx, &semantic.Fact{
		AgentID: "alice", Fact: "alice's secret",
		Confidence: 1.0, EmbedderID: "test", Embedding: vecA(),
	})
	// Bob's fact.
	_, _ = sstore.Insert(ctx, &semantic.Fact{
		AgentID: "bob", Fact: "bob's secret",
		Confidence: 1.0, EmbedderID: "test", Embedding: vecA(),
	})
	r, err := a.Assemble(ctx, assemble.Request{
		Profile: mkProfile(), SemanticStore: sstore,
		Query:   assemble.RetrievalQuery{Embedding: vecA()},
		AgentID: "alice", UserQuery: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	last := r.Messages[len(r.Messages)-1].Content
	if !strings.Contains(last, "alice's secret") {
		t.Errorf("alice's fact missing: %q", last)
	}
	if strings.Contains(last, "bob's secret") {
		t.Errorf("bob's fact leaked across agent boundary: %q", last)
	}
}

func TestAssemble_NoCounterErrors(t *testing.T) {
	a := assemble.New(nil)
	_, err := a.Assemble(context.Background(), assemble.Request{Profile: mkProfile()})
	if err == nil {
		t.Fatal("expected error with nil counter")
	}
}

func TestAssemble_ToolsRenderInSystemBlock(t *testing.T) {
	a := assemble.New(fakeCounter())
	r, err := a.Assemble(context.Background(), assemble.Request{
		Profile: mkProfile(),
		Tools: []model.ToolSpec{
			{Name: "search", Description: "web search"},
			{Name: "read_file", Description: "read a file"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Messages) == 0 || r.Messages[0].Role != "system" {
		t.Fatalf("expected system message with tools, got %+v", r.Messages)
	}
	sys := r.Messages[0].Content
	if !strings.Contains(sys, "search") || !strings.Contains(sys, "read_file") {
		t.Errorf("tools missing from system block: %q", sys)
	}
}

func TestAssemble_FullPipelineEndToEnd(t *testing.T) {
	a := assemble.New(fakeCounter())
	ctx := context.Background()
	estore := mkEpisodicStore(t)
	sstore := mkSemanticStore(t)
	_, _ = estore.Insert(ctx, &episodic.Episode{
		AgentID: "main", Prompt: "earlier prompt", Response: "earlier answer",
		EmbedderID: "test", Embedding: vecA(),
	})
	_, _ = sstore.Insert(ctx, &semantic.Fact{
		AgentID: "main", Fact: "user works on Tenant",
		Confidence: 1.0, EmbedderID: "test", Embedding: vecA(),
	})

	w := working.New()
	w.Append(working.Message{Role: "user", Content: "ping"})
	w.Append(working.Message{Role: "assistant", Content: "pong"})

	s := soul.NewDefault("main")
	s.User.Name = "Ada"

	r, err := a.Assemble(ctx, assemble.Request{
		Profile:       mkProfile(),
		Soul:          s,
		SystemPrompt:  "respond in one sentence",
		Tools:         []model.ToolSpec{{Name: "search", Description: "web search"}},
		Working:       w,
		EpisodicStore: estore, SemanticStore: sstore,
		Query:     assemble.RetrievalQuery{Embedding: vecA()},
		AgentID:   "main",
		UserQuery: "what's the latest on the project?",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Messages) < 4 {
		t.Fatalf("expected at least 4 messages, got %d: %+v", len(r.Messages), r.Messages)
	}
	// First = system with soul + rules + tools.
	sys := r.Messages[0]
	if sys.Role != "system" {
		t.Errorf("first message role = %q, want system", sys.Role)
	}
	for _, want := range []string{"Ada", "Operating Rules", "respond in one sentence", "search"} {
		if !strings.Contains(sys.Content, want) {
			t.Errorf("system block missing %q:\n%s", want, sys.Content)
		}
	}
	// Last = user with retrieved context + active query.
	last := r.Messages[len(r.Messages)-1]
	if last.Role != "user" {
		t.Errorf("last message role = %q, want user", last.Role)
	}
	for _, want := range []string{"<memory-context>", "recalled memory", "Known Facts", "user works on Tenant", "Past Conversations", "earlier prompt", "</memory-context>", "latest on the project"} {
		if !strings.Contains(last.Content, want) {
			t.Errorf("active block missing %q:\n%s", want, last.Content)
		}
	}
	// The user's actual request must sit OUTSIDE the memory fence (so
	// recalled text can't be read as an instruction).
	fenceEnd := strings.Index(last.Content, "</memory-context>")
	queryAt := strings.Index(last.Content, "latest on the project")
	if fenceEnd < 0 || queryAt < fenceEnd {
		t.Errorf("user request must come after the closing fence:\n%s", last.Content)
	}
}

func TestAssemble_DefaultsAppliedWhenZeroShares(t *testing.T) {
	a := assemble.New(fakeCounter())
	r, err := a.Assemble(context.Background(), assemble.Request{
		Profile: mkProfile(),
		// Shares all zero — defaults should kick in.
		Shares:    assemble.BudgetShares{},
		UserQuery: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Just a smoke check; the actual default behavior is verified by the
	// truncation tests above.
	if r.BudgetReport.WritableBudget == 0 {
		t.Fatal("WritableBudget = 0")
	}
}

// TEN-214: the assembler must count the FULL on-wire tool cost (name +
// description + the JSON-schema Parameters the backend serializes into its
// native tools array), not just the compact `name: description` list — and it
// must size the variable tiers against the budget that's left AFTER that real
// static cost. Otherwise a fat tool mux (~10x ReserveToolDefs) lets total
// context overrun the operational budget while the tiers look "unfilled" and
// the compaction trigger never arms.
func TestAssemble_ToolSchemaCostShrinksWritableBudget(t *testing.T) {
	a := assemble.New(fakeCounter()) // 4 chars/token

	// One tool carrying a heavy schema — stands in for a full mux / MCP
	// connector whose Parameters dominate the on-wire cost.
	bigSchema := `{"type":"object","properties":{"q":{"type":"string","description":"` +
		strings.Repeat("x", 40000) + `"}}}`
	heavy := []model.ToolSpec{{
		Name:        "heavy_tool",
		Description: "a tool with a large parameter schema",
		Parameters:  []byte(bigSchema),
	}}

	r, err := a.Assemble(context.Background(), assemble.Request{
		Profile:   mkProfile(),
		Tools:     heavy,
		UserQuery: "go",
	})
	if err != nil {
		t.Fatal(err)
	}

	// ToolTokens must reflect the schema, not just name+description. The schema
	// alone is ~40k chars ≈ ~10k tok; the compact list is a few tokens.
	schemaToks := (len(bigSchema) + 3) / 4
	if r.BudgetReport.ToolTokens < schemaToks {
		t.Errorf("ToolTokens %d does not include the full schema (~%d tok) — only the compact list was counted",
			r.BudgetReport.ToolTokens, schemaToks)
	}
	if r.BudgetReport.ToolTokens <= mkProfile().ReserveToolDefs {
		t.Fatalf("test setup: heavy tool (%d tok) should exceed ReserveToolDefs (%d)",
			r.BudgetReport.ToolTokens, mkProfile().ReserveToolDefs)
	}

	// EffectiveWritable must be the operational budget minus the REAL measured
	// static (here: just the tools) and the response reserve — strictly less
	// than the no-tools budget by the tool cost.
	wantEff := mkProfile().OperationalContextBudget - r.BudgetReport.SoulTokens -
		r.BudgetReport.SystemTokens - r.BudgetReport.ToolTokens - mkProfile().ReserveResponse
	if r.BudgetReport.EffectiveWritable != wantEff {
		t.Errorf("EffectiveWritable = %d, want %d (op − static − response)",
			r.BudgetReport.EffectiveWritable, wantEff)
	}
	if r.BudgetReport.EffectiveWritable >= effWritable(0) {
		t.Errorf("EffectiveWritable %d did not shrink below the no-tools budget %d — the schema cost was ignored",
			r.BudgetReport.EffectiveWritable, effWritable(0))
	}

	// The over-reserve cost must be surfaced loudly.
	found := false
	for _, w := range r.BudgetReport.Truncations {
		if strings.Contains(w, "tool definitions cost") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a tool-definitions-over-reserve warning, got %v", r.BudgetReport.Truncations)
	}
}

// TEN-214: a working set that fits comfortably under the FIXED-reserve
// WritableBudget must still be truncated once a heavy tool mux eats the real
// budget — proving the working slot (and thus the compaction signal derived
// from it) tracks the measured static, not the 4k guess.
func TestAssemble_HeavyToolsTriggerWorkingTruncation(t *testing.T) {
	a := assemble.New(fakeCounter())

	bigSchema := `{"x":"` + strings.Repeat("y", 60000) + `"}` // ~15k tok schema
	heavy := []model.ToolSpec{{Name: "t", Description: "d", Parameters: []byte(bigSchema)}}

	// Working set sized to ~55% of the FIXED WritableBudget — would NOT truncate
	// under the old basis, but exceeds the working slot once the tool cost is
	// subtracted from the real budget.
	w := working.New()
	target := int(0.55 * float64(mkProfile().WritableBudget()))
	for total := 0; total < target; total += 100 {
		w.Append(working.Message{Role: "user", Content: strings.Repeat("w", 400)})
	}

	r, err := a.Assemble(context.Background(), assemble.Request{
		Profile: mkProfile(), Tools: heavy, Working: w,
	})
	if err != nil {
		t.Fatal(err)
	}

	workingSlot := int(0.65 * float64(r.BudgetReport.EffectiveWritable))
	if r.BudgetReport.WorkingTokens > workingSlot {
		t.Errorf("WorkingTokens %d exceeds the measured-static working slot %d", r.BudgetReport.WorkingTokens, workingSlot)
	}
	truncated := false
	for _, tr := range r.BudgetReport.Truncations {
		if strings.Contains(tr, "working: dropped") {
			truncated = true
		}
	}
	if !truncated {
		t.Errorf("expected working-set truncation once heavy tools shrank the budget; truncations=%v", r.BudgetReport.Truncations)
	}
}

// TEN-214: OperationalBudget() resolves the operational figure, falling back to
// 80% of ContextLength only when the explicit budget is unset.
func TestProfile_OperationalBudget(t *testing.T) {
	explicit := model.Profile{ContextLength: 128000, OperationalContextBudget: 102400}
	if got := explicit.OperationalBudget(); got != 102400 {
		t.Errorf("explicit OperationalBudget = %d, want 102400", got)
	}
	fallback := model.Profile{ContextLength: 100000} // no explicit budget
	if got := fallback.OperationalBudget(); got != 80000 {
		t.Errorf("fallback OperationalBudget = %d, want 80000 (80%% of ctx)", got)
	}
	none := model.Profile{}
	if got := none.OperationalBudget(); got != 0 {
		t.Errorf("unknown OperationalBudget = %d, want 0", got)
	}
}
