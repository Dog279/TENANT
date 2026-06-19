package model

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Profile describes one model running at one endpoint, plus the policy
// the runtime should apply when calling it. Profiles are loaded from
// embedded YAML defaults and merged with optional user overrides at
// ~/.config/tenant/profiles/*.yaml.
//
// OperationalContextBudget is intentionally separate from ContextLength.
// ContextLength is what the model SUPPORTS; OperationalContextBudget is
// what we'll actually use under concurrent multi-endpoint load with KV
// cache pressure. Treating them as one number led the original plan to
// false confidence about 128K real-world usable space.
//
// Capabilities is advisory metadata. v1 routing uses Role directly;
// v1.1 may add capability-based resolution without a schema change.
type Profile struct {
	ID                       string `yaml:"id"`
	Role                     Role   `yaml:"role"`
	Backend                  string `yaml:"backend"`  // "vllm" in v1
	Endpoint                 string `yaml:"endpoint"` // e.g. http://your-llm-host:8000
	Model                    string `yaml:"model"`    // model name to send in API requests
	ContextLength            int    `yaml:"context_length"`
	OperationalContextBudget int    `yaml:"operational_context_budget"`
	ReserveSoul              int    `yaml:"reserve_soul"`          // identity, persona, persistent user facts (T0)
	ReserveSystemPrompt      int    `yaml:"reserve_system_prompt"` // structural rules, format specs, tool-use protocol
	ReserveToolDefs          int    `yaml:"reserve_tool_defs"`
	ReserveResponse          int    `yaml:"reserve_response"`
	ToolFormat               string `yaml:"tool_format"` // qwen | gemma | llama | mistral | openai
	EmbedDim                 int    `yaml:"embed_dim,omitempty"`
	SupportsGrammar          bool   `yaml:"supports_grammar"`

	// APIKey, when non-empty, is sent as `Authorization: Bearer <key>` so
	// one OpenAI-compatible backend serves hosted providers (OpenAI, xAI,
	// Z.ai, …) as well as a local vLLM. Resolved at router-build time from
	// an env var or the stored credentials file — never persisted in YAML.
	APIKey string `yaml:"-"`

	// EstimateTokensOnly skips the vLLM-specific /tokenize call and budgets
	// via a chars/4 estimate. Set for providers that don't expose /tokenize
	// (every hosted OpenAI-compatible API, Ollama, llama.cpp).
	EstimateTokensOnly bool `yaml:"estimate_tokens_only,omitempty"`

	// ForceHTTP11 disables HTTP/2 on the backend's HTTP client so each request
	// uses its own HTTP/1.1 connection. Set for hosted providers whose load
	// balancer recycles long-lived multiplexed HTTP/2 connections with a GOAWAY
	// mid-stream (Z.ai/GLM) — on HTTP/1.1 that recycling lands harmlessly
	// between requests instead. Pairs with the planner's transient retry
	// (TEN-218 / TEN-215). Only the vLLM (OpenAI-compatible) backend reads it.
	ForceHTTP11 bool `yaml:"force_http1,omitempty"`

	// ChatPath overrides the chat-completions URL suffix appended to
	// Endpoint. Default "/v1/chat/completions". Z.ai's base already carries
	// the version (…/paas/v4) so it needs "/chat/completions".
	ChatPath         string `yaml:"chat_path,omitempty"`
	MaxToolsPerCall  int    `yaml:"max_tools_per_call"`
	MaxParallelTools int    `yaml:"max_parallel_tools"`
	PlanLoopCeiling  int    `yaml:"plan_loop_ceiling"`
	// MaxToolResultTokens caps each tool result fed into the model's CONTEXT
	// (working set). 0 = no cap (frontier path unchanged). The full result is
	// always kept in the archive; only the in-context copy is truncated.
	MaxToolResultTokens int            `yaml:"max_tool_result_tokens"`
	Capabilities        map[string]any `yaml:"capabilities,omitempty"`
}

// OperationalBudget is the token budget the runtime actually plans against
// under concurrent multi-endpoint load — OperationalContextBudget when set,
// else an 80% fallback of ContextLength for profiles that predate the field.
// 0 only when neither is known. Both WritableBudget (fixed reserves) and the
// assembler's measured-static sizing (TEN-214) build on this single number.
func (p Profile) OperationalBudget() int {
	if p.OperationalContextBudget > 0 {
		return p.OperationalContextBudget
	}
	if p.ContextLength > 0 {
		return (p.ContextLength * 8) / 10
	}
	return 0
}

// defaultVLLMToolResultTokens caps tool results fed into a LOCAL vLLM model's
// context when a profile doesn't set MaxToolResultTokens explicitly. Local
// (often weak/30B) models drown in a 20K web_read or a wide SQL dump; hosted
// frontier backends (Backend != "vllm") stay uncapped unless they opt in. (TEN-259)
const defaultVLLMToolResultTokens = 4096

