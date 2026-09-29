package config

// ProviderKind is static metadata about a provider family: how to reach it,
// which Tenant backend factory serves it, and its auth/tokenizer quirks.
type ProviderKind struct {
	ID              string
	Label           string
	Backend         string // tenant backend factory: echo | vllm (OpenAI-compat) | anthropic
	DefaultEndpoint string
	DefaultToolFmt  string
	DefaultModel    string
	ChatPath        string // override when the base already carries a version
	NeedsKey        bool
	KeyEnv          string // conventional env var for the key
	EstimateTokens  bool   // no /tokenize endpoint
	Wired           bool   // false = config captured but backend not implemented yet
	Local           bool   // runs on localhost (no key, reachable probe meaningful)
	ForceHTTP1      bool   // disable HTTP/2 — the LB recycles long-lived h2 conns with a mid-stream GOAWAY (TEN-218)
	// SupportsReasoning marks a kind whose API accepts a reasoning.effort hint
	// (Sakana Fugu). Gates /reasoning so the operator can't set an effort on a
	// provider that would 400 on the unknown field.
	SupportsReasoning bool
}

// ClaudeCodeAutoEndpoint is the claudecode kind's endpoint: a discovery
// sentinel, not a URL. It equals claudecode.AutoEndpoint (a test holds them
// together); config does not import a model backend.
const ClaudeCodeAutoEndpoint = "claudecode://auto"

