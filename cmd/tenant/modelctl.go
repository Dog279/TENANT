package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"tenant/internal/agent"
	"tenant/internal/model"
	"tenant/internal/tui"
)

// modelControl implements tui.ModelControl: list configured backends, switch
// the primary live (rebuild a router → swap it into the running agent so the
// next turn routes there), and add a new vLLM backend mid-session. Guarded so
// concurrent /model commands serialize.
type modelControl struct {
	mu      sync.Mutex
	cfgDir  string
	dataDir string
	agentID string
	ag      *agent.Agent
	log     *slog.Logger
	// degraded is the shared echo-fallback gate (nil when the launch was healthy).
	// Set at launch by buildRouterResilient; cleared here on a reachable swap.
	degraded *degradedState
	// reinstallEmbedder, when set, re-resolves the tool-ranking embedder for the
	// now-active provider and reinstalls it on the tool mux after a model swap
	// (TEN-226 step 4). Without it, ranking keeps using the OLD embedder's
	// fingerprint/cache across a /model use — a dim change then makes every cosine
	// 0 (caught by Search's dim guard, which falls back to the full catalog).
	// Set by cmdTUI; nil elsewhere. Run async by the caller so a slow embedder
	// resolve never blocks the swap.
	reinstallEmbedder func(context.Context)
}

func (mc *modelControl) ModelList() []tui.ModelInfo {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	lc, err := loadLaunchConfig(mc.cfgDir)
	if err != nil {
		return nil
	}
	return modelInfos(lc, mc.degraded)
}

func modelInfos(lc *launchConfig, degraded *degradedState) []tui.ModelInfo {
	names := make([]string, 0, len(lc.Providers))
	for name := range lc.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	deg := degraded.Degraded()
	out := make([]tui.ModelInfo, 0, len(names))
	for _, name := range names {
		p := lc.Providers[name]
		active := name == lc.Provider
		out = append(out, tui.ModelInfo{
			Name: name, Kind: p.Kind, Endpoint: safeDisplayEndpoint(p.Endpoint), Model: p.Model,
			Active: active,
			// The active provider is the one we tried (and failed) to build, so
			// it's the one actually running on echo. Source of truth is the gate,
			// not config (config still names the real provider — we never persist echo).
			Degraded: deg && active,
		})
	}
	return out
}

func (mc *modelControl) UseModel(name, modelOverride string) (string, string, error) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	lc, err := loadLaunchConfig(mc.cfgDir)
	if err != nil {
		return "", "", err
	}
	if lc.Providers[name] == nil {
		return "", "", fmt.Errorf("no backend named %q (see /model)", name)
	}
	// Optional model override — operator-friendly path for providers that
	// serve multiple variants (Z.ai's glm-4.6/5/5.1/..., OpenAI's gpt-*).
	// Empty modelOverride preserves the provider's saved Model field.
	if mo := strings.TrimSpace(modelOverride); mo != "" {
		lc.Providers[name].Model = mo
	}
	lc.Provider = name
	if err := lc.save(mc.cfgDir); err != nil {
		return "", "", err
	}
	// Resolve the now-active provider (loads the config we just saved,
	// auto-detects the served model + context length).
	c2 := &commonFlags{cfgDir: mc.cfgDir, dataDir: mc.dataDir, agent: mc.agentID, embedDim: defaultEmbedDim}
	if err := c2.resolve(); err != nil {
		return "", "", err
	}
	gen, factories, err := genProfiles(c2)
	if err != nil {
		return "", "", err
	}
	// Mutate the SHARED router in place: re-point the generation roles at the
	// new provider. Because the main agent, every spawned sub-agent, and the
	// background jobs all hold this same router, they ALL re-route at once —
	// no more sub-agents calling the old model.
	r := mc.ag.Router()
	if r == nil {
		return "", "", fmt.Errorf("no active router")
	}
	for kind, f := range factories {
		r.RegisterBackend(kind, f)
	}
	if err := r.SetProfiles(gen); err != nil {
		return "", "", err
	}
	// Re-point tool-ranking at the new provider's embedder (TEN-226 step 4).
	// Async: re-resolving + the lazy precompute mustn't block the swap, and it
	// touches the mux's own lock (never mc.mu), so no nesting. Until it lands, the
	// dim guard keeps a stale-embedder turn correct (falls back to full catalog).
	if mc.reinstallEmbedder != nil {
		go mc.reinstallEmbedder(context.Background())
	}
	active := c2.vllmModel
	if active == "" {
		active = name
	}
	// Probe the new backend NOW so the operator sees connect success/fail in
	// the feed immediately, instead of discovering a 404 on the next message.
	status := probeSwap(name, active, c2)
	// If we launched degraded (echo fallback) and this swap reached a real,
	// REACHABLE model (status leads with ✓), clear the gate so the suppressed
	// background actors (self-improve, cron, relay) resume. A swap to a still-
	// down endpoint returns ⚠ and leaves the gate set — destructive jobs stay
	// suppressed until a real model is actually reachable.
	if mc.degraded.Degraded() && strings.HasPrefix(status, "✓") {
		mc.degraded.clear()
		status += "  (self-improvement / cron resumed)"
	}
	return status, active, nil
}

