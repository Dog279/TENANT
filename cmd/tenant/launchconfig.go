package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
)

// launchConfig is the persisted, machine-wide launch configuration written by
// `tenant setup` and merged into every command's flags. It exists so the
// operator configures endpoints + providers ONCE instead of repeating a wall
// of flags on every invocation.
//
// v2 layout (OpenClaw-style): a named set of model providers, an active
// selection, a separate embeddings provider, gateway/transport, and skill
// settings. v1 wrote flat fields (backend/vllm_*); those are still read and
// migrated forward on first load. Secrets never live here — see credentials.
type launchConfig struct {
	SchemaVersion int `json:"schema_version,omitempty"`

	// Provider is the active generation provider — a key into Providers.
	Provider string `json:"provider,omitempty"`
	// Providers holds each configured provider's settings, so switching is a
	// one-line change instead of a re-setup.
	Providers map[string]*providerConfig `json:"providers,omitempty"`
	// Embed is the embeddings provider (usually a local Ollama, separate from
	// the generation provider).
	Embed *providerConfig `json:"embed,omitempty"`
	// Gateway selects how `mcp-memory` is exposed.
	Gateway gatewayConfig `json:"gateway,omitempty"`
	// Dashboard configures the web control panel (TEN-76).
	Dashboard dashboardConfig `json:"dashboard,omitempty"`
	// Tailscale persists the operator's `/tailscale serve` choice (TEN-233) so it
	// is re-asserted at next launch — matching the dashboard/relay/imessage
	// toggles. Tailscale's own daemon also persists the serve config; this flag
	// makes Tenant restore it even if the tailnet config was cleared elsewhere.
	Tailscale tailscaleConfig `json:"tailscale,omitempty"`
	// Relay configures the offsite Discord relay (TEN-114).
	Relay relayConfig `json:"relay,omitempty"`
	// IMessage holds the iMessage drive-allowlist (TEN-68 follow-up): the
	// handles permitted to drive the agent over iMessage. Deny-by-default.
	IMessage imessageConfig `json:"imessage,omitempty"`
	// Cron holds the recurring-job definitions managed via /cron, the cron_*
	// tools, and the dashboard's Cron section. Only definitions persist here;
	// run state (next/last run) is in-memory and recomputed on load.
	Cron cronConfig `json:"cron,omitempty"`
	// MCPRemotes holds remote MCP connector server URLs added via `/mcp add`,
	// so they re-register (as disabled stubs) at next launch. Non-secret; OAuth
	// tokens live in the mcpremote token cache, never here. (TEN-164)
	MCPRemotes []string `json:"mcp_remotes,omitempty"`
	// Improve holds self-improvement settings (TEN-152): the auto-accept policy
	// for machine-induced skills. Deny-by-default (empty = off).
	Improve improveConfig `json:"improve,omitempty"`
	// Skills holds per-integration settings (non-secret). Secrets go to the
	// credentials file under "skill:<name>:<field>".
	Skills map[string]*skillConfig `json:"skills,omitempty"`
	// Agents holds named sub-agent profiles for the orchestrator to spawn.
	// Each profile pins a SPECIFIC provider+model and an optional soul
	// (identity/persona markdown). When the orchestrator calls spawn_agent
	// with a role that matches one of these names, the spawned sub-agent
	// runs on the profile's chosen model with the profile's soul — instead
	// of inheriting the orchestrator's primary model + role-stamped base
	// soul. Lets you mix models per role: researcher → glm-4.6 on Z.ai,
	// synthesizer → aeon-ultimate on DGX, etc.
	Agents map[string]*agentProfile `json:"agents,omitempty"`
	// PlanLoopCeiling caps planner↔tool iterations per turn (per agent). 0 =
	// built-in default (defaultPlanCeiling). Raise it for multi-step agentic
	// tasks that legitimately need many tool calls.
	PlanLoopCeiling int `json:"plan_loop_ceiling,omitempty"`
	// Goal holds /goal autonomous-loop settings (TEN-216). Persisted only — no
	// inline command sets it.
	Goal goalConfig `json:"goal,omitempty"`

	// --- deprecated v1 flat fields: read for migration, never written ---
	Backend       string `json:"backend,omitempty"`
	VLLMEndpoint  string `json:"vllm_endpoint,omitempty"`
	VLLMModel     string `json:"vllm_model,omitempty"`
	VLLMToolFmt   string `json:"vllm_tool_format,omitempty"`
	EmbedEndpoint string `json:"embed_endpoint,omitempty"`
	EmbedModel    string `json:"embed_model,omitempty"`
	EmbedDim      int    `json:"embed_dim,omitempty"`
	SSEAddr       string `json:"sse_addr,omitempty"`
}

