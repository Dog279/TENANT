// Package vllm implements Tenant's LLM + Embedder interfaces against a
// vLLM server speaking the OpenAI-compatible HTTP API plus the vLLM
// /tokenize extension.
//
// Endpoints used:
//
//	POST {Endpoint}/v1/chat/completions   — Generate
//	POST {Endpoint}/v1/embeddings         — Embed
//	POST {Endpoint}/tokenize              — TokenCount (vLLM-specific)
//
// GenerateStream lives in stream.go (separate file because SSE parsing
// is its own concern).
package vllm

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"tenant/internal/model"
	"tenant/internal/model/toolfmt"
)

// Backend is the concrete implementation. It satisfies both
// model.LLM and model.Embedder; the Router decides which interface to
// surface based on the Profile's Role.
type Backend struct {
	profile model.Profile
	client  *http.Client
	log     *slog.Logger
}

// New constructs a Backend for the given Profile. http.Client carries
// sensible local-network timeouts; vLLM responses on slow models can
// take 10+ minutes, hence the long response timeout.
func New(_ context.Context, p model.Profile, log *slog.Logger) (any, error) {
	if log == nil {
		log = slog.Default()
	}
	tr := &http.Transport{
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  false,
	}
	if p.ForceHTTP11 {
		// Disable HTTP/2: a non-nil, empty TLSNextProto stops the transport from
		// negotiating h2 over ALPN, so each request uses its own HTTP/1.1
		// connection. Hosted load balancers (Z.ai) recycle long-lived
		// multiplexed h2 connections with a mid-stream GOAWAY that Go can't
		// auto-retry; on HTTP/1.1 that recycling lands harmlessly between
		// requests instead (TEN-218, pairs with the planner retry in TEN-215).
		tr.TLSNextProto = map[string]func(authority string, c *tls.Conn) http.RoundTripper{}
	}
	return &Backend{
		profile: p,
		log:     log,
		client: &http.Client{
			Timeout:   0, // per-call via context; response time is unbounded for streaming
			Transport: tr,
		},
	}, nil
}

// ----- LLM.Generate -----

