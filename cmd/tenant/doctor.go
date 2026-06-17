package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"database/sql"

	"tenant/internal/improve"
	"tenant/internal/mcp"
	"tenant/internal/mcpserver"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/memory/skills"
	"tenant/internal/memory/soul"
	"tenant/internal/model"
	"tenant/internal/plugins/gsuite"
	"tenant/internal/plugins/imessage"
	"tenant/internal/plugins/web"
	"tenant/internal/research"

	"os/exec"
	"runtime"
)

// `tenant doctor` — diagnose (and with --fix, repair) the MCP/agent
// setup when things break. Modeled on hermes/openclaw/flutter doctor:
// a battery of named checks → OK/WARN/FAIL with an actionable fix, a
// summary, and a non-zero exit if anything FAILs (CI-usable).
//
// The checks target the failure modes we have actually hit building
// this: endpoint down, wrong model name, no embeddings endpoint,
// embedding-dimension mismatch silently zeroing cosine, stale distill
// cursor skipping everything, malformed soul TOML, unreadable store,
// tool-calling not configured server-side.
//
// Safety (openclaw pattern): read-only by default. --fix applies ONLY
// safe, reversible repairs (mkdir dirs, reset a stale cursor after
// recording the old value). It never deletes a store or re-embeds.

type checkStatus int

const (
	statusOK checkStatus = iota
	statusWarn
	statusFail
	statusSkip
)

func (s checkStatus) tag() string {
	switch s {
	case statusOK:
		return "OK  "
	case statusWarn:
		return "WARN"
	case statusFail:
		return "FAIL"
	default:
		return "SKIP"
	}
}

type checkResult struct {
	Name   string      `json:"name"`
	Status checkStatus `json:"-"`
	Stat   string      `json:"status"`
	Detail string      `json:"detail"`
	Fix    string      `json:"fix,omitempty"`
	Fixed  bool        `json:"fixed,omitempty"`
}

type doctorEnv struct {
	c        *commonFlags
	deep     bool
	fix      bool
	httpc    *http.Client
	router   *model.Router
	episodic *episodic.Store
	semantic *semantic.Store
	skills   *skills.Store
	lc       *launchConfig // populated by checkLaunchConfig (others reuse)
}

func cmdDoctor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	c := bindCommon(fs)
	deep := fs.Bool("deep", false, "include live network probes (tokenize, tool-calling, MCP self-loop)")
	fix := fs.Bool("fix", false, "apply safe, reversible repairs")
	asJSON := fs.Bool("json", false, "machine-readable output (for CI)")
	contextDebug := fs.String("context-debug", "", "trace what facts/episodes a query would retrieve, then exit (e.g. --context-debug \"what do I prefer\")")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := c.resolve(); err != nil {
		return err
	}
	env := &doctorEnv{
		c:     c,
		deep:  *deep,
		fix:   *fix,
		httpc: &http.Client{Timeout: 12 * time.Second},
	}
	// Best-effort wiring — if these fail their checks report it.
	if r, err := buildRouter(c, newLogger()); err == nil {
		env.router = r
	}
	if es, err := episodic.Open(filepath.Join(c.dataDir, "episodes.db")); err == nil {
		env.episodic = es
		defer es.Close()
	}
	if ss, err := semantic.Open(filepath.Join(c.dataDir, "facts.db")); err == nil {
		env.semantic = ss
		defer ss.Close()
	}
	if sk, err := skills.Open(filepath.Join(c.dataDir, "skills.db")); err == nil {
		env.skills = sk
		defer sk.Close()
	}

	// TEN-49: retrieval trace — show what the memory tiers return for a query
	// instead of running the health checks.
	if strings.TrimSpace(*contextDebug) != "" {
		return runContextDebug(ctx, env, *contextDebug)
	}

	checks := []struct {
		name string
		fn   func(context.Context, *doctorEnv) checkResult
	}{
		{"directories", checkDirs},
		// NEW: validates config.json + active provider exists. Runs early
		// because other checks read e.lc.
		{"launch config", checkLaunchConfig},
		// NEW: credentials.json schema + 0600 perms + every keyed provider
		// has a resolvable secret (env-var present OR stored in creds).
		{"credentials", checkCredentials},
		{"profiles/router", checkRouter},
		{"generation endpoint", checkGenEndpoint},
		{"tokenize endpoint", checkTokenize},
		{"embedding endpoint", checkEmbedEndpoint},
		{"embedding dimension consistency", checkEmbedDim},
		{"sqlite stores", checkStores},
		// NEW: PRAGMA integrity_check on each SQLite DB — catches the
		// "database disk image is malformed" class of corruption that
		// silently kills assemble. --fix renames corrupt DBs to .corrupted
		// so the next startup gets a fresh file (no data loss without an
		// explicit operator action).
		{"db integrity", checkDBIntegrity},
		// NEW: every agent profile resolves cleanly — provider exists,
		// model resolvable, keyed providers have secrets.
		{"agent profiles", checkAgentProfiles},
		// NEW: skills.db opens + count works (paired with db integrity).
		{"skills store", checkSkillsStore},
		// NEW: research store dir + List works (no corrupt manifests
		// blocking history).
		{"research store", checkResearchStore},
		{"soul", checkSoul},
		// NEW: optional wiki dir reachable when configured.
		{"wiki dir (if configured)", checkWikiDir},
		// TEN-48: detect "file on disk newer than sidecar index" — the
		// proximate diagnostic for the 2026-05-26 lost-context bug
		// (TEN-43). Independent of TEN-44's fix because that only
		// covered the research deposit path; manual `cp` to wikiDir or
		// future producers can still leave the index stale.
		{"wiki freshness", checkWikiFreshness},
		// NEW: Chrome detectable if web plugin will be used.
		{"chrome (if web enabled)", checkChrome},
		// TEN-25: os_processes shells out to `ps` / `tasklist`. Missing on
		// minimal containers; warn early so fitness-019 doesn't fail for an
		// infrastructure reason.
		{"os_processes tool", checkOSProcesses},
		// TEN-65: gsuite auth + Gmail reachability when enabled.
		{"gsuite (if enabled)", checkGSuite},
		// NEW: Discord bot token resolvable + reachable when discord enabled.
		{"discord (if enabled)", checkDiscord},
		// TEN-67: X bearer resolvable + api.x.com reachable when x enabled.
		{"x (if enabled)", checkX},
		// TEN-68: iMessage transport readiness. Native (default on macOS)
		// reads chat.db — probe Full Disk Access by opening it read-only so
		// the FDA requirement surfaces before a live read/send. A configured
		// BlueBubbles URL is noted instead (server bridge).
		{"imessage (if configured)", checkIMessage},
		// TEN-81: dashboard reachability + bind-policy config lint when the
		// web control panel is configured.
		{"dashboard (if configured)", checkDashboard},
		// TEN-194: serve-mode liveness — when a daemon (or TUI+dashboard) is up,
		// probe /api/status for a stuck turn or a wedged approval queue. SKIP
		// when nothing is reachable (a daemon simply not running is not a fault).
		{"serve liveness (if running)", checkServe},
		{"distill cursor", checkDistillCursor},
		{"tool-calling (deep)", checkToolCalling},
		{"mcp surface (deep)", checkMCPSurface},
	}

	results := make([]checkResult, 0, len(checks))
	for _, ck := range checks {
		r := ck.fn(ctx, env)
		r.Name = ck.name
		r.Stat = strings.TrimSpace(r.Status.tag())
		results = append(results, r)
	}

	var nFail, nWarn int
	for _, r := range results {
		if r.Status == statusFail {
			nFail++
		}
		if r.Status == statusWarn {
			nWarn++
		}
	}

	if *asJSON {
		out := map[string]any{
			"checks": results, "fail": nFail, "warn": nWarn,
			"healthy": nFail == 0,
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
	} else {
		fmt.Printf("tenant doctor — backend=%s agent=%s\n", c.backend, c.agent)
		fmt.Printf("  data:   %s\n  config: %s\n\n", c.dataDir, c.cfgDir)
		for _, r := range results {
			fmt.Printf("[%s] %s\n", r.Status.tag(), r.Name)
			if r.Detail != "" {
				fmt.Printf("       %s\n", r.Detail)
			}
			if r.Fix != "" && r.Status != statusOK {
				mark := "fix:"
				if r.Fixed {
					mark = "FIXED:"
				}
				fmt.Printf("       %s %s\n", mark, r.Fix)
			}
		}
		fmt.Printf("\nsummary: %d OK, %d WARN, %d FAIL", len(results)-nWarn-nFail, nWarn, nFail)
		if !*fix && (nFail > 0 || nWarn > 0) {
			fmt.Print("   (run `tenant doctor --fix` to auto-repair safe issues)")
		}
		fmt.Println()
	}

	if nFail > 0 {
		return fmt.Errorf("%d check(s) FAILED", nFail)
	}
	return nil
}

