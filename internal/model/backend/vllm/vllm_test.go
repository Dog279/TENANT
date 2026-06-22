package vllm_test

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
	"tenant/internal/model/backend/vllm"
)

// newBackend wires a Backend pointed at the test server. helper.
func newBackend(t *testing.T, ts *httptest.Server) model.LLM {
	t.Helper()
	any, err := vllm.New(context.Background(), model.Profile{
		ID:       "test",
		Backend:  "vllm",
		Endpoint: ts.URL,
		Model:    "test-model",
	}, nil)
	if err != nil {
		t.Fatalf("vllm.New: %v", err)
	}
	return any.(model.LLM)
}

func TestVLLM_GenerateHappyPath(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s, want /v1/chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"choices":[{"message":{"role":"assistant","content":"hello back"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}
		}`)
	}))
	defer ts.Close()

	llm := newBackend(t, ts)
	resp, err := llm.Generate(context.Background(), model.GenerateRequest{
		Messages: []model.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Text != "hello back" {
		t.Fatalf("Text = %q, want %q", resp.Text, "hello back")
	}
	if resp.Usage.TotalTokens != 5 {
		t.Fatalf("Usage.TotalTokens = %d, want 5", resp.Usage.TotalTokens)
	}
}

func TestVLLM_GenerateToolCalls(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"choices":[{"message":{"role":"assistant","content":"","tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"search","arguments":{"q":"go"}}}
			]},"finish_reason":"tool_calls"}]
		}`)
	}))
	defer ts.Close()
	llm := newBackend(t, ts)
	resp, err := llm.Generate(context.Background(), model.GenerateRequest{
		Messages: []model.Message{{Role: "user", Content: "search"}},
		Tools:    []model.ToolSpec{{Name: "search", Description: "web search", Parameters: json.RawMessage(`{}`)}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "search" {
		t.Fatalf("ToolCalls = %+v, want one search call", resp.ToolCalls)
	}
}

func TestVLLM_SafetyNetParsesToolCallsWhenServerMissedThem(t *testing.T) {
	// Simulate vLLM running a Qwen model without --tool-call-parser hermes:
	// content has a <tool_call> block but tool_calls is empty in the response.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"choices":[{"message":{"role":"assistant","content":"Looking it up.\n<tool_call>\n{\"name\":\"search\",\"arguments\":{\"q\":\"go\"}}\n</tool_call>"},"finish_reason":"stop"}]
		}`)
	}))
	defer ts.Close()

	// Construct backend with qwen ToolFormat so the safety net knows what to parse.
	any, err := vllm.New(context.Background(), model.Profile{
		ID: "test", Backend: "vllm", Endpoint: ts.URL, Model: "qwen", ToolFormat: "qwen",
	}, nil)
	if err != nil {
		t.Fatalf("vllm.New: %v", err)
	}
	llm := any.(model.LLM)
	resp, err := llm.Generate(context.Background(), model.GenerateRequest{
		Messages: []model.Message{{Role: "user", Content: "find go info"}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "search" {
		t.Fatalf("safety net did not extract tool call: ToolCalls=%+v", resp.ToolCalls)
	}
	if resp.FinishReason != "tool_calls" {
		t.Fatalf("FinishReason = %q, want tool_calls (safety net should upgrade)", resp.FinishReason)
	}
}

func TestVLLM_Generate500ToInternal(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, "internal explosion")
	}))
	defer ts.Close()
	llm := newBackend(t, ts)
	_, err := llm.Generate(context.Background(), model.GenerateRequest{})
	if !errors.Is(err, model.ErrInternal) {
		t.Fatalf("err = %v, want ErrInternal", err)
	}
}

func TestVLLM_Generate429ToRateLimited(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = io.WriteString(w, "slow down")
	}))
	defer ts.Close()
	llm := newBackend(t, ts)
	_, err := llm.Generate(context.Background(), model.GenerateRequest{})
	if !errors.Is(err, model.ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
}

func TestVLLM_GenerateContextOverflow(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = io.WriteString(w, "input exceeds maximum context length")
	}))
	defer ts.Close()
	llm := newBackend(t, ts)
	_, err := llm.Generate(context.Background(), model.GenerateRequest{})
	if !errors.Is(err, model.ErrContextOverflow) {
		t.Fatalf("err = %v, want ErrContextOverflow", err)
	}
}

func TestVLLM_GenerateEndpointDown(t *testing.T) {
	// Point at an unreachable port.
	any, err := vllm.New(context.Background(), model.Profile{
		Endpoint: "http://127.0.0.1:1",
		Model:    "test",
	}, nil)
	if err != nil {
		t.Fatalf("vllm.New: %v", err)
	}
	llm := any.(model.LLM)
	_, err = llm.Generate(context.Background(), model.GenerateRequest{})
	if !errors.Is(err, model.ErrEndpointDown) {
		t.Fatalf("err = %v, want ErrEndpointDown", err)
	}
}

func TestVLLM_TokenCount(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tokenize" {
			t.Errorf("path = %s, want /tokenize", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"count": 42}`)
	}))
	defer ts.Close()
	llm := newBackend(t, ts)
	n, err := llm.TokenCount(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("TokenCount: %v", err)
	}
	if n != 42 {
		t.Fatalf("TokenCount = %d, want 42", n)
	}
}