// goalConfig holds persisted /goal autonomous-loop settings (TEN-216).
type goalConfig struct {
	// LoopCeiling is the per-turn planner↔tool iteration budget used WHILE a
	// goal loop is active, overriding the global PlanLoopCeiling for goal turns
	// only. >0 = that many iterations per goal turn; <0 = unlimited (omit the
	// per-turn cap so a long /goal run can iterate freely — bounded only by the
	// goal turn cap, errors, and Esc); 0/unset = inherit the global ceiling (no
	// goal-specific override). Normal, non-goal turns always use PlanLoopCeiling.
	LoopCeiling int `json:"loop_ceiling,omitempty"`
}

// providerConfig is one model provider's connection + auth settings.
type providerConfig struct {
	Kind     string  `json:"kind"`                  // see providerKinds
	Endpoint string  `json:"endpoint,omitempty"`    // base URL (no /v1 suffix)
	Model    string  `json:"model,omitempty"`       // empty = auto-detect at launch
	ToolFmt  string  `json:"tool_format,omitempty"` // qwen|gemma|llama|mistral|openai
	EmbedDim int     `json:"embed_dim,omitempty"`   // embeddings only
	Auth     authCfg `json:"auth,omitempty"`
}

// agentProfile is a named sub-agent recipe the orchestrator can spawn. The
// orchestrator calls spawn_agent(role=<name>, task) and the runtime looks
// the name up here — when found, it builds the sub-agent with this profile's
// pinned model and soul, instead of inheriting the orchestrator's primary
// model + the role-stamped base soul.
//
// Provider is the id of an entry in launchConfig.Providers (e.g. "zai",
// "dgx", "anthropic"). Model optionally overrides that provider's default
// model — leave blank to use the provider's configured Model. Soul is the
// agent's identity/persona as markdown; when blank, the role label is
// stamped onto the base operator soul (the existing behavior). Description
// is shown in /agents listings and helps the orchestrator know what each
// agent is for (it's also injected into the orchestrator's system prompt).
type agentProfile struct {
	Provider    string `json:"provider"`              // provider id from Providers map
	Model       string `json:"model,omitempty"`       // override the provider's default model
	Soul        string `json:"soul,omitempty"`        // markdown identity/persona
	Description string `json:"description,omitempty"` // shown in /agents list
	// Builtin marks a persona shipped in the binary (TEN-132): read-only, and it
	// inherits the orchestrator's primary model (empty Provider). Never persisted.
	Builtin bool `json:"-"`
}

// authCfg describes how to obtain a provider's API key. Either reference an
// env var (KeyEnv) or store it in the credentials file (Stored). Never both;
// KeyEnv wins if set.
type authCfg struct {
	Mode   string `json:"mode,omitempty"`    // none|apikey|oauth
	KeyEnv string `json:"key_env,omitempty"` // env var name (reference mode)
	Stored bool   `json:"stored,omitempty"`  // secret saved in credentials.json
}

