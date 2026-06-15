// Package model is Tenant's abstraction over local LLM inference backends
// (vLLM by default, with the interface designed so additional backends —
// Ollama, llama.cpp HTTP — can be added without churn).
//
// The split between LLM and Embedder reflects the deployment reality: a
// generation model (Qwen 3.6-72B on a DGX Spark) and an embedding model
// (Qwen3-Embedding-8B on a Mac Studio) are different processes serving
// different endpoints. Coupling them onto one interface would force every
// backend implementation to support both or return ErrNotSupported.
package model

import (
	"context"
	"encoding/json"
)

// LLM is the generation interface. Backends speaking OpenAI-compatible HTTP
// (vLLM, llama.cpp's server, LM Studio) implement this. TokenCount is part
// of the interface, not a helper — the memory layer needs trustworthy
// per-model token accounting to make budgeting decisions before sending a
// request, not after receiving ErrContextOverflow. Computed via the
// backend's /tokenize endpoint when available; falls back to a tiktoken-
// shaped estimator otherwise.
type LLM interface {
	Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error)
	GenerateStream(ctx context.Context, req GenerateRequest) (<-chan StreamChunk, error)
	TokenCount(ctx context.Context, text string) (int, error)
}

// Embedder is the embedding interface. Implementations are typically a
// separate backend instance pointing at a dedicated embedding model.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// Role names a class of work the agent runtime needs done. Profiles
// declare which role they fulfill; the Router resolves Role → Profile.
// Shipped roles are constants below; users can extend by declaring
// new role strings in profile YAML.
type Role string

const (
	RolePlanner    Role = "planner"
	RoleExecutor   Role = "executor"
	RoleCoder      Role = "coder"
	RoleSummarizer Role = "summarizer"
	RoleEmbedder   Role = "embedder"
	RoleJudge      Role = "judge"
	RoleDefault    Role = "default"
)

// GenerateRequest is the inbound shape for a single generation call.
// Tool-related fields go through the toolfmt adapters before reaching
// the backend so each model family sees its native serialization.
type GenerateRequest struct {
	Messages      []Message       `json:"messages"`
	Tools         []ToolSpec      `json:"tools,omitempty"`
	ToolChoice    ToolChoice      `json:"tool_choice,omitempty"`
	JSONSchema    json.RawMessage `json:"json_schema,omitempty"`
	MaxTokens     int             `json:"max_tokens,omitempty"`
	Temperature   float32         `json:"temperature,omitempty"`
	StopSequences []string        `json:"stop,omitempty"`
}

// GenerateResponse is the buffered (non-streaming) result of a Generate
// call. Streaming callers receive StreamChunk values instead.
type GenerateResponse struct {
	Text         string     `json:"text"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	FinishReason string     `json:"finish_reason,omitempty"`
	Usage        Usage      `json:"usage,omitempty"`
}

// StreamChunk is one frame from a streaming Generate call. The terminal
// chunk carries FinishReason; if a mid-stream error occurs, Error is set
// on the final chunk. Consumers MUST check Error on the terminal chunk —
// ignoring it turns mid-stream failures into silent successes.
type StreamChunk struct {
	Delta         string    `json:"delta,omitempty"`
	ToolCallDelta *ToolCall `json:"tool_call_delta,omitempty"`
	FinishReason  string    `json:"finish_reason,omitempty"`
	Usage         *Usage    `json:"usage,omitempty"`
	Error         error     `json:"-"`
}

// Message is a single chat-format message.
type Message struct {
	Role       string     `json:"role"` // system | user | assistant | tool
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// ToolSpec is the abstract tool description before model-specific
// adaptation. The toolfmt layer converts this to each model family's
// preferred prompt fragment.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"` // JSON schema
	// Gated marks a tool whose handler gates a send/destructive action
	// through the plugin Policy (read tools are never gated). Internal
	// only — `json:"-"` keeps it out of the model-facing tool JSON; the
	// dashboard surfaces it as the authoritative `destructive` flag.
	Gated bool `json:"-"`
	// Gate, when non-empty, names a capability the active model Profile must
	// allow (Profile.AllowsTool) for this tool to be surfaced AND dispatched.
	// "" = always available. Used to restrict augmentation tools (e.g.
	// memory_recall) to capable models. Internal only — `json:"-"` keeps it
	// out of the model-facing tool JSON (TEN-103).
	Gate string `json:"-"`
	// Risk is the tool's capability tier (TEN-230). Zero value (RiskRead) plus
	// Gated is the common case — see RiskLevel(), which derives read for ungated
	// tools and write for gated ones. Set it EXPLICITLY only to raise a gated
	// tool above write: RiskExec (runs commands/code) or RiskDestructive
	// (irreversible: deletes, dangerous exec). Offsite channels (e.g. the
	// iMessage responder) cap the surface to a max tier; internal only.
	Risk RiskTier `json:"-"`
}

// RiskTier is a tool's capability tier — a coarse, ordered ladder that offsite
// channels cap against (read < write < exec < destructive). TEN-230.
type RiskTier uint8

const (
	RiskRead        RiskTier = iota // query/list/search/get — no side effects
	RiskWrite                       // create/update/send/post — reversible-ish mutation
	RiskExec                        // run a shell command or code
	RiskDestructive                 // irreversible: delete/overwrite/force, dangerous exec
)

// RiskLevel returns the tool's effective tier: an explicitly-set Risk wins;
// otherwise it derives from Gated (read tools are never gated, gated tools are
// at least write). So only exec/destructive tools need an explicit Risk tag.
func (s ToolSpec) RiskLevel() RiskTier {
	if s.Risk != RiskRead {
		return s.Risk
	}
	if s.Gated {
		return RiskWrite
	}
	return RiskRead
}

// String renders the tier as a lowercase keyword (config + /imessage cap).
func (r RiskTier) String() string {
	switch r {
	case RiskWrite:
		return "write"
	case RiskExec:
		return "exec"
	case RiskDestructive:
		return "destructive"
	default:
		return "read"
	}
}

// ParseRiskTier parses a tier keyword; ok=false on an unknown value.
func ParseRiskTier(s string) (RiskTier, bool) {
	switch s {
	case "read":
		return RiskRead, true
	case "write":
		return RiskWrite, true
	case "exec":
		return RiskExec, true
	case "destructive":
		return RiskDestructive, true
	default:
		return RiskRead, false
	}
}

// ToolChoice selects whether and how the model is forced to call a tool.
type ToolChoice struct {
	Mode string `json:"mode"`           // "auto" | "required" | "none"
	Name string `json:"name,omitempty"` // specific tool when Mode == "specific"
}

// ToolCall is a single model-emitted tool invocation.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Usage reports token accounting for a single call.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