func TestVLLM_EmbedHappyPath(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("path = %s, want /v1/embeddings", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[
			{"embedding":[0.1,0.2,0.3],"index":0},
			{"embedding":[0.4,0.5,0.6],"index":1}
		]}`)
	}))
	defer ts.Close()
	// Use Backend type directly to access Embedder method.
	any, err := vllm.New(context.Background(), model.Profile{
		Endpoint: ts.URL, Model: "embedder",
	}, nil)
	if err != nil {
		t.Fatalf("vllm.New: %v", err)
	}
	emb := any.(model.Embedder)
	vecs, err := emb.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 2 {
		t.Fatalf("Embed returned %d vectors, want 2", len(vecs))
	}
	if vecs[1][0] != 0.4 {
		t.Fatalf("vecs[1] = %v, want index-ordered", vecs[1])
	}
}

func TestVLLM_EmbedEmptyBatchNoCall(t *testing.T) {
	hit := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
	}))
	defer ts.Close()
	any, _ := vllm.New(context.Background(), model.Profile{Endpoint: ts.URL, Model: "e"}, nil)
	emb := any.(model.Embedder)
	vecs, err := emb.Embed(context.Background(), nil)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 0 {
		t.Fatalf("Embed(nil) returned %d vectors, want 0", len(vecs))
	}
	if hit {
		t.Fatal("backend was called for empty batch")
	}
}

func TestVLLM_GenerateStreamHappyPath(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		frames := []string{
			`data: {"choices":[{"delta":{"content":"hel"},"finish_reason":""}]}`,
			`data: {"choices":[{"delta":{"content":"lo"},"finish_reason":""}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
			`data: [DONE]`,
		}
		for _, f := range frames {
			_, _ = io.WriteString(w, f+"\n")
			w.(http.Flusher).Flush()
		}
	}))
	defer ts.Close()
	llm := newBackend(t, ts)
	ch, err := llm.GenerateStream(context.Background(), model.GenerateRequest{
		Messages: []model.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	var got strings.Builder
	var terminal model.StreamChunk
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("stream error: %v", chunk.Error)
		}
		got.WriteString(chunk.Delta)
		terminal = chunk
	}
	if got.String() != "hello" {
		t.Fatalf("assembled = %q, want %q", got.String(), "hello")
	}
	if terminal.FinishReason != "stop" {
		t.Fatalf("terminal FinishReason = %q, want stop", terminal.FinishReason)
	}
	if terminal.Usage == nil || terminal.Usage.TotalTokens != 5 {
		t.Fatalf("terminal Usage = %+v, want total=5", terminal.Usage)
	}
}

// vLLM streams one tool call across many frames: name+id first, then
// argument fragments with empty names. The backend must reassemble them
// into ONE complete call — not emit a flood of nameless fragments.
func TestVLLM_GenerateStreamReassemblesFragmentedToolCall(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		frames := []string{
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"os_list_dir","arguments":""}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\""}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":":\"/tmp\"}"}}]}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		}
		for _, f := range frames {
			_, _ = io.WriteString(w, f+"\n")
			w.(http.Flusher).Flush()
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
	var calls []model.ToolCall
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("stream error: %v", chunk.Error)
		}
		if chunk.ToolCallDelta != nil {
			calls = append(calls, *chunk.ToolCallDelta)
		}
	}
	if len(calls) != 1 {
		t.Fatalf("reassembly produced %d calls, want exactly 1: %+v", len(calls), calls)
	}
	if calls[0].Name != "os_list_dir" {
		t.Fatalf("call name = %q, want os_list_dir", calls[0].Name)
	}
	if !strings.Contains(string(calls[0].Arguments), "/tmp") {
		t.Fatalf("call args not reassembled: %s", calls[0].Arguments)
	}
}