// gatewayConfig selects the mcp-memory transport.
type gatewayConfig struct {
	Mode    string `json:"mode,omitempty"`     // local | sse | both
	SSEAddr string `json:"sse_addr,omitempty"` // when mode includes sse
}

// dashboardConfig configures the web control panel (TEN-76). TLSCert/
// TLSKey/Auth are present but unused until Wave 2 (TEN-79) — defined now so
// the persisted shape is stable.
//
// Enabled is tri-state (TEN-86): nil ⇒ default ON (dashboard auto-launches
// with the TUI), &true ⇒ on, &false ⇒ off. The pointer lets a fresh config
// (no dashboard block) default to serving, while a `/dashboard off` choice
// persists as an explicit &false that survives the next launch.
type dashboardConfig struct {
	Enabled *bool  `json:"enabled,omitempty"` // nil = default on; &false = explicitly off
	Addr    string `json:"addr,omitempty"`    // listen address (default 127.0.0.1:8770)
	TLSCert string `json:"tls_cert,omitempty"`
	TLSKey  string `json:"tls_key,omitempty"`
	Auth    string `json:"auth,omitempty"`
}

// relayConfig is the offsite Discord relay's persisted state (TEN-114): whether
// it auto-starts and the single operator's Discord user id. Default OFF — the
// relay is an explicit opt-in (exposes the agent over a third-party network).
// tailscaleConfig persists the `/tailscale serve` choice (TEN-233). Serve=true
// ⇒ re-assert `tailscale serve` for the dashboard port at launch (best-effort).
type tailscaleConfig struct {
	Serve bool `json:"serve,omitempty"`
}

type relayConfig struct {
	Enabled    bool   `json:"enabled,omitempty"`     // default false = opt-in
	OperatorID string `json:"operator_id,omitempty"` // the single allowed Discord user id
	// AllowExec opts the offsite session into the gated dangerous tools
	// (exec/write/destructive/outbound-send) — each still per-action button-
	// approved. Default false: read/research/comms only. (TEN-123)
	AllowExec bool `json:"allow_exec,omitempty"`
	// Permissions is the per-category ask|allow|deny map for the Discord agent's
	// gated tools (TEN-231), driven by /relay permissions with the SAME model as
	// the global /permissions and /imessage permissions. Empty ⇒ default ASK
	// (every dangerous action prompts a button). Categories: exec/write/
	// destructive/web/send.
	Permissions map[string]string `json:"permissions,omitempty"`
}

// dashboardEnabled resolves the tri-state Enabled to a concrete on/off: nil
// (unset) defaults to ON, otherwise the stored bool wins.
func (dc dashboardConfig) dashboardEnabled() bool {
	return dc.Enabled == nil || *dc.Enabled
}

// imessageConfig holds the iMessage drive-allowlist: the normalized handles
// (phone numbers / emails) permitted to DRIVE the agent over iMessage once the
// inbound responder (Layer 2) is live. DENY-BY-DEFAULT — an empty/missing list
// permits NOBODY (the safe default for "no unrestricted access"). Edited live
// via the /imessage TUI command; the inbound responder must gate every message
// on imessage.AllowList.Allows before invoking any tool. Stored here (not in
// the runtime Meta KV) because it is operator policy configured once, like the
// Discord relay's OperatorID — not hot per-poll state.
type imessageConfig struct {
	AllowFrom []string `json:"allow_from,omitempty"`
	// Enabled starts the autonomous responder at launch (TEN-230): poll chat.db,
	// drive an agent turn per inbound text from an allowed handle, reply. Native
	// transport only (macOS); off by default (deny-by-default). AllowFrom gates
	// who may drive it.
	Enabled bool `json:"enabled,omitempty"`
	// Operator is the handle (phone/email) whose YES approves gated tools in the
	// Phase-2 text-confirm flow — distinct from AllowFrom so an allowlisted
	// contact can chat but not approve dangerous actions. Unused until Phase 2.
	Operator string `json:"operator,omitempty"`
	// Permissions is the per-category ask|allow|deny policy for the responder's
	// gated tools (TEN-230) — same categories/modes as the global /permissions,
	// set via /imessage permissions. Empty ⇒ deny-by-default (offsite). Category
	// keys: exec | write | destructive | web | send.
	Permissions map[string]string `json:"permissions,omitempty"`
}

