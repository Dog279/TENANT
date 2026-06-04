package toolfmt_test

import (
	"encoding/json"
	"strings"
	"testing"

	"tenant/internal/model"
	"tenant/internal/model/toolfmt"
)

func TestAdapterFor(t *testing.T) {
	cases := map[string]string{
		"qwen":     "qwen",
		"hermes":   "qwen", // alias
		"gemma":    "gemma",
		"llama":    "llama",
		"mistral":  "mistral",
		"openai":   "openai",
		"":         "openai", // empty falls back to openai
		"nonsense": "openai", // unknown falls back to openai
	}
	for input, wantName := range cases {
		t.Run(input, func(t *testing.T) {
			got := toolfmt.AdapterFor(input).Name()
			if got != wantName {
				t.Fatalf("AdapterFor(%q).Name() = %q, want %q", input, got, wantName)
			}
		})
	}
}

func TestOpenAI_PassThrough(t *testing.T) {
	a := toolfmt.OpenAI{}
	prompt, err := a.FormatToolPrompt([]model.ToolSpec{{Name: "f"}})
	if err != nil || prompt != "" {
		t.Fatalf("FormatToolPrompt = %q, %v; want empty", prompt, err)
	}
	calls, err := a.ParseToolCalls(`<tool_call>{"name":"x","arguments":{}}</tool_call>`)
	if err != nil || calls != nil {
		t.Fatalf("ParseToolCalls = %v, %v; OpenAI adapter should be a no-op", calls, err)
	}
}

// ---- Qwen ----

func TestQwen_FormatAndParseRoundtrip(t *testing.T) {
	q := toolfmt.Qwen{}
	tools := []model.ToolSpec{
		{Name: "search", Description: "web search", Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)},
	}
	prompt, err := q.FormatToolPrompt(tools)
	if err != nil {
		t.Fatalf("FormatToolPrompt: %v", err)
	}
	if !strings.Contains(prompt, "<tools>") || !strings.Contains(prompt, `"search"`) {
		t.Fatalf("prompt missing expected content: %q", prompt)
	}

	output := `Sure, let me look that up.
<tool_call>
{"name": "search", "arguments": {"q": "go agents"}}
</tool_call>`
	calls, err := q.ParseToolCalls(output)
	if err != nil {
		t.Fatalf("ParseToolCalls: %v", err)
	}
	if len(calls) != 1 || calls[0].Name != "search" {
		t.Fatalf("ParseToolCalls = %+v, want one search call", calls)
	}
	var args map[string]any
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatalf("args not valid JSON: %v", err)
	}
	if args["q"] != "go agents" {
		t.Fatalf("args[q] = %v, want \"go agents\"", args["q"])
	}
}

func TestQwen_ParseMultipleCalls(t *testing.T) {
	q := toolfmt.Qwen{}
	output := `<tool_call>{"name":"a","arguments":{}}</tool_call>
<tool_call>{"name":"b","arguments":{"x":1}}</tool_call>`
	calls, err := q.ParseToolCalls(output)
	if err != nil {
		t.Fatalf("ParseToolCalls: %v", err)
	}
	if len(calls) != 2 || calls[0].Name != "a" || calls[1].Name != "b" {
		t.Fatalf("ParseToolCalls = %+v, want [a, b]", calls)
	}
}

func TestQwen_ParseNoCallsReturnsNil(t *testing.T) {
	q := toolfmt.Qwen{}
	calls, err := q.ParseToolCalls("This is a plain text response with no tool calls.")
	if err != nil {
		t.Fatalf("ParseToolCalls: %v", err)
	}
	if calls != nil {
		t.Fatalf("ParseToolCalls = %+v, want nil (no calls, no error)", calls)
	}
}

func TestQwen_ParseMalformedJSON(t *testing.T) {
	q := toolfmt.Qwen{}
	_, err := q.ParseToolCalls(`<tool_call>{not valid json}</tool_call>`)
	if err == nil {
		t.Fatal("expected error on malformed tool_call JSON")
	}
}

func TestQwen_MissingNameIsError(t *testing.T) {
	q := toolfmt.Qwen{}
	_, err := q.ParseToolCalls(`<tool_call>{"arguments":{}}</tool_call>`)
	if err == nil {
		t.Fatal("expected error on missing name")
	}
}

// ---- Gemma ----