// Fallback returns the configured auto-fallback provider chain (TEN-246).
func (mc *modelControl) Fallback() []string {
	lc, err := loadLaunchConfig(mc.cfgDir)
	if err != nil {
		return nil
	}
	return lc.Fallbacks
}

// SetFallback validates + persists the ordered fallback provider chain and
// re-installs it on the live router (TEN-246) — so it takes effect this session
// with no restart. Each name must be a configured provider; empty clears it.
func (mc *modelControl) SetFallback(names []string) (string, error) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	lc, err := loadLaunchConfig(mc.cfgDir)
	if err != nil {
		return "", err
	}
	valid := make([]string, 0, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if lc.Providers[n] == nil {
			return "", fmt.Errorf("no provider %q (see /model)", n)
		}
		if n == lc.Provider {
			return "", fmt.Errorf("%q is the active model — a fallback must be a DIFFERENT provider", n)
		}
		valid = append(valid, n)
	}
	lc.Fallbacks = valid
	if err := lc.save(mc.cfgDir); err != nil {
		return "", err
	}
	if r := mc.ag.Router(); r != nil {
		installFallbackChain(r, mc.cfgDir, lc, lc.PlanLoopCeiling, mc.log)
	}
	if len(valid) == 0 {
		return "model fallback cleared", nil
	}
	return "model fallback: " + lc.Provider + " → " + strings.Join(valid, " → "), nil
}

// ReloadKeys re-resolves the ACTIVE provider's API key (env → credentials.json)
// and hot-swaps it into the live router, so a key rotated at runtime takes effect
// on the next turn WITHOUT a restart. No-op when no provider is active. It is a
// re-`use` of the current provider — same resolve → SetProfiles → probe path,
// shared by every actor (main agent, cron, relay, dashboard) holding the router,
// and it clears the degraded gate on a reachable result.
func (mc *modelControl) ReloadKeys() (string, error) {
	lc, err := loadLaunchConfig(mc.cfgDir)
	if err != nil {
		return "", err
	}
	active := strings.TrimSpace(lc.Provider)
	if active == "" {
		return "", nil
	}
	status, _, err := mc.UseModel(active, "")
	return status, err
}