// cronConfig persists the recurring-job DEFINITIONS plus a few engine-wide
// settings. Run state (next/last run, history) is NOT here — history lives in
// <dataDir>/cron-history.json so config.json isn't rewritten on every run.
// improveConfig governs the self-improvement loop's auto-acceptance of induced
// skills (TEN-152). AutoAccept: "" / "off" (default — every induced skill waits
// for manual /skills accept), "on" (accept all new skills), or "trusted" (accept
// only while recent operator feedback is healthy — ≥ TrustMinAcks acks and zero
// undos). TrustMinAcks: threshold for "trusted" (0 ⇒ default 5).
type improveConfig struct {
	AutoAccept   string `json:"auto_accept,omitempty"`
	TrustMinAcks int    `json:"trust_min_acks,omitempty"`
	// TrustWindow is how many recent fed-back episodes the "trusted" gate
	// inspects (0 ⇒ default 20).
	TrustWindow int `json:"trust_window,omitempty"`
	// EvalEvery persists the nightly-eval cadence (TEN-34/157) as a duration
	// string, e.g. "24h", so a 24/7 appliance keeps its regression gate across
	// restarts. Empty/0/malformed ⇒ off (deny-by-default). An explicit
	// --eval-every flag overrides this. The clock survives restarts: it is
	// seeded from trend.jsonl (TEN-196), so a relaunch never re-fires a run
	// that already happened within the interval.
	EvalEvery string `json:"eval_every,omitempty"`
	// EvalAt schedules the nightly eval at a daily wall-clock time ("HH:MM",
	// 24-hour, local) instead of an uptime interval — for 24/7 boxes that want
	// the heavy run at, say, "03:15". Wins over EvalEvery when both are set; a
	// malformed value warns and falls back to EvalEvery (TEN-196). A box asleep
	// at the anchor catches up on next wake (trend-seeded clock).
	EvalAt string `json:"eval_at,omitempty"`
	// Profile pins the self-improvement PROPOSER/reflection LLM calls — the
	// soul-nudge proposer, the fact-consolidation summarizer, and the
	// distillation summarizer — to a named entry in the Agents profile map,
	// typically a stronger reasoning model than the daily driver (TEN-195).
	// Empty ⇒ today's behavior (those calls use the main router's
	// RoleSummarizer). It NEVER touches the embedder (always the main router,
	// so the embedding space stays consistent) and NEVER the SoulNudge A/B
	// fitness scorer / model-under-test (the hard invariant — the candidate is
	// always graded on the daily model). An unknown or unbuildable profile is
	// NOT fatal: a loud WARN is logged and the jobs fall back to the main
	// router. Note distillation runs frequently, so pinning it to a heavy model
	// raises token cost + latency — opt in deliberately.
	Profile string `json:"profile,omitempty"`
	// SoulNudgeEvery persists the SoulNudgeJob cadence (TEN-16) as a duration
	// string. Off (empty/0/malformed) by default — it runs the fitness suite to
	// gate each candidate, so it's heavy + model-gated. Candidates are queued for
	// HUMAN review (never auto-applied).
	SoulNudgeEvery string `json:"soul_nudge_every,omitempty"`
}

// validAutoAccept reports whether m is an accepted auto-accept mode.
func validAutoAccept(m string) bool {
	switch m {
	case "", "off", "on", "trusted":
		return true
	}
	return false
}