type chatRequest struct {
	Model       string          `json:"model"`
	Messages    []wireMsg       `json:"messages"`
	Tools       []chatTool      `json:"tools,omitempty"`
	ToolChoice  any             `json:"tool_choice,omitempty"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature float32         `json:"temperature,omitempty"`
	Stop        []string        `json:"stop,omitempty"`
	Stream      bool            `json:"stream"`
	GuidedJSON  json.RawMessage `json:"guided_json,omitempty"` // vLLM guided decoding
}

// wireMsg is the OpenAI chat-message wire shape. Our internal
// model.Message keeps a clean flat ToolCall; the OpenAI/vLLM API
// requires the nested {id,type:"function",function:{name,arguments}}
// form with arguments as a STRINGIFIED JSON object. toWireMessages
// translates outbound — the symmetric counterpart of
// normalizeToolArgs on the inbound side. Without this, replaying an
// assistant tool-call message back to vLLM (turn 2 of any tool flow)
// is rejected with a 400 — caught only by a real tool round-trip.
type wireMsg struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

type wireToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // always "function"
	Function wireToolFunc `json:"function"`
}

type wireToolFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // stringified JSON, per OpenAI spec
}

// toWireMessages maps internal messages to the OpenAI wire shape.
func toWireMessages(msgs []model.Message) []wireMsg {
	out := make([]wireMsg, len(msgs))
	for i, m := range msgs {
		wm := wireMsg{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
		for _, tc := range m.ToolCalls {
			args := strings.TrimSpace(string(tc.Arguments))
			// Never put malformed JSON on the wire — vLLM 400s ("Expecting
			// property name") parsing a replayed assistant tool call whose
			// arguments aren't valid JSON. Empty/invalid → "{}".
			if args == "" || !json.Valid([]byte(args)) {
				args = "{}"
			}
			wm.ToolCalls = append(wm.ToolCalls, wireToolCall{
				ID:       tc.ID,
				Type:     "function",
				Function: wireToolFunc{Name: tc.Name, Arguments: args},
			})
		}
		out[i] = wm
	}
	return out
}

type chatTool struct {
	Type     string       `json:"type"`
	Function chatToolFunc `json:"function"`
}

type chatToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
			// Reasoning is the separated chain-of-thought emitted by vLLM
			// servers configured with --reasoning-parser (qwen3 / deepseek-r1
			// /etc.). When content is empty but reasoning has substance, we
			// fall back to reasoning so callers don't see an empty response —
			// LIVE bug: aeon-ultimate ran out of max_tokens mid-thinking on a
			// short clarifier prompt, content came back null, the clarifier
			// silently degraded to "no questions" and C2 skipped on a 2-word
			// query it should have caught.
			Reasoning string `json:"reasoning,omitempty"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls,omitempty"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// Generate sends one buffered chat completion request.
func (b *Backend) Generate(ctx context.Context, req model.GenerateRequest) (*model.GenerateResponse, error) {
	body := chatRequest{
		Model:       b.profile.Model,
		Messages:    toWireMessages(req.Messages),
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		Stop:        req.StopSequences,
		Stream:      false,
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

	var resp chatResponse
	if err := b.postJSON(ctx, b.chatPath(), body, &resp); err != nil {
		return nil, err
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("%w: empty choices", model.ErrInternal)
	}
	ch := resp.Choices[0]
	adapter := toolfmt.AdapterFor(b.profile.ToolFormat)
	// Reasoning-parser fallback: when content is empty but reasoning has
	// substance (model ran out of tokens mid-think, or just emits only
	// reasoning for short-answer prompts), salvage the reasoning text so
	// the caller isn't handed an empty response. Better to leak some CoT
	// than to silently drop the model's output. LIVE trigger: aeon-ultimate
	// on a clarifier prompt hit max_tokens during thinking → content=null
	// → C2 clarifier degraded to nil and ran research on the bare query.
	content := ch.Message.Content
	if strings.TrimSpace(content) == "" && strings.TrimSpace(ch.Message.Reasoning) != "" {
		content = ch.Message.Reasoning
	}
	out := &model.GenerateResponse{
		// Strip this model family's display artifacts (harmony channel tokens
		// for gemma-channel models, <think> blocks for qwen) so they don't
		// show in the answer. No-op for clean output.
		Text:         adapter.CleanText(content),
		FinishReason: ch.FinishReason,
		Usage: model.Usage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		},
	}
	if len(ch.Message.ToolCalls) > 0 {
		out.ToolCalls = make([]model.ToolCall, len(ch.Message.ToolCalls))
		for i, tc := range ch.Message.ToolCalls {
			out.ToolCalls[i] = model.ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: normalizeToolArgs(tc.Function.Arguments),
			}
		}
		return out, nil
	}
	// Safety net: vLLM didn't return structured tool_calls (operator may
	// have forgotten --tool-call-parser, or the model emitted a variant
	// the parser didn't catch — Hermes XML from merged qwen variants like
	// aeon-ultimate is the canonical trigger). Scan the RAW content (not
	// the display-stripped Text) so channel-wrapped calls like
	// `to=functions.X {json}` survive for the adapter to extract.
	if strings.TrimSpace(content) != "" {
		parsed, err := adapter.ParseToolCalls(content)
		if err != nil {
			b.log.Debug("vllm: tool-call safety-net parse failed",
				"format", b.profile.ToolFormat, "err", err)
		} else if len(parsed) > 0 {
			out.ToolCalls = parsed
			out.FinishReason = "tool_calls"
			// Also strip the matched call markup from Text. Otherwise the
			// same XML/JSON blob ends up in resp.Text and pollutes any
			// caller (especially synthesizeFinal) that reads Text as "the
			// model's answer." Live bug: the XML became the agent's
			// final-response text → became the research finding → the
			// synthesizer correctly reported "findings are tool-call logs."
			out.Text = adapter.StripToolCalls(out.Text)
		}
	}
	if len(out.ToolCalls) == 0 {
		// No structured AND no parseable in-text call — the model
		// answered as prose. The usual cause of "it ignored the tool":
		// log the raw text so the format mismatch is diagnosable.
		b.log.Debug("vllm: completion had no tool call (treated as final answer)",
			"format", b.profile.ToolFormat, "text", clipDbg(out.Text, 600))
	}
	return out, nil
}

