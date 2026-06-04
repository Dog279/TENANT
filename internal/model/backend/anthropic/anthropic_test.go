package anthropic_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tenant/internal/model"
	"tenant/internal/model/backend/anthropic"
)

func newBackend(t *testing.T, ts *httptest.Server) model.LLM {
	t.Helper()
	a, err := anthropic.New(context.Background(), model.Profile{
		ID: "test", Backend: "anthropic", Endpoint: ts.URL,
		Model: "claude-sonnet-4-20250514", APIKey: "sk-ant-test",
	}, nil)
	if err != nil {
		t.Fatalf("anthropic.New: %v", err)
	}
	return a.(model.LLM)
}

func TestNew_RequiresAPIKey(t *testing.T) {
	if _, err := anthropic.New(context.Background(), model.Profile{Endpoint: "x", Model: "m"}, nil); err == nil {
		t.Fatal("New should reject a missing API key")
	}
}

func TestGenerate_HappyPathAndHeaders(t *testing.T) {
	var gotPath, gotKey, gotVer, gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotVer = r.Header.Get("anthropic-version")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant",
			"content":[{"type":"text","text":"hello back"}],
			"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":3}}`)
	}))
	defer ts.Close()

	llm := newBackend(t, ts)
	resp, err := llm.Generate(context.Background(), model.GenerateRequest{
		Messages: []model.Message{
			{Role: "system", Content: "be terse"},
			{Role: "user", Content: "hi"},
		},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Text != "hello back" {
		t.Fatalf("Text = %q", resp.Text)
	}
	if resp.FinishReason != "stop" {
		t.Fatalf("FinishReason = %q, want stop (mapped from end_turn)", resp.FinishReason)
	}
	if resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 3 || resp.Usage.TotalTokens != 13 {
		t.Fatalf("Usage = %+v", resp.Usage)
	}
	if gotPath != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", gotPath)
	}
	if gotKey != "sk-ant-test" || gotVer == "" {
		t.Errorf("auth headers wrong: key=%q ver=%q", gotKey, gotVer)
	}
	// system collected to top-level; max_tokens always present.
	if !strings.Contains(gotBody, `"system":"be terse"`) {
		t.Errorf("system not lifted to top-level: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"max_tokens"`) {
		t.Errorf("max_tokens missing (Anthropic requires it): %s", gotBody)
	}
}