type cronConfig struct {
	Jobs []cronJobConfig `json:"jobs,omitempty"`
	// Timezone is the default IANA zone for jobs without their own TZ. Empty =
	// server local time.
	Timezone string `json:"timezone,omitempty"`
	// AllowExec is the GLOBAL kill-switch for dangerous cron jobs: shell jobs and
	// exec-opted-in prompt jobs only RUN when this is true. Deny-by-default.
	AllowExec bool `json:"allow_exec,omitempty"`
	// Catchup, when true, fires a SAFE job once on startup if it missed a cycle
	// while the process was down (instead of waiting for the next occurrence).
	Catchup bool `json:"catchup,omitempty"`
}

// cronJobConfig is one persisted cron job definition. Kind/Exec/TZ are omitempty
// so configs written before they existed load with zero values (prompt / safe /
// engine-default timezone).
type cronJobConfig struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Spec    string `json:"spec"`
	Prompt  string `json:"prompt"`
	Enabled bool   `json:"enabled"`
	Kind    string `json:"kind,omitempty"`
	Exec    bool   `json:"exec,omitempty"`
	TZ      string `json:"tz,omitempty"`
}

// skillConfig is one integration's non-secret settings + on/off state.
type skillConfig struct {
	Enabled  bool              `json:"enabled,omitempty"`
	Settings map[string]string `json:"settings,omitempty"`
}

// Built-in defaults. Embeddings default to a local Ollama — the lowest-
// friction way to get real vectors on a workstation.
const (
	defaultEmbedEndpoint = "http://localhost:11434"
	defaultEmbedModel    = "nomic-embed-text"
	defaultEmbedDim      = 768
	defaultVLLMToolFmt   = "gemma"
	currentSchemaVersion = 2
	// defaultPlanCeiling is the per-agent planner↔tool iteration cap when not
	// overridden. Raised from the original 8 — modern agentic tasks (research,
	// multi-tool synthesis) routinely need more steps, and each sub-agent has
	// its own budget. Still bounded so a looping model can't run forever.
	defaultPlanCeiling = 16
)

// --- provider registry ---------------------------------------------------

// providerKind is static metadata about a provider family: how to reach it,
// which Tenant backend factory serves it, and its auth/tokenizer quirks.
type providerKind struct {
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
}

// providerKinds is the catalog the wizard offers. OpenAI-compatible providers
// all route through the vllm backend factory (it speaks /v1/chat/completions);
// only the endpoint/key/paths differ. Anthropic is captured but not yet wired.
var providerKinds = map[string]providerKind{
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
	"anthropic": {
		ID: "anthropic", Label: "Anthropic (Claude)", Backend: "anthropic",
		DefaultEndpoint: "https://api.anthropic.com", DefaultModel: "claude-sonnet-4-20250514",
		NeedsKey: true, KeyEnv: "ANTHROPIC_API_KEY", EstimateTokens: true, Wired: true,
	},
	"echo": {
		ID: "echo", Label: "Echo (offline dev, deterministic)", Backend: "echo",
		Wired: true, Local: true,
	},
}

// providerOrder is the stable display order for the wizard menu.
var providerOrder = []string{"vllm", "ollama", "llamacpp", "openai", "grok", "zai", "zai-coding", "zai-coding-cn", "zai-metered", "anthropic", "echo"}

// --- persistence ----------------------------------------------------------

func launchConfigPath(cfgDir string) string { return filepath.Join(cfgDir, "config.json") }

// loadLaunchConfig reads + migrates config.json. Missing file → empty config
// (first run). Corrupt → error so the caller warns instead of wiping setup.
func loadLaunchConfig(cfgDir string) (*launchConfig, error) {
	lc := &launchConfig{}
	b, err := os.ReadFile(launchConfigPath(cfgDir))
	if os.IsNotExist(err) {
		return lc, nil
	}
	if err != nil {
		return lc, err
	}
	if err := json.Unmarshal(b, lc); err != nil {
		return &launchConfig{}, fmt.Errorf("config.json corrupt: %w", err)
	}
	lc.migrate()
	lc.sanitizeEndpoints()
	return lc, nil
}