// probeSwap verifies the just-swapped backend is reachable and serving the
// expected model, returning a status line that leads with ✓ (good) or ⚠
// (degraded). The router is already swapped either way — a momentarily-down
// server shouldn't block the switch; the next message will retry.
func probeSwap(name, active string, c2 *commonFlags) string {
	switch c2.backend {
	case "echo":
		return fmt.Sprintf("✓ now using %q (echo, offline) — live, no restart", name)
	case "anthropic":
		// Anthropic's API isn't probed via /v1/models; trust the config.
		return fmt.Sprintf("✓ now using %q — %s (Anthropic) — live, no restart", name, active)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	ms, err := fetchModels(ctx, c2.vllmEndpoint, c2.genAPIKey)
	if err != nil {
		return fmt.Sprintf("⚠ switched to %q but the endpoint is UNREACHABLE at %s (%v) — start the server, then send a message to retry",
			name, c2.vllmEndpoint, err)
	}
	served := make([]string, 0, len(ms))
	for _, m := range ms {
		served = append(served, m.ID)
		if m.ID == active {
			return fmt.Sprintf("✓ now using %q — connected: %s @ %s (live, no restart)", name, active, c2.vllmEndpoint)
		}
	}
	return fmt.Sprintf("⚠ now using %q @ %s, but it does not serve model %q (it serves: %s) — calls may 404; fix with `/model` or set model to \"\" for auto-detect",
		name, c2.vllmEndpoint, active, strings.Join(served, ", "))
}

// LoopCeiling reports the active planner profile's PlanLoopCeiling.
// ListProviderModels resolves the provider's endpoint + auth, then
// queries its live models catalog (/v1/models with /models fallback,
// matching the swap-probe pattern). Empty name = currently-active
// provider. Returns ONLY the model IDs (no max-len, no metadata) —
// for the TUI's `/model models` discovery view.
//
// Best-effort: a slow or unreachable endpoint returns the network
// error to the caller. Auth-required endpoints (Z.ai/OpenAI/etc.)
// must already have their key set up via `/model add-cloud` or
// `tenant setup`; this method reads the saved secret, never accepts
// one inline.
func (mc *modelControl) ListProviderModels(name string) ([]string, error) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	lc, err := loadLaunchConfig(mc.cfgDir)
	if err != nil {
		return nil, err
	}
	target := strings.TrimSpace(name)
	if target == "" {
		target = lc.Provider
	}
	if target == "" {
		return nil, fmt.Errorf("no active provider — switch with /model use <name> first")
	}
	pc := lc.Providers[target]
	if pc == nil {
		return nil, fmt.Errorf("no backend named %q (see /model)", target)
	}
	endpoint := strings.TrimSpace(pc.Endpoint)
	if endpoint == "" {
		// Fall back to catalog default when the provider's saved config
		// lacks an endpoint (rare — usually only for echo or local kinds).
		if pk, ok := providerKinds[pc.Kind]; ok {
			endpoint = pk.DefaultEndpoint
		}
	}
	if endpoint == "" {
		return nil, fmt.Errorf("provider %q has no endpoint configured", target)
	}
	apiKey := resolveSecret(mc.cfgDir, target, pc.Auth)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	ms, err := fetchModels(ctx, endpoint, apiKey)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	ids := make([]string, 0, len(ms))
	for _, m := range ms {
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	return ids, nil
}

func (mc *modelControl) LoopCeiling() int {
	r := mc.ag.Router()
	if r == nil {
		return 0
	}
	if p, err := r.ForRole(model.RolePlanner); err == nil {
		return p.PlanLoopCeiling
	}
	return 0
}

// SetLoopCeiling retunes the loop ceiling live on the shared router (so the
// main agent, sub-agents, and jobs all follow on their next turn) and persists
// it to config so it survives a restart and applies across model swaps.
func (mc *modelControl) SetLoopCeiling(n int) (string, error) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if n < 1 || n > 200 {
		return "", fmt.Errorf("ceiling must be between 1 and 200")
	}
	r := mc.ag.Router()
	if r == nil {
		return "", fmt.Errorf("no active router")
	}
	updated := r.SetPlanLoopCeiling(n)
	if lc, err := loadLaunchConfig(mc.cfgDir); err == nil {
		lc.PlanLoopCeiling = n
		_ = lc.save(mc.cfgDir) // best-effort persistence
	}
	return fmt.Sprintf("loop ceiling → %d (updated %d role(s); applies next turn, incl. newly spawned sub-agents)", n, updated), nil
}

