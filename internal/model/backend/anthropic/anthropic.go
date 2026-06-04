// Package anthropic implements Tenant's model.LLM interface against the
// Anthropic Messages API (POST /v1/messages). It is NOT OpenAI-compatible —
// the wire shape differs enough (top-level `system`, content-block messages,
// tool_use / tool_result blocks, x-api-key auth, required max_tokens) that it
// gets its own backend rather than bending the vllm one.
//
// Embeddings are out of scope: Anthropic has no embeddings endpoint, so the
// embedder role stays on the local OpenAI-compatible server (Ollama). This
// backend implements LLM only.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"tenant/internal/model"
)

// anthropicVersion is the required API version header. Pinned; bump
// deliberately when adopting newer message features.
const anthropicVersion = "2023-06-01"

// defaultMaxTokens is used when the caller leaves MaxTokens unset. Anthropic
// REQUIRES max_tokens (unlike OpenAI), so we must always send a value.
const defaultMaxTokens = 8192

// Backend implements model.LLM against the Messages API.
type Backend struct {
	profile model.Profile
	client  *http.Client
	log     *slog.Logger
}

// New constructs a Backend. Signature matches model.BackendFactory.
func New(_ context.Context, p model.Profile, log *slog.Logger) (any, error) {
	if log == nil {
		log = slog.Default()
	}
	if p.APIKey == "" {
		return nil, fmt.Errorf("%w: anthropic backend requires an API key (set one via `tenant setup`)", model.ErrInvalidRequest)
	}
	return &Backend{
		profile: p,
		log:     log,
		client: &http.Client{
			Timeout: 0, // per-call via context
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}, nil
}

// ----- wire types -----

type messagesRequest struct {
	Model         string     `json:"model"`
	MaxTokens     int        `json:"max_tokens"`
	System        string     `json:"system,omitempty"`
	Messages      []wireMsg  `json:"messages"`
	Tools         []wireTool `json:"tools,omitempty"`
	ToolChoice    any        `json:"tool_choice,omitempty"`
	Temperature   float32    `json:"temperature,omitempty"`
	StopSequences []string   `json:"stop_sequences,omitempty"`
	Stream        bool       `json:"stream,omitempty"`
}

// wireMsg carries an array of content blocks (text / tool_use / tool_result).
type wireMsg struct {
	Role    string  `json:"role"` // user | assistant
	Content []block `json:"content"`
}

// block is one content block. Fields are populated per Type.
type block struct {
	Type string `json:"type"` // text | tool_use | tool_result

	// text
	Text string `json:"text,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	// Content for tool_result is a string here (Anthropic also accepts an
	// array of blocks; a string is sufficient for our tool outputs).
	ResultContent string `json:"content,omitempty"`
}

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type messagesResponse struct {
	ID         string  `json:"id"`
	Type       string  `json:"type"`
	Role       string  `json:"role"`
	Content    []block `json:"content"`
	StopReason string  `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// ----- LLM.Generate -----

func (b *Backend) Generate(ctx context.Context, req model.GenerateRequest) (*model.GenerateResponse, error) {
	body := b.buildRequest(req, false)
	var resp messagesResponse
	if err := b.postJSON(ctx, "/v1/messages", body, &resp); err != nil {
		return nil, err
	}
	out := &model.GenerateResponse{
		FinishReason: mapStopReason(resp.StopReason),
		Usage: model.Usage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	}
	var text strings.Builder
	for _, blk := range resp.Content {
		switch blk.Type {
		case "text":
			text.WriteString(blk.Text)
		case "tool_use":
			out.ToolCalls = append(out.ToolCalls, model.ToolCall{
				ID:        blk.ID,
				Name:      blk.Name,
				Arguments: normalizeInput(blk.Input),
			})
		}
	}
	out.Text = text.String()
	return out, nil
}

// GenerateStream is implemented in stream.go.
func (b *Backend) GenerateStream(ctx context.Context, req model.GenerateRequest) (<-chan model.StreamChunk, error) {
	return b.generateStream(ctx, req)
}

// TokenCount estimates via chars/4. Anthropic's count_tokens endpoint needs
// the full message payload (not bare text) and costs a round-trip; the
// budgeting layer only needs a stable estimate here.
func (b *Backend) TokenCount(_ context.Context, text string) (int, error) {
	n := len(text) / 4
	if n == 0 && len(text) > 0 {
		return 1, nil
	}
	return n, nil
}

// ----- request construction -----

func (b *Backend) buildRequest(req model.GenerateRequest, stream bool) messagesRequest {
	maxTok := req.MaxTokens
	if maxTok <= 0 {
		maxTok = defaultMaxTokens
	}
	out := messagesRequest{
		Model:         b.profile.Model,
		MaxTokens:     maxTok,
		System:        collectSystem(req.Messages),
		Messages:      toWireMessages(req.Messages),
		Temperature:   req.Temperature,
		StopSequences: req.StopSequences,
		Stream:        stream,
	}
	if len(req.Tools) > 0 {
		out.Tools = make([]wireTool, len(req.Tools))
		for i, t := range req.Tools {
			schema := t.Parameters
			if len(bytes.TrimSpace(schema)) == 0 {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			out.Tools[i] = wireTool{Name: t.Name, Description: t.Description, InputSchema: schema}
		}
		out.ToolChoice = toolChoiceWire(req.ToolChoice)
	}
	return out
}

// collectSystem joins all system-role messages into the top-level system
// prompt (Anthropic has no "system" message role).
func collectSystem(msgs []model.Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		if m.Role == "system" && m.Content != "" {
			if sb.Len() > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString(m.Content)
		}
	}
	return sb.String()
}

// toWireMessages converts internal messages to Anthropic's content-block
// shape, folding tool results into user messages and merging adjacent
// same-role messages (Anthropic requires user/assistant to alternate, and
// tool_result blocks live in a user turn following the assistant's tool_use).
func toWireMessages(msgs []model.Message) []wireMsg {
	var out []wireMsg
	appendBlocks := func(role string, blocks []block) {
		if len(blocks) == 0 {
			return
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			return
		}
		out = append(out, wireMsg{Role: role, Content: blocks})
	}

	for _, m := range msgs {
		switch m.Role {
		case "system":
			continue // handled by collectSystem
		case "tool":
			appendBlocks("user", []block{{
				Type:          "tool_result",
				ToolUseID:     m.ToolCallID,
				ResultContent: m.Content,
			}})
		case "assistant":
			var blocks []block
			if m.Content != "" {
				blocks = append(blocks, block{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, block{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Name,
					Input: normalizeInput(tc.Arguments),
				})
			}
			appendBlocks("assistant", blocks)
		default: // user (and anything unexpected)
			if m.Content != "" {
				appendBlocks("user", []block{{Type: "text", Text: m.Content}})
			}
		}
	}
	return out
}

// normalizeInput guarantees a JSON object for a tool_use input field. Empty /
// null / non-object payloads become {} so the wire stays valid.
func normalizeInput(raw json.RawMessage) json.RawMessage {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
		return json.RawMessage(`{}`)
	}
	if t[0] == '{' || t[0] == '[' {
		return t
	}
	// A stringified object (e.g. "{\"a\":1}") → unwrap.
	if t[0] == '"' {
		var s string
		if err := json.Unmarshal(t, &s); err == nil && json.Valid([]byte(s)) {
			return json.RawMessage(s)
		}
	}
	return json.RawMessage(`{}`)
}

// toolChoiceWire maps our ToolChoice to Anthropic's tool_choice object.
func toolChoiceWire(tc model.ToolChoice) any {
	switch tc.Mode {
	case "required":
		return map[string]string{"type": "any"}
	case "none":
		return map[string]string{"type": "none"}
	case "specific":
		return map[string]string{"type": "tool", "name": tc.Name}
	default: // "", "auto"
		return map[string]string{"type": "auto"}
	}
}

// mapStopReason translates Anthropic stop reasons to Tenant's convention so
// the agent loop's existing checks (tool_calls / stop / length) keep working.
func mapStopReason(r string) string {
	switch r {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "end_turn", "stop_sequence":
		return "stop"
	default:
		return r
	}
}

// ----- HTTP -----

func (b *Backend) postJSON(ctx context.Context, path string, body, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("%w: marshal: %v", model.ErrInternal, err)
	}
	url := strings.TrimRight(b.profile.Endpoint, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("%w: build request: %v", model.ErrInternal, err)
	}
	b.setHeaders(req, "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("%w: %v", model.ErrCancelled, err)
		}
		return fmt.Errorf("%w: %v", model.ErrEndpointDown, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return classifyHTTPError(resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%w: decode: %v", model.ErrInternal, err)
	}
	return nil
}

func (b *Backend) setHeaders(req *http.Request, accept string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	req.Header.Set("x-api-key", b.profile.APIKey)
	req.Header.Set("anthropic-version", anthropicVersion)
}

func classifyHTTPError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	snippet := strings.TrimSpace(string(body))
	lower := strings.ToLower(snippet)
	switch {
	case resp.StatusCode == 429 && isBillingError(lower):
		// Same billing-vs-rate-limit distinction as the vllm backend.
		// Anthropic itself uses 429 only for actual rate limits in
		// practice, but operators sometimes proxy through gateways that
		// return upstream billing errors under 429.
		return fmt.Errorf("%w: %s", model.ErrInsufficientBalance, snippet)
	case resp.StatusCode == 429:
		return fmt.Errorf("%w: %s", model.ErrRateLimited, snippet)
	case resp.StatusCode == 400 && strings.Contains(lower, "context"):
		return fmt.Errorf("%w: %s", model.ErrContextOverflow, snippet)
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return fmt.Errorf("%w: %d (check your Anthropic API key) %s", model.ErrInvalidRequest, resp.StatusCode, snippet)
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return fmt.Errorf("%w: %d %s", model.ErrInvalidRequest, resp.StatusCode, snippet)
	default:
		return fmt.Errorf("%w: %d %s", model.ErrInternal, resp.StatusCode, snippet)
	}
}

// isBillingError mirrors the vllm backend's helper — same patterns,
// same conservatism: only steal a 429 from ErrRateLimited when the
// body clearly names a balance/quota concept. Duplicated here to keep
// the anthropic package self-contained (no cross-backend imports).
func isBillingError(lowerBody string) bool {
	for _, marker := range []string{
		"insufficient balance",
		"insufficient_balance",
		"no resource package",
		"please recharge",
		"quota exhausted",
		"quota_exhausted",
		"quota exceeded",
		"quota_exceeded",
		"billing",
		"out of credits",
		"insufficient_quota",
	} {
		if strings.Contains(lowerBody, marker) {
			return true
		}
	}
	return false
}