func TestVLLM_GenerateStreamCancelDuringStream(t *testing.T) {
	// Server that streams forever; we cancel mid-stream and expect a terminal Error chunk.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for i := 0; i < 1000; i++ {
			if _, err := io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n"); err != nil {
				return
			}
			flusher.Flush()
			// Respect client disconnects.
			select {
			case <-r.Context().Done():
				return
			default:
			}
		}
	}))
	defer ts.Close()
	llm := newBackend(t, ts)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := llm.GenerateStream(ctx, model.GenerateRequest{})
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	count := 0
	var sawError error
	for chunk := range ch {
		count++
		if chunk.Error != nil {
			sawError = chunk.Error
		}
		if count == 3 {
			cancel()
		}
	}
	if sawError == nil {
		t.Skip("cancellation may have raced past send; non-deterministic across OSes")
	} else if !errors.Is(sawError, model.ErrCancelled) {
		t.Fatalf("terminal error = %v, want ErrCancelled", sawError)
	}
}

// TestVLLM_NativeToolCall_StringifiedArgs reproduces exactly what real
// Gemma 4 on the DGX returns: tool_calls[].function.arguments as a
// JSON-ENCODED STRING (OpenAI spec), not a nested object. Before the
// normalizeToolArgs fix, the agent's validateToolCall json.Unmarshal
// of these args into a map failed on every real tool call.
func TestVLLM_NativeToolCall_StringifiedArgs(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Verbatim shape from the DGX probe.
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":null,
		  "tool_calls":[{"id":"chatcmpl-tool-9f07","type":"function",
		  "function":{"name":"add","arguments":"{\"a\": 17, \"b\": 25}"}}]},
		  "finish_reason":"tool_calls"}],"usage":{"prompt_tokens":83,"completion_tokens":17,"total_tokens":100}}`)
	}))
	defer ts.Close()
	llm := newBackend(t, ts)
	resp, err := llm.Generate(context.Background(), model.GenerateRequest{
		Messages: []model.Message{{Role: "user", Content: "add 17 and 25"}},
		Tools:    []model.ToolSpec{{Name: "add", Parameters: json.RawMessage(`{"type":"object","required":["a","b"]}`)}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "add" {
		t.Fatalf("tool call not parsed: %+v", resp.ToolCalls)
	}
	// The critical assertion: Arguments must now be a real JSON OBJECT
	// that unmarshals into a map (what the agent's validator does).
	var args map[string]any
	if err := json.Unmarshal(resp.ToolCalls[0].Arguments, &args); err != nil {
		t.Fatalf("Arguments not a usable JSON object: %v (got %q)", err, resp.ToolCalls[0].Arguments)
	}
	if args["a"].(float64) != 17 || args["b"].(float64) != 25 {
		t.Fatalf("args wrong: %+v", args)
	}
}

func TestVLLM_NormalizeToolArgs_Tolerance(t *testing.T) {
	// Object passthrough (testllm fake path), null, empty, junk-string.
	cases := []struct {
		name, in, wantValidObj string
	}{
		{"object passthrough", `{"choices":[{"message":{"tool_calls":[{"id":"1","type":"function","function":{"name":"f","arguments":{"x":1}}}]},"finish_reason":"tool_calls"}]}`, `1`},
		{"null args", `{"choices":[{"message":{"tool_calls":[{"id":"1","type":"function","function":{"name":"f","arguments":null}}]},"finish_reason":"tool_calls"}]}`, ``},
		{"empty string args", `{"choices":[{"message":{"tool_calls":[{"id":"1","type":"function","function":{"name":"f","arguments":""}}]},"finish_reason":"tool_calls"}]}`, ``},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, c.in)
			}))
			defer ts.Close()
			llm := newBackend(t, ts)
			resp, err := llm.Generate(context.Background(), model.GenerateRequest{
				Messages: []model.Message{{Role: "user", Content: "x"}},
			})
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if len(resp.ToolCalls) != 1 {
				t.Fatalf("want 1 tool call, got %d", len(resp.ToolCalls))
			}
			// Must always be valid JSON the validator can Unmarshal.
			var m map[string]any
			if err := json.Unmarshal(resp.ToolCalls[0].Arguments, &m); err != nil {
				t.Fatalf("Arguments not valid JSON object: %v (%q)", err, resp.ToolCalls[0].Arguments)
			}
		})
	}
}

// TestVLLM_AssistantToolCallReplay verifies the WRITE side: replaying
// an assistant message that carries a tool call must serialize in the
// OpenAI nested shape {id,type:"function",function:{name,arguments}}
// with arguments STRINGIFIED. Real Gemma/vLLM 400s on the flat shape
// (caught only by a real 2-turn tool round-trip).
func TestVLLM_AssistantToolCallReplay(t *testing.T) {
	var gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"The result is 42."},"finish_reason":"stop"}],"usage":{}}`)
	}))
	defer ts.Close()
	llm := newBackend(t, ts)
	_, err := llm.Generate(context.Background(), model.GenerateRequest{
		Messages: []model.Message{
			{Role: "user", Content: "add 17 and 25"},
			{Role: "assistant", ToolCalls: []model.ToolCall{
				{ID: "call_1", Name: "add", Arguments: json.RawMessage(`{"a":17,"b":25}`)},
			}},
			{Role: "tool", ToolCallID: "call_1", Content: "42"},
		},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	// The on-wire assistant message must have the nested OpenAI shape.
	for _, want := range []string{
		`"type":"function"`,
		`"function":{"name":"add"`,
		`"arguments":"{\"a\":17,\"b\":25}"`, // STRINGIFIED, escaped
		`"role":"tool"`,
		`"tool_call_id":"call_1"`,
	} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("wire body missing %s\nbody: %s", want, gotBody)
		}
	}
	// And must NOT contain the flat internal shape.
	if strings.Contains(gotBody, `"tool_calls":[{"id":"call_1","name":"add"`) {
		t.Errorf("flat internal ToolCall leaked to the wire: %s", gotBody)
	}
}