// --- checks ---

func checkDirs(_ context.Context, e *doctorEnv) checkResult {
	r := checkResult{Status: statusOK, Detail: "data + config dirs present and writable"}
	for _, d := range []string{e.c.dataDir, e.c.cfgDir} {
		info, err := os.Stat(d)
		if err != nil || !info.IsDir() {
			if e.fix {
				if mkErr := os.MkdirAll(d, 0o755); mkErr == nil {
					r.Status, r.Fixed, r.Fix = statusWarn, true, "created "+d
					continue
				}
			}
			r.Status = statusFail
			r.Detail = "missing/inaccessible: " + d
			r.Fix = "mkdir -p " + d + " (or run with --fix)"
			return r
		}
		// writability probe
		probe := filepath.Join(d, ".doctor-write-probe")
		if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
			r.Status, r.Detail = statusFail, d+" is not writable: "+err.Error()
			r.Fix = "check permissions / disk space on " + d
			return r
		}
		_ = os.Remove(probe)
	}
	return r
}

func checkRouter(_ context.Context, e *doctorEnv) checkResult {
	if e.router == nil {
		return checkResult{Status: statusFail,
			Detail: "router/registry failed to build",
			Fix:    "check --backend and (for vllm) --vllm-endpoint/--vllm-model flags"}
	}
	missing := []string{}
	for _, role := range []model.Role{model.RolePlanner, model.RoleEmbedder} {
		if _, err := e.router.ForRole(role); err != nil {
			missing = append(missing, string(role))
		}
	}
	if len(missing) > 0 {
		return checkResult{Status: statusFail,
			Detail: "no profile bound to role(s): " + strings.Join(missing, ", "),
			Fix:    "add a profile for each role, or use --backend echo for offline dev"}
	}
	return checkResult{Status: statusOK, Detail: "planner + embedder roles resolve"}
}

func checkGenEndpoint(ctx context.Context, e *doctorEnv) checkResult {
	if e.c.backend != "vllm" || e.c.vllmEndpoint == "" {
		return checkResult{Status: statusSkip, Detail: "not a remote vLLM backend (nothing to probe)"}
	}
	models, err := e.listModels(ctx, e.c.vllmEndpoint)
	if err != nil {
		return checkResult{Status: statusFail,
			Detail: fmt.Sprintf("%s unreachable: %v", e.c.vllmEndpoint, err),
			Fix:    "is the vLLM server up? check the host/port and network"}
	}
	if e.c.vllmModel != "" && !slices.Contains(models, e.c.vllmModel) {
		return checkResult{Status: statusWarn,
			Detail: fmt.Sprintf("model %q not in served list %v", e.c.vllmModel, models),
			Fix:    "set --vllm-model to one of the served ids"}
	}
	return checkResult{Status: statusOK,
		Detail: fmt.Sprintf("%s serving %v", e.c.vllmEndpoint, models)}
}

func checkTokenize(ctx context.Context, e *doctorEnv) checkResult {
	if e.c.backend != "vllm" || e.c.vllmEndpoint == "" {
		return checkResult{Status: statusSkip, Detail: "no remote vLLM (TokenCount estimator used)"}
	}
	body := map[string]any{"model": e.c.vllmModel, "prompt": "doctor probe", "add_special_tokens": true}
	var resp struct {
		Count int `json:"count"`
	}
	if err := e.postJSON(ctx, e.c.vllmEndpoint+"/tokenize", body, &resp); err != nil || resp.Count == 0 {
		return checkResult{Status: statusWarn,
			Detail: "/tokenize unavailable — budgeting falls back to a ~4-chars/token estimate",
			Fix:    "fine for most use; exact budgets need a vLLM build with /tokenize"}
	}
	return checkResult{Status: statusOK, Detail: fmt.Sprintf("/tokenize OK (probe=%d tok)", resp.Count)}
}

func checkEmbedEndpoint(ctx context.Context, e *doctorEnv) checkResult {
	if e.router == nil {
		return checkResult{Status: statusSkip, Detail: "router unavailable"}
	}
	emb, _, err := e.router.EmbedderForRole(ctx, model.RoleEmbedder)
	if err != nil {
		return checkResult{Status: statusFail, Detail: "embedder role unresolved: " + err.Error(),
			Fix: "set --embed-endpoint/--embed-model, or --backend echo"}
	}
	v, err := emb.Embed(ctx, []string{"doctor probe"})
	if err != nil || len(v) != 1 || len(v[0]) == 0 {
		return checkResult{Status: statusFail,
			Detail: fmt.Sprintf("embedder produced no vector: %v", err),
			Fix:    "is the embeddings endpoint up? (Ollama: `ollama pull nomic-embed-text`)"}
	}
	return checkResult{Status: statusOK, Detail: fmt.Sprintf("embedder OK (dim=%d)", len(v[0]))}
}

func checkEmbedDim(ctx context.Context, e *doctorEnv) checkResult {
	if e.router == nil || e.episodic == nil || e.semantic == nil {
		return checkResult{Status: statusSkip, Detail: "stores/router unavailable"}
	}
	emb, _, err := e.router.EmbedderForRole(ctx, model.RoleEmbedder)
	if err != nil {
		return checkResult{Status: statusSkip, Detail: "embedder unresolved (see embedding endpoint check)"}
	}
	v, err := emb.Embed(ctx, []string{"dim probe"})
	if err != nil || len(v) != 1 {
		return checkResult{Status: statusSkip, Detail: "could not get live embedder dim"}
	}
	liveDim := len(v[0])

	mismatches := []string{}
	if eps, _ := e.episodic.List(ctx, episodic.ListFilter{Limit: 1}); len(eps) == 1 && len(eps[0].Embedding) != liveDim {
		mismatches = append(mismatches, fmt.Sprintf("episodes.db stored=%dd", len(eps[0].Embedding)))
	}
	if fs, _ := e.semantic.List(ctx, semantic.ListFilter{Limit: 1}); len(fs) == 1 && len(fs[0].Embedding) != liveDim {
		mismatches = append(mismatches, fmt.Sprintf("facts.db stored=%dd", len(fs[0].Embedding)))
	}
	if len(mismatches) > 0 {
		return checkResult{Status: statusFail,
			Detail: fmt.Sprintf("embedder now produces %dd but %s — cosine returns 0 on length mismatch; retrieval is SILENTLY broken",
				liveDim, strings.Join(mismatches, ", ")),
			Fix: "run `tenant memory reembed` to re-embed stored vectors with the current embedder (recovers retrieval, keeps your data), or use a fresh --data dir to start clean"}
	}
	return checkResult{Status: statusOK, Detail: fmt.Sprintf("stored vectors match live embedder (%dd)", liveDim)}
}