func TestGenerate_ToolUseParsed(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"m","type":"message","role":"assistant",
			"content":[{"type":"text","text":"let me check"},
			           {"type":"tool_use","id":"toolu_1","name":"search","input":{"q":"go"}}],
			"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":7}}`)
	}))
	defer ts.Close()
	llm := newBackend(t, ts)
	resp, err := llm.Generate(context.Background(), model.GenerateRequest{
		Messages: []model.Message{{Role: "user", Content: "search go"}},
		Tools:    []model.ToolSpec{{Name: "search", Description: "web", Parameters: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.FinishReason != "tool_calls" {
		t.Fatalf("FinishReason = %q, want tool_calls", resp.FinishReason)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "search" {
		t.Fatalf("ToolCalls = %+v", resp.ToolCalls)
	}
	var args map[string]any
	if err := json.Unmarshal(resp.ToolCalls[0].Arguments, &args); err != nil || args["q"] != "go" {
		t.Fatalf("tool args not usable: %v (%s)", err, resp.ToolCalls[0].Arguments)
	}
}

// The WRITE side: an assistant tool_use + a following tool result must
// serialize as Anthropic content blocks (tool_use in assistant, tool_result
// folded into a user message).
func TestGenerate_ToolResultRoundTrip(t *testing.T) {
	var gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"m","type":"message","role":"assistant",
			"content":[{"type":"text","text":"42"}],"stop_reason":"end_turn",
			"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer ts.Close()
	llm := newBackend(t, ts)
	_, err := llm.Generate(context.Background(), model.GenerateRequest{
		Messages: []model.Message{
			{Role: "user", Content: "add 17 and 25"},
			{Role: "assistant", ToolCalls: []model.ToolCall{
				{ID: "toolu_1", Name: "add", Arguments: json.RawMessage(`{"a":17,"b":25}`)},
			}},
			{Role: "tool", ToolCallID: "toolu_1", Content: "42"},
		},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for _, want := range []string{
		`"type":"tool_use"`,
		`"id":"toolu_1"`,
		`"input":{"a":17,"b":25}`,
		`"type":"tool_result"`,
		`"tool_use_id":"toolu_1"`,
	} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("wire body missing %s\nbody: %s", want, gotBody)
		}
	}
	// No OpenAI-style "role":"tool" or "tool_calls" should leak.
	if strings.Contains(gotBody, `"role":"tool"`) || strings.Contains(gotBody, `"tool_calls"`) {
		t.Errorf("OpenAI shape leaked into Anthropic body: %s", gotBody)
	}
}

func TestGenerate_ErrorMapping(t *testing.T) {
	cases := []struct {
		code int
		body string
		want error
	}{
		{429, "slow down", model.ErrRateLimited},
		{401, "bad key", model.ErrInvalidRequest},
		{400, "prompt is too long for the context window", model.ErrContextOverflow},
		{500, "boom", model.ErrInternal},
	}
	for _, c := range cases {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.code)
			_, _ = io.WriteString(w, c.body)
		}))
		llm := newBackend(t, ts)
		_, err := llm.Generate(context.Background(), model.GenerateRequest{})
		if !errors.Is(err, c.want) {
			t.Errorf("status %d → %v, want %v", c.code, err, c.want)
		}
		ts.Close()
	}
}

func TestGenerateStream_TextAndToolUse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		frames := []string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"usage":{"input_tokens":11,"output_tokens":0}}}`,
			`event: content_block_start`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hel"}}`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`,
			`data: {"type":"content_block_stop","index":0}`,
			`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_9","name":"ls"}}`,
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\""}}`,
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":":\"/tmp\"}"}}`,
			`data: {"type":"content_block_stop","index":1}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`,
			`data: {"type":"message_stop"}`,
		}
		for _, f := range frames {
			_, _ = io.WriteString(w, f+"\n")
			fl.Flush()
		}
	}))
	defer ts.Close()
	llm := newBackend(t, ts)
	ch, err := llm.GenerateStream(context.Background(), model.GenerateRequest{
		Messages: []model.Message{{Role: "user", Content: "ls /tmp"}},
	})
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	var text strings.Builder
	var calls []model.ToolCall
	var terminal model.StreamChunk
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("stream error: %v", chunk.Error)
		}
		text.WriteString(chunk.Delta)
		if chunk.ToolCallDelta != nil {
			calls = append(calls, *chunk.ToolCallDelta)
		}
		if chunk.FinishReason != "" || chunk.Usage != nil {
			terminal = chunk
		}
	}
	if text.String() != "hello" {
		t.Fatalf("text = %q, want hello", text.String())
	}
	if len(calls) != 1 || calls[0].Name != "ls" || !strings.Contains(string(calls[0].Arguments), "/tmp") {
		t.Fatalf("tool call reassembly wrong: %+v", calls)
	}
	if terminal.FinishReason != "tool_calls" {
		t.Fatalf("terminal FinishReason = %q, want tool_calls", terminal.FinishReason)
	}
	if terminal.Usage == nil || terminal.Usage.PromptTokens != 11 || terminal.Usage.CompletionTokens != 9 {
		t.Fatalf("terminal Usage = %+v", terminal.Usage)
	}
}

func TestTokenCount_Estimates(t *testing.T) {
	a, _ := anthropic.New(context.Background(), model.Profile{Endpoint: "x", Model: "m", APIKey: "k"}, nil)
	n, err := a.(model.LLM).TokenCount(context.Background(), "12345678") // 8 chars → ~2
	if err != nil || n != 2 {
		t.Fatalf("TokenCount = %d, %v; want 2", n, err)
	}
}