func clipDbg(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ----- LLM.GenerateStream (skeleton — full SSE parsing in stream.go) -----

// GenerateStream is implemented in stream.go to keep this file focused
// on the synchronous request paths. Declared here for interface clarity.
func (b *Backend) GenerateStream(ctx context.Context, req model.GenerateRequest) (<-chan model.StreamChunk, error) {
	return b.generateStream(ctx, req)
}

// ----- LLM.TokenCount -----

type tokenizeRequest struct {
	Model            string `json:"model"`
	Prompt           string `json:"prompt"`
	AddSpecialTokens bool   `json:"add_special_tokens"`
}

type tokenizeResponse struct {
	Count int `json:"count"`
}

// TokenCount asks the vLLM server for the token count under the active
// model's tokenizer. This is the only honest way to budget — Go-side
// estimators drift per model family. Hosted OpenAI-compatible providers
// (OpenAI, xAI, Z.ai, Ollama, llama.cpp) don't expose /tokenize, so when
// EstimateTokensOnly is set we budget via a chars/4 estimate instead. We
// also fall back to the estimate if /tokenize is simply unavailable, so a
// missing extension degrades budgeting rather than breaking the turn.
func (b *Backend) TokenCount(ctx context.Context, text string) (int, error) {
	if b.profile.EstimateTokensOnly {
		return estimateTokens(text), nil
	}
	body := tokenizeRequest{Model: b.profile.Model, Prompt: text, AddSpecialTokens: true}
	var resp tokenizeResponse
	if err := b.postJSON(ctx, "/tokenize", body, &resp); err != nil {
		// Cancellation is a real failure; an unsupported endpoint is not.
		if errors.Is(err, model.ErrCancelled) {
			return 0, err
		}
		return estimateTokens(text), nil
	}
	return resp.Count, nil
}

// estimateTokens approximates 1 token per 4 characters — the same heuristic
// the echo backend uses. Good enough for budget headroom math.
func estimateTokens(text string) int {
	n := len(text) / 4
	if n == 0 && len(text) > 0 {
		return 1
	}
	return n
}

// chatPath returns the chat-completions URL suffix, honoring a per-provider
// override (Z.ai's base already includes the version path).
func (b *Backend) chatPath() string {
	if b.profile.ChatPath != "" {
		return b.profile.ChatPath
	}
	return "/v1/chat/completions"
}

// ----- Embedder.Embed -----

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
}

// Embed returns one vector per input text, in input order.
func (b *Backend) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return [][]float32{}, nil
	}
	body := embedRequest{Model: b.profile.Model, Input: texts}
	var resp embedResponse
	if err := b.postJSON(ctx, "/v1/embeddings", body, &resp); err != nil {
		return nil, err
	}
	out := make([][]float32, len(texts))
	for _, d := range resp.Data {
		if d.Index < 0 || d.Index >= len(texts) {
			return nil, fmt.Errorf("%w: embedding index %d out of range", model.ErrInternal, d.Index)
		}
		out[d.Index] = d.Embedding
	}
	return out, nil
}

// ----- internal HTTP helpers -----

// postJSON marshals body, POSTs to {endpoint}{path}, and decodes the
// response into out. Maps transport / status errors to typed model.Err*.
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
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if b.profile.APIKey != "" {
		// Hosted OpenAI-compatible providers authenticate via a bearer
		// token; vLLM ignores it unless --api-key was set, so it's safe
		// to always send when configured.
		req.Header.Set("Authorization", "Bearer "+b.profile.APIKey)
	}

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

