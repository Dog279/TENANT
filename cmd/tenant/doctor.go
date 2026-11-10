package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"tenant/internal/fsperm"
	"tenant/internal/improve"
	"tenant/internal/logs/store"
	"tenant/internal/mcp"
	"tenant/internal/mcpserver"
	"tenant/internal/memory/episodic"
	"tenant/internal/memory/semantic"
	"tenant/internal/memory/skills"
	"tenant/internal/memory/sme"
	"tenant/internal/memory/soul"
	"tenant/internal/model"
	"tenant/internal/model/backend/vllm"
	"tenant/internal/plugins/mcpremote"
	"tenant/internal/research"
	"tenant/internal/sandbox"
	"tenant/internal/secrets"
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
// Safety (openclaw pattern): read-only by default. Without --fix doctor
// creates no dir or database and changes no permission, so its audit reports
// what is on disk (TEN-333); a live probe of an enabled OAuth plugin may still
// refresh that plugin's token cache, but only after the permission checks ran.
// --fix applies ONLY safe, reversible repairs (mkdir dirs, owner-only modes,
// reset a stale cursor after recording the old value). It never deletes a
// store or re-embeds.

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

// doctorCheck is one named check. Plugins contribute theirs through their
// descriptors (plugin_<id>.go).
type doctorCheck struct {
	Name string
	Run  func(context.Context, *doctorEnv) checkResult
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

	logbook    *store.Store // logbook.db, read-only (doctorlogbook.go)
	logbookErr error
}

// readCredentials loads credentials.json for a check: as-is in plain doctor,
// hardened like any other secret read under --fix (TEN-333).
func (e *doctorEnv) readCredentials() (*credentials, error) {
	return e.c.credentialsLoader()(e.c.cfgDir)
}