// sanitizeEndpoints is the load-time guard against a pre-validation
// config containing a non-URL endpoint (most commonly: an API key
// pasted into the endpoint field via the bug `/model add zai <key>`
// that AddModel's URL gate now blocks).
//
// Policy: if a provider's saved endpoint isn't an http(s) URL, we
// REPLACE it with the catalog default for that kind (if known) and
// leave the auth alone. If no catalog default exists, the endpoint
// is BLANKED (so any later display shows "(unset)" via
// safeDisplayEndpoint, never the secret). The provider stays in the
// map so the operator can fix it with `/model add-cloud <kind> <key>`
// or by editing config.json.
//
// Mutation is in-memory only — we don't auto-save. Persisting the
// sanitized state happens on the next intentional save (e.g. the
// next `/model use`).
func (lc *launchConfig) sanitizeEndpoints() {
	for _, p := range lc.Providers {
		if p == nil {
			continue
		}
		ep := strings.TrimSpace(p.Endpoint)
		if ep == "" || looksLikeURL(ep) {
			continue
		}
		// Non-URL slipped in. Replace with catalog default if we have
		// one; else blank. NEVER preserve the bad value — that's how it
		// leaked into the model list / prompt context.
		if pk, ok := providerKinds[p.Kind]; ok && pk.DefaultEndpoint != "" {
			p.Endpoint = pk.DefaultEndpoint
		} else {
			p.Endpoint = ""
		}
	}
}

// migrate folds v1 flat fields into the v2 providers model, in-memory. The
// next save() persists the upgraded shape and drops the legacy fields.
func (lc *launchConfig) migrate() {
	if lc.Providers == nil && (lc.Backend != "" || lc.VLLMEndpoint != "") {
		kind := lc.Backend
		if kind == "" {
			kind = "vllm"
		}
		lc.Providers = map[string]*providerConfig{
			kind: {
				Kind:     kind,
				Endpoint: lc.VLLMEndpoint,
				Model:    lc.VLLMModel,
				ToolFmt:  lc.VLLMToolFmt,
			},
		}
		lc.Provider = kind
		if lc.EmbedEndpoint != "" || lc.EmbedModel != "" {
			lc.Embed = &providerConfig{
				Kind:     "ollama",
				Endpoint: lc.EmbedEndpoint,
				Model:    lc.EmbedModel,
				EmbedDim: lc.EmbedDim,
			}
		}
		if lc.SSEAddr != "" {
			lc.Gateway = gatewayConfig{Mode: "sse", SSEAddr: lc.SSEAddr}
		}
	}
	// Drop legacy fields so they aren't re-serialized.
	lc.Backend, lc.VLLMEndpoint, lc.VLLMModel, lc.VLLMToolFmt = "", "", "", ""
	lc.EmbedEndpoint, lc.EmbedModel, lc.EmbedDim, lc.SSEAddr = "", "", 0, ""
}

// save writes config.json atomically (temp + rename).
func (lc *launchConfig) save(cfgDir string) error {
	lc.SchemaVersion = currentSchemaVersion
	b, err := json.MarshalIndent(lc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		return err
	}
	return atomicWrite(launchConfigPath(cfgDir), b, 0o644)
}

// active returns the selected generation provider's config (or nil).
func (lc *launchConfig) active() *providerConfig {
	if lc == nil || lc.Provider == "" || lc.Providers == nil {
		return nil
	}
	return lc.Providers[lc.Provider]
}

// --- credentials (separate 0600 file) ------------------------------------

// credentials stores secrets the operator chose to keep on disk, away from
// the (often shared / committed) config.json. Keyed by an opaque id —
// provider id for model keys, "skill:<name>:<field>" for integration secrets.
type credentials struct {
	Secrets map[string]string `json:"secrets"`
}

