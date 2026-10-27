// Package config is Tenant's persisted configuration (TEN-314): the
// machine-wide launch configuration in config.json, the owner-only
// credentials.json, and the per-agent settings.<agent>.json. It holds the
// types, their defaults and validation, and the only safe ways to read and
// write the files. It knows nothing of the runtime that consumes them.
//
// config.json and settings.<agent>.json are documented operator interfaces
// (ADR 0001): their JSON shape is stable, and a field is added, never
// renamed.
package config

import (
	"tenant/internal/orchestra"
	"tenant/internal/route"
)

// Launch is the persisted, machine-wide launch configuration written by
// `tenant setup` and merged into every command's flags. It exists so the
// operator configures endpoints + providers ONCE instead of repeating a wall
// of flags on every invocation.
//
// v2 layout (OpenClaw-style): a named set of model providers, an active
// selection, a separate embeddings provider, gateway/transport, and skill
// settings. v1 wrote flat fields (backend/vllm_*); those are still read and
// migrated forward on first load. Secrets never live here — see Credentials.
type Launch struct {
	SchemaVersion int `json:"schema_version,omitempty"`

	// InstanceID is this installation's stable identity (a UUID minted once at
	// first launch via ensureInstanceID). Exchanged at peer pairing (TEN-183)
	// and reused as the federation bus Origin (TEN-189). Never secret.
	InstanceID string `json:"instance_id,omitempty"`

	// Peer holds the Tenant-to-Tenant federation listener config (TEN-184).
	Peer Peer `json:"peer,omitempty"`

	// Provider is the active generation provider — a key into Providers.
	Provider string `json:"provider,omitempty"`
	// Providers holds each configured provider's settings, so switching is a
	// one-line change instead of a re-setup.
	Providers map[string]*Provider `json:"providers,omitempty"`
	// Embed is the embeddings provider (usually a local Ollama, separate from
	// the generation provider).
	Embed *Provider `json:"embed,omitempty"`
	// Route configures the local prompt classifier and its code-owned model-tier
	// policy. The rollout stage controls which calling surfaces may apply it.
	Route *Route `json:"route,omitempty"`
	// Gateway selects how `mcp-memory` is exposed.
	Gateway Gateway `json:"gateway,omitempty"`
	// Dashboard configures the web control panel (TEN-76).
	Dashboard Dashboard `json:"dashboard,omitempty"`
	// TurnScheduler bounds and apportions ingress to the single shared agent
	// used by serve-mode interactive and delegated turn sources (TEN-329).
	TurnScheduler TurnScheduler `json:"turn_scheduler,omitempty"`
	// Tailscale persists the operator's `/tailscale serve` choice (TEN-233) so it
	// is re-asserted at next launch — matching the dashboard/relay/imessage
	// toggles. Tailscale's own daemon also persists the serve config; this flag
	// makes Tenant restore it even if the tailnet config was cleared elsewhere.
	Tailscale Tailscale `json:"tailscale,omitempty"`
	// TUI keeps the terminal app's own choices (TEN-514).
	TUI TUI `json:"tui,omitzero"`
	// Relay configures the offsite Discord relay (TEN-114).
	Relay Relay `json:"relay,omitempty"`
	// IMessage holds the iMessage drive-allowlist (TEN-68 follow-up): the
	// handles permitted to drive the agent over iMessage. Deny-by-default.
	IMessage IMessage `json:"imessage,omitempty"`
	// Cron holds the recurring-job definitions managed via /cron, the cron_*
	// tools, and the dashboard's Cron section. Only definitions persist here;
	// run state (next/last run) is in-memory and recomputed on load.
	Cron Cron `json:"cron,omitempty"`
	// MCPRemotes holds remote MCP connector server URLs added via `/mcp add`,
	// so they re-register (as disabled stubs) at next launch. Non-secret; OAuth
	// tokens live in the mcpremote token cache, never here. (TEN-164)
	MCPRemotes []string `json:"mcp_remotes,omitempty"`
	// MCPTrust is each connector's live trust mode by server URL
	// (ask | allow | deny; absent = ask): whether its tools run without per-call
	// approval. Set at adoption (the mcp_add / /mcp add prompt) or any time via
	// /mcp trust and the dashboard; re-applied when the connector reconnects at
	// launch.
	MCPTrust map[string]string `json:"mcp_trust,omitempty"`
	// MCPServers are local MCP servers Tenant starts itself and talks to over
	// stdio (TEN-318). A server is third-party code, so it runs out of process
	// under the execution sandbox: the restricted backend unless the operator
	// chooses otherwise. Each appears as connector mcp:<name>, a stub until
	// /enable; its trust is under "stdio:<name>" in MCPTrust.
	MCPServers []MCPServer `json:"mcp_servers,omitempty"`
	// Improve holds self-improvement settings (TEN-152): the auto-accept policy
	// for machine-induced skills. Deny-by-default (empty = off).
	Improve Improve `json:"improve,omitempty"`
	// Skills holds per-integration settings (non-secret). Secrets go to the
	// credentials file under "skill:<name>:<field>".
	Skills map[string]*Skill `json:"skills,omitempty"`
	// Agents holds named sub-agent profiles for the orchestrator to spawn.
	// Each profile pins a SPECIFIC provider+model and an optional soul
	// (identity/persona markdown). When the orchestrator calls spawn_agent
	// with a role that matches one of these names, the spawned sub-agent
	// runs on the profile's chosen model with the profile's soul — instead
	// of inheriting the orchestrator's primary model + role-stamped base
	// soul. Lets you mix models per role: researcher → glm-4.6 on Z.ai,
	// synthesizer → aeon-ultimate on a self-hosted vLLM, etc.
	Agents map[string]*AgentProfile `json:"agents,omitempty"`
	// Budgets is the resource envelope for each agent-team run. Run limits
	// (agents, tool calls, tokens, cost, wall time) are off when omitted or 0;
	// the machine guards (concurrent sub-agents, browsers, bus history) get
	// orchestra.DefaultBudgets (TEN-508).
	Budgets orchestra.Budgets `json:"budgets,omitempty"`
	// PlanLoopCeiling caps planner↔tool iterations per turn (per agent). 0 =
	// built-in default (DefaultPlanCeiling). Raise it for multi-step agentic
	// tasks that legitimately need many tool calls.
	PlanLoopCeiling int `json:"plan_loop_ceiling,omitempty"`
	// RepeatGuard is how many rounds in a row of the same tool calls getting
	// the same results end a turn early with a forced synthesis (the
	// oscillation guard, TEN-261/TEN-506). 0 = built-in default (8), < 0 = off.
	// Live-tunable with /ceiling repeat.
	RepeatGuard int `json:"repeat_guard,omitempty"`
	// Judge is the decision model in the loop of a turn (TEN-510, TEN-516): it
	// triages each prompt and checks a task turn's answer before it goes out,
	// sending it back up to Rounds times. Off when nil or not Enabled.
	// Live-switchable with /judge. Its key, if the server wants one, lives in
	// credentials.json under "judge", never here.
	Judge *Judge `json:"judge,omitempty"`
	// LegacyJudgeLoop is the pre-TEN-516 spelling of Judge. Migrate moves it
	// into Judge and clears it; it is never written again. Kept so the key
	// stays readable (ADR 0001: a config key is never dropped).
	LegacyJudgeLoop *JudgeLoop `json:"judge_loop,omitempty"`
	// Fallbacks is the ordered list of provider names (keys into Providers) to
	// try when the active provider rate-limits / runs out of credits / is
	// unreachable (TEN-246). Empty (default) = no auto-fallback. e.g. with
	// provider "zai" + `"fallbacks": ["qwen-gpu"]`, a glm 429 transparently routes
	// to the self-hosted Qwen for that call; the preferred provider is retried first once
	// its cooldown lapses.
	Fallbacks []string `json:"fallbacks,omitempty"`
	// HealthGating turns on TEN-282's latency gating for the fallback chain: a
	// provider that keeps answering, but slowly, is skipped for a cooldown so
	// the next provider in Fallbacks serves. Nil or a zero slow_threshold_ms
	// keeps it off (TEN-358).
	HealthGating *HealthGating `json:"health_gating,omitempty"`
	// LazyTools enables on-demand tool loading (TEN-228): the per-turn tool array
	// carries only the ranked working set + a load_tool meta-tool, with a cheap
	// name+description catalog of the rest in the system prompt. Off by default
	// (additive). Set `"lazy_tools": true` in config.json to try it.
	LazyTools bool `json:"lazy_tools,omitempty"`
	// PerTurnToolRanking re-ranks the surfaced tools every turn instead of
	// keeping a sticky, append-only set per session (TEN-505). The sticky set
	// (default) keeps the tools array stable so a self-hosted model server's
	// prefix cache survives between turns; set this only to get the old
	// behaviour back.
	PerTurnToolRanking bool `json:"per_turn_tool_ranking,omitempty"`
	// BrowserNoSandbox is an insecure, container-only escape hatch for Chrome
	// environments that cannot use its process sandbox. The web plugin rejects
	// this setting unless it detects a Linux container or user namespace.
	BrowserNoSandbox bool `json:"browser_no_sandbox,omitempty"`
	// Goal holds /goal autonomous-loop settings (TEN-216). Persisted only — no
	// inline command sets it.
	Goal Goal `json:"goal,omitempty"`
	// Logs tunes the unified log book (internal/logs): where the per-source
	// folders live, how long files are kept, the level floor.
	Logs Logs `json:"logs,omitempty"`
	// Runs configures durable operational turn records.
	Runs RunTrace `json:"runs,omitempty"`
	// Privacy holds per-class retention (TEN-309); see `tenant privacy`.
	Privacy Privacy `json:"privacy,omitempty"`
	// UpdateCheck is serve's daily look for a newer release (TEN-500, D2):
	// a log-book event, never an update applied. Nil is on; false opts out.
	UpdateCheck *bool `json:"update_check,omitempty"`

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

// Logs is the log book's persisted tuning. Every field is optional.
type Logs struct {
	// Dir is the log root (default <data>/logs). One folder per source inside.
	Dir string `json:"dir,omitempty"`
	// RetentionDays deletes a source's files older than this (0 ⇒ 30; -1 ⇒ never).
	RetentionDays int `json:"retention_days,omitempty"`
	// Level is the floor for the files + feed: "debug", "info" (default),
	// "warn" or "error". TENANT_LOG=debug also lowers it for that launch.
	Level string `json:"level,omitempty"`
	// MaxFieldBytes caps one value (a tool result, an argument blob) per line
	// (0 ⇒ 4096; -1 ⇒ unlimited).
	MaxFieldBytes int `json:"max_field_bytes,omitempty"`
	// Store is ignored: since TEN-378 the log book always keeps every event
	// in <logs root>/logbook.db as well as the text files, and false only
	// draws a warning at start.
	Store *bool `json:"store,omitempty"`
	// The store's retention (docs/LOGGING.md), applied live. RetentionDays
	// above is every operational log's max age there too.
	// BudgetMB caps logbook.db and its WAL together (0 ⇒ 2048; -1 ⇒ no cap).
	BudgetMB int `json:"budget_mb,omitempty"`
	// Diagnostic limits every log's diagnostic events together (256 MB, 7 days).
	Diagnostic LogLimits `json:"diagnostic,omitzero"`
	// Security limits the Security log (256 MB, 365 days); its oldest records
	// are archived, never just deleted. It also names the chain's witness.
	Security SecurityLog `json:"security,omitzero"`
	// PerLog sets one log's policy, by log name, over its class default
	// (system: 128 MB, 90 days; any other: 64 MB, retention_days).
	PerLog map[string]LogPolicy `json:"per_log,omitempty"`
	// RateLimit caps how fast one provider may write one event (the
	// Security log is never limited), applied live.
	RateLimit LogRate `json:"rate_limit,omitzero"`
	// Alerts are the alert rules (docs/LOGGING.md), read live. An entry
	// named like a built-in rule (critical, unexpected-exit, audit-failures,
	// sandbox-unattended, logbook-full) changes or disables it.
	Alerts []AlertRule `json:"alerts,omitempty"`
}

// AlertRule is one entry of logs.alerts: a filter in the filter language,
// an optional threshold, an action and a throttle.
type AlertRule struct {
	Name      string         `json:"name"`
	Filter    string         `json:"filter,omitempty"`
	Threshold AlertThreshold `json:"threshold,omitzero"`
	Action    AlertAction    `json:"action,omitzero"`
	Throttle  string         `json:"throttle,omitempty"` // fire at most once per this long ("" ⇒ 15m; at least 1m)
	Disabled  bool           `json:"disabled,omitempty"`
}

// AlertThreshold fires a rule once Count events match within Window (a
// duration, 10m); without one, every match fires.
type AlertThreshold struct {
	Count  int    `json:"count,omitempty"`
	Window string `json:"window,omitempty"`
}

// AlertAction is what a firing does besides filing the alert, which the
// dashboard shows: Kind is dashboard (the default: nothing more), discord
// or imessage (a message to the operator), cron (a one-off prompt job,
// Target its prompt), or investigate (a read-only agent turn over the
// event, once the operator approves it; Target is "", discord or imessage,
// where its result also goes).
type AlertAction struct {
	Kind   string `json:"kind,omitempty"`
	Target string `json:"target,omitempty"`
}

// LogRate is a token bucket: PerSecond events a second (0 ⇒ 100; -1 ⇒ no
// limit), in bursts of up to Burst (0 ⇒ 500).
type LogRate struct {
	PerSecond float64 `json:"per_second,omitempty"`
	Burst     int     `json:"burst,omitempty"`
}

// LogLimits bounds records by size and age: 0 keeps the default, -1 lifts
// the limit.
type LogLimits struct {
	MaxMB      int `json:"max_mb,omitempty"`
	MaxAgeDays int `json:"max_age_days,omitempty"`
}

// SecurityLog is logs.security: LogLimits, plus the witness the chain's head
// is also sent to at each anchor (docs/LOGGING.md): "" (none), "discord"
// (the operator's DM), or the path of a file, best on another machine.
// "peer:<name>" is reserved; no peer call stores a head yet. OnFailure is
// what a gated action does when its audit record can't be written: continue
// (the default: it goes ahead, with a critical event and a banner) or deny
// (refused until records are written again). Read at each failure, so an
// edit applies at once.
type SecurityLog struct {
	MaxMB      int    `json:"max_mb,omitempty"`
	MaxAgeDays int    `json:"max_age_days,omitempty"`
	Witness    string `json:"witness,omitempty"`
	OnFailure  string `json:"on_failure,omitempty"`
}

// LogPolicy is one log's entry in logs.per_log; a key left out keeps the
// default.
type LogPolicy struct {
	MaxMB      int    `json:"max_mb,omitempty"`       // -1 ⇒ no size limit
	MaxAgeDays int    `json:"max_age_days,omitempty"` // -1 ⇒ kept forever
	MinLevel   string `json:"min_level,omitempty"`    // critical, error, warning, info or verbose: less severe events are not written
	OnFull     string `json:"on_full,omitempty"`      // overwrite, archive or keep
}

// Privacy is per-class retention (TEN-309).
type Privacy struct {
	// RetentionDays keeps each data class (episodes, facts, archive, usage,
	// research, screenshots, relay_inbox, eval_artifacts) this many days.
	// Absent or 0 keeps it forever, except relay_inbox, whose default is 30
	// days and which -1 keeps forever.
	RetentionDays map[string]int `json:"retention_days,omitempty"`
}

// RunTrace configures durable operational turn records.
type RunTrace struct {
	MaxAgeDays       int  `json:"max_age_days,omitempty"` // 0=90 default; negative disables age pruning
	MaxCount         int  `json:"max_count,omitempty"`    // 0=10000 default; negative disables count pruning
	RedactToolBodies bool `json:"redact_tool_bodies,omitempty"`
}

// HealthGating is the persisted form of model.HealthConfig. Latency is
// timed per call, from the request to the end of the answer, so the threshold
// has to sit above the normal time for a long reply. Omitted fields take the
// engine defaults.
type HealthGating struct {
	SlowThresholdMS int     `json:"slow_threshold_ms,omitempty"` // average call time that marks a provider slow; <=0 = off
	MinSamples      int     `json:"min_samples,omitempty"`       // successful calls before the average can trip; 0 = 5
	Alpha           float64 `json:"alpha,omitempty"`             // weight of the newest call in the average, (0,1]; 0 = 0.3
	CooldownMS      int     `json:"cooldown_ms,omitempty"`       // how long a slow provider is skipped; 0 = 30000
}

// EffectiveBudgets is the team-run resource envelope lc configures, with the
// defaults filled in.
func EffectiveBudgets(lc *Launch) orchestra.Budgets {
	if lc == nil {
		return orchestra.DefaultBudgets()
	}
	return lc.Budgets.WithDefaults()
}

// Goal holds persisted /goal autonomous-loop settings (TEN-216).
type Goal struct {
	// LoopCeiling is the per-turn planner↔tool iteration budget used WHILE a
	// goal loop is active, overriding the global PlanLoopCeiling for goal turns
	// only. >0 = that many iterations per goal turn; <0 = unlimited (omit the
	// per-turn cap so a long /goal run can iterate freely — bounded only by the
	// goal turn cap, errors, and Esc); 0/unset = inherit the global ceiling (no
	// goal-specific override). Normal, non-goal turns always use PlanLoopCeiling.
	LoopCeiling int `json:"loop_ceiling,omitempty"`
}

// TurnScheduler controls serve-mode admission to the shared agent.
// QueueDepth bounds unique waiting requests (the active turn is separate).
// SourceQuotas bounds queued requests per RouteSurface; omitted entries use
// the built-in quota for that source. Values <= 0 use the built-in default.
type TurnScheduler struct {
	QueueDepth   int            `json:"queue_depth,omitempty"`
	SourceQuotas map[string]int `json:"source_quotas,omitempty"`
}

// Provider is one model provider's connection + auth settings.
type Provider struct {
	Kind     string `json:"kind"`                  // see ProviderKinds
	Endpoint string `json:"endpoint,omitempty"`    // base URL (no /v1 suffix)
	Model    string `json:"model,omitempty"`       // empty = auto-detect at launch
	ToolFmt  string `json:"tool_format,omitempty"` // qwen|gemma|llama|mistral|glm|openai
	EmbedDim int    `json:"embed_dim,omitempty"`   // embeddings only
	Auth     Auth   `json:"auth,omitempty"`
	// Reasoning is the per-provider reasoning-effort hint for reasoning-capable
	// kinds (Sakana Fugu): "" (off, default) | "high" | "xhigh". Tuned live via
	// /reasoning and only honored when the kind's SupportsReasoning is set.
	Reasoning string `json:"reasoning,omitempty"`
	// Vision says whether the model can see images (TEN-493): "on", "off",
	// or "" / "auto" to let Tenant probe it. Set live with /model vision.
	Vision string `json:"vision,omitempty"`
	// RequestTimeout bounds one request to this endpoint (chat, tokenize, embed),
	// a Go duration like "90s". Empty = no limit beyond the turn's own. Read by
	// the OpenAI-compatible (vllm-family) backend only.
	RequestTimeout string `json:"request_timeout,omitempty"`
}

// Route is the persisted prompt-router configuration. Provider is
// intentionally narrower than Provider: the classifier is local-only,
// has no authentication surface, and speaks Ollama's native API. Pointer
// booleans distinguish an omitted deny-by-default setting from an explicit
// operator opt-out.
type Route struct {
	Enabled             bool                  `json:"enabled,omitempty"`
	Mode                string                `json:"mode,omitempty"`
	Assist              bool                  `json:"assist,omitempty"`
	Provider            *RouteProvider        `json:"provider,omitempty"`
	TimeoutMS           int                   `json:"timeout_ms,omitempty"`
	NeverCloud          *bool                 `json:"never_cloud,omitempty"`
	MinComplexityForBig int                   `json:"min_complexity_for_big,omitempty"`
	StickySession       *bool                 `json:"sticky_session,omitempty"`
	BigRole             string                `json:"big_role,omitempty"`
	ForceProfile        string                `json:"force_profile,omitempty"`
	Agents              map[string]RouteAgent `json:"agents,omitempty"`
	// Rules is the ordered N-way rule list (TEN-292): first match selects a
	// role; no match keeps the 2-way small/big behavior.
	Rules []RouteRule `json:"rules,omitempty"`
	// LogRetentionDays bounds the decision JSONL. 0 = default (90); negative
	// disables pruning explicitly.
	LogRetentionDays int `json:"log_retention_days,omitempty"`
}

// RouteProvider is the classifier's local endpoint and model.
type RouteProvider struct {
	Endpoint string `json:"endpoint,omitempty"`
	Model    string `json:"model,omitempty"`
}

// RouteAgent is an optional per-agent policy override keyed by AgentID
// under route.agents. NeverCloud remains tri-state here so surface-specific
// defaults (notably deny-by-default relays) can be applied by runtime wiring.
type RouteAgent struct {
	Mode       string `json:"mode,omitempty"`
	NeverCloud *bool  `json:"never_cloud,omitempty"`
}

// ResolvedRoute is the launch-ready route configuration consumed by
// runtime wiring. Keeping resolution here gives every surface the same
// defaults and, critically, the same local-only endpoint policy.
type ResolvedRoute struct {
	Enabled             bool
	Mode                string
	Assist              bool
	Endpoint            string
	Model               string
	TimeoutMS           int
	NeverCloud          bool
	MinComplexityForBig int
	StickySession       bool
	BigRole             string
	ForceProfile        string
	Agents              map[string]RouteAgent
	Rules               route.Rules
	LogRetentionDays    int
}

// AgentProfile is a named sub-agent recipe the orchestrator can spawn. The
// orchestrator calls spawn_agent(role=<name>, task) and the runtime looks
// the name up here — when found, it builds the sub-agent with this profile's
// pinned model and soul, instead of inheriting the orchestrator's primary
// model + the role-stamped base soul.
//
// Provider is the id of an entry in Launch.Providers (e.g. "zai",
// "gpu", "anthropic"). Model optionally overrides that provider's default
// model — leave blank to use the provider's configured Model. Soul is the
// agent's identity/persona as markdown; when blank, the role label is
// stamped onto the base operator soul (the existing behavior). Description
// is shown in /agents listings and helps the orchestrator know what each
// agent is for (it's also injected into the orchestrator's system prompt).
type AgentProfile struct {
	Provider    string `json:"provider"`              // provider id from Providers map
	Model       string `json:"model,omitempty"`       // override the provider's default model
	Soul        string `json:"soul,omitempty"`        // markdown identity/persona
	Description string `json:"description,omitempty"` // shown in /agents list
	// Builtin marks a persona shipped in the binary (TEN-132): read-only, and it
	// inherits the orchestrator's primary model (empty Provider). Never persisted.
	Builtin bool `json:"-"`
}

// Auth describes how to obtain a provider's API key. Either reference an
// env var (KeyEnv) or store it in the credentials file (Stored). Never both;
// KeyEnv wins if set.
type Auth struct {
	Mode   string `json:"mode,omitempty"`    // none|apikey|oauth
	KeyEnv string `json:"key_env,omitempty"` // env var name (reference mode)
	Stored bool   `json:"stored,omitempty"`  // secret saved in credentials.json
}

// Gateway selects the mcp-memory transport.
type Gateway struct {
	Mode    string `json:"mode,omitempty"`     // local | sse (a saved "both" loads as sse)
	SSEAddr string `json:"sse_addr,omitempty"` // when mode includes sse
}

// Dashboard configures the web control panel (TEN-76). Auth is an
// optional explicit override; otherwise launch uses the owner-only generated
// token under the config directory.
//
// Enabled is tri-state (TEN-86): nil ⇒ default ON (dashboard auto-launches
// with the TUI), &true ⇒ on, &false ⇒ off. The pointer lets a fresh config
// (no dashboard block) default to serving, while a `/dashboard off` choice
// persists as an explicit &false that survives the next launch.
type Dashboard struct {
	Enabled      *bool    `json:"enabled,omitempty"` // nil = default on; &false = explicitly off
	Addr         string   `json:"addr,omitempty"`    // listen address (default 127.0.0.1:9770)
	TLSCert      string   `json:"tls_cert,omitempty"`
	TLSKey       string   `json:"tls_key,omitempty"`
	Auth         string   `json:"auth,omitempty"`          // explicit override; empty uses dashboard-auth-token
	AllowedHosts []string `json:"allowed_hosts,omitempty"` // trusted reverse-proxy Host values
}

// IsEnabled resolves the tri-state Enabled to a concrete on/off: nil
// (unset) defaults to ON, otherwise the stored bool wins.
func (dc Dashboard) IsEnabled() bool {
	return dc.Enabled == nil || *dc.Enabled
}

// Tailscale persists the `/tailscale serve` choice (TEN-233). Serve=true
// ⇒ re-assert `tailscale serve` for the dashboard port at launch (best-effort).
type Tailscale struct {
	Serve bool `json:"serve,omitempty"`
}

// TUI is the terminal app's saved choices. Feed is "off" when the operator
// hid the activity feed (`/feed off`, Alt+F); anything else shows it.
type TUI struct {
	Feed string `json:"feed,omitempty"`
}

// Peer holds Tenant-to-Tenant federation listener settings (TEN-184).
// Empty Listen ⇒ federation off (no listener bound). Transport "overlay" means
// the operator runs an overlay network (Tailscale/WireGuard) and plain HTTP is
// permitted on it; otherwise a non-loopback bind is refused until TLS (TEN-185).
type Peer struct {
	Listen    string `json:"listen,omitempty"`
	Transport string `json:"transport,omitempty"` // "" | "overlay"
}

// Relay is the offsite Discord relay's persisted state (TEN-114): whether
// it auto-starts and the single operator's Discord user id. Default OFF — the
// relay is an explicit opt-in (exposes the agent over a third-party network).
type Relay struct {
	Enabled    bool   `json:"enabled,omitempty"`     // default false = opt-in
	OperatorID string `json:"operator_id,omitempty"` // the single allowed Discord user id
	// AllowExec is the relay's exec mode (TEN-123; enforced at the Discord
	// broker again since TEN-338). False, the default: exec, write,
	// destructive, sandbox and send are refused offsite whatever Permissions or
	// a tool rule says; Discord's own post/react still follow send. True: those
	// categories follow Permissions.
	AllowExec bool `json:"allow_exec,omitempty"`
	// Permissions is the per-category ask|allow|deny map for the Discord agent's
	// gated tools (TEN-231), driven by /relay permissions with the SAME model as
	// the global /permissions and /imessage permissions. Empty ⇒ default ASK
	// (every dangerous action prompts a button). Categories: exec/write/
	// destructive/web/send/egress/mcp/sandbox; AllowExec gates five of them.
	Permissions map[string]string `json:"permissions,omitempty"`
	// KnownCategories is the Discord broker's chosen-categories marker, as
	// Settings.KnownCategories is the host's.
	KnownCategories []string `json:"known_categories,omitempty"`
	// ApprovalTimeout is how long an approval card waits for the operator before
	// auto-denying (a Go duration, e.g. "10m"). Empty ⇒ 10m. /relay timeout.
	ApprovalTimeout string `json:"approval_timeout,omitempty"`
	// CardStyle selects the approval card layout: "v2" (a Components-v2
	// container card — the default) or "legacy" (plain content + a button row).
	// v2 falls back to legacy on its own if Discord rejects the post.
	CardStyle string `json:"card_style,omitempty"`
}

// IMessage holds the iMessage drive-allowlist: the normalized handles
// (phone numbers / emails) permitted to DRIVE the agent over iMessage once the
// inbound responder (Layer 2) is live. DENY-BY-DEFAULT — an empty/missing list
// permits NOBODY (the safe default for "no unrestricted access"). Edited live
// via the /imessage TUI command; the inbound responder must gate every message
// on imessage.AllowList.Allows before invoking any tool. Stored here (not in
// the runtime Meta KV) because it is operator policy configured once, like the
// Discord relay's OperatorID — not hot per-poll state.
type IMessage struct {
	AllowFrom []string `json:"allow_from,omitempty"`
	// Enabled starts the autonomous responder at launch (TEN-230): poll chat.db,
	// drive an agent turn per inbound text from an allowed handle, reply. Native
	// transport only (macOS); off by default (deny-by-default). AllowFrom gates
	// who may drive it.
	Enabled bool `json:"enabled,omitempty"`
	// Passive keeps the running responder read-only (TEN-485): every text is
	// observed (imessage log + activity feed) and none drives a turn or gets a
	// reply, even from AllowFrom or the Operator. Toggled live via
	// /imessage passive | respond.
	Passive bool `json:"passive,omitempty"`
	// Operator is the handle (phone/email) whose YES approves gated tools in the
	// Phase-2 text-confirm flow — distinct from AllowFrom so an allowlisted
	// contact can chat but not approve dangerous actions. Unset ⇒ an "ask"
	// category prompts at the host instead.
	Operator string `json:"operator,omitempty"`
	// Permissions is the per-category ask|allow|deny policy for the responder's
	// gated tools (TEN-230) — same categories/modes as the global /permissions,
	// set via /imessage permissions. Empty ⇒ deny-by-default (offsite). Category
	// keys: exec | write | destructive | web | send.
	Permissions map[string]string `json:"permissions,omitempty"`
	// KnownCategories is the responder broker's chosen-categories marker, as
	// Settings.KnownCategories is the host's.
	KnownCategories []string `json:"known_categories,omitempty"`
}

// Improve governs the self-improvement loop's auto-acceptance of induced
// skills (TEN-152). AutoAccept: "" / "off" (default — every induced skill waits
// for manual /skills accept), "on" (accept all new skills), or "trusted" (accept
// only while recent operator feedback is healthy — ≥ TrustMinAcks acks and zero
// undos). TrustMinAcks: threshold for "trusted" (0 ⇒ default 5).
type Improve struct {
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
	// ReflectEvery persists the ReflectionJob cadence (TEN-255 Phase 3) as a
	// duration string. Off (empty/0/malformed) by default — it synthesizes the
	// per-project SME doc on the proposer model. The doc is loud + reversible
	// (versioned, dashboard-inspectable); turn it on for dogfooding.
	ReflectEvery string `json:"reflect_every,omitempty"`
	// QualityModel persists the Quality model override (TEN-91, renamed in
	// TEN-516): the model that grades eval answers. Empty ⇒ the default (the
	// planner / main-agent model grades its own answers). Set ⇒ a separate
	// model grades in BOTH `tenant eval` and the nightly eval job.
	// QualityModelKind selects the backend (any wired ProviderKinds id —
	// anthropic | openai | zai | …); QualityModelEndpoint optionally overrides
	// the catalog default; the API key is read at runtime from
	// $QualityModelKeyEnv and is NEVER stored here. A --quality-model flag still
	// overrides this per-invocation.
	QualityModel         string `json:"quality_model,omitempty"`          // model id ("" = planner default)
	QualityModelKind     string `json:"quality_model_kind,omitempty"`     // provider kind (default "anthropic")
	QualityModelEndpoint string `json:"quality_model_endpoint,omitempty"` // "" = provider default
	QualityModelKeyEnv   string `json:"quality_model_key_env,omitempty"`  // env var holding the key (default per kind)
	// LegacyJudge* are the pre-TEN-516 spellings of QualityModel*. Migrate
	// moves them and clears them; they are never written again (ADR 0001).
	LegacyJudge         string `json:"judge,omitempty"`
	LegacyJudgeKind     string `json:"judge_kind,omitempty"`
	LegacyJudgeEndpoint string `json:"judge_endpoint,omitempty"`
	LegacyJudgeKeyEnv   string `json:"judge_key_env,omitempty"`
}

// ValidAutoAccept reports whether m is an accepted auto-accept mode.
func ValidAutoAccept(m string) bool {
	switch m {
	case "", "off", "on", "trusted":
		return true
	}
	return false
}

// Cron persists the recurring-job DEFINITIONS plus a few engine-wide
// settings. Run state (next/last run, history) is NOT here — history lives in
// <dataDir>/cron-history.json so config.json isn't rewritten on every run.
type Cron struct {
	Jobs []CronJob `json:"jobs,omitempty"`
	// Timezone is the default IANA zone for jobs without their own TZ. Empty =
	// server local time.
	Timezone string `json:"timezone,omitempty"`
	// AllowExec is the GLOBAL kill-switch for dangerous cron jobs: shell jobs and
	// exec-opted-in prompt jobs only RUN when this is true. Deny-by-default.
	AllowExec bool `json:"allow_exec,omitempty"`
	// Catchup, when true, fires a SAFE job once on startup if it missed a cycle
	// while the process was down (instead of waiting for the next occurrence).
	Catchup bool `json:"catchup,omitempty"`
	// PerRunMinutes caps one job run's wall clock. 0 = the built-in default
	// (15m; see cronPerRun).
	PerRunMinutes int `json:"per_run_minutes,omitempty"`
}

// CronJob is one persisted cron job definition. Kind/Exec/TZ are omitempty
// so configs written before they existed load with zero values (prompt / safe /
// engine-default timezone).
type CronJob struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Spec    string `json:"spec"`
	Prompt  string `json:"prompt"`
	Enabled bool   `json:"enabled"`
	Kind    string `json:"kind,omitempty"`
	Exec    bool   `json:"exec,omitempty"`
	TZ      string `json:"tz,omitempty"`
}

