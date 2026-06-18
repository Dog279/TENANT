package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"tenant/internal/memory/archive"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/memory/soul"
	"tenant/internal/model"
	"tenant/internal/model/backend/anthropic"
	"tenant/internal/model/backend/echo"
	"tenant/internal/model/backend/vllm"
)

// commonFlags holds the flags every subcommand shares.
type commonFlags struct {
	backend string
	agent   string
	dataDir string
	cfgDir  string

	// vLLM overrides. When backend=vllm and these are set, the CLI
	// builds an in-memory registry: generation roles point at this
	// endpoint, the embedder role falls back to echo (a generation-
	// only vLLM has no /v1/embeddings — the role-routing design
	// handles heterogeneous backends per role).
	vllmEndpoint string
	vllmModel    string
	vllmToolFmt  string

	// Embedder overrides. When set (backend=vllm), the embedder role
	// points at this OpenAI-compatible /v1/embeddings endpoint
	// (vLLM OR Ollama — same wire shape) instead of the echo stand-in.
	// Real vectors → meaningful semantic retrieval.
	embedEndpoint string
	embedModel    string
	embedDim      int

	// Resolved at config-merge time (not raw flags):
	genKind     string // active provider kind (vllm|openai|ollama|…); "" = raw --backend
	genAPIKey   string // resolved generation API key (env ref or stored secret or --api-key)
	embedAPIKey string // resolved embeddings API key (usually empty — local)
	contextLen  int    // served model's max_model_len (auto-detected); 0 = use default
	planCeiling int    // planner↔tool iteration cap per turn; 0 = defaultPlanCeiling

	// fs is the FlagSet these flags were bound to. resolve() uses
	// fs.Visit to tell which flags the operator set EXPLICITLY, so the
	// precedence is: explicit flag > persisted config.json > default.
	fs *flag.FlagSet
	// lc is the persisted launch config, loaded once by resolve(). May be
	// empty (first run). Cached so callers (e.g. mcp-memory's --sse-addr)
	// can read it without re-reading the file.
	lc *launchConfig
}

// bindCommon registers the shared flags on fs and returns a pointer
// whose fields are populated after fs.Parse.
func bindCommon(fs *flag.FlagSet) *commonFlags {
	c := &commonFlags{}
	fs.StringVar(&c.backend, "backend", "echo", "inference backend: echo|vllm")
	fs.StringVar(&c.agent, "agent", "main", "agent id")
	fs.StringVar(&c.dataDir, "data", "", "data dir (default OS data dir)")
	fs.StringVar(&c.cfgDir, "config", "", "config dir (default OS config dir)")
	fs.StringVar(&c.vllmEndpoint, "vllm-endpoint", "", "vLLM base URL, e.g. http://localhost:8000 (backend=vllm)")
	fs.StringVar(&c.vllmModel, "vllm-model", "", "vLLM model id, e.g. gemma-4-26b (backend=vllm)")
	fs.StringVar(&c.vllmToolFmt, "vllm-tool-format", "gemma", "tool format for the vLLM model: qwen|gemma|llama|mistral|openai")
	fs.StringVar(&c.embedEndpoint, "embed-endpoint", "", "OpenAI-compat embeddings base URL, e.g. http://localhost:11434 (Ollama) — backend=vllm")
	fs.StringVar(&c.embedModel, "embed-model", "", "embedding model id, e.g. nomic-embed-text")
	fs.IntVar(&c.embedDim, "embed-dim", defaultEmbedDim, "embedding dimensionality (advisory; nomic-embed-text=768, bge-m3=1024)")
	fs.StringVar(&c.genAPIKey, "api-key", "", "API key for a hosted provider (overrides config/credentials; prefer `tenant setup`)")
	fs.IntVar(&c.planCeiling, "plan-loop-ceiling", 0, "max planner↔tool iterations per turn, per agent (0 = default 16)")
	c.fs = fs
	return c
}

// resolveDirs fills in default OS dirs when not overridden, expanding a
// leading ~ (Go/PowerShell don't always do this). Split out so the setup
// wizard can resolve dirs WITHOUT the config auto-merge.
func (c *commonFlags) resolveDirs() error {
	c.dataDir = expandPath(c.dataDir)
	c.cfgDir = expandPath(c.cfgDir)
	if c.dataDir == "" {
		d, err := archive.DefaultDir()
		if err != nil {
			return fmt.Errorf("resolve data dir: %w", err)
		}
		c.dataDir = d
	}
	if c.cfgDir == "" {
		d, err := soul.DefaultDir()
		if err != nil {
			return fmt.Errorf("resolve config dir: %w", err)
		}
		c.cfgDir = d
	}
	if err := os.MkdirAll(c.dataDir, 0o755); err != nil {
		return fmt.Errorf("mkdir data dir: %w", err)
	}
	return nil
}

