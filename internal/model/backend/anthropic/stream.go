package anthropic

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
)

// Anthropic streaming SSE event flow (data lines carry a "type"):
//
//	message_start        → usage.input_tokens
//	content_block_start  → block index + {type:text|tool_use, id, name}
//	content_block_delta  → {type:text_delta,text} | {type:input_json_delta,partial_json}
//	content_block_stop   → finalize the block at index
//	message_delta        → {delta:{stop_reason}}, usage.output_tokens
//	message_stop         → end
//
// We stream text deltas immediately, accumulate tool_use input JSON per block,
// and emit a complete ToolCallDelta when each tool block stops.

type sseEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`

	Message *struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message,omitempty"`

	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block,omitempty"`

	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta,omitempty"`

	Usage *struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage,omitempty"`
}

func (b *Backend) generateStream(ctx context.Context, req model.GenerateRequest) (<-chan model.StreamChunk, error) {
	body := b.buildRequest(req, true)
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal stream req: %v", model.ErrInternal, err)
	}
	url := strings.TrimRight(b.profile.Endpoint, "/") + "/v1/messages"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("%w: build stream req: %v", model.ErrInternal, err)
	}
	b.setHeaders(httpReq, "text/event-stream")

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

// toolAcc accumulates a streamed tool_use block.
type toolAcc struct {
	id, name string
	input    strings.Builder
}

func (b *Backend) pumpSSE(ctx context.Context, resp *http.Response, out chan<- model.StreamChunk) {
	defer close(out)
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	blocks := map[int]*toolAcc{} // tool_use blocks by index
	var usage model.Usage
	var finish string

	emit := func(c model.StreamChunk) bool {
		select {
		case out <- c:
			return true
		case <-ctx.Done():
			select {
			case out <- model.StreamChunk{Error: fmt.Errorf("%w: %v", model.ErrCancelled, ctx.Err())}:
			default:
			}
			return false
		}
	}

	for sc.Scan() {
		if ctx.Err() != nil {
			_ = emit(model.StreamChunk{Error: fmt.Errorf("%w: %v", model.ErrCancelled, ctx.Err())})
			return
		}
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue // skip `event:` and blank lines
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 {
			continue
		}
		var ev sseEvent
		if err := json.Unmarshal(payload, &ev); err != nil {
			b.log.Debug("anthropic: skipping malformed sse frame", "err", err)
			continue
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				usage.PromptTokens = ev.Message.Usage.InputTokens
			}
		case "content_block_start":
			if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
				blocks[ev.Index] = &toolAcc{id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
			}
		case "content_block_delta":
			if ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text != "" && !emit(model.StreamChunk{Delta: ev.Delta.Text}) {
					return
				}
			case "input_json_delta":
				if a := blocks[ev.Index]; a != nil {
					a.input.WriteString(ev.Delta.PartialJSON)
				}
			}
		case "content_block_stop":
			if a := blocks[ev.Index]; a != nil && a.name != "" {
				if !emit(model.StreamChunk{ToolCallDelta: &model.ToolCall{
					ID: a.id, Name: a.name, Arguments: normalizeInput(json.RawMessage(a.input.String())),
				}}) {
					return
				}
				delete(blocks, ev.Index)
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				finish = mapStopReason(ev.Delta.StopReason)
			}
			if ev.Usage != nil {
				usage.CompletionTokens = ev.Usage.OutputTokens
			}
		case "message_stop":
			// terminal handled after the loop
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		_ = emit(model.StreamChunk{Error: fmt.Errorf("%w: stream read: %v", model.ErrInternal, err)})
		return
	}

	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	var up *model.Usage
	if usage.PromptTokens != 0 || usage.CompletionTokens != 0 {
		up = &usage
	}
	if finish != "" || up != nil {
		_ = emit(model.StreamChunk{FinishReason: finish, Usage: up})
	}
}