func checkStores(ctx context.Context, e *doctorEnv) checkResult {
	if e.episodic == nil || e.semantic == nil {
		return checkResult{Status: statusFail,
			Detail: "could not open episodes.db / facts.db",
			Fix:    "check the --data dir for corruption or permission issues"}
	}
	en, err1 := e.episodic.Count(ctx, true)
	fn, err2 := e.semantic.Count(ctx, true, true)
	if err1 != nil || err2 != nil {
		return checkResult{Status: statusFail,
			Detail: fmt.Sprintf("store query failed (ep=%v fact=%v)", err1, err2),
			Fix:    "the SQLite file may be corrupt — restore from backup or start fresh"}
	}
	return checkResult{Status: statusOK, Detail: fmt.Sprintf("episodes=%d facts=%d, queries OK", en, fn)}
}

func checkSoul(_ context.Context, e *doctorEnv) checkResult {
	_, err := soul.Load(e.c.cfgDir, e.c.agent)
	if err == nil {
		return checkResult{Status: statusOK, Detail: "soul TOML loads"}
	}
	if isNotFound(err) {
		return checkResult{Status: statusOK, Detail: "no saved soul (default scaffold will be used)"}
	}
	return checkResult{Status: statusFail,
		Detail: "soul TOML present but unparseable: " + err.Error(),
		Fix:    "fix the TOML at " + soul.Path(e.c.cfgDir, e.c.agent) + " (or delete it to regenerate)"}
}

func checkDistillCursor(ctx context.Context, e *doctorEnv) checkResult {
	meta, err := improve.OpenMeta(filepath.Join(e.c.dataDir, "tenant_meta.db"))
	if err != nil {
		return checkResult{Status: statusSkip, Detail: "meta store unavailable"}
	}
	defer meta.Close()
	key := "distill_cursor:" + e.c.agent
	cursor, ok, _ := meta.GetInt64(ctx, key)
	if !ok || cursor == 0 {
		return checkResult{Status: statusOK, Detail: "cursor unset/zero (distill will process from the start)"}
	}
	if e.episodic == nil {
		return checkResult{Status: statusSkip, Detail: "episodic store unavailable"}
	}
	n, _ := e.episodic.Count(ctx, true)
	if n == 0 {
		res := checkResult{Status: statusWarn,
			Detail: fmt.Sprintf("cursor=%d but episodes.db is empty — distill will skip forever", cursor),
			Fix:    "reset cursor to 0 (run with --fix)"}
		if e.fix {
			_ = meta.Set(ctx, key+":prev", fmt.Sprintf("%d", cursor)) // record old value
			if err := meta.SetInt64(ctx, key, 0); err == nil {
				res.Fixed, res.Fix = true, fmt.Sprintf("reset cursor %d→0 (old value saved at %s:prev)", cursor, key)
			}
		}
		return res
	}
	return checkResult{Status: statusOK, Detail: fmt.Sprintf("cursor=%d, %d episode(s) in store", cursor, n)}
}

func checkToolCalling(ctx context.Context, e *doctorEnv) checkResult {
	if !e.deep {
		return checkResult{Status: statusSkip, Detail: "run with --deep for the live tool-calling probe"}
	}
	if e.router == nil {
		return checkResult{Status: statusSkip, Detail: "router unavailable"}
	}
	llm, _, err := e.router.LLMForRole(ctx, model.RolePlanner)
	if err != nil {
		return checkResult{Status: statusFail, Detail: "planner unresolved: " + err.Error()}
	}
	resp, err := llm.Generate(ctx, model.GenerateRequest{
		Messages: []model.Message{{Role: "user", Content: "Call the add tool with a=2 b=3."}},
		Tools: []model.ToolSpec{{
			Name: "add", Description: "add two integers",
			Parameters: json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a","b"]}`),
		}},
		ToolChoice: model.ToolChoice{Mode: "required"},
	})
	if err != nil {
		return checkResult{Status: statusFail, Detail: "tool probe generate failed: " + err.Error(),
			Fix: "see the generation-endpoint check"}
	}
	if len(resp.ToolCalls) == 0 {
		return checkResult{Status: statusWarn,
			Detail: "model did not emit a tool call (no native parser AND toolfmt safety net missed it)",
			Fix:    "start vLLM with --enable-auto-tool-choice --tool-call-parser <fmt>, or set the right --vllm-tool-format"}
	}
	// Verify args normalized to a usable object (the hardened path).
	var m map[string]any
	if json.Unmarshal(resp.ToolCalls[0].Arguments, &m) != nil {
		return checkResult{Status: statusFail,
			Detail: "tool call emitted but arguments are not a usable JSON object",
			Fix:    "this should not happen post-hardening — file a bug with the raw response"}
	}
	return checkResult{Status: statusOK,
		Detail: fmt.Sprintf("native tool call OK: %s(%s)", resp.ToolCalls[0].Name, string(resp.ToolCalls[0].Arguments))}
}

func checkMCPSurface(ctx context.Context, e *doctorEnv) checkResult {
	if !e.deep {
		return checkResult{Status: statusSkip, Detail: "run with --deep for the in-process MCP self-check"}
	}
	if e.episodic == nil || e.semantic == nil {
		return checkResult{Status: statusSkip, Detail: "stores unavailable"}
	}
	srv, err := mcpserver.New(mcpserver.Config{
		AgentID: e.c.agent, SoulDir: e.c.cfgDir,
		Episodic: e.episodic, Semantic: e.semantic, Logger: newLogger(),
	})
	if err != nil {
		return checkResult{Status: statusFail, Detail: "mcpserver.New failed: " + err.Error()}
	}
	h := srv.Handler()
	// initialize + tools/list directly against the handler (no transport).
	if _, err := h.HandleRequest(ctx, mcp.MethodInitialize, nil); err != nil {
		return checkResult{Status: statusFail, Detail: "MCP initialize failed: " + err.Error()}
	}
	raw, err := h.HandleRequest(ctx, "tools/list", nil)
	if err != nil {
		return checkResult{Status: statusFail, Detail: "MCP tools/list failed: " + err.Error()}
	}
	var tl struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(raw, &tl)
	if len(tl.Tools) == 0 {
		return checkResult{Status: statusWarn, Detail: "MCP responds but exposes no tools"}
	}
	return checkResult{Status: statusOK,
		Detail: fmt.Sprintf("MCP surface intact (initialize + tools/list, %d tool(s))", len(tl.Tools))}
}

// --- helpers ---

func (e *doctorEnv) listModels(ctx context.Context, base string) ([]string, error) {
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := e.getJSON(ctx, strings.TrimRight(base, "/")+"/v1/models", &out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out.Data))
	for _, d := range out.Data {
		ids = append(ids, d.ID)
	}
	return ids, nil
}