// resolve fills default dirs, then merges persisted config + smart defaults.
func (c *commonFlags) resolve() error {
	if err := c.resolveDirs(); err != nil {
		return err
	}

	// Merge persisted config (explicit flags win), then apply smart
	// defaults, then auto-detect the vLLM model if still unset. This is
	// what lets everyday launch be `tenant tui` with no flags.
	c.applyLaunchConfig()
	c.applyDefaults()
	if c.backend == "vllm" && c.vllmEndpoint != "" && c.vllmModel == "" {
		c.autodetectModel()
	}
	return nil
}

// flagSet reports which flags the operator passed EXPLICITLY on the command
// line (vs left at default), so persisted config doesn't clobber an explicit
// override.
func (c *commonFlags) flagSet() map[string]bool {
	set := map[string]bool{}
	if c.fs != nil {
		c.fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	}
	return set
}

// applyLaunchConfig loads config.json (v2 providers model) and fills any flag
// the operator did not set explicitly from the active provider + embeddings
// provider. Precedence: explicit flag > config.json > default.
func (c *commonFlags) applyLaunchConfig() {
	lc, err := loadLaunchConfig(c.cfgDir)
	if err != nil {
		// Corrupt file: warn on stderr, keep flag defaults rather than
		// silently honoring a broken config.
		fmt.Fprintf(os.Stderr, "warning: %v (ignoring config.json)\n", err)
		lc = &launchConfig{}
	}
	c.lc = lc
	set := c.flagSet()

	if p := lc.active(); p != nil {
		pk := providerKinds[p.Kind]
		c.genKind = p.Kind
		if !set["backend"] && pk.Backend != "" {
			c.backend = pk.Backend
		}
		if !set["vllm-endpoint"] {
			c.vllmEndpoint = firstNonEmpty(p.Endpoint, pk.DefaultEndpoint)
		}
		if !set["vllm-model"] {
			c.vllmModel = p.Model
		}
		if !set["vllm-tool-format"] {
			c.vllmToolFmt = firstNonEmpty(p.ToolFmt, pk.DefaultToolFmt)
		}
		if !set["api-key"] {
			c.genAPIKey = resolveSecret(c.cfgDir, lc.Provider, p.Auth)
		}
	}

	if e := lc.Embed; e != nil {
		ek := providerKinds[e.Kind]
		if !set["embed-endpoint"] {
			c.embedEndpoint = firstNonEmpty(e.Endpoint, ek.DefaultEndpoint, defaultEmbedEndpoint)
		}
		if !set["embed-model"] {
			c.embedModel = e.Model
		}
		if !set["embed-dim"] && e.EmbedDim != 0 {
			c.embedDim = e.EmbedDim
		}
		c.embedAPIKey = resolveSecret(c.cfgDir, "embed", e.Auth)
	}

	if !set["plan-loop-ceiling"] && lc.PlanLoopCeiling != 0 {
		c.planCeiling = lc.PlanLoopCeiling
	}
}

// applyDefaults fills the zero-config defaults that make a model backend
// usable on its own: a default model for the active provider kind, and
// embeddings pointed at a local Ollama with nomic-embed-text.
func (c *commonFlags) applyDefaults() {
	if c.backend == "echo" {
		return
	}
	pk := providerKinds[c.genKind]
	if c.backend == "vllm" && c.vllmToolFmt == "" {
		c.vllmToolFmt = firstNonEmpty(pk.DefaultToolFmt, defaultVLLMToolFmt)
	}
	if c.vllmModel == "" && pk.DefaultModel != "" {
		c.vllmModel = pk.DefaultModel
	}
	// Embeddings default to a local Ollama for any real generation backend
	// (vLLM, hosted OpenAI-compat, or Anthropic — Claude has no embeddings).
	if c.embedEndpoint == "" {
		c.embedEndpoint = defaultEmbedEndpoint
	}
	if c.embedModel == "" {
		c.embedModel = defaultEmbedModel
	}
	if c.embedDim == 0 {
		c.embedDim = defaultEmbedDim
	}
}

