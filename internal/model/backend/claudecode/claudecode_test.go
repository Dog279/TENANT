package claudecode

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tenant/internal/model"
	"tenant/internal/model/toolfmt"
)

// --- result parsing -------------------------------------------------------

const successEnvelope = `{
  "type": "result", "subtype": "success", "is_error": false,
  "result": "Escalation answer.",
  "session_id": "sess-abc", "num_turns": 1, "duration_ms": 4200,
  "total_cost_usd": 0,
  "usage": {"input_tokens": 12, "output_tokens": 40,
            "cache_creation_input_tokens": 3000, "cache_read_input_tokens": 25000}
}`

func TestParseResultSuccess(t *testing.T) {
	res, err := parseResult(successEnvelope)
	if err != nil {
		t.Fatalf("parseResult: %v", err)
	}
	if res.Result != "Escalation answer." || res.SessionID != "sess-abc" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.Usage.CacheReadInputTokens != 25000 {
		t.Fatalf("usage not parsed: %+v", res.Usage)
	}
}

func TestParseResultToleratesLeadingNoise(t *testing.T) {
	res, err := parseResult("warning: some update notice\n" + successEnvelope)
	if err != nil {
		t.Fatalf("parseResult with noise: %v", err)
	}
	if res.Result != "Escalation answer." {
		t.Fatalf("unexpected result text %q", res.Result)
	}
}

func TestParseResultGarbage(t *testing.T) {
	if _, err := parseResult("not json at all"); !errors.Is(err, model.ErrInternal) {
		t.Fatalf("want ErrInternal, got %v", err)
	}
}

// --- response mapping (tool-call extraction) ------------------------------

func TestBuildResponsePlainText(t *testing.T) {
	res, _ := parseResult(successEnvelope)
	out, err := buildResponse(res, toolfmt.AdapterFor("qwen"))
	if err != nil {
		t.Fatalf("buildResponse: %v", err)
	}
	if out.FinishReason != "stop" || out.Text != "Escalation answer." {
		t.Fatalf("unexpected response: %+v", out)
	}
	// PromptTokens folds fresh + cache-read + cache-write input.
	if out.Usage.PromptTokens != 12+25000+3000 || out.Usage.CompletionTokens != 40 {
		t.Fatalf("usage mapping wrong: %+v", out.Usage)
	}
}

func TestBuildResponseExtractsQwenToolCalls(t *testing.T) {
	res := &cliResult{
		Result: "I'll check the ledger.\n<tool_call>\n{\"name\": \"memory_recall\", \"arguments\": {\"q\": \"burn rate\"}}\n</tool_call>",
	}
	out, err := buildResponse(res, toolfmt.AdapterFor("qwen"))
	if err != nil {
		t.Fatalf("buildResponse: %v", err)
	}
	if out.FinishReason != "tool_calls" || len(out.ToolCalls) != 1 {
		t.Fatalf("tool call not extracted: %+v", out)
	}
	if out.ToolCalls[0].Name != "memory_recall" {
		t.Fatalf("wrong tool: %+v", out.ToolCalls[0])
	}
	if strings.Contains(out.Text, "<tool_call>") {
		t.Fatalf("markup leaked into text: %q", out.Text)
	}
}

// --- prompt construction --------------------------------------------------

func TestBuildPromptSplitsSystemAndTranscript(t *testing.T) {
	req := model.GenerateRequest{
		Messages: []model.Message{
			{Role: "system", Content: "You are Tenant's planner."},
			{Role: "user", Content: "What should today look like?"},
			{Role: "assistant", Content: "Checking.", ToolCalls: []model.ToolCall{
				{ID: "call_0", Name: "list_tasks", Arguments: json.RawMessage(`{}`)},
			}},
			{Role: "tool", ToolCallID: "call_0", Content: "3 open tasks"},
		},
		Tools: []model.ToolSpec{{Name: "list_tasks", Description: "List tasks", Parameters: json.RawMessage(`{"type":"object"}`)}},
	}
	system, prompt, err := buildPrompt(req, toolfmt.AdapterFor("qwen"))
	if err != nil {
		t.Fatalf("buildPrompt: %v", err)
	}
	if !strings.Contains(system, "You are Tenant's planner.") || !strings.Contains(system, "<tools>") {
		t.Fatalf("system missing pieces:\n%s", system)
	}
	for _, want := range []string{"[user]", "[assistant]", "[assistant tool call call_0]", "[tool result for call_0]", "3 open tasks"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "planner") {
		t.Fatalf("system leaked into transcript:\n%s", prompt)
	}
}