func (e *doctorEnv) getJSON(ctx context.Context, url string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := e.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (e *doctorEnv) postJSON(ctx context.Context, url string, body, out any) error {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func isNotFound(err error) bool {
	return errors.Is(err, soul.ErrNotFound)
}

// --- new checks (2026-05-25): config, credentials, agent profiles, db
// integrity, skills store, research store, wiki dir, chrome. These cover
// failure modes we've shipped fixes for but didn't have a doctor probe
// to catch upfront. ---

// checkLaunchConfig validates config.json + populates e.lc for downstream
// checks. Required fields per the v2 schema: at least an active provider
// that exists in the Providers map. Missing-config is OK (the agent uses
// defaults / echo backend); BROKEN config is FAIL.
func checkLaunchConfig(_ context.Context, e *doctorEnv) checkResult {
	lc, err := loadLaunchConfig(e.c.cfgDir)
	if err != nil {
		return checkResult{Status: statusFail,
			Detail: "config.json present but unparseable: " + err.Error(),
			Fix:    "fix or `rm " + launchConfigPath(e.c.cfgDir) + "` then `tenant setup`"}
	}
	e.lc = lc
	if lc.Provider == "" {
		return checkResult{Status: statusOK,
			Detail: "no active provider configured (defaults / flags used)"}
	}
	pc := lc.Providers[lc.Provider]
	if pc == nil {
		return checkResult{Status: statusFail,
			Detail: fmt.Sprintf("active provider %q is not in the providers map", lc.Provider),
			Fix:    "`tenant setup` or `tenant model use <name>` with a registered name"}
	}
	if _, ok := providerKinds[pc.Kind]; !ok {
		return checkResult{Status: statusFail,
			Detail: fmt.Sprintf("provider %q has unknown kind %q", lc.Provider, pc.Kind),
			Fix:    "edit config.json — kind must be one of: " + strings.Join(providerOrder, ", ")}
	}
	return checkResult{Status: statusOK,
		Detail: fmt.Sprintf("active=%s/%s (%s)", lc.Provider, pc.Kind, firstNonEmpty(pc.Model, "auto-detect"))}
}

// checkCredentials validates credentials.json — parses cleanly, has the
// right perms (0600 on Unix), and every KEYED provider in the active
// config has a resolvable secret (env-var ref OR stored). On Windows
// the perm check is informational only (POSIX bits don't apply).
func checkCredentials(_ context.Context, e *doctorEnv) checkResult {
	creds, err := loadCredentials(e.c.cfgDir)
	if err != nil {
		return checkResult{Status: statusFail,
			Detail: "credentials.json present but unparseable: " + err.Error(),
			Fix:    "rm " + credentialsPath(e.c.cfgDir) + " then re-run `tenant setup` for any keyed provider"}
	}
	// Perm check (Unix only — Windows ACLs don't map cleanly).
	if info, err := os.Stat(credentialsPath(e.c.cfgDir)); err == nil {
		mode := info.Mode().Perm()
		if mode&0o077 != 0 && !isWindowsRuntime() {
			return checkResult{Status: statusWarn,
				Detail: fmt.Sprintf("credentials.json has world/group-readable perms %#o (should be 0600)", mode),
				Fix:    "chmod 0600 " + credentialsPath(e.c.cfgDir)}
		}
	}
	// Per-provider secret resolution.
	if e.lc == nil {
		return checkResult{Status: statusOK, Detail: "credentials.json OK; no providers to validate"}
	}
	var missing []string
	for name, pc := range e.lc.Providers {
		pk, ok := providerKinds[pc.Kind]
		if !ok || !pk.NeedsKey {
			continue
		}
		if pc.Auth.KeyEnv == "" && !pc.Auth.Stored {
			missing = append(missing, fmt.Sprintf("%s (no auth configured)", name))
			continue
		}
		if pc.Auth.KeyEnv != "" {
			if os.Getenv(pc.Auth.KeyEnv) == "" {
				missing = append(missing, fmt.Sprintf("%s (env %s unset)", name, pc.Auth.KeyEnv))
			}
			continue
		}
		// Stored: check the secret actually lives in creds.
		if creds.get(name) == "" {
			missing = append(missing, fmt.Sprintf("%s (stored secret missing)", name))
		}
	}
	if len(missing) > 0 {
		return checkResult{Status: statusWarn,
			Detail: "keyed providers with missing secrets: " + strings.Join(missing, ", "),
			Fix:    "rerun `/model add-cloud <kind> <key>` (TUI) or `tenant model add <name> --kind <kind> --api-key <key>` (CLI)"}
	}
	return checkResult{Status: statusOK, Detail: fmt.Sprintf("credentials.json OK; %d secret(s) stored", len(creds.Secrets))}
}

// checkDBIntegrity runs PRAGMA integrity_check on every SQLite DB tenant
// uses. Catches the "database disk image is malformed" class of corruption
// that silently kills assemble (a recurring live failure, May 2026). With
// --fix, a corrupt DB is renamed to .corrupted-<timestamp> so the next
// startup creates a fresh file — no data loss without an explicit operator
// action.
func checkDBIntegrity(ctx context.Context, e *doctorEnv) checkResult {
	type dbProbe struct {
		label string
		path  string
		db    *sql.DB // when nil, the file might still exist — we try to open it
	}
	probes := []dbProbe{}
	if e.episodic != nil {
		probes = append(probes, dbProbe{"episodes.db", filepath.Join(e.c.dataDir, "episodes.db"), e.episodic.DB()})
	}
	if e.semantic != nil {
		probes = append(probes, dbProbe{"facts.db", filepath.Join(e.c.dataDir, "facts.db"), e.semantic.DB()})
	}
	if e.skills != nil {
		probes = append(probes, dbProbe{"skills.db", filepath.Join(e.c.dataDir, "skills.db"), e.skills.DB()})
	}
	if len(probes) == 0 {
		return checkResult{Status: statusSkip, Detail: "no stores opened"}
	}
	var bad []string
	var fixed []string
	for _, p := range probes {
		issue := runIntegrityCheck(ctx, p.db)
		if issue == "" {
			continue
		}
		bad = append(bad, fmt.Sprintf("%s: %s", p.label, clip(issue, 100)))
		if e.fix {
			// Close the DB before rename. Rename keeps the corrupt file as
			// evidence; tenant will create a fresh one on next startup.
			_ = p.db.Close()
			stamp := time.Now().Format("20060102-150405")
			target := p.path + ".corrupted-" + stamp
			if rerr := os.Rename(p.path, target); rerr == nil {
				// Also move the WAL + SHM siblings if present.
				_ = os.Rename(p.path+"-wal", target+"-wal")
				_ = os.Rename(p.path+"-shm", target+"-shm")
				fixed = append(fixed, fmt.Sprintf("%s → %s (restart tenant to create fresh)", p.label, filepath.Base(target)))
			}
		}
	}
	if len(bad) == 0 {
		return checkResult{Status: statusOK,
			Detail: fmt.Sprintf("integrity_check OK on %d store(s)", len(probes))}
	}
	r := checkResult{Status: statusFail, Detail: "corrupt: " + strings.Join(bad, "; ")}
	if len(fixed) > 0 {
		r.Status, r.Fixed, r.Fix = statusWarn, true, "renamed corrupt: "+strings.Join(fixed, ", ")
	} else {
		r.Fix = "rerun with --fix to rename the corrupt file(s) (tenant creates a fresh DB on next startup; the old file is preserved as .corrupted-<timestamp>)"
	}
	return r
}

// runIntegrityCheck returns "" on healthy, else a description of the issues.
// Tolerates the case where the DB is so corrupt the PRAGMA itself errors
// (treated as a single issue).
func runIntegrityCheck(ctx context.Context, db *sql.DB) string {
	if db == nil {
		return "db handle nil"
	}
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return err.Error()
	}
	defer rows.Close()
	var issues []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "scan: " + err.Error()
		}
		if strings.TrimSpace(line) != "ok" {
			issues = append(issues, line)
		}
	}
	if err := rows.Err(); err != nil {
		return "iter: " + err.Error()
	}
	if len(issues) == 0 {
		return ""
	}
	return strings.Join(issues, "; ")
}