// autodetectModel asks a LOCAL endpoint which model it serves so the operator
// never has to type --vllm-model. Only meaningful for single-model local
// servers (vLLM/Ollama/llama.cpp); hosted providers list dozens of models, so
// we rely on the provider's DefaultModel there instead. Best-effort.
func (c *commonFlags) autodetectModel() {
	pk := providerKinds[c.genKind]
	// genKind=="" means a raw `--backend vllm` with no provider entry — treat
	// as a local single-model server (the original behavior).
	if c.genKind != "" && !pk.Local {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if ms, err := fetchModels(ctx, c.vllmEndpoint, c.genAPIKey); err == nil && len(ms) > 0 {
		c.vllmModel = ms[0].ID
		// Adapt the context budget to whatever the server actually serves, so
		// we never over-pack a smaller-context model (e.g. a 32K Qwen) and get
		// 400s on long prompts.
		if ms[0].MaxLen > 0 {
			c.contextLen = ms[0].MaxLen
		}
	}
}

// logger goes to STDERR always. Stdout is reserved: for mcp-memory it
// is the JSON-RPC channel; for chat it is the conversation. Logs must
// never contaminate it.
func newLogger() *slog.Logger {
	// TENANT_LOG=debug|info surfaces the agent/backend internals —
	// essential for diagnosing model tool-format adherence in the field.
	lvl := slog.LevelWarn
	switch strings.ToLower(os.Getenv("TENANT_LOG")) {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

// logLevel resolves the TENANT_LOG level (default WARN).
func logLevel() slog.Level {
	switch strings.ToLower(os.Getenv("TENANT_LOG")) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	}
	return slog.LevelWarn
}

// newFileLogger routes logs to a file instead of stderr — required by
// the TUI, whose alt-screen would be corrupted by stderr writes. Falls
// back to discarding logs if the file can't be opened.
func newFileLogger(path string) (*slog.Logger, func()) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil)), func() {}
	}
	return slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: logLevel()})), func() { _ = f.Close() }
}

// buildRouter constructs a model.Router for the chosen backend.
//
//   - echo: in-memory registry, echo profile per role (offline).
//   - vllm + --vllm-endpoint set: generation roles point at that
//     endpoint/model; the embedder role uses echo (a gen-only vLLM
//     has no embeddings endpoint). Both backend factories registered;
//     the Router resolves each role to its profile's backend. This is
//     the heterogeneous role-routing the eng review designed.
//   - vllm without --vllm-endpoint: shipped embedded profiles
//     (localhost defaults), vllm backend only.
//
// routerBuildQuiet suppresses buildRouter's embed-unreachable warning to
// os.Stderr. Set true around a live /model swap (the TUI owns the screen).
var routerBuildQuiet bool

func buildRouter(c *commonFlags, log *slog.Logger) (*model.Router, error) {
	switch c.backend {
	case "echo":
		reg := model.NewEmptyRegistry()
		for _, p := range echoProfiles() {
			if err := reg.Add(p); err != nil {
				return nil, fmt.Errorf("register echo profile %s: %w", p.ID, err)
			}
		}
		r := model.NewRouter(reg, log)
		r.RegisterBackend("echo", echo.New)
		return r, nil

	case "vllm":
		// No endpoint configured at all → fall back to the shipped
		// embedded profiles (localhost defaults). Keeps zero-arg dev working.
		if c.vllmEndpoint == "" {
			reg, err := model.NewRegistry("")
			if err != nil {
				return nil, fmt.Errorf("load vllm registry: %w", err)
			}
			r := model.NewRouter(reg, log)
			r.RegisterBackend("vllm", vllm.New)
			return r, nil
		}
		// Endpoint set but model still unknown (auto-detect failed) → a
		// clear, actionable error beats silently loading wrong profiles.
		if c.vllmModel == "" {
			return nil, fmt.Errorf("could not determine vLLM model at %s — is the server up? "+
				"pass --vllm-model, or run `tenant setup`", c.vllmEndpoint)
		}
		pk := providerKinds[c.genKind]
		reg := model.NewEmptyRegistry()
		gen := genProfileOpts{
			endpoint:     c.vllmEndpoint,
			model:        c.vllmModel,
			toolFmt:      c.vllmToolFmt,
			apiKey:       c.genAPIKey,
			chatPath:     pk.ChatPath,
			estimateOnly: pk.EstimateTokens,
			forceHTTP1:   pk.ForceHTTP1,
			contextLen:   c.contextLen,
			planCeiling:  c.planCeiling,
		}
		for _, p := range vllmGenProfiles(gen) {
			if err := reg.Add(p); err != nil {
				return nil, fmt.Errorf("register vllm profile %s: %w", p.ID, err)
			}
		}
		// Generation is vLLM; the embedder backend is also vLLM (Ollama).
		return finishModelRouter(reg, c, log, map[string]model.BackendFactory{"vllm": vllm.New})

	case "anthropic":
		if c.genAPIKey == "" {
			return nil, fmt.Errorf("Anthropic provider needs an API key — run `tenant setup` " +
				"(paste a key or reference an env var like ANTHROPIC_API_KEY)")
		}
		if c.vllmModel == "" {
			return nil, fmt.Errorf("Anthropic provider needs a model — run `tenant setup` (e.g. claude-sonnet-4-20250514)")
		}
		reg := model.NewEmptyRegistry()
		for _, p := range anthropicGenProfiles(c.vllmEndpoint, c.vllmModel, c.genAPIKey, c.planCeiling) {
			if err := reg.Add(p); err != nil {
				return nil, fmt.Errorf("register anthropic profile %s: %w", p.ID, err)
			}
		}
		// Generation is Anthropic; embeddings still come from the local
		// OpenAI-compatible server (vLLM/Ollama), so register both factories.
		return finishModelRouter(reg, c, log, map[string]model.BackendFactory{"anthropic": anthropic.New})

	default:
		return nil, fmt.Errorf("unknown backend %q (use echo or vllm)", c.backend)
	}
}