func TestGemma_FormatAndParse(t *testing.T) {
	g := toolfmt.Gemma{}
	tools := []model.ToolSpec{
		{Name: "search", Description: "web search", Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)},
	}
	prompt, err := g.FormatToolPrompt(tools)
	if err != nil {
		t.Fatalf("FormatToolPrompt: %v", err)
	}
	if !strings.Contains(prompt, "tool_code") || !strings.Contains(prompt, "search(q: string)") {
		t.Fatalf("prompt missing expected content: %q", prompt)
	}

	output := "Let me search.\n```tool_code\nsearch(q=\"hello world\")\n```"
	calls, err := g.ParseToolCalls(output)
	if err != nil {
		t.Fatalf("ParseToolCalls: %v", err)
	}
	if len(calls) != 1 || calls[0].Name != "search" {
		t.Fatalf("ParseToolCalls = %+v", calls)
	}
	var args map[string]any
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatalf("args invalid JSON: %v", err)
	}
	if args["q"] != "hello world" {
		t.Fatalf("args[q] = %v, want \"hello world\"", args["q"])
	}
}

func TestGemma_ParseMixedArgTypes(t *testing.T) {
	g := toolfmt.Gemma{}
	output := "```tool_code\nf(s=\"hi\", n=42, b=True, x=null)\n```"
	calls, err := g.ParseToolCalls(output)
	if err != nil {
		t.Fatalf("ParseToolCalls: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	var args map[string]any
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatalf("args invalid JSON: %v", err)
	}
	if args["s"] != "hi" {
		t.Errorf("s = %v, want hi", args["s"])
	}
	if args["n"].(float64) != 42 {
		t.Errorf("n = %v, want 42", args["n"])
	}
	if args["b"] != true {
		t.Errorf("b = %v, want true", args["b"])
	}
	if args["x"] != nil {
		t.Errorf("x = %v, want nil", args["x"])
	}
}

func TestGemma_NoCallsReturnsNil(t *testing.T) {
	g := toolfmt.Gemma{}
	calls, err := g.ParseToolCalls("Plain text, no tool_code block.")
	if err != nil || calls != nil {
		t.Fatalf("ParseToolCalls = %v, %v; want nil, nil", calls, err)
	}
}

// Gemma 4 in the wild: drops the ```tool_code fence, uses `key: value`
// (colon) args, writes long unquoted values with commas + parens, and a
// bare zero-arg call. The hardened parser must catch all of it.
func TestGemma_HardenedUnfencedColonAndLongArgs(t *testing.T) {
	g := toolfmt.Gemma{}
	out := "I will deploy the team now.\n" +
		"spawn_agent(role: researcher, task: Research the core pillars of MCP (the protocol). Cover skills (DX, integration, etc.) for adoption.)\n" +
		"spawn_agent(role: critic, task: Challenge the findings.)\n" +
		"team_await"
	calls, err := g.ParseToolCalls(out)
	if err != nil {
		t.Fatalf("ParseToolCalls: %v", err)
	}
	if len(calls) != 3 {
		t.Fatalf("got %d calls, want 3: %+v", len(calls), calls)
	}
	// First spawn: long task with commas + parens stays intact as one arg.
	var a map[string]any
	if err := json.Unmarshal(calls[0].Arguments, &a); err != nil {
		t.Fatalf("args invalid: %v", err)
	}
	if a["role"] != "researcher" {
		t.Errorf("role = %v", a["role"])
	}
	task, _ := a["task"].(string)
	if !strings.Contains(task, "DX, integration, etc.") || !strings.Contains(task, "(the protocol)") {
		t.Errorf("long task arg not preserved whole: %q", task)
	}
	// Bare zero-arg call recognized.
	if calls[2].Name != "team_await" {
		t.Errorf("third call = %q, want team_await", calls[2].Name)
	}
	var empty map[string]any
	if err := json.Unmarshal(calls[2].Arguments, &empty); err != nil || len(empty) != 0 {
		t.Errorf("team_await args = %s, want {}", calls[2].Arguments)
	}
}

// Gemma 4 also emits PAREN-LESS calls: `tool_name key=value` (e.g.
// `os_list_dir path=C:\Users\Ada`). Must parse, with the Windows path
// (backslashes, drive colon) preserved as the arg value.
func TestGemma_HardenedSpaceSeparatedNoParens(t *testing.T) {
	g := toolfmt.Gemma{}
	out := "I am proceeding now.\nos_list_dir path=C:\\Users\\Ada"
	calls, err := g.ParseToolCalls(out)
	if err != nil {
		t.Fatalf("ParseToolCalls: %v", err)
	}
	if len(calls) != 1 || calls[0].Name != "os_list_dir" {
		t.Fatalf("got %+v, want one os_list_dir call", calls)
	}
	var a map[string]any
	if err := json.Unmarshal(calls[0].Arguments, &a); err != nil {
		t.Fatalf("args invalid: %v", err)
	}
	if a["path"] != `C:\Users\Ada` {
		t.Errorf("path = %q, want C:\\Users\\Ada", a["path"])
	}
}

// Correct syntax with a Windows path: os_list_dir(path="C:\Users\Ada").
// The backslashes make the literal INVALID JSON (\U, \D), which used to
// make the whole call silently fail to parse. It must now parse, with the
// path preserved verbatim.
func TestGemma_WindowsPathInQuotedArg(t *testing.T) {
	g := toolfmt.Gemma{}
	out := `I'll look there now.` + "\n" + `os_list_dir(path="C:\Users\Ada")`
	calls, err := g.ParseToolCalls(out)
	if err != nil {
		t.Fatalf("ParseToolCalls: %v", err)
	}
	if len(calls) != 1 || calls[0].Name != "os_list_dir" {
		t.Fatalf("got %+v, want one os_list_dir call", calls)
	}
	// Arguments must be valid JSON and round-trip the path.
	var a map[string]any
	if err := json.Unmarshal(calls[0].Arguments, &a); err != nil {
		t.Fatalf("args not valid JSON: %v (%s)", err, calls[0].Arguments)
	}
	if a["path"] != `C:\Users\Ada` {
		t.Errorf("path = %q, want C:\\Users\\Ada", a["path"])
	}
}

// Gemma 4 also wraps calls in chat-template markers with a `call:` prefix
// and single-quoted args: <|tool_call>call:os_list_dir(path='C:\x')<tool_call|>.
// The wrappers + prefix must be stripped and the single-quoted path parsed.
func TestGemma_WrappedToolCallMarkers(t *testing.T) {
	g := toolfmt.Gemma{}
	out := `<|tool_call>call:os_list_dir(path='C:\Users\Ada\Desktop')<tool_call|>`
	calls, err := g.ParseToolCalls(out)
	if err != nil {
		t.Fatalf("ParseToolCalls: %v", err)
	}
	if len(calls) != 1 || calls[0].Name != "os_list_dir" {
		t.Fatalf("got %+v, want one os_list_dir call", calls)
	}
	var a map[string]any
	if err := json.Unmarshal(calls[0].Arguments, &a); err != nil {
		t.Fatalf("args not valid JSON: %v (%s)", err, calls[0].Arguments)
	}
	if a["path"] != `C:\Users\Ada\Desktop` {
		t.Errorf("path = %q, want C:\\Users\\Ada\\Desktop", a["path"])
	}
}

// Prose with no actual calls (and capitalized words on their own lines)
// must NOT false-positive in the unfenced fallback.
func TestGemma_UnfencedProseNoFalsePositives(t *testing.T) {
	g := toolfmt.Gemma{}
	for _, s := range []string{
		"I will research this.\nDone.\nResearching now.",
		"Here is my plan:\nFirst, gather sources.\nThen synthesize.",
	} {
		if calls, err := g.ParseToolCalls(s); err != nil || calls != nil {
			t.Fatalf("prose %q parsed as calls: %+v (err %v)", s, calls, err)
		}
	}
}

// A "gemma" deployment that actually emits OpenAI-harmony channels: the tool
// call lives inside a commentary channel as `to=functions.NAME <|message|>{json}`.
func TestGemma_HarmonyChannelToolCall(t *testing.T) {
	g := toolfmt.Gemma{}
	out := `<|start|>assistant<|channel|>analysis<|message|>I should spawn a researcher.<|end|>` +
		`<|channel|>commentary to=functions.spawn_agent <|constrain|>json<|message|>` +
		`{"role":"researcher","task":"deep dive on Hermes"}<|call|>`
	calls, err := g.ParseToolCalls(out)
	if err != nil {
		t.Fatalf("ParseToolCalls: %v", err)
	}
	if len(calls) != 1 || calls[0].Name != "spawn_agent" {
		t.Fatalf("got %+v, want one spawn_agent call", calls)
	}
	var a map[string]any
	if err := json.Unmarshal(calls[0].Arguments, &a); err != nil {
		t.Fatalf("args not valid JSON: %v (%s)", err, calls[0].Arguments)
	}
	if a["role"] != "researcher" || a["task"] != "deep dive on Hermes" {
		t.Fatalf("args wrong: %+v", a)
	}
}

// Multiple harmony calls in one completion, each with its own JSON.
func TestGemma_HarmonyMultipleCalls(t *testing.T) {
	g := toolfmt.Gemma{}
	out := `<|channel|>commentary to=functions.team_send <|message|>{"to":"a","content":"hi"}<|call|>` +
		`<|channel|>commentary to=functions.team_await <|message|>{}<|call|>`
	calls, _ := g.ParseToolCalls(out)
	if len(calls) != 2 || calls[0].Name != "team_send" || calls[1].Name != "team_await" {
		t.Fatalf("got %+v, want team_send + team_await", calls)
	}
}

// Gemma.CleanText removes harmony tokens (well-formed and the malformed
// <|channel>thought<channel|> form the field model emits) so they don't leak
// into the displayed answer.
func TestGemma_CleanText(t *testing.T) {
	g := toolfmt.Gemma{}
	cases := []struct{ in, wantContains, wantAbsent string }{
		{"<|channel>thought<channel|>I'll write the files now.", "write the files now", "channel"},
		{"<|start|>assistant<|channel|>final<|message|>Here is the answer.<|return|>", "Here is the answer", "<|"},
		{"plain gemma output, no tokens", "plain gemma output", "<|"},
	}
	for _, c := range cases {
		got := g.CleanText(c.in)
		if !strings.Contains(got, c.wantContains) {
			t.Errorf("CleanText(%q) = %q, want it to contain %q", c.in, got, c.wantContains)
		}
		if strings.Contains(got, c.wantAbsent) {
			t.Errorf("CleanText(%q) = %q, should not contain %q", c.in, got, c.wantAbsent)
		}
	}
}

// Qwen.CleanText strips Qwen3/DeepSeek <think> reasoning blocks from the
// displayed answer (paired blocks and a lone close tag).
func TestQwen_CleanText(t *testing.T) {
	q := toolfmt.Qwen{}
	cases := []struct{ in, want string }{
		{"<think>let me reason about this</think>The answer is 42.", "The answer is 42."},
		{"<thinking>step 1\nstep 2</thinking>Done.", "Done."},
		{"reasoning without an open tag</think>Final answer.", "Final answer."},
		{"no reasoning here", "no reasoning here"},
	}
	for _, c := range cases {
		if got := q.CleanText(c.in); got != c.want {
			t.Errorf("CleanText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// And gemma's harmony cleaner must NOT touch <think> (separation of concerns).
	if got := (toolfmt.Gemma{}).CleanText("<think>x</think>ans"); got != "<think>x</think>ans" {
		t.Errorf("Gemma.CleanText should leave <think> alone, got %q", got)
	}
}

// Merged thinking models (aeon-ultimate et al.) sometimes emit ONLY a
// reasoning block when forced into a tools-off synthesis. Stripping it to
// "" cascaded into "(no result)" research reports because the agent loop
// treats empty text as "the model said nothing." CleanText must salvage
// the reasoning content instead of erasing it.
func TestQwen_CleanText_SalvagesAllThinkResponse(t *testing.T) {
	q := toolfmt.Qwen{}
	cases := []struct{ in, mustContain string }{
		{"<think>The answer is that NVDA closed at $850 yesterday.</think>", "NVDA closed at $850"},
		{"<thinking>Hundred Health is a telehealth platform focused on...</thinking>", "telehealth platform"},
		{"   <think>some reasoning</think>   ", "some reasoning"},
	}
	for _, c := range cases {
		got := q.CleanText(c.in)
		if got == "" {
			t.Errorf("CleanText(%q) returned empty — should salvage reasoning content", c.in)
		}
		if !strings.Contains(got, c.mustContain) {
			t.Errorf("CleanText(%q) = %q, want it to contain %q", c.in, got, c.mustContain)
		}
		// Salvage must not leak the tags themselves into the report.
		if strings.Contains(got, "<think>") || strings.Contains(got, "</think>") {
			t.Errorf("CleanText(%q) leaked tags: %q", c.in, got)
		}
	}
	// Truly-empty input still returns empty (not a salvage).
	if got := q.CleanText(""); got != "" {
		t.Errorf("empty input should stay empty, got %q", got)
	}
	if got := q.CleanText("   "); got != "" {
		t.Errorf("whitespace-only should normalize to empty, got %q", got)
	}
}

// Qwen.ParseToolCalls must accept the Hermes XML shape that NousResearch
// models — and merged Qwen variants like aeon-ultimate — emit when vLLM's
// hermes parser doesn't recognize the variant. This was a LIVE failure:
// research subagents emitted these XML blobs as their "final reports"
// because the agent loop saw no structured tool_calls and no parser fallback.
func TestQwen_ParseHermesXMLToolCall_Basic(t *testing.T) {
	const raw = `<tool_call>
<function=web_search>
<parameter=query>
"Hermes 4" model DGX Spark vLLM
</parameter>
</function>
</tool_call>`
	calls, err := toolfmt.Qwen{}.ParseToolCalls(raw)
	if err != nil {
		t.Fatalf("ParseToolCalls: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(calls))
	}
	if calls[0].Name != "web_search" {
		t.Errorf("name = %q, want %q", calls[0].Name, "web_search")
	}
	// Args must be valid JSON with the query string.
	var args map[string]any
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatalf("args not valid JSON: %v (%s)", err, calls[0].Arguments)
	}
	if q, _ := args["query"].(string); !strings.Contains(q, "Hermes 4") {
		t.Errorf("query lost: %+v", args)
	}
}

// Multiple parameters in one call — typed values (numbers) survive as numbers.
func TestQwen_ParseHermesXMLToolCall_MultipleParamsTypedNumber(t *testing.T) {
	const raw = `<tool_call>
<function=web_search>
<parameter=query>
"Hermes 4" vLLM
</parameter>
<parameter=k>
5
</parameter>
</function>
</tool_call>`
	calls, err := toolfmt.Qwen{}.ParseToolCalls(raw)
	if err != nil {
		t.Fatalf("ParseToolCalls: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(calls))
	}
	var args map[string]any
	_ = json.Unmarshal(calls[0].Arguments, &args)
	// k must be a number (so schema validation accepts integer-type slots),
	// not the string "5".
	switch v := args["k"].(type) {
	case float64:
		if v != 5 {
			t.Errorf("k = %v, want 5", v)
		}
	default:
		t.Errorf("k should be number, got %T (%v)", v, v)
	}
}

// Multiple <function=> blocks inside a single <tool_call> wrapper — extract
// each as its own call (aeon-ultimate occasionally batches them).
func TestQwen_ParseHermesXMLToolCall_MultipleCalls(t *testing.T) {
	const raw = `<tool_call>
<function=web_navigate>
<parameter=url>https://a.com</parameter>
</function>
<function=web_navigate>
<parameter=url>https://b.com</parameter>
</function>
</tool_call>`
	calls, err := toolfmt.Qwen{}.ParseToolCalls(raw)
	if err != nil {
		t.Fatalf("ParseToolCalls: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("want 2 calls, got %d", len(calls))
	}
	for i, c := range calls {
		if c.Name != "web_navigate" {
			t.Errorf("[%d] name = %q", i, c.Name)
		}
	}
}

// Tolerance: a malformed parameter (live observed: `<parameter=k=5\n</parameter>`
// where the name carries a stray `=`) must NOT discard the whole call — the
// rest of the parameters should still arrive.
func TestQwen_ParseHermesXMLToolCall_TolerantToBrokenParam(t *testing.T) {
	const raw = `<tool_call>
<function=web_search>
<parameter=k=5
</parameter>
<parameter=query>
Hermes 4 NVFP4 DGX
</parameter>
</function>
</tool_call>`
	calls, err := toolfmt.Qwen{}.ParseToolCalls(raw)
	if err != nil {
		t.Fatalf("ParseToolCalls: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(calls))
	}
	var args map[string]any
	_ = json.Unmarshal(calls[0].Arguments, &args)
	if q, _ := args["query"].(string); !strings.Contains(q, "Hermes 4") {
		t.Errorf("query lost despite malformed sibling param: %+v", args)
	}
}

// Tolerance: a missing </tool_call> closing tag (truncation) — still parse
// the inner <function=> match. Live observed in researcher-7's finding.
func TestQwen_ParseHermesXMLToolCall_MissingOuterClose(t *testing.T) {
	const raw = `<tool_call>
<function=web_navigate>
<parameter=url>
https://example.com/x
</parameter>
</function>`
	calls, err := toolfmt.Qwen{}.ParseToolCalls(raw)
	if err != nil {
		t.Fatalf("ParseToolCalls: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(calls))
	}
	if calls[0].Name != "web_navigate" {
		t.Errorf("name = %q", calls[0].Name)
	}
}

// JSON shape still works after the XML fallback was added — guard against
// the dispatch deciding "XML" on a JSON-shape call.
func TestQwen_ParseHermesXML_DoesNotBreakJSONShape(t *testing.T) {
	const raw = `<tool_call>{"name":"web_search","arguments":{"query":"go"}}</tool_call>`
	calls, err := toolfmt.Qwen{}.ParseToolCalls(raw)
	if err != nil {
		t.Fatalf("ParseToolCalls: %v", err)
	}
	if len(calls) != 1 || calls[0].Name != "web_search" {
		t.Errorf("JSON path broken: %+v", calls)
	}
}

// StripToolCalls must remove BOTH JSON-shape and Hermes XML blocks. Used by
// the vLLM safety net after it extracts calls from raw content — without
// this, the XML survived into resp.Text and ended up as the agent's "final
// answer" / a research finding (the original live bug).
func TestQwen_StripToolCalls(t *testing.T) {
	q := toolfmt.Qwen{}
	cases := []struct{ in, mustNotContain, mustContain string }{
		{
			in:             `Here we go:<tool_call>{"name":"web_search","arguments":{"q":"x"}}</tool_call> done`,
			mustNotContain: "<tool_call>",
			mustContain:    "Here we go:",
		},
		{
			in:             "<tool_call>\n<function=web_search>\n<parameter=q>\nx\n</parameter>\n</function>\n</tool_call>",
			mustNotContain: "<function=",
			mustContain:    "", // empty after strip is fine
		},
		{
			in:             "Pre-text <function=web_navigate><parameter=url>https://x</parameter></function> post-text",
			mustNotContain: "<function=",
			mustContain:    "post-text",
		},
	}
	for i, c := range cases {
		got := q.StripToolCalls(c.in)
		if strings.Contains(got, c.mustNotContain) {
			t.Errorf("[%d] StripToolCalls left %q in:\n%s", i, c.mustNotContain, got)
		}
		if c.mustContain != "" && !strings.Contains(got, c.mustContain) {
			t.Errorf("[%d] StripToolCalls removed %q from:\n%s", i, c.mustContain, got)
		}
	}
}

// Adapter interface contract: every concrete adapter implements StripToolCalls
// (no nil-pointer compile-time gap).
func TestStripToolCalls_AllAdaptersImplement(t *testing.T) {
	for _, fmt := range []string{"openai", "qwen", "hermes", "llama", "mistral", "gemma"} {
		a := toolfmt.AdapterFor(fmt)
		// Identity behavior on plain prose is fine for all.
		if got := a.StripToolCalls("plain prose"); got != "plain prose" {
			t.Errorf("%s: should be no-op on plain prose, got %q", fmt, got)
		}
	}
}

// Plain prose with neither shape → no calls, no error.
func TestQwen_ParseHermesXML_NoCallsReturnsNil(t *testing.T) {
	calls, err := toolfmt.Qwen{}.ParseToolCalls("just some prose, no tool calls here")
	if err != nil {
		t.Fatalf("plain prose errored: %v", err)
	}
	if calls != nil {
		t.Errorf("plain prose should return nil, got %+v", calls)
	}
}

// A normal gemma fenced call must still parse after the harmony additions.
func TestGemma_StillParsesPlainFencedAfterHarmony(t *testing.T) {
	g := toolfmt.Gemma{}
	out := "Let me search.\n```tool_code\nweb_navigate(url=\"https://x.com\")\n```"
	calls, _ := g.ParseToolCalls(out)
	if len(calls) != 1 || calls[0].Name != "web_navigate" {
		t.Fatalf("plain fenced call regressed: %+v", calls)
	}
}

// ---- Llama ----

func TestLlama_FormatAndParse(t *testing.T) {
	l := toolfmt.Llama{}
	tools := []model.ToolSpec{
		{Name: "search", Description: "web search", Parameters: json.RawMessage(`{}`)},
	}
	prompt, err := l.FormatToolPrompt(tools)
	if err != nil {
		t.Fatalf("FormatToolPrompt: %v", err)
	}
	if !strings.Contains(prompt, "python_tag") {
		t.Fatalf("prompt missing python_tag: %q", prompt)
	}

	output := `<|python_tag|>{"name":"search","parameters":{"q":"go"}}<|eom_id|>`
	calls, err := l.ParseToolCalls(output)
	if err != nil {
		t.Fatalf("ParseToolCalls: %v", err)
	}
	if len(calls) != 1 || calls[0].Name != "search" {
		t.Fatalf("ParseToolCalls = %+v", calls)
	}
	var args map[string]any
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatalf("args invalid: %v", err)
	}
	if args["q"] != "go" {
		t.Fatalf("args[q] = %v", args["q"])
	}
}

func TestLlama_AcceptsBothParametersAndArguments(t *testing.T) {
	l := toolfmt.Llama{}
	// Some Llama variants emit "arguments" (OpenAI convention) instead of "parameters".
	output := `<|python_tag|>{"name":"f","arguments":{"a":1}}<|eom_id|>`
	calls, err := l.ParseToolCalls(output)
	if err != nil {
		t.Fatalf("ParseToolCalls: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("got %d calls", len(calls))
	}
	var args map[string]any
	_ = json.Unmarshal(calls[0].Arguments, &args)
	if args["a"].(float64) != 1 {
		t.Fatalf("args[a] = %v", args["a"])
	}
}

func TestLlama_MissingNameIsError(t *testing.T) {
	l := toolfmt.Llama{}
	_, err := l.ParseToolCalls(`<|python_tag|>{"parameters":{}}<|eom_id|>`)
	if err == nil {
		t.Fatal("expected error on missing name")
	}
}

// ---- Mistral ----

func TestMistral_FormatAndParse(t *testing.T) {
	m := toolfmt.Mistral{}
	tools := []model.ToolSpec{
		{Name: "search", Description: "web search", Parameters: json.RawMessage(`{}`)},
	}
	prompt, err := m.FormatToolPrompt(tools)
	if err != nil {
		t.Fatalf("FormatToolPrompt: %v", err)
	}
	if !strings.Contains(prompt, "[TOOL_CALLS]") {
		t.Fatalf("prompt missing [TOOL_CALLS] marker: %q", prompt)
	}

	output := `[TOOL_CALLS] [{"name":"search","arguments":{"q":"go"}}]`
	calls, err := m.ParseToolCalls(output)
	if err != nil {
		t.Fatalf("ParseToolCalls: %v", err)
	}
	if len(calls) != 1 || calls[0].Name != "search" {
		t.Fatalf("ParseToolCalls = %+v", calls)
	}
}

func TestMistral_ParseMultipleInArray(t *testing.T) {
	m := toolfmt.Mistral{}
	output := `[TOOL_CALLS] [{"name":"a","arguments":{}},{"name":"b","arguments":{"x":1}}]`
	calls, err := m.ParseToolCalls(output)
	if err != nil {
		t.Fatalf("ParseToolCalls: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(calls))
	}
}

func TestMistral_NoMarkerReturnsNil(t *testing.T) {
	m := toolfmt.Mistral{}
	calls, err := m.ParseToolCalls("just text")
	if err != nil || calls != nil {
		t.Fatalf("ParseToolCalls = %v, %v; want nil, nil", calls, err)
	}
}

// ---- Empty tools ----

func TestAllAdapters_EmptyToolsReturnsEmptyPrompt(t *testing.T) {
	for _, a := range []toolfmt.Adapter{toolfmt.Qwen{}, toolfmt.Gemma{}, toolfmt.Llama{}, toolfmt.Mistral{}, toolfmt.OpenAI{}} {
		t.Run(a.Name(), func(t *testing.T) {
			p, err := a.FormatToolPrompt(nil)
			if err != nil || p != "" {
				t.Fatalf("%s.FormatToolPrompt(nil) = %q, %v; want empty", a.Name(), p, err)
			}
		})
	}
}