// looksLikeURL is a lenient gate for "this string is plausibly an HTTP
// URL the operator typed." Doesn't validate the full URL — just enough
// to catch "I pasted my API key as the endpoint" mistakes. False
// positives are acceptable (a malformed URL still hits the backend and
// fails with a clear network error); false negatives (rejecting a real
// URL) are the danger we minimize.
func looksLikeURL(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// safeDisplayEndpoint is the LAST-LINE-OF-DEFENSE renderer for any
// endpoint string that's about to be shown in operator-facing output
// (the /model list, the CLI listing, transcripts, etc.). If the input
// doesn't look like an HTTP URL — meaning something malformed slipped
// past the entry-point validation OR the operator loaded an old
// pre-fix config — we replace it with a redacted marker instead of
// echoing what might be a secret.
//
// Defense-in-depth. The actual fix is preventing the bad value from
// reaching config in the first place (looksLikeURL in AddModel + the
// load-time guard). This catches anything that did anyway.
func safeDisplayEndpoint(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(unset)"
	}
	if looksLikeURL(s) {
		return s
	}
	return "[redacted — non-URL endpoint, see `tenant doctor`]"
}

// clipSecret renders a suspect string for an error message WITHOUT
// echoing the whole thing. If the value happened to be an API key,
// printing it verbatim in an error message leaks it to the operator's
// transcript, scrollback, and any log capture. Show first 6 chars +
// total length so the operator can still recognize what they typed.
func clipSecret(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 8 {
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf("%q (%d chars; redacted — looked like a token, not a URL)", s[:6]+"…", len(s))
}

func (mc *modelControl) AddModel(name, endpoint, toolFormat string) (string, error) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	name = strings.TrimSpace(name)
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if name == "" || endpoint == "" {
		return "", fmt.Errorf("name and endpoint are required")
	}
	// Reject non-URL endpoints. This catches the very-easy-to-make mistake
	// of typing `/model add zai <api-key>` (which would save the key as the
	// endpoint URL and leak it through the model list). When the name
	// matches a known cloud-provider kind, point the operator at the right
	// command instead of just rejecting. NEVER echo the full bad input —
	// it might be a real secret.
	if !looksLikeURL(endpoint) {
		safe := clipSecret(endpoint)
		if _, isKind := providerKinds[name]; isKind {
			return "", fmt.Errorf("endpoint %s is not a URL — did you mean `/model add-cloud %s <api-key>`? "+
				"`/model add` is for self-hosted vLLM endpoints; cloud providers use `add-cloud`",
				safe, name)
		}
		return "", fmt.Errorf("endpoint must be an http:// or https:// URL; got %s", safe)
	}
	lc, err := loadLaunchConfig(mc.cfgDir)
	if err != nil {
		return "", err
	}
	if lc.Providers == nil {
		lc.Providers = map[string]*providerConfig{}
	}
	lc.Providers[name] = &providerConfig{Kind: "vllm", Endpoint: endpoint, ToolFmt: strings.TrimSpace(toolFormat)}
	// Make sure an embeddings provider exists so a later `use` builds a full router.
	if lc.Embed == nil {
		lc.Embed = &providerConfig{Kind: "ollama", Endpoint: defaultEmbedEndpoint, Model: defaultEmbedModel, EmbedDim: defaultEmbedDim}
	}
	if err := lc.save(mc.cfgDir); err != nil {
		return "", err
	}
	return fmt.Sprintf("added vLLM backend %q → %s — switch with `/model use %s`", name, endpoint, name), nil
}