// --- resilient launch (interactive only) --------------------------------
//
// buildRouter aborts the process when the configured model can't be built (down
// server, missing key, corrupt config). That's correct for headless commands
// (eval/serve/research/doctor) but wrong for the interactive TUI, where the
// operator has /model to fix it live. buildRouterResilient degrades to the
// pure-Go echo backend so the TUI still opens. NEVER call it from a headless
// command — buildRouter stays fail-fast there.

// degradeClass describes why generation degraded: it drives banner wording and
// whether reconnect polling can ever recover (a missing key won't).
type degradeClass int

const (
	degradeNone         degradeClass = iota
	degradeReachability              // server down / auto-detect failed → poll to recover
	degradeCredential                // missing API key → operator must add a key; do NOT poll
	degradeConfig                    // unknown backend / missing model → fix config; do NOT poll
)

// degradedState is the SINGLE source of truth for "running on the echo fallback
// because the real model couldn't be built." One instance is shared by the TUI
// model display, the self-improve scheduler, the cron engine, and the relay —
// so autonomous actors suppress destructive work while it's set. Cleared only on
// a reachable /model use.
type degradedState struct {
	on    atomic.Bool
	class degradeClass // written once at boot, read-only thereafter
	cause string       // original buildRouter error, for the banner
}

// Degraded reports whether we're on the echo fallback. Nil-safe so a normal
// (non-degraded) launch can pass a nil *degradedState everywhere.
func (d *degradedState) Degraded() bool { return d != nil && d.on.Load() }

func (d *degradedState) clear() {
	if d != nil {
		d.on.Store(false)
	}
}

// buildRouterResilient wraps buildRouter for INTERACTIVE launch only. On a
// model-related failure it degrades to an echo router so the TUI still opens,
// returning a shared *degradedState (nil when healthy) and a banner. It returns
// an error ONLY for a non-model failure the echo branch can't paper over (e.g.
// embedder registry I/O) — those still abort.
func buildRouterResilient(c *commonFlags, log *slog.Logger) (*model.Router, *degradedState, string, error) {
	r, err := buildRouter(c, log)
	if err == nil {
		return r, nil, "", nil
	}
	cause := err.Error()

	// Degrade to echo (pure-Go, no network). Copy c so we don't mutate the
	// caller's backend here — the call site sets c.backend="echo" deliberately
	// for the status bar, but the real provider must stay in config.json.
	ec := *c
	ec.backend = "echo"
	er, eerr := buildRouter(&ec, log)
	if eerr != nil {
		return nil, nil, "", fmt.Errorf("model unavailable and echo fallback failed: %w (cause: %s)", eerr, cause)
	}

	ds := &degradedState{class: classifyDegrade(cause), cause: cause}
	ds.on.Store(true)
	return er, ds, degradeBanner(c.genKind, ds.class, cause), nil
}

// classifyDegrade buckets a buildRouter error (by its stable, operator-facing
// message) so the banner + reconnect policy match the fault. An unrecognized
// cause defaults to reachability (degrade + poll) — safe, since polling is
// read-only.
func classifyDegrade(cause string) degradeClass {
	switch {
	case strings.Contains(cause, "needs an API key"):
		return degradeCredential
	case strings.Contains(cause, "unknown backend"), strings.Contains(cause, "needs a model"):
		return degradeConfig
	case strings.Contains(cause, "is the server up"), strings.Contains(cause, "could not determine"):
		return degradeReachability
	default:
		return degradeReachability
	}
}