func credentialsPath(cfgDir string) string { return filepath.Join(cfgDir, "credentials.json") }

func loadCredentials(cfgDir string) (*credentials, error) {
	c := &credentials{Secrets: map[string]string{}}
	b, err := os.ReadFile(credentialsPath(cfgDir))
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, c); err != nil {
		return &credentials{Secrets: map[string]string{}}, fmt.Errorf("credentials.json corrupt: %w", err)
	}
	if c.Secrets == nil {
		c.Secrets = map[string]string{}
	}
	return c, nil
}

// save writes credentials.json with 0600 perms (owner read/write only).
func (c *credentials) save(cfgDir string) error {
	if c.Secrets == nil {
		c.Secrets = map[string]string{}
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		return err
	}
	return atomicWrite(credentialsPath(cfgDir), b, 0o600)
}

func (c *credentials) get(id string) string { return c.Secrets[id] }
func (c *credentials) set(id, secret string) {
	if c.Secrets == nil {
		c.Secrets = map[string]string{}
	}
	c.Secrets[id] = secret
}

// resolveSecret returns the API key for a provider per its auth config:
// an env-var reference takes priority over a stored secret. Empty when the
// provider needs no key or none is configured.
func resolveSecret(cfgDir, providerID string, auth authCfg) string {
	if auth.KeyEnv != "" {
		if v := os.Getenv(auth.KeyEnv); v != "" {
			return v
		}
	}
	if auth.Stored {
		if creds, err := loadCredentials(cfgDir); err == nil {
			return creds.get(providerID)
		}
	}
	return ""
}