// AddCloudModel registers a KEYED cloud provider (zai/openai/grok/anthropic)
// in one shot using the catalog's known endpoint + chat path + default model
// + tool format. The API key is stored in credentials.json (0600). The
// provider gets registered under its kind id as the name (so `zai` is its
// own bucket, distinct from any vLLM-with-name-zai an operator might add).
// Idempotent: re-running with a new key overwrites just the secret.
func (mc *modelControl) AddCloudModel(kindID, apiKey string) (string, error) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	kindID = strings.TrimSpace(kindID)
	apiKey = strings.TrimSpace(apiKey)
	pk, ok := providerKinds[kindID]
	if !ok {
		return "", fmt.Errorf("unknown provider kind %q (known: %s)", kindID, strings.Join(providerOrder, ", "))
	}
	if !pk.NeedsKey {
		return "", fmt.Errorf("%s does not use an API key — use `/model add %s <endpoint>` for self-hosted backends", pk.Label, kindID)
	}
	if !pk.Wired {
		return "", fmt.Errorf("%s is in the catalog but its backend is not yet implemented", pk.Label)
	}
	if apiKey == "" {
		return "", fmt.Errorf("an API key is required for %s (passed as the second arg)", pk.Label)
	}
	if pk.DefaultEndpoint == "" {
		return "", fmt.Errorf("internal: %s has no DefaultEndpoint in the catalog", pk.Label)
	}
	lc, err := loadLaunchConfig(mc.cfgDir)
	if err != nil {
		return "", err
	}
	if lc.Providers == nil {
		lc.Providers = map[string]*providerConfig{}
	}
	// Build the provider entry from the catalog. Preserve any pre-existing
	// model override (don't blow away a custom DefaultModel the operator set).
	existing := lc.Providers[kindID]
	model := pk.DefaultModel
	if existing != nil && existing.Model != "" {
		model = existing.Model
	}
	lc.Providers[kindID] = &providerConfig{
		Kind: kindID, Endpoint: pk.DefaultEndpoint, Model: model,
		ToolFmt: pk.DefaultToolFmt,
		Auth:    authCfg{Mode: "apikey", Stored: true},
	}
	// Make sure an embeddings provider exists so a later `use` builds a full
	// router. Cloud LLM providers don't ship embeddings (yet), so default to
	// the local Ollama + nomic-embed-text stack.
	if lc.Embed == nil {
		lc.Embed = &providerConfig{Kind: "ollama", Endpoint: defaultEmbedEndpoint, Model: defaultEmbedModel, EmbedDim: defaultEmbedDim}
	}
	if err := lc.save(mc.cfgDir); err != nil {
		return "", err
	}
	// Store the key in credentials.json (0600).
	creds, _ := loadCredentials(mc.cfgDir)
	creds.set(kindID, apiKey)
	if err := creds.save(mc.cfgDir); err != nil {
		return "", fmt.Errorf("save credentials: %w", err)
	}
	return fmt.Sprintf("added %s backend %q → %s (key stored, 0600). Switch with `/model use %s`.",
		pk.Label, kindID, pk.DefaultEndpoint, kindID), nil
}

// RemoveModel deletes a configured backend (and its stored credential).
// Refuses to remove the currently-active model — switch first.
func (mc *modelControl) RemoveModel(name string) (string, error) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	lc, err := loadLaunchConfig(mc.cfgDir)
	if err != nil {
		return "", err
	}
	if lc.Providers[name] == nil {
		return "", fmt.Errorf("no backend named %q (see /model)", name)
	}
	if lc.Provider == name {
		return "", fmt.Errorf("%q is the active model — `/model use <other>` first, then remove it", name)
	}
	delete(lc.Providers, name)
	forgetProviderSecret(mc.cfgDir, name)
	if err := lc.save(mc.cfgDir); err != nil {
		return "", err
	}
	return fmt.Sprintf("removed backend %q", name), nil
}

// forgetProviderSecret drops any stored API key keyed by the provider name.
func forgetProviderSecret(cfgDir, name string) {
	creds, err := loadCredentials(cfgDir)
	if err != nil {
		return
	}
	if _, ok := creds.Secrets[name]; ok {
		delete(creds.Secrets, name)
		_ = creds.save(cfgDir)
	}
}