// degradeBanner is the un-missable startup notice. It is explicit that this is
// echo (no real LLM, no tool execution) and gives the class-specific fix.
func degradeBanner(genKind string, class degradeClass, cause string) string {
	const head = "⚠ model %q unavailable — running on ECHO (no real LLM, no tool execution). "
	switch class {
	case degradeCredential:
		return fmt.Sprintf(head+"Credential missing: add a key with `/model add` or re-run `tenant setup`, then `/model use`. Reason: %s", genKind, cause)
	case degradeConfig:
		return fmt.Sprintf(head+"Config error: fix config.json or re-run `tenant setup`, then `/model use`. Reason: %s", genKind, cause)
	default:
		return fmt.Sprintf(head+"Endpoint unreachable: start the server, then `/model use <provider>` (auto-reconnect is polling). Reason: %s", genKind, cause)
	}
}

// finishModelRouter attaches the embedder profile (probing the local
// OpenAI-compatible embeddings server, falling back to echo with a stern
// warning), then builds the Router and registers the generation factories
// plus the vLLM factory the embedder needs. Shared by the vllm + anthropic
// generation paths.
func finishModelRouter(reg *model.Registry, c *commonFlags, log *slog.Logger, genFactories map[string]model.BackendFactory) (*model.Router, error) {
	usesEcho, err := attachEmbedder(reg, c)
	if err != nil {
		return nil, err
	}
	r := model.NewRouter(reg, log)
	for name, f := range genFactories {
		r.RegisterBackend(name, f)
	}
	// The embedder role always runs on the OpenAI-compatible vllm backend.
	r.RegisterBackend("vllm", vllm.New)
	if usesEcho {
		r.RegisterBackend("echo", echo.New)
	}
	return r, nil
}

// attachEmbedder adds the embedder profile to reg. Embeddings default to a
// local Ollama (set in applyDefaults); we probe it and, if it's down, warn
// with setup instructions and fall back to the echo embedder so the agent
// still runs (degraded semantic recall) rather than failing outright.
func attachEmbedder(reg *model.Registry, c *commonFlags) (usesEcho bool, err error) {
	embedReachable := c.embedEndpoint != "" && c.embedModel != ""
	if embedReachable {
		pctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if !reachable(pctx, c.embedEndpoint+"/v1/models") {
			embedReachable = false
			// During a live /model swap we must not write to os.Stderr (it
			// would corrupt the TUI alt-screen) — the warning is irrelevant to
			// a gen-only swap anyway, since embeddings don't change.
			if !routerBuildQuiet {
				fmt.Fprintf(os.Stderr, "\n"+embedSetupHint+"\n\n", c.embedEndpoint, c.embedModel)
			}
		}
		cancel()
	}
	if embedReachable {
		if err := reg.Add(vllmEmbedderProfile(c.embedEndpoint, c.embedModel, c.embedDim, c.embedAPIKey)); err != nil {
			return false, fmt.Errorf("register vllm embedder: %w", err)
		}
		return false, nil
	}
	for _, p := range echoProfiles() {
		if p.Role == model.RoleEmbedder {
			if err := reg.Add(p); err != nil {
				return false, fmt.Errorf("register echo embedder: %w", err)
			}
		}
	}
	return true, nil
}

// semanticMemoryReady reports whether the REAL vector embedder is configured AND
// reachable — i.e. NOT the hash stand-in (TEN-254). The long-running memory-backed
// sessions (TUI + serve) refuse to start when this is false unless the operator
// passes --allow-no-memory. Mirrors attachEmbedder's reachability probe exactly so
// the guard and the actual embedder selection agree.
func semanticMemoryReady(ctx context.Context, c *commonFlags) bool {
	if c.embedEndpoint == "" || c.embedModel == "" {
		return false
	}
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return reachable(pctx, c.embedEndpoint+"/v1/models")
}

// memoryDownError is the refuse-to-start error when semantic memory is down and
// the operator hasn't forced it (TEN-254). It reuses the stern embedSetupHint so
// the fix is identical to the warning, then adds the override.
func memoryDownError(c *commonFlags) error {
	var b strings.Builder
	if c.embedEndpoint != "" {
		fmt.Fprintf(&b, embedSetupHint+"\n\n", c.embedEndpoint, c.embedModel)
	} else {
		b.WriteString("EMBEDDINGS DOWN — SEMANTIC MEMORY IS DISABLED\n\n" +
			"No embedding endpoint is configured, so memory falls back to a hash stand-in " +
			"(no real vector recall). Point at one with --embed-endpoint http://HOST:PORT " +
			"--embed-model NAME.\n\n")
	}
	b.WriteString("Refusing to start: semantic memory is a core pillar, not optional. " +
		"To start anyway and run amnesiac, pass --allow-no-memory. For offline dev, use --backend echo.")
	return errors.New(b.String())
}