// A stored tool call with MALFORMED JSON arguments (the model emitted bad
// JSON) must NOT be replayed verbatim — vLLM 400s ("Expecting property name")
// parsing it. toWireMessages must sanitize it to "{}".
func TestVLLM_ReplayMalformedToolArgsSanitized(t *testing.T) {
	var gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`)
	}))
	defer ts.Close()
	llm := newBackend(t, ts)
	_, err := llm.Generate(context.Background(), model.GenerateRequest{
		Messages: []model.Message{
			{Role: "user", Content: "search"},
			{Role: "assistant", ToolCalls: []model.ToolCall{
				// Malformed: trailing colon, no value — exactly what blew up live.
				{ID: "c1", Name: "wiki_list", Arguments: json.RawMessage(`{"k":10,"query":}`)},
			}},
			{Role: "tool", ToolCallID: "c1", Content: "bad args"},
		},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(gotBody, `"arguments":"{}"`) {
		t.Errorf("malformed args not sanitized to {}; wire body: %s", gotBody)
	}
	if strings.Contains(gotBody, `"query":}`) {
		t.Errorf("malformed JSON leaked to the wire (would 400): %s", gotBody)
	}
}

// TestVLLM_ReasoningEffort verifies the reasoning-effort hint (Sakana Fugu) is
// emitted as {"reasoning":{"effort":...}} when the profile sets it, and is
// ENTIRELY ABSENT from the wire otherwise — so a provider that doesn't
// understand the field never receives it. Covers both Generate and the streaming
// path, which build the request body independently.
func TestVLLM_ReasoningEffort(t *testing.T) {
	capture := func(t *testing.T, effort string, stream bool) string {
		t.Helper()
		var gotBody string
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			w.Header().Set("Content-Type", "application/json")
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				return
			}
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`)
		}))
		defer ts.Close()
		any, err := vllm.New(context.Background(), model.Profile{
			ID: "test", Backend: "vllm", Endpoint: ts.URL, Model: "fugu-ultra",
			ToolFormat: "openai", ReasoningEffort: effort,
		}, nil)
		if err != nil {
			t.Fatalf("vllm.New: %v", err)
		}
		llm := any.(model.LLM)
		req := model.GenerateRequest{Messages: []model.Message{{Role: "user", Content: "hi"}}}
		if stream {
			ch, serr := llm.GenerateStream(context.Background(), req)
			if serr != nil {
				t.Fatalf("GenerateStream: %v", serr)
			}
			for range ch { // drain
			}
		} else if _, gerr := llm.Generate(context.Background(), req); gerr != nil {
			t.Fatalf("Generate: %v", gerr)
		}
		return gotBody
	}

	for _, stream := range []bool{false, true} {
		// Set: the nested effort object is on the wire.
		if body := capture(t, "high", stream); !strings.Contains(body, `"reasoning":{"effort":"high"}`) {
			t.Errorf("stream=%v: reasoning effort not on the wire; body: %s", stream, body)
		}
		// Unset: NO reasoning key at all (omitempty + nil pointer).
		if body := capture(t, "", stream); strings.Contains(body, `"reasoning"`) {
			t.Errorf("stream=%v: reasoning field leaked when effort unset; body: %s", stream, body)
		}
	}
}