// cmdModel is the `tenant model` CLI: manage model backends in config.json.
//
//	tenant model list
//	tenant model show
//	tenant model use <name>
//	tenant model add <name> --endpoint URL [--kind vllm] [--tool-format qwen] [--api-key K]
//	tenant model remove <name>
func cmdModel(_ context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	sub, rest := args[0], args[1:]
	// Pull leading positional(s) before parsing — Go's flag parser stops
	// at the first non-flag token, so `model add NAME --endpoint X` would
	// otherwise drop the flags. We support up to 2 leading positionals:
	// for `use`, that's [name, optional model variant].
	var name, posModel string
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		name, rest = rest[0], rest[1:]
	}
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		posModel, rest = rest[0], rest[1:]
	}
	fs := flag.NewFlagSet("model "+sub, flag.ContinueOnError)
	c := bindCommon(fs)
	// Note: --api-key + --vllm-* are already registered by bindCommon; reuse
	// them (re-registering would panic). For `add` we read c.genAPIKey.
	var endpoint, kind, mdl, toolFmt string
	if sub == "add" {
		fs.StringVar(&endpoint, "endpoint", "", "backend base URL (no /v1 suffix)")
		fs.StringVar(&kind, "kind", "vllm", "backend kind: "+strings.Join(providerOrder, "|"))
		fs.StringVar(&mdl, "model", "", "model id (blank = auto-detect at launch)")
		fs.StringVar(&toolFmt, "tool-format", "", "qwen|gemma|llama|mistral|openai")
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}
	c.cfgDir = expandPath(c.cfgDir)
	if err := c.resolveDirs(); err != nil {
		return err
	}
	lc, err := loadLaunchConfig(c.cfgDir)
	if err != nil {
		return err
	}
	if lc.Providers == nil {
		lc.Providers = map[string]*providerConfig{}
	}
	if name == "" && fs.NArg() > 0 { // flags-first form: `model use --foo bar NAME`
		name = fs.Arg(0)
	}

	switch sub {
	case "list", "ls":
		printModelList(lc)
		return nil
	case "show":
		p := lc.active()
		if p == nil {
			fmt.Println("no active model backend (run `tenant setup` or `tenant model add`)")
			return nil
		}
		fmt.Printf("active: %s\n  kind:     %s\n  endpoint: %s\n  model:    %s\n  tool fmt: %s\n",
			lc.Provider, p.Kind, orDash(safeDisplayEndpoint(p.Endpoint)), orDash(p.Model), orDash(p.ToolFmt))
		return nil
	case "use":
		if name == "" {
			return fmt.Errorf("usage: tenant model use <name> [<model>]   (model optional — pins a variant, e.g. `tenant model use zai glm-5.1`)")
		}
		if lc.Providers[name] == nil {
			return fmt.Errorf("no backend named %q (see `tenant model list`)", name)
		}
		// Optional model override — second positional. Mirrors the TUI's
		// `/model use <name> [<model>]` shape.
		if posModel != "" {
			lc.Providers[name].Model = posModel
		}
		lc.Provider = name
		if err := lc.save(c.cfgDir); err != nil {
			return err
		}
		extra := ""
		if posModel != "" {
			extra = " (model pinned: " + posModel + ")"
		}
		fmt.Printf("primary model → %q%s (restart tenant, or use `/model use %s` in the TUI for a live swap)\n", name, extra, name)
		return nil
	case "models", "model-list", "list-models":
		// `tenant model models [<name>]` — live fetch of the model
		// catalog served by the provider's endpoint. Name optional;
		// defaults to active.
		target := name
		if target == "" {
			target = lc.Provider
		}
		if target == "" {
			return fmt.Errorf("no active provider — switch with `tenant model use <name>` first")
		}
		pc := lc.Providers[target]
		if pc == nil {
			return fmt.Errorf("no backend named %q (see `tenant model list`)", target)
		}
		endpoint := strings.TrimSpace(pc.Endpoint)
		if endpoint == "" {
			if pk, ok := providerKinds[pc.Kind]; ok {
				endpoint = pk.DefaultEndpoint
			}
		}
		if endpoint == "" {
			return fmt.Errorf("provider %q has no endpoint configured", target)
		}
		apiKey := resolveSecret(c.cfgDir, target, pc.Auth)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		ms, err := fetchModels(ctx, endpoint, apiKey)
		if err != nil {
			return fmt.Errorf("list models from %s: %w", endpoint, err)
		}
		fmt.Printf("%d model(s) served by %q (%s):\n", len(ms), target, endpoint)
		ids := make([]string, 0, len(ms))
		for _, m := range ms {
			ids = append(ids, m.ID)
		}
		sort.Strings(ids)
		for _, id := range ids {
			fmt.Printf("  · %s\n", id)
		}
		if posModel != "" {
			// `tenant model models zai glm-5.1` is a noop-ish form;
			// most operators probably meant `use`. Hint, don't error.
			fmt.Fprintf(os.Stderr, "\n(hint: to switch + pin a variant, use: tenant model use %s %s)\n", target, posModel)
		}
		return nil
	case "add":
		if name == "" {
			return fmt.Errorf("usage: tenant model add <name> --endpoint URL [--kind ...] [--tool-format ...]")
		}
		pk, ok := providerKinds[kind]
		if !ok {
			return fmt.Errorf("unknown kind %q (one of %s)", kind, strings.Join(providerOrder, ", "))
		}
		if endpoint == "" {
			endpoint = pk.DefaultEndpoint
		}
		if endpoint == "" {
			return fmt.Errorf("--endpoint is required for kind %q", kind)
		}
		// Don't silently default the tool format to gemma (TEN-138): when it
		// wasn't passed, prompt on a TTY. selectOne returns the supplied default
		// on a non-TTY, so non-interactive use keeps working.
		if pk.Backend == "vllm" && toolFmt == "" {
			toolFmt = selectOne("Tool format for "+name, toolFormatOpts(), firstNonEmpty(pk.DefaultToolFmt, defaultVLLMToolFmt))
		}
		pc := &providerConfig{Kind: kind, Endpoint: strings.TrimRight(endpoint, "/"), Model: mdl, ToolFmt: toolFmt}
		if c.genAPIKey != "" {
			creds, _ := loadCredentials(c.cfgDir)
			creds.set(name, c.genAPIKey)
			if err := creds.save(c.cfgDir); err != nil {
				return err
			}
			pc.Auth = authCfg{Mode: "apikey", Stored: true}
		}
		lc.Providers[name] = pc
		if lc.Embed == nil {
			lc.Embed = &providerConfig{Kind: "ollama", Endpoint: defaultEmbedEndpoint, Model: defaultEmbedModel, EmbedDim: defaultEmbedDim}
		}
		if err := lc.save(c.cfgDir); err != nil {
			return err
		}
		fmt.Printf("added backend %q (kind=%s endpoint=%s) — `tenant model use %s` to make it primary\n", name, kind, safeDisplayEndpoint(pc.Endpoint), name)
		return nil
	case "remove", "rm", "delete":
		if name == "" {
			return fmt.Errorf("usage: tenant model remove <name>")
		}
		if lc.Providers[name] == nil {
			return fmt.Errorf("no backend named %q", name)
		}
		if lc.Provider == name {
			return fmt.Errorf("%q is the active model — `tenant model use <other>` first, then remove it", name)
		}
		delete(lc.Providers, name)
		forgetProviderSecret(c.cfgDir, name)
		if err := lc.save(c.cfgDir); err != nil {
			return err
		}
		fmt.Printf("removed backend %q\n", name)
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q (list|show|use|add|remove)", sub)
	}
}

func printModelList(lc *launchConfig) {
	infos := modelInfos(lc, nil) // headless CLI: never degraded
	if len(infos) == 0 {
		fmt.Println("no model backends configured — `tenant model add <name> --endpoint URL` or run `tenant setup`")
		return
	}
	for _, mi := range infos {
		mark := " "
		if mi.Active {
			mark = "*"
		}
		model := mi.Model
		if model == "" {
			model = "(auto)"
		}
		fmt.Printf("%s %-16s %-10s %-22s %s\n", mark, mi.Name, mi.Kind, model, mi.Endpoint)
	}
}