// anthropicGenProfiles builds planner/executor/summarizer profiles for the
// Anthropic Messages API. Claude models carry a 200K context window; the
// operational budget is ~80% of that. ToolFormat is irrelevant (Anthropic
// returns structured tool_use blocks, not text the toolfmt layer parses).
func anthropicGenProfiles(endpoint, modelID, apiKey string, planCeiling int) []model.Profile {
	if endpoint == "" {
		endpoint = "https://api.anthropic.com"
	}
	ceiling := planCeiling
	if ceiling <= 0 {
		ceiling = defaultPlanCeiling
	}
	base := func(id string, role model.Role, respReserve int) model.Profile {
		return model.Profile{
			ID:                       id,
			Role:                     role,
			Backend:                  "anthropic",
			Endpoint:                 endpoint,
			Model:                    modelID,
			APIKey:                   apiKey,
			EstimateTokensOnly:       true,
			ContextLength:            200000,
			OperationalContextBudget: 160000,
			ReserveSoul:              2048,
			ReserveSystemPrompt:      3072,
			ReserveToolDefs:          4096,
			ReserveResponse:          respReserve,
			ToolFormat:               "openai", // unused by this backend
			SupportsGrammar:          false,
			MaxToolsPerCall:          12,
			MaxParallelTools:         3,
			PlanLoopCeiling:          ceiling,
		}
	}
	return []model.Profile{
		base("anthropic-planner", model.RolePlanner, 8192),
		base("anthropic-executor", model.RoleExecutor, 8192),
		base("anthropic-summarizer", model.RoleSummarizer, 4096),
	}
}

// anthropicJudgeProfile builds a single RoleJudge profile for the Anthropic
// Messages API — the eval harness's LLM-as-judge. Kept a DIFFERENT model
// family than the local planner on purpose (self-bias is linearly correlated
// with self-recognition; eval-harness-plan-v1 §3a). The judge makes short,
// temperature-0 grading calls, so only the backend/endpoint/model/key matter;
// the budget fields mirror the gen profiles to keep a valid Profile shape.
func anthropicJudgeProfile(endpoint, modelID, apiKey string) model.Profile {
	if endpoint == "" {
		endpoint = "https://api.anthropic.com"
	}
	return model.Profile{
		ID:                       "anthropic-judge",
		Role:                     model.RoleJudge,
		Backend:                  "anthropic",
		Endpoint:                 endpoint,
		Model:                    modelID,
		APIKey:                   apiKey,
		EstimateTokensOnly:       true,
		ContextLength:            200000,
		OperationalContextBudget: 160000,
		ReserveResponse:          1024,
		ToolFormat:               "openai", // unused by this backend
		MaxToolsPerCall:          12,
		MaxParallelTools:         3,
		PlanLoopCeiling:          defaultPlanCeiling,
	}
}

// attachAnthropicJudge registers the Anthropic backend (idempotent) and binds
// RoleJudge to a cloud judge profile on an EXISTING router. This is what lets
// the eval CLI add a cloud judge to a local-planner router (planner = vLLM /
// Qwen, judge = Claude) without rebuilding the router from scratch.
func attachAnthropicJudge(r *model.Router, endpoint, modelID, apiKey string) error {
	r.RegisterBackend("anthropic", anthropic.New)
	return r.SetProfiles([]model.Profile{anthropicJudgeProfile(endpoint, modelID, apiKey)})
}

// attachJudge generalizes attachAnthropicJudge to ANY configured provider kind
// (TEN-91): anthropic (Claude) or a vLLM/OpenAI-compatible one (z.ai/GLM, OpenAI,
// Grok, …). It reuses genProfiles so the judge inherits every per-provider quirk
// (ChatPath, ForceHTTP1, tokenizer mode) instead of re-deriving them, then
// re-roles the planner-shaped profile to RoleJudge and binds it on the EXISTING
// router. endpoint "" ⇒ the catalog default for that kind.
func attachJudge(r *model.Router, kind, endpoint, modelID, apiKey string) error {
	kind = strings.TrimSpace(kind)
	pk, ok := providerKinds[kind]
	if !ok {
		return fmt.Errorf("unknown judge provider kind %q (known: %s)", kind, strings.Join(providerOrder, ", "))
	}
	if !pk.Wired {
		return fmt.Errorf("%s judge backend is in the catalog but not implemented", pk.Label)
	}
	if endpoint == "" {
		endpoint = pk.DefaultEndpoint
	}
	c := &commonFlags{
		backend:      pk.Backend,
		genKind:      kind,
		vllmEndpoint: endpoint,
		vllmModel:    modelID,
		vllmToolFmt:  pk.DefaultToolFmt,
		genAPIKey:    apiKey,
	}
	gen, factories, err := genProfiles(c)
	if err != nil {
		return fmt.Errorf("judge %q: %w", kind, err)
	}
	if len(gen) == 0 {
		return fmt.Errorf("judge %q: no profile built", kind)
	}
	// Re-role the planner-shaped profile (same backend/endpoint/model/key/quirks)
	// to RoleJudge — the judge makes short grading calls, so the gen budgets are
	// harmless.
	jp := gen[0]
	for _, p := range gen {
		if p.Role == model.RolePlanner {
			jp = p
			break
		}
	}
	jp.Role = model.RoleJudge
	jp.ID = "judge-" + kind
	for name, f := range factories {
		r.RegisterBackend(name, f)
	}
	return r.SetProfiles([]model.Profile{jp})
}