func classifyHTTPError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	snippet := strings.TrimSpace(string(body))
	lower := strings.ToLower(snippet)
	switch {
	case resp.StatusCode == 429 && isBillingError(lower):
		// Z.ai and some other OpenAI-compat providers return 429 for
		// "out of credits" / "quota exhausted" — distinct from a real
		// rate limit. Surface accurately so the operator doesn't burn
		// time debugging a phantom rate limit when the actual fix is
		// recharging the account or checking plan coverage.
		return fmt.Errorf("%w: %s", model.ErrInsufficientBalance, snippet)
	case resp.StatusCode == 429:
		return fmt.Errorf("%w: %s", model.ErrRateLimited, snippet)
	case resp.StatusCode == 400 && strings.Contains(lower, "context"):
		return fmt.Errorf("%w: %s", model.ErrContextOverflow, snippet)
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return fmt.Errorf("%w: %d %s", model.ErrInvalidRequest, resp.StatusCode, snippet)
	default:
		return fmt.Errorf("%w: %d %s", model.ErrInternal, resp.StatusCode, snippet)
	}
}

// isBillingError detects billing/quota-shaped 429 bodies that providers
// (notably Z.ai) return instead of a proper 402/403. Pattern set is
// CONSERVATIVE — we only steal a 429 away from ErrRateLimited when the
// body clearly names a balance/quota/billing concept. Otherwise actual
// rate limits with vague bodies still surface as ErrRateLimited
// (correct + retryable). Substrings are matched case-insensitively
// against the lowercased body.
func isBillingError(lowerBody string) bool {
	for _, marker := range []string{
		"insufficient balance",
		"insufficient_balance",
		"no resource package", // verbatim from Z.ai code 1113
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

func toolChoiceWire(tc model.ToolChoice) any {
	switch tc.Mode {
	case "", "auto":
		return "auto"
	case "required":
		return "required"
	case "none":
		return "none"
	case "specific":
		return map[string]any{
			"type":     "function",
			"function": map[string]string{"name": tc.Name},
		}
	default:
		return "auto"
	}
}

// normalizeToolArgs reconciles how tool-call arguments arrive on the
// wire. Per the OpenAI spec (which vLLM follows), function.arguments
// is a JSON-encoded STRING — e.g. "{\"a\": 17, \"b\": 25}" — not a
// nested object. Verified against real Gemma 4 on the DGX. The agent's
// validateToolCall json.Unmarshal's the args into a map, which fails
// on a quoted string. So:
//
//   - JSON string ("...")  → unquote, return the inner object bytes
//   - JSON object/array    → already structured, pass through
//   - empty / null / junk  → "{}" (a tool with no args is valid; a
//     broken payload becomes an empty object so
//     schema validation reports the real
//     missing-field error, not a parse error)
//
// Tolerant by design: the testllm fake emits objects directly, real
// vLLM emits strings — both must work.
func normalizeToolArgs(raw json.RawMessage) json.RawMessage {
	b := bytes.TrimSpace(raw)
	if len(b) == 0 || string(b) == "null" {
		return json.RawMessage(`{}`)
	}
	switch b[0] {
	case '{', '[':
		// Looks structured — but the model may have emitted MALFORMED JSON
		// (e.g. `{"k":10,"query":}`). Validate it: invalid → "{}" so schema
		// validation reports the real missing field, and (critically) we never
		// store/replay broken JSON that vLLM would 400 on next iteration.
		if json.Valid(b) {
			return b
		}
		return json.RawMessage(`{}`)
	case '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return json.RawMessage(`{}`)
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return json.RawMessage(`{}`)
		}
		// The unwrapped string should itself be a JSON object. If the
		// model emitted a non-JSON string, hand back {} so the schema
		// validator surfaces the real problem (missing required args)
		// rather than a cryptic parse error.
		if !json.Valid([]byte(s)) {
			return json.RawMessage(`{}`)
		}
		return json.RawMessage(s)
	default:
		return json.RawMessage(`{}`)
	}
}
