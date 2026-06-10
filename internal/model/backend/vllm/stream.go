package vllm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"tenant/internal/model"
	"tenant/internal/model/toolfmt"
)

// SSE wire format from /v1/chat/completions when stream=true:
//
//	data: {"choices":[{"delta":{"content":"hel"}}]}\n
//	data: {"choices":[{"delta":{"content":"lo"}}]}\n
//	data: [DONE]\n
//
// Chunks may contain tool_call deltas; the assembled tool call is
// emitted as a single ToolCallDelta on the terminal chunk per OpenAI
// convention (vLLM follows it).

type streamChoice struct {
	Delta struct {
		Content   string `json:"content,omitempty"`
		ToolCalls []struct {
			Index    int    `json:"index"`
			ID       string `json:"id,omitempty"`
			Function struct {
				Name      string `json:"name,omitempty"`
				Arguments string `json:"arguments,omitempty"`
			} `json:"function"`
		} `json:"tool_calls,omitempty"`
	} `json:"delta"`
	FinishReason string `json:"finish_reason,omitempty"`
}

type streamFrame struct {
	Choices []streamChoice `json:"choices"`
	Usage   *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage,omitempty"`
}

// generateStream POSTs the same chat-completions request with stream=true
// and parses the SSE response into a channel of StreamChunks. The channel
// closes after the terminal chunk is emitted. On error, the terminal chunk
// carries Error set — callers MUST check it (this is the silent-failure
// gap flagged in the eng review).
func (b *Backend) generateStream(ctx context.Context, req model.GenerateRequest) (<-chan model.StreamChunk, error) {
	body := chatRequest{
		Model:       b.profile.Model,
		Messages:    toWireMessages(req.Messages),
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		Stop:        req.StopSequences,
		Stream:      true,
		GuidedJSON:  req.JSONSchema,
	}
	if len(req.Tools) > 0 {
		body.Tools = make([]chatTool, len(req.Tools))
		for i, t := range req.Tools {
			body.Tools[i] = chatTool{
				Type:     "function",
				Function: chatToolFunc{Name: t.Name, Description: t.Description, Parameters: t.Parameters},
			}
		}
		body.ToolChoice = toolChoiceWire(req.ToolChoice)
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal stream req: %v", model.ErrInternal, err)
	}
	url := strings.TrimRight(b.profile.Endpoint, "/") + b.chatPath()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("%w: build stream req: %v", model.ErrInternal, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if b.profile.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+b.profile.APIKey)
	}

	resp, err := b.client.Do(httpReq)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w: %v", model.ErrCancelled, err)
		}
		return nil, fmt.Errorf("%w: %v", model.ErrEndpointDown, err)
	}
	if resp.StatusCode >= 400 {
		err := classifyHTTPError(resp)
		_ = resp.Body.Close()
		return nil, err
	}

	out := make(chan model.StreamChunk, 16)
	go b.pumpSSE(ctx, resp, out)
	return out, nil
}