// genProfiles builds JUST the generation-role profiles (planner/executor/
// summarizer) + the backend factories for c's active provider — the pieces a
// live model swap replaces in the shared router. Embeddings are deliberately
// left untouched (they stay on the local server across a gen swap). Mirrors
// buildRouter's per-backend gen-profile construction.
func genProfiles(c *commonFlags) ([]model.Profile, map[string]model.BackendFactory, error) {
	switch c.backend {
	case "echo":
		var gen []model.Profile
		for _, p := range echoProfiles() {
			if p.Role != model.RoleEmbedder {
				gen = append(gen, p)
			}
		}
		return gen, map[string]model.BackendFactory{"echo": echo.New}, nil
	case "vllm":
		if c.vllmEndpoint == "" || c.vllmModel == "" {
			return nil, nil, fmt.Errorf("vLLM provider needs an endpoint + model (server reachable for auto-detect, or set the model)")
		}
		pk := providerKinds[c.genKind]
		gen := vllmGenProfiles(genProfileOpts{
			endpoint: c.vllmEndpoint, model: c.vllmModel, toolFmt: c.vllmToolFmt,
			apiKey: c.genAPIKey, chatPath: pk.ChatPath, estimateOnly: pk.EstimateTokens,
			forceHTTP1: pk.ForceHTTP1,
			contextLen: c.contextLen, planCeiling: c.planCeiling,
		})
		return gen, map[string]model.BackendFactory{"vllm": vllm.New}, nil
	case "anthropic":
		if c.genAPIKey == "" {
			return nil, nil, fmt.Errorf("Anthropic provider needs an API key")
		}
		if c.vllmModel == "" {
			return nil, nil, fmt.Errorf("Anthropic provider needs a model")
		}
		return anthropicGenProfiles(c.vllmEndpoint, c.vllmModel, c.genAPIKey, c.planCeiling),
			map[string]model.BackendFactory{"anthropic": anthropic.New}, nil
	default:
		return nil, nil, fmt.Errorf("unknown backend %q", c.backend)
	}
}

// genProfileOpts bundles what vllmGenProfiles needs — grew past a sensible
// positional arg count once hosted providers added api keys + path overrides.
type genProfileOpts struct {
	endpoint, model, toolFmt, apiKey, chatPath string
	estimateOnly                               bool
	forceHTTP1                                 bool // disable HTTP/2 (GOAWAY-prone hosted LBs, TEN-218)
	contextLen                                 int  // 0 = default (131072)
	planCeiling                                int  // 0 = defaultPlanCeiling
}