// ToolResultCap is the effective per-tool-result token cap the agent applies
// before feeding a result back into the model's context. An explicit
// MaxToolResultTokens wins (a negative value force-disables the cap); otherwise
// local vLLM profiles get a sane default and other backends are uncapped. This
// keeps the cap live for config-provider runtimes (which don't carry the field)
// without threading it through every profile builder. (TEN-259)
func (p Profile) ToolResultCap() int {
	if p.MaxToolResultTokens != 0 {
		return p.MaxToolResultTokens
	}
	// Self-hosted vLLM is the only OpenAI-compatible backend that exposes
	// guided_json / a /tokenize endpoint (SupportsGrammar). Hosted providers on
	// the same "vllm" backend — Z.ai/GLM, OpenAI, Grok — and Ollama/llama.cpp set
	// EstimateTokens and leave SupportsGrammar false. We use that as the proxy
	// for "local inference that benefits from a context cap", so frontier
	// providers stay uncapped by default.
	if p.Backend == "vllm" && p.SupportsGrammar {
		return defaultVLLMToolResultTokens
	}
	return 0
}

// WritableBudget reports the tokens available for the working set +
// retrieved memory after soul, system prompt, tool defs, and response
// reserves are deducted from the operational budget. The memory
// assembler asks for this number, not for ContextLength.
//
// Soul and SystemPrompt are tracked separately because they serve
// different concerns: Soul is identity / persona / persistent user
// facts (relatively static, grows with the agent's tenure); System
// is structural rules and format specs (varies per task and model).
// The TODO for empirical tuning of these per-profile values is in
// TODOS.md — current numbers are calibrated guesses.
//
// This uses the FIXED per-class reserves. They're calibrated guesses, and a
// full tool mux's real schema cost can be ~10x ReserveToolDefs (TEN-214), so
// the assembler sizes its variable tiers off MEASURED static instead. This
// value is retained as a back-compat field and as the recall-gating heuristic
// proxy (AllowsTool), where a static capability estimate is the right input.
func (p Profile) WritableBudget() int {
	w := p.OperationalBudget() - p.ReserveSoul - p.ReserveSystemPrompt - p.ReserveToolDefs - p.ReserveResponse
	if w < 0 {
		return 0
	}
	return w
}

// recallHeuristicBudget is the WritableBudget at/above which memory_recall is
// enabled by default (no explicit capability set) — a proxy for "strong enough
// to use augmentation well." Gated on the usable budget, NOT raw ContextLength
// (some small local models advertise 128K but have little usable space).
const recallHeuristicBudget = 32768

// AllowsTool reports whether a ToolSpec.Gate capability is permitted for this
// profile. An explicit Capabilities[gate] bool always wins; otherwise a
// per-gate heuristic decides (today only "recall", gated on WritableBudget).
// Unknown gates default to false (fail-safe: a gated tool is hidden unless
// explicitly understood). The empty gate ("") is handled by the caller.
func (p Profile) AllowsTool(gate string) bool {
	if v, ok := p.Capabilities[gate]; ok {
		b, _ := v.(bool)
		return b
	}
	switch gate {
	case "recall":
		return p.WritableBudget() >= recallHeuristicBudget
	default:
		return false
	}
}

// validate is called at registry load. Returns ErrInvalidProfile wrapping
// the specific failure so the operator sees which file / field broke.
func (p Profile) validate() error {
	if p.ID == "" {
		return fmt.Errorf("%w: missing id", ErrInvalidProfile)
	}
	if p.Role == "" {
		return fmt.Errorf("%w: id=%s missing role", ErrInvalidProfile, p.ID)
	}
	if p.Endpoint == "" {
		return fmt.Errorf("%w: id=%s missing endpoint", ErrInvalidProfile, p.ID)
	}
	if p.Backend == "" {
		return fmt.Errorf("%w: id=%s missing backend", ErrInvalidProfile, p.ID)
	}
	if p.ContextLength <= 0 {
		return fmt.Errorf("%w: id=%s context_length must be > 0", ErrInvalidProfile, p.ID)
	}
	return nil
}

// LoadProfileYAML parses one Profile from YAML bytes. Strict on syntax,
// tolerant of unknown fields (forward compatibility — adding a new
// Profile field should never break older user configs).
func LoadProfileYAML(data []byte) (Profile, error) {
	var p Profile
	if err := yaml.Unmarshal(data, &p); err != nil {
		return Profile{}, fmt.Errorf("%w: %v", ErrInvalidProfile, err)
	}
	if err := p.validate(); err != nil {
		return Profile{}, err
	}
	return p, nil
}