// checkAgentProfiles validates every entry in launchConfig.Agents — the
// named sub-agent registry (the /agents feature). A stale profile pointing
// at a deleted provider, or one whose model can't be resolved, would
// silently fall back to the orchestrator's router at spawn time (wrong
// model, no error). Doctor surfaces the drift upfront.
func checkAgentProfiles(_ context.Context, e *doctorEnv) checkResult {
	if e.lc == nil || len(e.lc.Agents) == 0 {
		return checkResult{Status: statusSkip, Detail: "no named agent profiles configured"}
	}
	var bad []string
	var ok int
	for name, ap := range e.lc.Agents {
		pc := e.lc.Providers[ap.Provider]
		if pc == nil {
			bad = append(bad, fmt.Sprintf("%s → provider %q missing", name, ap.Provider))
			continue
		}
		pk, knownKind := providerKinds[pc.Kind]
		if !knownKind {
			bad = append(bad, fmt.Sprintf("%s → provider %q has unknown kind %q", name, ap.Provider, pc.Kind))
			continue
		}
		mdl := firstNonEmpty(ap.Model, pc.Model, pk.DefaultModel)
		if mdl == "" {
			bad = append(bad, fmt.Sprintf("%s → no model resolvable from provider %q", name, ap.Provider))
			continue
		}
		// Keyed provider must have a resolvable secret (env-var present or
		// stored). Mirrors the buildProfileRouter pre-flight check.
		if pk.NeedsKey {
			key := resolveSecret(e.c.cfgDir, ap.Provider, pc.Auth)
			if key == "" {
				bad = append(bad, fmt.Sprintf("%s → provider %q has no resolvable API key", name, ap.Provider))
				continue
			}
		}
		ok++
	}
	if len(bad) > 0 {
		return checkResult{Status: statusFail,
			Detail: fmt.Sprintf("%d valid, %d broken: %s", ok, len(bad), strings.Join(bad, "; ")),
			Fix:    "fix or remove the broken profile(s) via `/agents remove <name>` or `tenant agents remove <name>`"}
	}
	return checkResult{Status: statusOK,
		Detail: fmt.Sprintf("%d agent profile(s) valid", ok)}
}

// checkSkillsStore — skills.db opens + count works. Integrity is covered
// by checkDBIntegrity; this is the higher-level "the T4 library is queryable"
// check. Optionally reports whether the GStack starter bundle is installed.
func checkSkillsStore(ctx context.Context, e *doctorEnv) checkResult {
	if e.skills == nil {
		return checkResult{Status: statusWarn,
			Detail: "skills.db could not be opened",
			Fix:    "check perms / disk on " + filepath.Join(e.c.dataDir, "skills.db")}
	}
	n, err := e.skills.Count(ctx, e.c.agent)
	if err != nil {
		return checkResult{Status: statusFail,
			Detail: "skills.Count failed: " + err.Error(),
			Fix:    "see db-integrity check; corrupt skills.db blocks T4 retrieval"}
	}
	// Optional GStack bundle presence (informational only — many operators
	// won't install it).
	gstack := 0
	if list, _ := e.skills.List(ctx, skills.ListFilter{AgentID: e.c.agent, IncludeDisabled: true}); list != nil {
		want := map[string]bool{
			"investigate-systematically": true, "boil-the-lake-completeness": true,
			"structured-ask": true, "founder-voice": true, "status-escalation": true,
		}
		for _, s := range list {
			if want[s.Name] {
				gstack++
			}
		}
	}
	detail := fmt.Sprintf("skills.db OK, %d skill(s) for agent=%s", n, e.c.agent)
	if gstack > 0 {
		detail += fmt.Sprintf(" (GStack bundle: %d/5 installed)", gstack)
	}
	return checkResult{Status: statusOK, Detail: detail}
}

// checkResearchStore — research dir opens + List works without erroring on
// corrupt manifests. Surfaces stuck/orphaned runs as info.
func checkResearchStore(_ context.Context, e *doctorEnv) checkResult {
	store, err := research.New(e.c.dataDir)
	if err != nil {
		return checkResult{Status: statusFail,
			Detail: "research store: " + err.Error(),
			Fix:    "check perms / disk on " + filepath.Join(e.c.dataDir, "research")}
	}
	runs, err := store.List(0)
	if err != nil {
		return checkResult{Status: statusFail,
			Detail: "research.List failed: " + err.Error(),
			Fix:    "check perms on " + store.Dir()}
	}
	stuck := 0
	for _, r := range runs {
		if r.Status == research.StatusRunning {
			stuck++
		}
	}
	detail := fmt.Sprintf("research store OK, %d run(s) in %s", len(runs), store.Dir())
	if stuck > 0 {
		return checkResult{Status: statusWarn,
			Detail: detail + fmt.Sprintf(" — %d orphaned 'running' run(s) (tenant crashed mid-research?)", stuck),
			Fix:    "`/research history` to inspect; `/research delete <id>` to remove the orphan(s)"}
	}
	return checkResult{Status: statusOK, Detail: detail}
}

// checkWikiDir — only meaningful when an operator configured a wiki dir
// for the wiki plugin. Skips silently when none is set; warns if set but
// missing/unreadable.
func checkWikiDir(_ context.Context, e *doctorEnv) checkResult {
	if e.lc == nil || e.lc.Skills == nil {
		return checkResult{Status: statusSkip, Detail: "no wiki configured"}
	}
	wcfg := e.lc.Skills["wiki"]
	if wcfg == nil || wcfg.Settings == nil {
		return checkResult{Status: statusSkip, Detail: "no wiki configured"}
	}
	dir := expandPath(strings.TrimSpace(wcfg.Settings["dir"]))
	if dir == "" {
		return checkResult{Status: statusSkip, Detail: "no wiki dir configured"}
	}
	info, err := os.Stat(dir)
	if err != nil {
		return checkResult{Status: statusFail,
			Detail: fmt.Sprintf("wiki dir %s: %v", dir, err),
			Fix:    "create it or update the wiki.settings.dir in config.json"}
	}
	if !info.IsDir() {
		return checkResult{Status: statusFail,
			Detail: fmt.Sprintf("wiki path %s is not a directory", dir),
			Fix:    "point wiki.settings.dir at an actual directory"}
	}
	// Count .md files. Empty dir is a configuration-not-yet-done state,
	// not a failure — but it silently breaks fitness-004/015/017 and any
	// wiki_search/wiki_list user query, so surface as WARN with a
	// remediation. fitness-016 (no-result) actually passes on an empty
	// wiki, but the 3-of-4 silent-failure case is what we're warning on.
	// TEN-24 motivation: operator runs `tenant eval --subset=fitness`,
	// most wiki tasks fail, no obvious why. Doctor catches it upfront.
	files, _ := os.ReadDir(dir)
	mdCount := 0
	for _, f := range files {
		if strings.HasSuffix(strings.ToLower(f.Name()), ".md") {
			mdCount++
		}
	}
	if mdCount == 0 {
		return checkResult{Status: statusWarn,
			Detail: fmt.Sprintf("wiki dir %s exists but contains no markdown files — wiki_search/wiki_list will return empty", dir),
			Fix:    "drop .md notes into the dir, OR point wiki.settings.dir at an existing notes directory"}
	}
	return checkResult{Status: statusOK,
		Detail: fmt.Sprintf("wiki dir %s OK (%d markdown file(s))", dir, mdCount)}
}