// --- failure classification -----------------------------------------------

func TestClassifyCLIFailure(t *testing.T) {
	cases := []struct {
		stderr string
		want   error
	}{
		{"Claude usage limit reached. Your limit will reset at 6pm", model.ErrRateLimited},
		{"You've hit your weekly limit — try again at Tuesday 3am", model.ErrRateLimited},
		{"Credit balance too low", model.ErrInsufficientBalance},
		{"Not logged in. Please run /login", model.ErrInvalidRequest},
		{"segfault in flux capacitor", model.ErrInternal},
	}
	for _, c := range cases {
		got := classifyCLIFailure(c.stderr, "")
		if !errors.Is(got, c.want) {
			t.Errorf("classify(%q) = %v, want %v", c.stderr, got, c.want)
		}
	}
}

// --- env scrubbing --------------------------------------------------------

func TestScrubbedEnv(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=sk-live-oops",
		"ANTHROPIC_BASE_URL=https://api.z.ai/api/anthropic",
		"ANTHROPIC_AUTH_TOKEN=tok",
		"TENANT_LOG=debug",
	}
	out := scrubbedEnv(in)
	joined := strings.Join(out, "\n")
	for _, banned := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN"} {
		if strings.Contains(joined, banned) {
			t.Fatalf("%s survived scrub:\n%s", banned, joined)
		}
	}
	for _, kept := range []string{"PATH=/usr/bin", "TENANT_LOG=debug", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"} {
		if !strings.Contains(joined, kept) {
			t.Fatalf("%s missing after scrub:\n%s", kept, joined)
		}
	}
}

// --- binary discovery -----------------------------------------------------

func TestDiscoverBinaryExplicitPathMissing(t *testing.T) {
	_, err := discoverBinary(filepath.Join(t.TempDir(), "nope", "claude.exe"))
	if !errors.Is(err, model.ErrEndpointDown) {
		t.Fatalf("want ErrEndpointDown, got %v", err)
	}
}

func TestDiscoverBinaryExplicitPathFound(t *testing.T) {
	p := filepath.Join(t.TempDir(), "claude.exe")
	if err := os.WriteFile(p, []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := discoverBinary(p)
	if err != nil || got != p {
		t.Fatalf("discoverBinary(%q) = %q, %v", p, got, err)
	}
}

func TestVersionLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"2.1.221", "2.1.1000", true},
		{"2.1.1000", "2.1.221", false},
		{"2.1.221", "2.1.221", false},
		{"2.0.9", "2.1.0", true},
	}
	for _, c := range cases {
		if got := versionLess(c.a, c.b); got != c.want {
			t.Errorf("versionLess(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// --- ledger ---------------------------------------------------------------

func testLedger(t *testing.T, budget int64) *ledger {
	t.Helper()
	return &ledger{
		path:   filepath.Join(t.TempDir(), "usage.json"),
		budget: budget,
	}
}

func TestLedgerAccumulatesAndGuards(t *testing.T) {
	l := testLedger(t, 100)
	if l.overBudget() {
		t.Fatal("fresh ledger should not be over budget")
	}
	l.add(60)
	if l.overBudget() {
		t.Fatalf("60/100 should not trip the guard (spent=%d)", l.spent())
	}
	l.add(50)
	if !l.overBudget() {
		t.Fatalf("110/100 should trip the guard (spent=%d)", l.spent())
	}
}

func TestLedgerPrunesOldEntries(t *testing.T) {
	l := testLedger(t, 100)
	stale := ledgerFile{Entries: []ledgerEntry{
		{TS: time.Now().Add(-8 * 24 * time.Hour), Tokens: 999},
		{TS: time.Now().Add(-time.Hour), Tokens: 10},
	}}
	if err := l.save(stale); err != nil {
		t.Fatal(err)
	}
	if got := l.spent(); got != 10 {
		t.Fatalf("spent() = %d, want 10 (stale entry must not count)", got)
	}
	l.add(5) // triggers prune-on-write
	lf := l.load()
	if len(lf.Entries) != 2 {
		t.Fatalf("prune failed, entries = %+v", lf.Entries)
	}
}

func TestLedgerZeroBudgetNeverGuards(t *testing.T) {
	l := testLedger(t, 0)
	l.add(1 << 30)
	if l.overBudget() {
		t.Fatal("budget 0 means unlimited")
	}
}