// pumpSSE reads SSE lines until [DONE] or transport error, then closes
// the channel. Errors are surfaced as a final chunk with Error set —
// per the eng review's "callers MUST check terminal Error" contract.
func (b *Backend) pumpSSE(ctx context.Context, resp *http.Response, out chan<- model.StreamChunk) {
	defer close(out)
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	// Accumulate text + track whether vLLM gave us a structured tool
	// call. If it didn't (e.g. Gemma emits tool calls as a ```tool_code```
	// text block, not OpenAI tool_calls), we run the same family-aware
	// safety-net parse Generate() uses, so streaming callers get the
	// same tool calls — otherwise the agent mistakes a tool call for a
	// final answer.
	var text strings.Builder
	// A single tool call is streamed across MANY frames (OpenAI
	// convention): the first frame carries function.name + id, later
	// frames carry argument FRAGMENTS with an empty name. We must
	// accumulate by index and emit complete calls at the end —
	// emitting per-frame would produce one real call plus a flood of
	// nameless fragments (→ "tool call missing name" + a malformed
	// assistant message that vLLM 400s on the next turn).
	type toolAcc struct {
		id, name string
		args     strings.Builder
	}
	accs := map[int]*toolAcc{}
	var order []int
	sawStructured := false
	var usage *model.Usage
	var finish string

	for sc.Scan() {
		if ctx.Err() != nil {
			select {
			case out <- model.StreamChunk{Error: fmt.Errorf("%w: %v", model.ErrCancelled, ctx.Err())}:
			default:
			}
			return
		}
		line := sc.Bytes()
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if bytes.Equal(payload, []byte("[DONE]")) {
			break
		}
		var fr streamFrame
		if err := json.Unmarshal(payload, &fr); err != nil {
			b.log.Debug("vllm: skipping malformed sse frame", "err", err)
			continue
		}
		if fr.Usage != nil {
			usage = &model.Usage{PromptTokens: fr.Usage.PromptTokens, CompletionTokens: fr.Usage.CompletionTokens, TotalTokens: fr.Usage.TotalTokens}
		}
		if len(fr.Choices) == 0 {
			continue // e.g. the terminal usage-only frame
		}
		ch := fr.Choices[0]
		if ch.FinishReason != "" {
			finish = ch.FinishReason
		}
		if ch.Delta.Content != "" {
			text.WriteString(ch.Delta.Content)
			select {
			case out <- model.StreamChunk{Delta: ch.Delta.Content}:
			case <-ctx.Done():
				select {
				case out <- model.StreamChunk{Error: fmt.Errorf("%w: %v", model.ErrCancelled, ctx.Err())}:
				default:
				}
				return
			}
		}
		for _, tc := range ch.Delta.ToolCalls {
			sawStructured = true
			a := accs[tc.Index]
			if a == nil {
				a = &toolAcc{}
				accs[tc.Index] = a
				order = append(order, tc.Index)
			}
			if tc.ID != "" {
				a.id = tc.ID
			}
			if tc.Function.Name != "" {
				a.name = tc.Function.Name
			}
			a.args.WriteString(tc.Function.Arguments)
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		// A cancel/deadline that fires while the scanner is blocked in a body
		// read surfaces here as a wrapped context error rather than via the
		// top-of-loop ctx.Err() check, so map it to ErrCancelled.
		streamErr := fmt.Errorf("%w: stream read: %v", model.ErrInternal, err)
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			streamErr = fmt.Errorf("%w: %v", model.ErrCancelled, err)
		}
		select {
		case out <- model.StreamChunk{Error: streamErr}:
		default:
		}
		return
	}

	// Emit the reassembled structured tool calls (complete, one each).
	for _, idx := range order {
		a := accs[idx]
		if a.name == "" {
			continue // pure-fragment with no name ever seen — drop
		}
		argsRaw := a.args.String()
		if argsRaw == "" {
			argsRaw = "{}"
		}
		select {
		case out <- model.StreamChunk{ToolCallDelta: &model.ToolCall{ID: a.id, Name: a.name, Arguments: normalizeToolArgs(json.RawMessage(argsRaw))}}:
		case <-ctx.Done():
			return
		}
	}
	// Gemma-style text tool calls (when vLLM returned no structured ones).
	b.emitTextToolCalls(ctx, out, text.String(), sawStructured)
	// Terminal chunk carries the finish reason + usage.
	if finish != "" || usage != nil {
		select {
		case out <- model.StreamChunk{FinishReason: finish, Usage: usage}:
		default:
		}
	}
}

// emitTextToolCalls runs the family adapter's text parse when vLLM
// returned no structured tool calls, emitting any found calls as
// terminal ToolCallDelta chunks (matches Generate()'s safety net).
func (b *Backend) emitTextToolCalls(ctx context.Context, out chan<- model.StreamChunk, text string, sawToolCall bool) {
	if sawToolCall || text == "" {
		return
	}
	parsed, err := toolfmt.AdapterFor(b.profile.ToolFormat).ParseToolCalls(text)
	if err != nil {
		b.log.Debug("vllm: stream tool-call safety-net parse failed", "format", b.profile.ToolFormat, "err", err)
		return
	}
	for i := range parsed {
		select {
		case out <- model.StreamChunk{ToolCallDelta: &parsed[i]}:
		case <-ctx.Done():
			return
		}
	}
}