// resolveSecret is the package resolveSecret through the same audit-aware
// credentials reader.
func (e *doctorEnv) resolveSecret(providerID string, auth authCfg) string {
	return resolveSecretWith(e.c.credentialsLoader(), e.c.cfgDir, providerID, auth)
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
	// Without --fix, doctor changes nothing it audits (TEN-333): no dir is
	// created or tightened, credentials are read as-is, and only existing
	// databases are opened, without touching their permissions.
	c.auditOnly = !*fix
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
	openEpisodic, openSemantic, openSkills := episodic.OpenExisting, semantic.OpenExisting, skills.OpenExisting
	if *fix {
		openEpisodic, openSemantic, openSkills = episodic.Open, semantic.Open, skills.Open
	}
	if es, err := openEpisodic(filepath.Join(c.dataDir, "episodes.db")); err == nil {
		env.episodic = es
		defer es.Close()
	}
	if ss, err := openSemantic(filepath.Join(c.dataDir, "facts.db")); err == nil {
		env.semantic = ss
		defer ss.Close()
	}
	if sk, err := openSkills(filepath.Join(c.dataDir, "skills.db")); err == nil {
		env.skills = sk
		defer sk.Close()
	}
	// The log book, read-only even under --fix: doctor repairs nothing in it.
	if env.logbook, env.logbookErr = store.Open(filepath.Join(c.logsRoot(), "logbook.db"), store.Options{ReadOnly: true}); env.logbook != nil {
		defer env.logbook.Close()
	}

	// TEN-49: retrieval trace — show what the memory tiers return for a query
	// instead of running the health checks.
	if strings.TrimSpace(*contextDebug) != "" {
		return runContextDebug(ctx, env, *contextDebug)
	}

	checks := []doctorCheck{
		{"directories", checkDirs},
		{"private data permissions", checkPrivatePermissions},
		// NEW: validates config.json + active provider exists. Runs early
		// because other checks read e.lc.
		{"launch config", checkLaunchConfig},
		{"secret storage protection", checkSecretProtection},
		// credentials.json schema + every keyed provider has a resolvable
		// secret (env-var present OR stored in creds).
		{"credentials", checkCredentials},
		{"profiles/router", checkRouter},
		// TEN-358: TEN-282's latency gating on the fallback chain — whether it is
		// on, what it skips, and whether there is a chain for it to move to.
		{"fallback health gating", checkHealthGating},
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
		// TEN-345/347: what each surface's commands run under, probed here.
		{"exec sandbox", checkExecSandbox},
		// TEN-455: when a surface uses the container backend, is it ready?
		{"container sandbox", checkContainerSandbox},
		// TEN-375: the log book (doctorlogbook.go).
		{"log book location", checkLogbookLocation},
		{"log book integrity", checkLogbookIntegrity},
		{"log book size", checkLogbookSize},
		{"log book housekeeper", checkLogbookHousekeeper},
		{"log book mirror-only", checkLogbookMirrorOnly},
		{"log book losses", checkLogbookLosses},
	}
	// Each plugin's own checks (plugin_<id>.go), in registry order.
	checks = append(checks, pluginDoctorChecks()...)
	checks = append(checks, []doctorCheck{
		// TEN-81: dashboard reachability + bind-policy config lint when the
		// web control panel is configured.
		{"dashboard (if configured)", checkDashboard},
		// TEN-194: serve-mode liveness — when a daemon (or TUI+dashboard) is up,
		// probe /api/status for a stuck turn or a wedged approval queue. SKIP
		// when nothing is reachable (a daemon simply not running is not a fault).
		{"serve liveness (if running)", checkServe},
		// TEN-498: the background service — installed, pointing at these
		// dirs, answering; --fix rewrites a drifted unit.
		{"background service", checkService},
		// TEN-500: a newer release, or an update that never finished.
		{"update", checkUpdate},
		{"distill cursor", checkDistillCursor},
		// TEN-255 Phase 4: per-project SME doc health — present/stale when
		// reflection is on, and a guard on the merge-protected fraction (a
		// majority-protected store means consolidation is being starved).
		{"project SME (memory)", checkProjectSME},
		{"tool-calling (deep)", checkToolCalling},
		{"mcp surface (deep)", checkMCPSurface},
		// A remote connector whose OAuth refresh token died: read offline from
		// its token cache, so a dead Jira/Confluence bridge is caught by doctor
		// instead of by a silently empty daily brief.
		{"remote MCP auth", checkRemoteMCPAuth},
	}...)

	results := make([]checkResult, 0, len(checks))
	for _, ck := range checks {
		r := ck.Run(ctx, env)
		r.Name = ck.Name
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

// checkExecSandbox says, a line per surface, what its commands run under and
// what that backend enforces on this host. It warns when commands run on the
// host because this OS has no restricted backend, or it can't run here (and
// says why), and fails when an enforcing profile's backend is missing, so its
// commands do not run.
func checkExecSandbox(ctx context.Context, e *doctorEnv) checkResult {
	notes := loadSandboxProfiles(e.c) // exec_profiles as the hub loads them
	probes := probeExecSurfaces(ctx, e.c.cfgDir, e.c.dataDir)
	r := checkResult{Status: statusOK}
	for _, p := range probes {
		switch {
		case p.failsClosed():
			r.Status = statusFail
			r.Fix = "install what the error names (`tenant sandbox probe`), or set another backend for the surface in exec_profiles"
		case (p.Fallback != "" || p.Error != "") && r.Status == statusOK:
			r.Status = statusWarn
			r.Fix = "commands run unconfined on the host (the restricted backend is macOS-only; the container backend runs on every OS: set \"backend\": \"container\" for the surface in exec_profiles); `tenant sandbox probe` has the detail"
		}
	}
	lines := append(notes, execProfileLines(probes)...)
	lines = append(lines, envSecretLines(probes)...) // a hint, so the status stays as it is
	r.Detail = strings.Join(lines, "\n       ")
	return r
}

// checkContainerSandbox says whether the container backend can run a command
// right now, for the surfaces whose profile names it: the docker CLI is there,
// the engine answers and meets the floors, the sandbox image is built, and a
// container starts. Any of those failing means those surfaces' commands do not
// run (the backend never falls back to the host), so it is a FAIL, with the
// reason and the fix for that reason. It starts one short-lived container;
// nothing else changes. SKIP when no surface uses the backend.
func checkContainerSandbox(ctx context.Context, e *doctorEnv) checkResult {
	loadSandboxProfiles(e.c)
	var surfaces []string
	for _, p := range probeExecSurfaces(ctx, e.c.cfgDir, e.c.dataDir) {
		if p.Backend == "container" {
			surfaces = append(surfaces, p.Surface)
		}
	}
	if len(surfaces) == 0 {
		return checkResult{Status: statusSkip, Detail: "no surface's exec profile uses the container backend"}
	}
	on := strings.Join(surfaces, ", ")
	be, err := sandbox.For("container")
	if err == nil {
		if a, ok := be.(sandbox.Available); ok {
			err = a.Available(ctx)
		}
	}
	if err != nil {
		r := checkResult{Status: statusFail, Detail: on + ": commands do not run: " + err.Error(),
			Fix: "`tenant sandbox probe` has the detail"}
		var u *sandbox.Unavailable
		if errors.As(err, &u) {
			r.Fix = u.Fix
		}
		return r
	}
	return checkResult{Status: statusOK, Detail: on + ": the engine meets the floors, the sandbox image is built and a container starts"}
}

func checkDirs(_ context.Context, e *doctorEnv) checkResult {
	r := checkResult{Status: statusOK, Detail: "data + config dirs present and writable"}
	for _, d := range []string{e.c.dataDir, e.c.cfgDir} {
		info, err := os.Stat(d)
		if err != nil || !info.IsDir() {
			if e.fix {
				if mkErr := fsperm.MkdirAll(d); mkErr == nil {
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
		if err := fsperm.WriteFile(probe, []byte("ok")); err != nil {
			r.Status, r.Detail = statusFail, d+" is not writable: "+err.Error()
			r.Fix = "check permissions / disk space on " + d
			return r
		}
		_ = os.Remove(probe)
	}
	return r
}

func checkPrivatePermissions(_ context.Context, e *doctorEnv) checkResult {
	if !fsperm.Supported() {
		return checkResult{
			Status: statusSkip,
			Detail: "POSIX modes do not represent Windows DACLs; DACL hardening is tracked by TEN-325",
		}
	}

	targets, err := privatePermissionTargets(e.c.dataDir, e.c.cfgDir, e.c.logsRoot())
	if err != nil {
		return checkResult{Status: statusFail, Detail: "permission target discovery failed: " + err.Error()}
	}

	issueCount := 0
	for _, target := range targets {
		var issues []fsperm.Issue
		if target.recursive {
			issues, err = fsperm.AuditTree(target.path)
		} else if issue, auditErr := fsperm.Audit(target.path); auditErr != nil {
			err = auditErr
		} else if issue != nil {
			issues = []fsperm.Issue{*issue}
		}
		if err != nil {
			return checkResult{
				Status: statusFail,
				Detail: "permission audit failed for " + target.path + ": " + err.Error(),
				Fix:    "check ownership and access to " + target.path,
			}
		}
		issueCount += len(issues)
	}
	if issueCount == 0 {
		return checkResult{Status: statusOK, Detail: "config/data directories and artifacts are owner-only"}
	}
	if !e.fix {
		return checkResult{
			Status: statusWarn,
			Detail: fmt.Sprintf("%d config/data path(s) exceed owner-only permissions", issueCount),
			Fix:    "run `tenant doctor --fix` to apply owner-only permissions",
		}
	}

	fixed := 0
	for _, target := range targets {
		n := 0
		if target.recursive {
			n, err = fsperm.RepairTree(target.path)
		} else if changed, repairErr := fsperm.Repair(target.path); repairErr != nil {
			err = repairErr
		} else if changed {
			n = 1
		}
		fixed += n
		if err != nil {
			return checkResult{
				Status: statusFail,
				Detail: fmt.Sprintf("repaired %d path(s), then failed at %s: %v", fixed, target.path, err),
				Fix:    "check ownership and retry `tenant doctor --fix`",
			}
		}
	}
	return checkResult{
		Status: statusWarn,
		Detail: fmt.Sprintf("tightened %d config/data path(s) to owner-only permissions", fixed),
		Fix:    "private permission migration applied",
		Fixed:  true,
	}
}

type permissionTarget struct {
	path      string
	recursive bool
}

func privatePermissionTargets(dataDir, cfgDir, logsRoot string) ([]permissionTarget, error) {
	if strings.TrimSpace(dataDir) == "" || strings.TrimSpace(cfgDir) == "" {
		return nil, errors.New("config and data roots must be resolved")
	}
	targets := []permissionTarget{{path: dataDir}, {path: cfgDir}}
	for _, name := range []string{"archive", "attachments", "research", "wiki", "screenshots", "eval-artifacts", "logs"} {
		targets = append(targets, permissionTarget{path: filepath.Join(dataDir, name), recursive: true})
	}
	// A log book moved by logs.dir (TEN-375), unless its root holds more than
	// the log book: then it is the operator's to tighten.
	if logsRoot != "" && filepath.Clean(logsRoot) != filepath.Join(dataDir, "logs") && logsRootTooBroad(logsRoot, dataDir, cfgDir) == "" {
		targets = append(targets, permissionTarget{path: logsRoot, recursive: true})
	}
	for _, name := range []string{"soul", "mcp"} {
		targets = append(targets, permissionTarget{path: filepath.Join(cfgDir, name), recursive: true})
	}

	dataEntries, err := os.ReadDir(dataDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, entry := range dataEntries {
		name := entry.Name()
		if !entry.IsDir() && privateDataRootFile(name) {
			targets = append(targets, permissionTarget{path: filepath.Join(dataDir, name)})
		}
	}

	cfgEntries, err := os.ReadDir(cfgDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, entry := range cfgEntries {
		name := entry.Name()
		if !entry.IsDir() && privateConfigRootFile(name) {
			targets = append(targets, permissionTarget{path: filepath.Join(cfgDir, name)})
		}
	}
	return targets, nil
}

func privateDataRootFile(name string) bool {
	if strings.Contains(name, ".db") || strings.HasPrefix(name, "profile.") {
		return true
	}
	switch name {
	case "cron-history.json", "route-decisions.jsonl", "tui.log", "transcript.txt":
		return true
	}
	return pluginSecretName(pluginDataSecrets, name)
}

func privateConfigRootFile(name string) bool {
	if strings.HasPrefix(name, "settings.") && strings.HasSuffix(name, ".json") {
		return true
	}
	switch name {
	case "config.json", "credentials.json", "peers.json", "peer-cert.pem", "peer-key.pem", "logbook.key", "logbook.anchor":
		return true
	}
	return pluginSecretName(pluginConfigSecrets, name)
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

// checkHealthGating reports config.json `health_gating` (TEN-358): off, on with
// a fallback chain to move to, or on with nothing to move to or with values
// the engine will replace.
func checkHealthGating(_ context.Context, e *doctorEnv) checkResult {
	if e.lc == nil {
		return checkResult{Status: statusSkip, Detail: "launch config unavailable (see launch config check)"}
	}
	cfg := e.lc.HealthGating.ModelConfig()
	if cfg.SlowThreshold <= 0 {
		return checkResult{Status: statusSkip,
			Detail: "off (set health_gating.slow_threshold_ms in config.json to skip a slow model in the fallback chain)"}
	}
	desc := describeHealthGating(cfg)
	var chain []string
	for _, name := range e.lc.Fallbacks {
		name = strings.TrimSpace(name)
		if name != "" && name != e.lc.Provider && e.lc.Providers[name] != nil {
			chain = append(chain, name)
		}
	}
	if len(chain) == 0 {
		return checkResult{Status: statusWarn,
			Detail: desc + ", but no fallback provider is configured, so there is nothing to move to",
			Fix:    "`/model fallback <provider...>` or set \"fallbacks\" in config.json"}
	}
	if problems := e.lc.HealthGating.Problems(); len(problems) > 0 {
		return checkResult{Status: statusWarn,
			Detail: desc + "; " + strings.Join(problems, "; "),
			Fix:    "correct health_gating in config.json"}
	}
	return checkResult{Status: statusOK,
		Detail: desc + "; chain: " + firstNonEmpty(e.lc.Provider, "active") + " → " + strings.Join(chain, " → ")}
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

// tokenizePlatforms names the server behind each /tokenize dialect.
var tokenizePlatforms = map[string]string{vllm.DialectVLLM: "vLLM", vllm.DialectLlamaCpp: "llama.cpp"}

// checkTokenize reports how serve counts tokens for the active provider, by
// platform. vLLM and llama.cpp's server each have a /tokenize, in their own
// dialects, and serve budgets with it. Ollama (none as of 0.32) and the hosted
// APIs have no tokenize endpoint and serve estimates ~4 chars/token by design,
// which is nothing to fix. The probe asks which dialect the server really
// speaks, so a kind that doesn't match its server is named with the fix: a
// llama.cpp server declared "vllm" otherwise budgets by estimate without a
// word, until an under-count sends a prompt the server rejects.
func checkTokenize(ctx context.Context, e *doctorEnv) checkResult {
	if e.c.backend != "vllm" || e.c.vllmEndpoint == "" {
		return checkResult{Status: statusSkip, Detail: "no OpenAI-compatible provider (TokenCount estimator used)"}
	}
	kind := e.c.genKind
	pk := providerKinds[kind]
	want := vllm.TokenizeDialect(kind, pk.EstimateTokens)
	if want == "" && !pk.Local {
		return checkResult{Status: statusSkip, Detail: pk.Label + " is hosted: serve budgets with a ~4-chars/token estimate"}
	}
	b, _ := vllm.New(ctx, model.Profile{
		ProviderKind: kind, Endpoint: e.c.vllmEndpoint, Model: e.c.vllmModel,
		APIKey: e.c.genAPIKey, ForceHTTP11: pk.ForceHTTP1,
	}, nil)
	backend, ok := b.(*vllm.Backend)
	if !ok {
		return checkResult{Status: statusSkip, Detail: "no vLLM backend to probe"}
	}
	// The configured dialect first; the other only to explain a failure.
	order := []string{vllm.DialectVLLM, vllm.DialectLlamaCpp}
	if want == vllm.DialectLlamaCpp {
		order = []string{vllm.DialectLlamaCpp, vllm.DialectVLLM}
	}
	var speaks string
	var n int
	var wantErr error
	for _, d := range order {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		got, err := backend.Tokenize(probeCtx, d, "doctor probe")
		cancel()
		if err == nil && got > 0 {
			speaks, n = d, got
			break
		}
		if d == want {
			wantErr = err // nil: the server answered without a count
		}
		if errors.Is(err, model.ErrEndpointDown) || errors.Is(err, model.ErrCancelled) {
			break // no dialect will answer an unreachable server
		}
	}
	const estimate = "~4-chars/token estimate"
	switch {
	case speaks != "" && speaks == want:
		return checkResult{Status: statusOK, Detail: fmt.Sprintf("/tokenize OK (%s, probe=%d tok)", tokenizePlatforms[speaks], n)}
	case speaks != "" && want != "":
		return checkResult{Status: statusWarn,
			Detail: fmt.Sprintf("the server answers %s's /tokenize, not %s's, so serve can't count tokens and budgets with a %s",
				tokenizePlatforms[speaks], tokenizePlatforms[want], estimate),
			Fix: fmt.Sprintf("set this provider's kind to %q in config.json", speaks)}
	case speaks != "":
		return checkResult{Status: statusWarn,
			Detail: fmt.Sprintf("the server answers %s's /tokenize, but kind %q budgets with a %s", tokenizePlatforms[speaks], kind, estimate),
			Fix:    fmt.Sprintf("set this provider's kind to %q in config.json for exact token budgets", speaks)}
	case want == "":
		return checkResult{Status: statusSkip, Detail: pk.Label + " has no tokenize endpoint: serve budgets with a " + estimate}
	}
	detail := tokenizePlatforms[want] + "'s /tokenize answered without a count"
	if wantErr != nil {
		detail = tokenizePlatforms[want] + "'s /tokenize failed: " + clip(wantErr.Error(), 160)
	}
	fix := "fine for most use; exact budgets need a vLLM build with /tokenize"
	if want == vllm.DialectLlamaCpp {
		fix = "llama.cpp's server always has /tokenize: check that this endpoint is one"
	}
	return checkResult{Status: statusWarn, Detail: detail + "; budgeting falls back to a " + estimate, Fix: fix}
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

// missingDataFiles returns which of the named data-dir files do not exist.
// Plain doctor opens only existing databases (TEN-333), so on a fresh install
// a nil store means "not created yet", not "failed to open".
func missingDataFiles(dataDir string, names ...string) []string {
	var missing []string
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(dataDir, name)); os.IsNotExist(err) {
			missing = append(missing, name)
		}
	}
	return missing
}

func checkStores(ctx context.Context, e *doctorEnv) checkResult {
	if e.episodic == nil || e.semantic == nil {
		var unopened []string
		if e.episodic == nil {
			unopened = append(unopened, "episodes.db")
		}
		if e.semantic == nil {
			unopened = append(unopened, "facts.db")
		}
		if missing := missingDataFiles(e.c.dataDir, unopened...); len(missing) == len(unopened) {
			return checkResult{Status: statusSkip,
				Detail: strings.Join(missing, " / ") + " not created yet (Tenant creates them on first run)"}
		}
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

// checkProjectSME reports per-project SME health (TEN-255 Phase 4): whether the
// doc exists/stale when reflection is on, plus a guard on the merge-protected
// fraction (a majority-protected store starves consolidation — design §7).
func checkProjectSME(ctx context.Context, e *doctorEnv) checkResult {
	if e.semantic == nil {
		return checkResult{Status: statusSkip, Detail: "semantic store unavailable"}
	}
	reflectEvery := ""
	if e.lc != nil {
		reflectEvery = strings.TrimSpace(e.lc.Improve.ReflectEvery)
	}

	// Protected-fraction guard (independent of reflection being on).
	var protNote string
	if prot, live, err := e.semantic.MergeProtectedStats(ctx, e.c.agent); err == nil && live > 0 {
		frac := float64(prot) / float64(live)
		protNote = fmt.Sprintf("%d/%d facts merge-protected (%.0f%%)", prot, live, frac*100)
		if frac >= 0.7 {
			return checkResult{Status: statusWarn,
				Detail: protNote + " — consolidation may be starved",
				Fix:    "raise improve protectImportance, review pins, or /memory to inspect protected facts"}
		}
	}

	st, err := sme.New(e.semantic.DB())
	if err != nil {
		return checkResult{Status: statusWarn, Detail: "sme store init failed: " + err.Error()}
	}
	secs, err := st.ActiveSections(ctx, "", e.c.agent)
	if err != nil {
		return checkResult{Status: statusWarn, Detail: "sme query failed: " + err.Error()}
	}
	detail := protNote
	if reflectEvery == "" {
		if len(secs) == 0 {
			return checkResult{Status: statusOK,
				Detail: join(detail, "reflection off — no per-project SME maintained"),
				Fix:    "set improve.reflect_every (e.g. \"6h\") in config.json to synthesize a per-project SME"}
		}
		return checkResult{Status: statusOK, Detail: join(detail, fmt.Sprintf("SME present (%d sections); reflection currently off", len(secs)))}
	}
	// Reflection is on.
	if len(secs) == 0 {
		return checkResult{Status: statusWarn,
			Detail: join(detail, "reflect_every set but no SME doc yet"),
			Fix:    "give it a cycle — the ReflectionJob synthesizes the doc on its next run"}
	}
	// Staleness: stale if older than 5× the cadence (fallback 7d on parse fail).
	stale := 7 * 24 * time.Hour
	if d, perr := time.ParseDuration(reflectEvery); perr == nil && d > 0 {
		stale = 5 * d
	}
	var newest time.Time
	for _, s := range secs {
		if s.UpdatedAt.After(newest) {
			newest = s.UpdatedAt
		}
	}
	age := time.Since(newest)
	if age > stale {
		return checkResult{Status: statusWarn,
			Detail: join(detail, fmt.Sprintf("SME doc is stale (%d sections, last updated %s ago)", len(secs), age.Round(time.Hour))),
			Fix:    "check the ReflectionJob is running (self-improve on, model reachable)"}
	}
	return checkResult{Status: statusOK, Detail: join(detail, fmt.Sprintf("SME fresh (%d sections, updated %s ago)", len(secs), age.Round(time.Hour)))}
}

// join concatenates two non-empty detail fragments with "; ".
func join(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "; " + b
}

func checkDistillCursor(ctx context.Context, e *doctorEnv) checkResult {
	openMeta := improve.OpenMetaExisting // plain doctor changes nothing (TEN-333)
	if e.fix {
		openMeta = improve.OpenMeta
	}
	meta, err := openMeta(filepath.Join(e.c.dataDir, "tenant_meta.db"))
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

// checkRemoteMCPAuth reads each configured remote connector's persisted auth
// state from its token cache — no network, no browser. FAIL names the dead
// connector(s) with the exact reconnect command; WARN flags servers in config
// that were never connected from this config dir.
func checkRemoteMCPAuth(_ context.Context, e *doctorEnv) checkResult {
	if e.lc == nil || len(e.lc.MCPRemotes) == 0 {
		return checkResult{Status: statusSkip, Detail: "no remote MCP connectors configured"}
	}
	cacheDir := filepath.Join(e.c.cfgDir, "mcp")
	var dead, unknown []string
	for _, u := range e.lc.MCPRemotes {
		h, ok := mcpremote.ReadHealth(cacheDir, u)
		switch {
		case !ok:
			unknown = append(unknown, u)
		case h.NeedsReauth:
			when := "unknown time"
			if !h.Since.IsZero() {
				when = h.Since.Format("2006-01-02 15:04 UTC")
			}
			dead = append(dead, fmt.Sprintf("%s (since %s) — fix: %s", u, when, mcpremote.FixCommand(e.c.cfgDir, u)))
		}
	}
	switch {
	case len(dead) > 0:
		return checkResult{Status: statusFail, Detail: "needs re-authorization: " + strings.Join(dead, "; ")}
	case len(unknown) > 0:
		return checkResult{Status: statusWarn, Detail: "never connected from this config dir (no cached token): " + strings.Join(unknown, ", ")}
	}
	return checkResult{Status: statusOK, Detail: fmt.Sprintf("%d connector(s) hold a valid cached sign-in", len(e.lc.MCPRemotes))}
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

// checkSecretProtection audits the effective platform protection for every
// at-rest secret file on the authoritative managedSecrets list, config and
// data dirs both (TEN-337). POSIX reports verified 0600/0700 modes; Windows
// reports the protected DACL (current user only, or on a machine install also
// Administrators and the built-in service accounts). It only probes: nothing
// is tightened here.
func checkSecretProtection(_ context.Context, e *doctorEnv) checkResult {
	paths, err := managedSecrets(e.c.cfgDir, e.c.dataDir)
	if err != nil {
		return checkResult{Status: statusFail, Detail: err.Error()}
	}

	protected := 0
	mechanisms := map[string]bool{}
	var bad []string
	for _, item := range paths {
		report := secrets.Probe(item.path)
		if !report.Exists {
			if report.Detail != "not present" {
				bad = append(bad, item.label+": "+report.Detail)
			}
			continue
		}
		if !report.Protected {
			bad = append(bad, item.label+": "+report.Detail)
			continue
		}
		protected++
		mechanisms[report.Mechanism] = true
	}
	if len(bad) > 0 {
		return checkResult{
			Status: statusFail,
			Detail: "secret files without verified owner-only protection: " + strings.Join(bad, "; "),
			Fix:    "re-save the affected credential/token with Tenant, then rerun `tenant doctor`",
		}
	}
	if protected == 0 {
		return checkResult{Status: statusOK, Detail: "no at-rest secret files present"}
	}
	mechanismNames := make([]string, 0, len(mechanisms))
	for mechanism := range mechanisms {
		mechanismNames = append(mechanismNames, mechanism)
	}
	slices.Sort(mechanismNames)
	return checkResult{Status: statusOK, Detail: fmt.Sprintf("%d secret file(s) protected via %s", protected, strings.Join(mechanismNames, ", "))}
}

// checkCredentials validates credentials.json — parses cleanly and every
// keyed provider in the active config has a resolvable secret (environment
// reference or stored credential). Effective file protection is checked above.
func checkCredentials(_ context.Context, e *doctorEnv) checkResult {
	creds, err := e.readCredentials()
	if err != nil {
		return checkResult{Status: statusFail,
			Detail: "credentials.json present but unparseable: " + err.Error(),
			Fix:    "rm " + credentialsPath(e.c.cfgDir) + " then re-run `tenant setup` for any keyed provider"}
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
		if creds.Get(name) == "" {
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
			key := e.resolveSecret(ap.Provider, pc.Auth)
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
		if len(missingDataFiles(e.c.dataDir, "skills.db")) == 1 {
			return checkResult{Status: statusSkip, Detail: "skills.db not created yet (Tenant creates it on first run)"}
		}
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
	openStore := research.OpenExisting // plain doctor changes nothing (TEN-333)
	if e.fix {
		openStore = research.New
	}
	store, err := openStore(e.c.dataDir)
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

// checkDashboard verifies the web control panel (TEN-76/79) when it's
// configured. Tiers mirror checkDiscord:
//
//	skip   — dashboard not configured (Enabled=false AND no Addr)
//	FAIL   — non-loopback Addr without TLS in config (the same
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
	if !dc.IsEnabled() && dc.Addr == "" {
		return checkResult{Status: statusSkip, Detail: "dashboard disabled"}
	}
	addr := dc.Addr
	if addr == "" {
		addr = "127.0.0.1:9770" // default bind (mirrors dashboardConfig docs)
	}

	// Config lint (independent of whether the server is up): a non-loopback
	// bind without TLS is fail-closed. Authentication no longer needs to be
	// present in config.json because launch creates the default token file.
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr // no port → treat whole value as host (best-effort)
	}
	if !isLoopbackAddr(host) {
		if dc.TLSCert == "" || dc.TLSKey == "" {
			return checkResult{Status: statusFail,
				Detail: fmt.Sprintf("dashboard binds non-loopback %q without TLS — launch will refuse to start", addr),
				Fix:    "set dashboard.tls_cert/tls_key, or bind 127.0.0.1"}
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
	// A short key is guessable (TEN-403): the generated one is 32+ characters;
	// a dashboard.auth someone typed may not be.
	if key, _ := dashboardClientToken(e.c.cfgDir, e.lc, true); key != "" && len(key) < 32 {
		return checkResult{Status: statusWarn,
			Detail: fmt.Sprintf("dashboard healthy at %s, but its sign-in key is %d characters", addr, len(key)),
			Fix:    "use 32 or more random characters in dashboard.auth, or delete dashboard-auth-token and restart to generate one"}
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
	if !dc.IsEnabled() && dc.Addr == "" {
		return checkResult{Status: statusSkip, Detail: "dashboard disabled — no hub to probe"}
	}
	addr := dc.Addr
	if addr == "" {
		addr = "127.0.0.1:9770"
	}
	scheme := "http"
	if dc.TLSCert != "" {
		scheme = "https"
	}
	// A default install keeps the token in its file, not in dashboard.auth.
	tok, err := dashboardClientToken(e.c.cfgDir, e.lc, e.c.auditOnly)
	if err != nil {
		return checkResult{Status: statusWarn, Detail: "cannot read the dashboard token: " + err.Error()}
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(probeCtx, http.MethodGet, fmt.Sprintf("%s://%s/api/status", scheme, addr), nil)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
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
			Fix:    "check the turns log on the dashboard's Logs page; cancel from the dashboard if it's hung"}
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

// checkService (TEN-498): the background service is installed, its unit
// runs this binary with this command's --config and --data, and a hub
// answers. Not installed is a WARN with the one command that fixes it; a
// drifted unit is rewritten under --fix.
func checkService(ctx context.Context, e *doctorEnv) checkResult {
	spec, err := newServiceSpec(e.c, systemInstalled(ctx))
	if err != nil {
		return checkResult{Status: statusSkip, Detail: err.Error()}
	}
	ok, where := spec.installed(ctx)
	if !ok {
		return checkResult{Status: statusWarn, Detail: "no background service: Tenant does not start at login",
			Fix: "tenant service install"}
	}
	current, err := spec.unitCurrent(ctx)
	if err != nil {
		return checkResult{Status: statusWarn, Detail: where + ": cannot read its unit: " + err.Error(), Fix: "tenant service install"}
	}
	if !current {
		if e.fix {
			if err := spec.install(ctx); err != nil {
				return checkResult{Status: statusFail, Detail: "rewriting the unit failed: " + err.Error()}
			}
			return checkResult{Status: statusOK, Fixed: true,
				Detail: fmt.Sprintf("unit rewritten for %s --config %s --data %s", spec.Exe, spec.CfgDir, spec.DataDir)}
		}
		return checkResult{Status: statusWarn,
			Detail: fmt.Sprintf("%s does not run %s with --config %s --data %s", where, spec.Exe, spec.CfgDir, spec.DataDir),
			Fix:    "tenant service install (rewrites the unit; --fix does it)"}
	}
	base := attachBaseURL("", e.lc)
	tok, _ := dashboardClientToken(e.c.cfgDir, e.lc, e.c.auditOnly)
	st, err := attachFetchStatus(ctx, e.httpc, base, tok)
	if err != nil {
		return checkResult{Status: statusWarn, Detail: where + " is installed but no hub answers at " + base,
			Fix: "tenant service start, then tenant service status"}
	}
	return checkResult{Status: statusOK, Detail: fmt.Sprintf("%s, running version %s", where, firstNonEmpty(st.Version, "unknown"))}
}