// checkWikiFreshness — TEN-48 minimal diagnostic. Detects the "wiki
// file on disk is newer than the index sidecar" condition that caused
// the 2026-05-26 lost-context bug (TEN-43). Compares each .md file's
// mtime to the sidecar's mtime; reports a WARN if any file is newer
// (the next wiki_search will lazy-reindex, but until then the file is
// dark to retrieval). TEN-44 fixed the proximate cause for research
// deposits; this check catches the GENERAL case (manual `cp` to wiki,
// editor saves, future producers that bypass the deposit path).
func checkWikiFreshness(_ context.Context, e *doctorEnv) checkResult {
	if e.lc == nil || e.lc.Skills == nil {
		return checkResult{Status: statusSkip, Detail: "no wiki configured"}
	}
	wcfg := e.lc.Skills["wiki"]
	if wcfg == nil || wcfg.Settings == nil {
		return checkResult{Status: statusSkip, Detail: "no wiki configured"}
	}
	dir := expandPath(strings.TrimSpace(wcfg.Settings["dir"]))
	if dir == "" {
		return checkResult{Status: statusSkip, Detail: "no wiki dir configured"}
	}
	// Sidecar path: mirror the formula used in toolmux.go::buildWikiIndex
	// and commands.go::cmdWiki (one sidecar per vault, hashed by abs path).
	absVault, _ := filepath.Abs(dir)
	h := fnv.New64a()
	_, _ = h.Write([]byte(absVault))
	sidecar := filepath.Join(e.c.dataDir, "wiki", fmt.Sprintf("%x.json", h.Sum64()))
	sideInfo, err := os.Stat(sidecar)
	if err != nil {
		// No sidecar yet → first launch / wiki never indexed. Not a failure;
		// the index builds on first wiki_search.
		return checkResult{Status: statusOK,
			Detail: "wiki sidecar not present (will be built on first search)"}
	}
	sideMtime := sideInfo.ModTime()

	// Walk wiki dir for .md files; count those newer than the sidecar.
	var staleFiles []string
	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // tolerant: skip unreadable entries
		}
		if d.IsDir() {
			if d.Name() != "." && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir // skip hidden dirs (.git, .obsidian)
			}
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if info.ModTime().After(sideMtime) {
			rel, _ := filepath.Rel(dir, path)
			staleFiles = append(staleFiles, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return checkResult{Status: statusWarn,
			Detail: fmt.Sprintf("wiki freshness walk failed: %v", err)}
	}
	if len(staleFiles) == 0 {
		return checkResult{Status: statusOK,
			Detail: fmt.Sprintf("wiki index fresh (sidecar mtime %s)", sideMtime.Format("2006-01-02 15:04:05"))}
	}
	// Cap the rendered list so a huge dir doesn't blow the doctor output.
	preview := staleFiles
	const maxShown = 5
	suffix := ""
	if len(preview) > maxShown {
		preview = preview[:maxShown]
		suffix = fmt.Sprintf(" (+%d more)", len(staleFiles)-maxShown)
	}
	return checkResult{Status: statusWarn,
		Detail: fmt.Sprintf("%d file(s) on disk newer than wiki index%s: %s — search will return stale results until next wiki_search re-indexes",
			len(staleFiles), suffix, strings.Join(preview, ", ")),
		Fix: "run `tenant wiki reindex` OR fire any wiki_search call to trigger lazy reindex"}
}

// checkChrome — only meaningful when the web plugin is configured/enabled.
// detectChrome probes the well-known OS install paths; missing Chrome
// silently breaks web_navigate at first use, hence the upfront warning.
func checkChrome(_ context.Context, e *doctorEnv) checkResult {
	if e.lc == nil || e.lc.Skills == nil {
		return checkResult{Status: statusSkip, Detail: "web plugin not configured"}
	}
	wcfg := e.lc.Skills["web"]
	if wcfg == nil || !wcfg.Enabled {
		return checkResult{Status: statusSkip, Detail: "web plugin not enabled"}
	}
	// Optional override path from the web plugin config.
	overridePath := ""
	if wcfg.Settings != nil {
		overridePath = wcfg.Settings["chrome_path"]
	}
	path, err := web.DetectChrome(overridePath)
	if err != nil {
		return checkResult{Status: statusFail,
			Detail: "no Chrome/Chromium/Edge found on this system",
			Fix:    "install Chrome, OR set web.settings.chrome_path in config.json"}
	}
	return checkResult{Status: statusOK, Detail: "chrome at " + path}
}

// checkOSProcesses verifies the platform-specific process-listing tool
// (`ps` on Unix, `tasklist` on Windows) is on PATH. The os_processes
// agent tool shells out to it (see internal/plugins/osys/osys.go:270);
// on minimal containers (Alpine, distroless, scratch) `ps` is commonly
// absent and the agent's process-listing tool silently fails at first
// use with "executable file not found in $PATH". Doctor surfaces it
// upfront so the operator can apk add procps (or equivalent) before
// running fitness evals.
//
// TEN-25 motivation: fitness-019-os-processes depends on this binary;
// if it's missing the task fails for an infrastructure reason, not an
// agent-skill reason.
func checkOSProcesses(_ context.Context, _ *doctorEnv) checkResult {
	tool := "ps"
	if runtime.GOOS == "windows" {
		tool = "tasklist"
	}
	path, err := exec.LookPath(tool)
	if err != nil {
		fix := "install procps (Alpine: apk add procps; Debian: apt install procps)"
		if runtime.GOOS == "windows" {
			fix = "tasklist ships with Windows by default — check PATH"
		}
		return checkResult{Status: statusWarn,
			Detail: fmt.Sprintf("%s not found on PATH — os_processes tool will fail at runtime", tool),
			Fix:    fix}
	}
	return checkResult{Status: statusOK, Detail: tool + " at " + path}
}

// checkGSuite verifies the gsuite plugin's stored auth config can mint
// a token against Google. TEN-65: mirrors checkDiscord's tier shape.
//
//	skip   — gsuite not configured / not enabled
//	FAIL   — config invalid (auth mode unknown, sa_json missing/invalid,
//	         subject missing for sa mode)
//	WARN   — config valid but probe couldn't reach Google (offline tolerant)
//	OK     — Gmail.Search returns successfully (token mints + Gmail responds)
//
// Reuses probeGSuite from skillctl_gsuite.go so the doctor check and
// the `/skill probe gsuite` command share a code path. 5s timeout
// enforced via runProbe.
func checkGSuite(_ context.Context, e *doctorEnv) checkResult {
	if e.lc == nil || e.lc.Skills == nil {
		return checkResult{Status: statusSkip, Detail: "gsuite plugin not configured"}
	}
	gcfg := e.lc.Skills["gsuite"]
	if gcfg == nil || !gcfg.Enabled {
		return checkResult{Status: statusSkip, Detail: "gsuite plugin not enabled"}
	}
	settings := gcfg.Settings
	if settings == nil {
		settings = map[string]string{}
	}
	// Don't load credentials separately — gsuite has no credentials.json
	// secrets (auth/sa_json/subject are all non-secret settings).
	creds, _ := loadCredentials(e.c.cfgDir)

	auth := settings["auth"]
	if auth == "" {
		auth = "sa" // catalog default — business via SA + DWD is primary
	}
	switch auth {
	case "gcloud", "sa", "oauth":
		// fall through
	default:
		return checkResult{Status: statusFail,
			Detail: fmt.Sprintf("gsuite auth mode %q invalid (expected oauth, gcloud, or sa)", auth),
			Fix:    "run `/configure gsuite` and pick a sign-in method"}
	}
	if auth == "sa" {
		if settings["sa_json"] == "" || settings["subject"] == "" {
			return checkResult{Status: statusFail,
				Detail: "gsuite auth=sa requires sa_json + subject",
				Fix:    "run `/configure gsuite auth=sa sa_json=/path/to/key.json subject=user@example.com`"}
		}
		// Operator note (DWD scopes): write posture (--allow-send) now needs
		// gmail.modify+gmail.send, full calendar, and full drive. The
		// domain-wide-delegation client in the Admin console pins an explicit
		// scope allow-list — if it only lists the readonly scopes, every
		// write call fails with "unauthorized_client". Admin must add the
		// write scopes there. We can't detect that here without a live write.
	}
	if auth == "oauth" {
		// Need EITHER an operator-supplied creds path OR embedded creds
		// available (via go:embed compiled-in, or runtime file under
		// cfgDir from `tenant oauth-setup gsuite ...`). Caught here so
		// the probe doesn't fail with a confusing "no OAuth creds" later.
		hasOperatorCreds := settings["oauth_creds_json"] != ""
		hasEmbedded := gsuite.HasEmbeddedOAuth(e.c.cfgDir)
		if !hasOperatorCreds && !hasEmbedded {
			return checkResult{Status: statusFail,
				Detail: "gsuite auth=oauth needs an OAuth client (none configured)",
				Fix: "if you maintain this build: run `tenant oauth-setup gsuite <path-to-client.json>` (see docs/SETUP-GSUITE-OAUTH.md). " +
					"Otherwise: run `/configure gsuite` and provide an OAuth client JSON path, OR pick a different auth mode."}
		}
		// Operator note (scope migration): wiring in the official clients
		// broadened scopes (Drive read/write, gmail.modify, full calendar).
		// A token minted BEFORE the upgrade lacks them, so those calls fail
		// with "Insufficient Permission" until the user re-consents (the
		// browser flow re-runs once the cached token is cleared). We don't
		// detect this here (would need a tokeninfo round-trip per run).
	}

	// runProbe enforces the 5s timeout + goroutine-safe wrapping.
	identity, err := runProbe(skillKinds["gsuite"], creds, settings)
	if err != nil {
		return checkResult{Status: statusWarn,
			Detail: "gsuite config looks valid but probe failed: " + err.Error(),
			Fix:    "if intentional offline, ignore; else verify auth setup with `/skill probe gsuite`"}
	}
	return checkResult{Status: statusOK, Detail: "gsuite reachable — " + identity}
}