// Skill is one integration's non-secret settings + on/off state.
type Skill struct {
	Enabled  bool              `json:"enabled,omitempty"`
	Settings map[string]string `json:"settings,omitempty"`
}

// Built-in defaults. Embeddings default to a local Ollama — the lowest-
// friction way to get real vectors on a workstation.
const (
	DefaultEmbedEndpoint            = "http://localhost:11434"
	DefaultEmbedModel               = "nomic-embed-text"
	DefaultEmbedDim                 = 768
	DefaultRouteMode                = "shadow"
	DefaultRouteTimeoutMS           = 250
	DefaultRouteModel               = route.DefaultModel
	DefaultRouteNeverCloud          = route.DefaultNeverCloud
	DefaultRouteMinComplexityForBig = route.DefaultMinComplexityForBig
	DefaultRouteStickySession       = route.DefaultStickySession
	DefaultVLLMToolFmt              = "gemma"
	CurrentSchemaVersion            = 2
	// DefaultPlanCeiling is the per-agent planner↔tool iteration cap when not
	// overridden. Raised from the original 8 — modern agentic tasks (research,
	// multi-tool synthesis) routinely need more steps, and each sub-agent has
	// its own budget. Still bounded so a looping model can't run forever.
	DefaultPlanCeiling = 16
)