// atomicWrite writes via temp + rename so a crash can't truncate the file. The
// temp file is created with a UNIQUE name (os.CreateTemp) in the destination
// directory so concurrent writers — now possible since keys can be rotated live
// from the settings page, the credentials watcher, and /model at once — never
// share one ".tmp" path and clobber each other mid-write. The rename is atomic,
// so a concurrent reader always sees either the whole old or whole new file.
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	dir, base := filepath.Dir(path), filepath.Base(path)
	f, err := os.CreateTemp(dir, base+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// Best-effort cleanup if we bail before the rename.
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// --- helpers --------------------------------------------------------------

// servedModel is one entry from /v1/models. MaxLen is vLLM's reported
// max_model_len (0 when the server doesn't report it).
type servedModel struct {
	ID     string
	MaxLen int
}

// fetchModels queries an OpenAI-compatible models-list endpoint. vLLM,
// Ollama, OpenAI, etc. expose this, which is why the model (and, for
// vLLM, its context length) can be auto-detected instead of typed every
// launch.
//
// Two paths attempted in order:
//  1. <base>/v1/models   — standard OpenAI convention (works when base
//     does NOT already carry a version segment:
//     vllm/ollama/openai/grok/anthropic-vertex)
//  2. <base>/models      — works when base ALREADY carries the version:
//     Z.ai's /api/[coding/]paas/v4 routes; some
//     Azure deployments; bigmodel.cn
//
// Only one network call is paid on the happy path; the fallback fires
// only on 404. Either success short-circuits. Discovered 2026-05-26
// when Z.ai's `/api/coding/paas/v4/v1/models` 404'd while the chat
// completions endpoint at the same base worked fine — the probe lied
// about reachability and operators (correctly) didn't trust the swap.
func fetchModels(ctx context.Context, base, apiKey string) ([]servedModel, error) {
	trimmed := strings.TrimRight(base, "/")
	// Primary attempt — standard OpenAI convention.
	models, err := fetchModelsAt(ctx, trimmed+"/v1/models", apiKey)
	if err == nil {
		return models, nil
	}
	// Only fall back to /models on a 404 — that's the "you have the
	// wrong path" signal. For 401/connection-refused/500 the alternate
	// path won't help and we'd just double the operator's wait.
	if !isHTTP404(err) {
		return nil, err
	}
	models, err2 := fetchModelsAt(ctx, trimmed+"/models", apiKey)
	if err2 == nil {
		return models, nil
	}
	if isHTTP404(err2) {
		// Both 404 — operator needs to know we exhausted the conventions.
		return nil, fmt.Errorf("models list 404 at both %s/v1/models and %s/models", trimmed, trimmed)
	}
	return nil, err2
}

// isHTTP404 detects the "HTTP 404 ..." string we emit in fetchModelsAt.
// Kept narrow so we don't mis-retry on a 4xx that genuinely shouldn't
// fall through (e.g. 401 auth-missing — alternate path won't fix that).
func isHTTP404(err error) bool {
	return err != nil && strings.Contains(err.Error(), "HTTP 404")
}

// anthropicModelsAPIVersion mirrors internal/model/backend/anthropic's
// anthropicVersion — the value Anthropic requires in the anthropic-version
// header. Kept here (not imported) to avoid a cmd→backend dependency.
const anthropicModelsAPIVersion = "2023-06-01"

// setProviderAuth applies the correct auth scheme for a model-list request.
// Anthropic REJECTS Bearer auth ("Invalid bearer token", HTTP 401) and requires
// `x-api-key` + `anthropic-version` — the same headers the anthropic generate
// backend already uses. OpenAI-compatible providers (vLLM, OpenAI, Grok, Z.ai)
// use Bearer. Without this, `/model add-cloud anthropic` 401'd while listing
// models for the picker even though the stored key was valid (TEN-171).
func setProviderAuth(req *http.Request, rawURL, apiKey string) {
	if apiKey == "" {
		return
	}
	if isAnthropicEndpoint(rawURL) {
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", anthropicModelsAPIVersion)
		return
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
}

// isAnthropicEndpoint reports whether rawURL targets Anthropic's API host.
func isAnthropicEndpoint(rawURL string) bool {
	u, err := neturl.Parse(rawURL)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "api.anthropic.com" || strings.HasSuffix(h, ".anthropic.com")
}

func fetchModelsAt(ctx context.Context, url, apiKey string) ([]servedModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	setProviderAuth(req, url, apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	var out struct {
		Data []struct {
			ID          string `json:"id"`
			MaxModelLen int    `json:"max_model_len"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	models := make([]servedModel, 0, len(out.Data))
	for _, d := range out.Data {
		if d.ID != "" {
			models = append(models, servedModel{ID: d.ID, MaxLen: d.MaxModelLen})
		}
	}
	return models, nil
}

// fetchModelIDs returns just the served model ids (used by the setup wizard).
func fetchModelIDs(ctx context.Context, base, apiKey string) ([]string, error) {
	models, err := fetchModels(ctx, base, apiKey)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

// embedSetupHint is the stern, actionable warning shown when the embedding
// server is unreachable. It does NOT abort (the agent still runs), but it
// makes the degradation impossible to miss — semantic memory is a core
// pillar, not an optional extra.
const embedSetupHint = `╔══════════════════════════════════════════════════════════════════════╗
║  EMBEDDINGS DOWN — SEMANTIC MEMORY IS DISABLED                         ║
╚══════════════════════════════════════════════════════════════════════╝
Embedding server unreachable at %s.

This is NOT optional plumbing. Without it, Tenant falls back to a hash
stand-in: real vector recall is OFF, so the agent CANNOT meaningfully
search past episodes, recall stored facts, or surface relevant memories.
It will run, but it is effectively amnesiac. Fix this before relying on it.

Fix (Ollama — recommended):
  1. install Ollama       → https://ollama.com/download
  2. pull the model       → ollama pull %s
  3. ensure it's running  → ollama serve   (listens on :11434)
Or point elsewhere:       --embed-endpoint http://HOST:PORT --embed-model NAME
Or accept it explicitly:  --backend echo   (offline dev; no real memory)`