// checkDiscord verifies the Discord plugin can talk to discord.com when
// it's enabled. Tiers:
//
//	skip   — plugin not configured / not enabled
//	FAIL   — token unresolvable (env var empty, secret missing)
//	WARN   — token present but /users/@me 4xx (likely revoked / wrong token)
//	OK     — /users/@me returns the bot's identity
//
// Best-effort and offline-tolerant: a network error during the probe
// degrades to WARN (we know the token resolved; can't confirm Discord
// is reachable). The plugin still works offline-tolerant via the
// activator pattern at first /enable.
func checkDiscord(ctx context.Context, e *doctorEnv) checkResult {
	if e.lc == nil || e.lc.Skills == nil {
		return checkResult{Status: statusSkip, Detail: "discord plugin not configured"}
	}
	dcfg := e.lc.Skills["discord"]
	if dcfg == nil || !dcfg.Enabled {
		return checkResult{Status: statusSkip, Detail: "discord plugin not enabled"}
	}
	// Resolve token. Env var wins over stored secret (matches
	// pluginFlags resolution at toolmux.go).
	token := os.Getenv("DISCORD_BOT_TOKEN")
	if token == "" {
		token = resolveSecret(e.c.cfgDir, skillSecretID("discord", "token"), authCfg{Stored: true})
	}
	if token == "" {
		return checkResult{Status: statusFail,
			Detail: "discord enabled but bot token is missing",
			Fix:    "set $DISCORD_BOT_TOKEN, or run `tenant setup` and re-enter under Discord"}
	}
	// Probe /users/@me with a short timeout — confirms the token is
	// valid AND Discord is reachable. Best-effort: a network error
	// becomes WARN, not FAIL, so an offline diagnostic still passes
	// the meaningful checks.
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(probeCtx, "GET", "https://discord.com/api/v10/users/@me", nil)
	req.Header.Set("Authorization", "Bot "+token)
	req.Header.Set("User-Agent", "DiscordBot (https://github.com/tenant-mcp/tenant, 1.0)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return checkResult{Status: statusWarn,
			Detail: "token present but could not reach discord.com: " + err.Error(),
			Fix:    "if intentional offline, ignore; else check network / firewall"}
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 {
		return checkResult{Status: statusFail,
			Detail: "discord returned 401 Unauthorized — token is invalid or revoked",
			Fix:    "regenerate the bot token at https://discord.com/developers/applications and rerun `tenant setup`"}
	}
	if resp.StatusCode >= 400 {
		return checkResult{Status: statusWarn,
			Detail: fmt.Sprintf("discord /users/@me returned HTTP %d", resp.StatusCode),
			Fix:    "check the Developer Portal for the bot's status"}
	}
	return checkResult{Status: statusOK, Detail: "bot token valid; discord.com reachable"}
}