// Judge configures the decision model in the loop of a turn (TEN-510,
// TEN-516). Kind is the server's dialect ("clef" today; every kind speaks the
// System One API); the endpoint is always just an endpoint, defaulted per
// kind and overridable.
type Judge struct {
	Enabled  bool   `json:"enabled"`
	Kind     string `json:"kind,omitempty"`     // "" = clef
	Endpoint string `json:"endpoint,omitempty"` // a System One server, e.g. http://host:30000
	Model    string `json:"model,omitempty"`    // "" = the kind's default (clef-flash)
	Rounds   int    `json:"rounds,omitempty"`   // send-backs per turn, 1..JudgeMaxRounds; 0 = JudgeDefaultRounds
}

// JudgeLoop is the pre-TEN-516 shape of Judge, read for migration only.
type JudgeLoop struct {
	Enabled   bool   `json:"enabled"`
	Endpoint  string `json:"endpoint,omitempty"`
	Model     string `json:"model,omitempty"`
	MaxRounds int    `json:"max_rounds,omitempty"`
}

const (
	// JudgeDefaultRounds is how many times a turn is sent back when the file
	// says nothing; JudgeMaxRounds bounds a hand-edited value (each round is a
	// free planner iteration).
	JudgeDefaultRounds = 3
	JudgeMaxRounds     = 5
)

// RoundsOrDefault is Rounds clamped to 1..JudgeMaxRounds, JudgeDefaultRounds
// when unset.
func (j *Judge) RoundsOrDefault() int {
	if j == nil || j.Rounds <= 0 {
		return JudgeDefaultRounds
	}
	return min(j.Rounds, JudgeMaxRounds)
}