// vllmGenProfiles builds planner/executor/summarizer profiles pointing
// at one vLLM endpoint+model. ContextLength 131072 matches the probed
// gemma-4-26b max_model_len; operational budget = 80% of that.
func vllmGenProfiles(o genProfileOpts) []model.Profile {
	if o.toolFmt == "" {
		o.toolFmt = "gemma"
	}
	// Context window: the server-reported max_model_len when known, else the
	// 128K default. Operational budget is ~80% of it.
	ctxLen := o.contextLen
	if ctxLen <= 0 {
		ctxLen = 131072
	}
	opBudget := ctxLen * 8 / 10
	ceiling := o.planCeiling
	if ceiling <= 0 {
		ceiling = defaultPlanCeiling
	}
	base := func(id string, role model.Role, respReserve int) model.Profile {
		// Cap the response reserve so a small-context model keeps a positive
		// writable budget (the planner's 80K reserve is nonsense at 32K).
		if respReserve > ctxLen/4 {
			respReserve = ctxLen / 4
		}
		return model.Profile{
			ID:                       id,
			Role:                     role,
			Backend:                  "vllm",
			Endpoint:                 o.endpoint,
			Model:                    o.model,
			APIKey:                   o.apiKey,
			ChatPath:                 o.chatPath,
			EstimateTokensOnly:       o.estimateOnly,
			ForceHTTP11:              o.forceHTTP1,
			ContextLength:            ctxLen,
			OperationalContextBudget: opBudget,
			ReserveSoul:              2048,
			ReserveSystemPrompt:      3072,
			ReserveToolDefs:          4096,
			ReserveResponse:          respReserve,
			ToolFormat:               o.toolFmt,
			// Only real vLLM exposes guided_json; hosted APIs + Ollama +
			// llama.cpp do not (they'd 400 on the field). estimateOnly is the
			// same set that lacks /tokenize, so reuse it as the discriminator.
			SupportsGrammar: !o.estimateOnly,
			// Upper bound for relevance-ranked registries. The TUI's
			// toolMux ignores this — it surfaces the full curated enabled
			// set (see toolMux.Search), since an order-based cap silently
			// dropped late-registered tools like web. gemma-4-26b handles a
			// plugin's full cohesive toolset (web = 8) without drowning.
			MaxToolsPerCall:  12,
			MaxParallelTools: 3,
			PlanLoopCeiling:  ceiling,
		}
	}
	return []model.Profile{
		base("vllm-planner", model.RolePlanner, 80000),
		base("vllm-executor", model.RoleExecutor, 16000),
		base("vllm-summarizer", model.RoleSummarizer, 4096),
	}
}

// vllmEmbedderProfile points the embedder role at an OpenAI-compatible
// /v1/embeddings endpoint (real vLLM embed server OR Ollama — identical
// wire shape). The vllm backend's Embed() is all that's exercised for
// this role; /tokenize is never called on an embedder.
func vllmEmbedderProfile(endpoint, modelID string, dim int, apiKey string) model.Profile {
	if dim <= 0 {
		dim = 768
	}
	return model.Profile{
		ID:                       "vllm-embedder",
		Role:                     model.RoleEmbedder,
		Backend:                  "vllm",
		Endpoint:                 endpoint,
		Model:                    modelID,
		APIKey:                   apiKey,
		EstimateTokensOnly:       true, // embedders never call /tokenize
		ContextLength:            8192,
		OperationalContextBudget: 8192,
		ToolFormat:               "openai",
		EmbedDim:                 dim,
	}
}

// echoProfiles returns one echo-backed profile per role so the Router
// can resolve planner/executor/summarizer/embedder offline.
func echoProfiles() []model.Profile {
	base := func(id string, role model.Role) model.Profile {
		return model.Profile{
			ID:                       id,
			Role:                     role,
			Backend:                  "echo",
			Endpoint:                 "echo://local",
			Model:                    "echo",
			ContextLength:            32000,
			OperationalContextBudget: 25600,
			ReserveSoul:              1024,
			ReserveSystemPrompt:      1024,
			ReserveToolDefs:          1024,
			ReserveResponse:          8000,
			ToolFormat:               "openai",
			SupportsGrammar:          true,
			MaxToolsPerCall:          3,
			MaxParallelTools:         1,
			PlanLoopCeiling:          3,
		}
	}
	planner := base("echo-planner", model.RolePlanner)
	executor := base("echo-executor", model.RoleExecutor)
	summarizer := base("echo-summarizer", model.RoleSummarizer)
	embedder := base("echo-embedder", model.RoleEmbedder)
	embedder.EmbedDim = echo.EmbedDim
	embedder.ReserveResponse = 0
	return []model.Profile{planner, executor, summarizer, embedder}
}

// stores bundles the open persistence handles for a subcommand.
type stores struct {
	episodic *episodic.Store
	semantic *semantic.Store
	archive  *archive.Writer
	soul     *soul.Soul
}

func openStores(c *commonFlags) (*stores, func(), error) {
	es, err := episodic.Open(filepath.Join(c.dataDir, "episodes.db"))
	if err != nil {
		return nil, nil, fmt.Errorf("open episodic: %w", err)
	}
	ss, err := semantic.Open(filepath.Join(c.dataDir, "facts.db"))
	if err != nil {
		_ = es.Close()
		return nil, nil, fmt.Errorf("open semantic: %w", err)
	}
	sl, lerr := soul.Load(c.cfgDir, c.agent)
	if lerr != nil {
		sl = soul.NewDefault(c.agent)
	}
	st := &stores{episodic: es, semantic: ss, archive: archive.NewWriter(c.dataDir), soul: sl}
	closer := func() { _ = es.Close(); _ = ss.Close() }
	return st, closer, nil
}
