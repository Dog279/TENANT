package toolfmt_test

import (
	"encoding/json"
	"strings"
	"testing"

	"tenant/internal/model"
	"tenant/internal/model/toolfmt"
)

// GLM-4.5+ writes a call as its name on the open tag's line and
// <arg_key>/<arg_value> pairs: strings bare, anything else JSON.
func TestGLM_FormatAndParseRoundtrip(t *testing.T) {
	g := toolfmt.GLM{}
	prompt, err := g.FormatToolPrompt([]model.ToolSpec{{Name: "get_weather", Description: "weather",
		Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<tools>\n", `"name":"get_weather"`, `"type":"function"`, "</tools>", "<arg_key>{arg-key-1}</arg_key>"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
	calls, err := g.ParseToolCalls("I'll check.\n<tool_call>get_weather\n<arg_key>city</arg_key>\n<arg_value>Beijing</arg_value>\n" +
		"<arg_key>days</arg_key>\n<arg_value>3</arg_value>\n<arg_key>units</arg_key>\n<arg_value>{\"temp\":\"c\"}</arg_value>\n</tool_call>")
	if err != nil || len(calls) != 1 {
		t.Fatalf("calls = %v, %v", calls, err)
	}
	if calls[0].Name != "get_weather" || string(calls[0].Arguments) != `{"city":"Beijing","days":3,"units":{"temp":"c"}}` {
		t.Errorf("call = %s %s", calls[0].Name, calls[0].Arguments)
	}
}

// Two calls, the last cut off by max_tokens: its complete pairs still count,
// and StripToolCalls removes both blocks and keeps the prose.
func TestGLM_MultipleCallsCutOffAndStrip(t *testing.T) {
	g := toolfmt.GLM{}
	text := "<tool_call>a\n<arg_key>x</arg_key>\n<arg_value>1</arg_value>\n</tool_call>\n" +
		"<tool_call>b\n<arg_key>y</arg_key>\n<arg_value>two words</arg_value>\n<arg_key>z</arg_key>\n<arg_value>cut of"
	calls, err := g.ParseToolCalls(text)
	if err != nil || len(calls) != 2 {
		t.Fatalf("calls = %v, %v", calls, err)
	}
	if calls[0].Name != "a" || string(calls[0].Arguments) != `{"x":1}` || calls[1].Name != "b" || string(calls[1].Arguments) != `{"y":"two words"}` {
		t.Errorf("calls = %s %s / %s %s", calls[0].Name, calls[0].Arguments, calls[1].Name, calls[1].Arguments)
	}
	if got := g.StripToolCalls("Done.\n" + text); got != "Done." {
		t.Errorf("StripToolCalls = %q", got)
	}
	if got := g.StripToolCalls("write a <tool_call> block to call a tool"); got != "write a <tool_call> block to call a tool" {
		t.Errorf("prose stripped: %q", got)
	}
}

// Prose is no call; the JSON shape is read as Qwen's; a JSON object after
// the name is the arguments; a <think> block is display-only.
func TestGLM_OtherShapesAndCleanText(t *testing.T) {
	g := toolfmt.GLM{}
	if calls, err := g.ParseToolCalls("write a <tool_call> block with <arg_key> tags to call a tool"); err != nil || calls != nil {
		t.Fatalf("prose: calls = %v, %v", calls, err)
	}
	calls, err := g.ParseToolCalls(`<tool_call>{"name":"add","arguments":{"a":2,"b":3}}</tool_call>`)
	if err != nil || len(calls) != 1 || calls[0].Name != "add" {
		t.Fatalf("json shape: %v, %v", calls, err)
	}
	calls, err = g.ParseToolCalls("<tool_call>add\n{\"a\": 2, \"b\": 3}\n</tool_call>")
	if err != nil || len(calls) != 1 || calls[0].Name != "add" || string(calls[0].Arguments) != `{"a": 2, "b": 3}` {
		t.Fatalf("name+json shape: %v, %v", calls, err)
	}
	if got := g.CleanText("<think>hmm</think>\nSunny."); got != "Sunny." {
		t.Errorf("CleanText = %q", got)
	}
}