// checkX verifies the X / Twitter plugin can talk to api.x.com when it's
// enabled (TEN-67). Tiers mirror checkDiscord:
//
//	skip — plugin not configured / not enabled
//	FAIL — bearer unresolvable (env empty, secret missing) or 401 (revoked)
//	WARN — bearer present but network unreachable, or a non-401 4xx/5xx
//	OK   — /2/users/me returns the account identity
//
// Best-effort and offline-tolerant: a network error degrades to WARN (the
// bearer resolved; we just can't confirm x.com is reachable).
func checkX(ctx context.Context, e *doctorEnv) checkResult {
	if e.lc == nil || e.lc.Skills == nil {
		return checkResult{Status: statusSkip, Detail: "x plugin not configured"}
	}
	xcfg := e.lc.Skills["x"]
	if xcfg == nil || !xcfg.Enabled {
		return checkResult{Status: statusSkip, Detail: "x plugin not enabled"}
	}
	// Resolve bearer. Env wins over stored secret (matches toolmux.go).
	bearer := os.Getenv("X_BEARER_TOKEN")
	if bearer == "" {
		bearer = resolveSecret(e.c.cfgDir, skillSecretID("x", "bearer"), authCfg{Stored: true})
	}
	if bearer == "" {
		return checkResult{Status: statusFail,
			Detail: "x enabled but bearer token is missing",
			Fix:    "set $X_BEARER_TOKEN, or run `/configure x <bearer>`"}
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(probeCtx, "GET", "https://api.x.com/2/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return checkResult{Status: statusWarn,
			Detail: "bearer present but could not reach api.x.com: " + err.Error(),
			Fix:    "if intentional offline, ignore; else check network / firewall"}
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 {
		return checkResult{Status: statusFail,
			Detail: "x returned 401 Unauthorized — bearer token is invalid or revoked",
			Fix:    "regenerate the bearer at https://developer.x.com and run `/configure x <bearer>`"}
	}
	if resp.StatusCode >= 400 {
		return checkResult{Status: statusWarn,
			Detail: fmt.Sprintf("x /2/users/me returned HTTP %d", resp.StatusCode),
			Fix:    "check the app's status / permissions at https://developer.x.com"}
	}
	return checkResult{Status: statusOK, Detail: "bearer valid; api.x.com reachable"}
}

// checkIMessage verifies the iMessage transport is usable (TEN-68). The
// native macOS transport reads ~/Library/Messages/chat.db (requires Full
// Disk Access) and sends via osascript (Automation access, prompted on
// first send). Tiers:
//
//	skip — imessage not configured; or native on a non-macOS host; nothing
//	       to probe locally without a BlueBubbles URL
//	OK   — BlueBubbles URL configured (server bridge), OR native chat.db is
//	       readable (FDA granted)
//	WARN — native selected but chat.db can't be read yet (FDA not granted)
//	       or osascript is missing — both come with an actionable fix
//
// The FDA probe opens chat.db read-only, which forces a real read, so a
// macOS TCC denial surfaces here rather than at first use. It's a WARN
// (not FAIL) because a present-but-unconfigured skill shouldn't flip the
// doctor exit code; the fix text is explicit.
func checkIMessage(_ context.Context, e *doctorEnv) checkResult {
	if e.lc == nil || e.lc.Skills == nil {
		return checkResult{Status: statusSkip, Detail: "imessage not configured"}
	}
	icfg := e.lc.Skills["imessage"]
	if icfg == nil {
		return checkResult{Status: statusSkip, Detail: "imessage not configured"}
	}
	// Backend selection mirrors toolmux/cmdIMessage: a BlueBubbles URL
	// (env wins over settings) opts into the server bridge; else native.
	url := os.Getenv("BLUEBUBBLES_URL")
	if url == "" && icfg.Settings != nil {
		url = strings.TrimSpace(icfg.Settings["url"])
	}
	if url != "" {
		return checkResult{Status: statusOK, Detail: "BlueBubbles transport configured (url=" + url + ")"}
	}
	// Native transport is macOS-only.
	if runtime.GOOS != "darwin" {
		return checkResult{Status: statusSkip,
			Detail: "native iMessage is macOS-only; set a BlueBubbles --bb-url to use iMessage on this host"}
	}
	if _, err := exec.LookPath("osascript"); err != nil {
		return checkResult{Status: statusWarn,
			Detail: "osascript not found — iMessage sends will fail",
			Fix:    "osascript ships with macOS at /usr/bin/osascript; verify your PATH / system integrity"}
	}
	nat, err := imessage.OpenNative(imessage.NativeConfig{})
	if err != nil {
		return checkResult{Status: statusWarn,
			Detail: "native transport selected but Messages chat.db is unreadable: " + err.Error(),
			Fix:    "grant Full Disk Access to your terminal (or the tenant binary) in System Settings → Privacy & Security → Full Disk Access, then rerun"}
	}
	_ = nat.Close()
	return checkResult{Status: statusOK,
		Detail: "native chat.db readable (Full Disk Access granted); Automation is prompted on first send"}
}

// checkDashboard verifies the web control panel (TEN-76/79) when it's
// configured. Tiers mirror checkDiscord:
//
//	skip   — dashboard not configured (Enabled=false AND no Addr)
//	FAIL   — non-loopback Addr without TLS+Auth in config (the same
//	         fail-closed rule Server.checkBindPolicy enforces at Run);
//	         this is a config lint surfaced even if the server is down
//	FAIL   — reachable but returns 401 (auth token mismatch)
//	WARN   — configured but connection refused (not running)
//	OK     — reachable + 200 (dashboard healthy)
//
// The probe hits GET <scheme>://<addr>/healthz (https when TLSCert set)
// with a short timeout, sending Authorization: Bearer <auth> when an
// auth token is configured. Uses e.httpc (the injectable client seam the
// other HTTP checks share) so tests can point it at an httptest server.
func checkDashboard(ctx context.Context, e *doctorEnv) checkResult {
	if e.lc == nil {
		return checkResult{Status: statusSkip, Detail: "dashboard not configured"}
	}
	dc := e.lc.Dashboard
	// Tri-state Enabled (TEN-86): nil/&true ⇒ on (default). Only an explicit
	// &false with no addr counts as "not configured" → skip the probe.
	if !dc.dashboardEnabled() && dc.Addr == "" {
		return checkResult{Status: statusSkip, Detail: "dashboard disabled"}
	}
	addr := dc.Addr
	if addr == "" {
		addr = "127.0.0.1:8770" // default bind (mirrors dashboardConfig docs)
	}

	// Config lint (independent of whether the server is up): a non-loopback
	// bind without BOTH TLS and an auth token is fail-closed — mirrors
	// dashboard.Server.checkBindPolicy. Surface it as FAIL so `doctor`
	// catches the misconfig before launch refuses to bind.
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr // no port → treat whole value as host (best-effort)
	}
	if !isLoopbackAddr(host) {
		if dc.TLSCert == "" || dc.TLSKey == "" || dc.Auth == "" {
			return checkResult{Status: statusFail,
				Detail: fmt.Sprintf("dashboard binds non-loopback %q without TLS+auth — launch will refuse to start", addr),
				Fix:    "set dashboard.tls_cert/tls_key and dashboard.auth, or bind 127.0.0.1"}
		}
	}

	scheme := "http"
	if dc.TLSCert != "" {
		scheme = "https"
	}
	url := fmt.Sprintf("%s://%s/healthz", scheme, addr)
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
	if dc.Auth != "" {
		req.Header.Set("Authorization", "Bearer "+dc.Auth)
	}
	// e.httpc is the injectable seam (set in cmdDoctor); fall back to the
	// default client when unset so a directly-constructed doctorEnv (tests)
	// doesn't nil-panic.
	httpc := e.httpc
	if httpc == nil {
		httpc = http.DefaultClient
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return checkResult{Status: statusWarn,
			Detail: fmt.Sprintf("dashboard configured (%s) but not reachable: %v", addr, err),
			Fix:    "start it with `tenant --dashboard` (or check the addr/firewall)"}
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 {
		return checkResult{Status: statusFail,
			Detail: "dashboard returned 401 — auth token mismatch",
			Fix:    "ensure dashboard.auth here matches the running server's token"}
	}
	if resp.StatusCode != 200 {
		return checkResult{Status: statusWarn,
			Detail: fmt.Sprintf("dashboard /healthz returned HTTP %d", resp.StatusCode)}
	}
	return checkResult{Status: statusOK, Detail: "dashboard healthy at " + addr}
}

// serveStuckTurnSecs is how long an in-flight turn may run before checkServe
// flags it as possibly stuck — well past a normal turn (seconds to ~1 min).
const serveStuckTurnSecs = 300

// checkServe probes a running hub's /api/status (TEN-194) for the headless
// wedge symptoms a daemon operator can't see from a terminal: a turn stuck
// active for too long, or dangerous-action approvals waiting in the queue. It
// SKIPs when nothing answers — a daemon that simply isn't running is not a
// fault (and a *configured but down* dashboard is already WARNed by
// checkDashboard, so this never double-reports).
func checkServe(ctx context.Context, e *doctorEnv) checkResult {
	if e.lc == nil {
		return checkResult{Status: statusSkip, Detail: "no launch config"}
	}
	dc := e.lc.Dashboard
	if !dc.dashboardEnabled() && dc.Addr == "" {
		return checkResult{Status: statusSkip, Detail: "dashboard disabled — no hub to probe"}
	}
	addr := dc.Addr
	if addr == "" {
		addr = "127.0.0.1:8770"
	}
	scheme := "http"
	if dc.TLSCert != "" {
		scheme = "https"
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(probeCtx, http.MethodGet, fmt.Sprintf("%s://%s/api/status", scheme, addr), nil)
	if dc.Auth != "" {
		req.Header.Set("Authorization", "Bearer "+dc.Auth)
	}
	httpc := e.httpc
	if httpc == nil {
		httpc = http.DefaultClient
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return checkResult{Status: statusSkip, Detail: "no hub reachable at " + addr + " (not running)"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return checkResult{Status: statusSkip, Detail: fmt.Sprintf("/api/status returned HTTP %d", resp.StatusCode)}
	}
	var st struct {
		TurnActive       bool `json:"turn_active"`
		TurnAgeSecs      int  `json:"turn_age_secs"`
		PendingApprovals int  `json:"pending_approvals"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return checkResult{Status: statusWarn, Detail: "hub up but /api/status was unparseable: " + err.Error()}
	}
	if st.PendingApprovals > 0 {
		return checkResult{Status: statusWarn,
			Detail: fmt.Sprintf("%d dangerous-action approval(s) waiting at %s", st.PendingApprovals, addr),
			Fix:    fmt.Sprintf("resolve them: GET %s://%s/api/approvals then POST the decision", scheme, addr)}
	}
	if st.TurnActive && st.TurnAgeSecs >= serveStuckTurnSecs {
		return checkResult{Status: statusWarn,
			Detail: fmt.Sprintf("a turn has been active %ds — possibly stuck", st.TurnAgeSecs),
			Fix:    "check the activity feed; cancel from the dashboard if it's hung"}
	}
	state := "idle"
	if st.TurnActive {
		state = fmt.Sprintf("turn active %ds", st.TurnAgeSecs)
	}
	return checkResult{Status: statusOK, Detail: fmt.Sprintf("hub healthy at %s (%s, 0 pending approvals)", addr, state)}
}

// isLoopbackAddr reports whether host names the local machine. Mirrors
// dashboard.isLoopbackHost (kept local so doctor doesn't import the
// dashboard package just for one predicate).
func isLoopbackAddr(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// isWindowsRuntime returns true when the build target is windows. Used to
// suppress POSIX perm warnings on Windows where they don't apply.
func isWindowsRuntime() bool {
	return os.PathSeparator == '\\'
}