// ProviderKinds is the catalog the wizard offers. OpenAI-compatible providers
// all route through the vllm backend factory (it speaks /v1/chat/completions);
// only the endpoint/key/paths differ. It is never modified at runtime.
var ProviderKinds = map[string]ProviderKind{
	"vllm": {
		ID: "vllm", Label: "vLLM (self-hosted)", Backend: "vllm",
		DefaultToolFmt: "gemma", Local: true, Wired: true,
	},
	"ollama": {
		ID: "ollama", Label: "Ollama (local)", Backend: "vllm",
		DefaultEndpoint: "http://localhost:11434", DefaultToolFmt: "openai",
		EstimateTokens: true, Local: true, Wired: true,
	},
	"llamacpp": {
		ID: "llamacpp", Label: "llama.cpp server (local)", Backend: "vllm",
		DefaultEndpoint: "http://localhost:8080", DefaultToolFmt: "openai",
		EstimateTokens: true, Local: true, Wired: true,
	},
	// Any other self-hosted server that speaks /v1/chat/completions (LM Studio,
	// LocalAI, SGLang, a LiteLLM proxy…). The dashboard files a server here
	// when it isn't vLLM, llama.cpp or Ollama. EstimateTokens keeps Tenant off
	// the vLLM-only extras (/tokenize, guided_json), which such a server would
	// refuse with a 400. The key is optional: stored per provider when given.
	"openai-compat": {
		ID: "openai-compat", Label: "OpenAI-compatible server (self-hosted)", Backend: "vllm",
		DefaultToolFmt: "openai", EstimateTokens: true, Local: true, Wired: true,
	},
	"openai": {
		ID: "openai", Label: "OpenAI", Backend: "vllm",
		DefaultEndpoint: "https://api.openai.com", DefaultToolFmt: "openai",
		DefaultModel: "gpt-4o", NeedsKey: true, KeyEnv: "OPENAI_API_KEY",
		EstimateTokens: true, Wired: true,
	},
	"grok": {
		ID: "grok", Label: "Grok (xAI)", Backend: "vllm",
		DefaultEndpoint: "https://api.x.ai", DefaultToolFmt: "openai",
		DefaultModel: "grok-2-latest", NeedsKey: true, KeyEnv: "XAI_API_KEY",
		EstimateTokens: true, Wired: true,
	},
	// Z.ai catalog entries. The default `zai` kind points at the
	// CODING PLAN endpoint because that's the common operator case
	// (coding tools use the coding plan, not the metered API). Operators
	// who specifically need metered API access use `zai-metered`. The
	// distinction is the path segment: `/api/coding/paas/v4` bills
	// against the coding-plan subscription, `/api/paas/v4` bills
	// against the per-token quota. Per OpenClaw's resolveZaiBaseUrl
	// (extensions/zai/model-definitions.ts).
	"zai": {
		ID: "zai", Label: "Z.ai (GLM — coding plan, global) [DEFAULT]", Backend: "vllm",
		DefaultEndpoint: "https://api.z.ai/api/coding/paas/v4", ChatPath: "/chat/completions",
		DefaultToolFmt: "openai", DefaultModel: "glm-4.6", NeedsKey: true,
		KeyEnv: "ZAI_API_KEY", EstimateTokens: true, Wired: true, ForceHTTP1: true,
	},
	"zai-coding": {
		// Explicit alias for `zai` — same as the default. Kept so
		// existing operator configs (`kind: "zai-coding"`) keep
		// loading without migration.
		ID: "zai-coding", Label: "Z.ai (GLM — coding plan, global)", Backend: "vllm",
		DefaultEndpoint: "https://api.z.ai/api/coding/paas/v4", ChatPath: "/chat/completions",
		DefaultToolFmt: "openai", DefaultModel: "glm-4.6", NeedsKey: true,
		KeyEnv: "ZAI_API_KEY", EstimateTokens: true, Wired: true, ForceHTTP1: true,
	},
	"zai-coding-cn": {
		ID: "zai-coding-cn", Label: "Z.ai (GLM — coding plan, China / bigmodel.cn)", Backend: "vllm",
		DefaultEndpoint: "https://open.bigmodel.cn/api/coding/paas/v4", ChatPath: "/chat/completions",
		DefaultToolFmt: "openai", DefaultModel: "glm-4.6", NeedsKey: true,
		KeyEnv: "ZAI_API_KEY", EstimateTokens: true, Wired: true, ForceHTTP1: true,
	},
	"zai-metered": {
		// The PER-TOKEN metered API. Operators on the standard
		// (non-coding-plan) billing tier use this. Returns
		// HTTP 429 "Insufficient balance" when the account quota
		// is exhausted — surfaced as ErrInsufficientBalance.
		ID: "zai-metered", Label: "Z.ai (GLM — metered API, per-token billing)", Backend: "vllm",
		DefaultEndpoint: "https://api.z.ai/api/paas/v4", ChatPath: "/chat/completions",
		DefaultToolFmt: "openai", DefaultModel: "glm-4.6", NeedsKey: true,
		KeyEnv: "ZAI_API_KEY", EstimateTokens: true, Wired: true, ForceHTTP1: true,
	},
	// Sakana AI's Fugu API is OpenAI-compatible (base https://api.sakana.ai/v1,
	// Authorization: Bearer, /v1/chat/completions, native tool_calls + parallel
	// tool calls), so it rides the same vllm backend as OpenAI/Grok/Z.ai. The
	// base is given WITHOUT the /v1 segment so the default ChatPath
	// (/v1/chat/completions) and the standard /v1/models listing both resolve —
	// exactly like the "openai" kind. Default model is fugu-ultra (the advanced
	// variant); `fugu` is also offered live via /model pick. Per
	// https://console.sakana.ai/get-started.
	"sakana": {
		ID: "sakana", Label: "Sakana AI (Fugu)", Backend: "vllm",
		DefaultEndpoint: "https://api.sakana.ai", DefaultToolFmt: "openai",
		DefaultModel: "fugu-ultra", NeedsKey: true, KeyEnv: "SAKANA_API_KEY",
		EstimateTokens: true, Wired: true, SupportsReasoning: true,
	},
	"anthropic": {
		ID: "anthropic", Label: "Anthropic (Claude)", Backend: "anthropic",
		DefaultEndpoint: "https://api.anthropic.com", DefaultModel: "claude-sonnet-4-20250514",
		NeedsKey: true, KeyEnv: "ANTHROPIC_API_KEY", EstimateTokens: true, Wired: true,
	},
	// Claude Code CLI (`claude -p`): frontier models billed against the
	// operator's SUBSCRIPTION (Max/Pro weekly allowance), not an API key —
	// which is why NeedsKey is false; auth is the logged-in CLI install. The
	// endpoint is a discovery sentinel, not a URL (explicit binary paths go
	// via TENANT_CLAUDE_BIN — SanitizeEndpoints would clobber a bare file
	// path here). Local is FALSE deliberately: routing policy must treat
	// this as a scarce cloud resource, never as free local capacity. Model
	// accepts Claude Code aliases (opus | sonnet | haiku | fable) or full
	// IDs. Backed by internal/model/backend/claudecode.
	"claudecode": {
		ID: "claudecode", Label: "Claude Code CLI (subscription — claude -p)", Backend: "claudecode",
		DefaultEndpoint: ClaudeCodeAutoEndpoint, DefaultModel: "opus", DefaultToolFmt: "qwen",
		EstimateTokens: true, Wired: true,
		// The claude CLI has a native --effort flag (full Claude ladder,
		// low→max) — /reasoning tunes it like Fugu's reasoning.effort.
		SupportsReasoning: true,
	},
	"echo": {
		ID: "echo", Label: "Echo (offline dev, deterministic)", Backend: "echo",
		Wired: true, Local: true,
	},
}

// ProviderOrder is the stable display order for the wizard menu.
var ProviderOrder = []string{"vllm", "ollama", "llamacpp", "openai-compat", "openai", "grok", "zai", "zai-coding", "zai-coding-cn", "zai-metered", "sakana", "anthropic", "claudecode", "echo"}
