// Package claudecode implements Tenant's model.LLM interface by spawning the
// Claude Code CLI (`claude -p --output-format json`) as a subprocess. This is
// the SUBSCRIPTION path: the spawned CLI authenticates with the operator's
// logged-in Claude Code install (Max/Pro plan), not an API key — which is why
// the child environment is scrubbed of ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN
// / ANTHROPIC_BASE_URL before exec. A stray key or a GLM base-url override in
// the parent env would silently re-bill every call to the wrong place.
//
// Spawning the real binary (rather than borrowing its OAuth token for direct
// API calls) is the sanctioned automation surface for subscription plans; the
// weekly usage cap is the enforcement boundary. Two guards keep the daemon
// from eating the operator's interactive allowance:
//
//   - a local rolling-7-day token ledger (TENANT_CLAUDECODE_WEEKLY_BUDGET)
//     that trips ErrInsufficientBalance BEFORE spawning once exceeded, and
//   - CLI limit errors ("usage limit reached", 5-hour window / weekly cap)
//     classified as ErrRateLimited — both of which the FallbackLLM chain
//     (TEN-246) treats as failover triggers, so a capped subscription degrades
//     to the next configured provider instead of erroring the turn.
//
// Tool support: Claude Code has no wire-level tool_calls field in -p mode, so
// tools ride the toolfmt text path — the profile's ToolFormat adapter renders
// ToolSpecs into the appended system prompt and parses structured calls back
// out of the result text (the "backends WITHOUT native tool-call support" case
// toolfmt was designed for). Claude follows the qwen/hermes XML format
// reliably, so claudecode profiles default to ToolFormat "qwen".
//
// The spawned CLI runs non-interactively, so its own permission-gated tools
// (Bash writes, file edits) auto-deny; read-only tools still work. We do NOT
// pass --dangerously-skip-permissions: an escalation backend should reason,
// not mutate the host.
package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"tenant/internal/logs/capture"
	"tenant/internal/model"
	"tenant/internal/model/toolfmt"
)

// AutoEndpoint is the sentinel Profile.Endpoint meaning "discover the claude
// binary automatically" (PATH, then the desktop-app managed install, then the
// native installer location). A non-sentinel endpoint is treated as an
// explicit binary path. Operators can also force a path with TENANT_CLAUDE_BIN
// (wins over discovery, loses to an explicit non-sentinel Endpoint).
const AutoEndpoint = "claudecode://auto"

// defaultTimeout bounds a single escalation call when the caller supplied no
// deadline. Frontier reasoning turns legitimately run minutes; a hung CLI
// (network stall, update prompt) must not wedge the daemon forever.
const defaultTimeout = 15 * time.Minute

// budgetEnv names the rolling-7-day token budget for this backend. 0/unset =
// unlimited (the CLI's own plan limits are then the only guard).
const budgetEnv = "TENANT_CLAUDECODE_WEEKLY_BUDGET"

// extraArgsEnv appends operator-supplied flags to every spawn (split on
// spaces — no quoting support; for anything complex, wrap the binary).
const extraArgsEnv = "TENANT_CLAUDECODE_ARGS"

// binEnv overrides binary discovery.
const binEnv = "TENANT_CLAUDE_BIN"

// maxArgvSystem is the size above which the system prompt moves from an
// --append-system-prompt argument into the stdin transcript. Windows caps the
// whole command line at ~32K chars; tool schemas for a full mux can approach
// that on their own.
const maxArgvSystem = 24000

// Backend implements model.LLM over the claude CLI.
type Backend struct {
	profile model.Profile
	log     *slog.Logger

	binOnce sync.Once
	bin     string
	binErr  error

	ledger *ledger
}

// New constructs a Backend. Signature matches model.BackendFactory. No API
// key is required — a missing/logged-out CLI surfaces as a classified error
// on first call (fail-at-call, like an unreachable vLLM endpoint, so the
// resilient TUI launch path still opens).
func New(_ context.Context, p model.Profile, log *slog.Logger) (any, error) {
	if log == nil {
		log = slog.Default()
	}
	return &Backend{
		profile: p,
		log:     log,
		ledger:  openLedger(log),
	}, nil
}

// ----- LLM.Generate -----

func (b *Backend) Generate(ctx context.Context, req model.GenerateRequest) (*model.GenerateResponse, error) {
	if b.ledger.overBudget() {
		return nil, fmt.Errorf("%w: claudecode weekly token budget exhausted (%s=%d, spent≈%d in the last 7d)",
			model.ErrInsufficientBalance, budgetEnv, b.ledger.budget, b.ledger.spent())
	}
	bin, err := b.resolveBin()
	if err != nil {
		return nil, err
	}

	adapter := toolfmt.AdapterFor(b.profile.ToolFormat)
	system, prompt, err := buildPrompt(req, adapter)
	if err != nil {
		return nil, fmt.Errorf("%w: build prompt: %v", model.ErrInternal, err)
	}

	args := []string{"-p", "--output-format", "json"}
	if b.profile.Model != "" {
		args = append(args, "--model", b.profile.Model)
	}
	// Native CLI effort flag (low|medium|high|xhigh|max). "" = CLI default;
	// set per-provider via /reasoning, same operator surface as Fugu.
	if b.profile.ReasoningEffort != "" {
		args = append(args, "--effort", b.profile.ReasoningEffort)
	}
	if system != "" && len(system) <= maxArgvSystem {
		args = append(args, "--append-system-prompt", system)
	} else if system != "" {
		// Too big for argv (Windows ~32K command-line cap): fold into stdin.
		prompt = "System instructions (follow these for this task):\n" + system + "\n\n---\n\n" + prompt
	}
	if extra := strings.Fields(os.Getenv(extraArgsEnv)); len(extra) > 0 {
		args = append(args, extra...)
	}

	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = scrubbedEnv(os.Environ())
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	// Its stderr classifies a failure, and is filed in the model log with its
	// start and exit, failed or not (logging v2 §8.4).
	child := capture.New(ctx, "model", "child", "claude")
	cmd.Stderr = io.MultiWriter(&stderr, child)

	start := time.Now()
	runErr := child.Run(cmd)
	if runErr != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: claude -p: %v", model.ErrCancelled, ctx.Err())
		}
		if errors.Is(runErr, exec.ErrNotFound) {
			return nil, fmt.Errorf("%w: claude binary vanished at %s", model.ErrEndpointDown, bin)
		}
		return nil, classifyCLIFailure(stderr.String(), stdout.String())
	}

	res, err := parseResult(stdout.String())
	if err != nil {
		return nil, err
	}
	if res.IsError && strings.TrimSpace(res.Result) == "" {
		return nil, fmt.Errorf("%w: claude -p reported %s with no result text", model.ErrInternal, res.Subtype)
	}

	b.ledger.add(res.Usage.InputTokens + res.Usage.CacheCreationInputTokens + res.Usage.OutputTokens)
	b.log.Debug("claudecode call complete",
		"model", b.profile.Model, "session", res.SessionID,
		"turns", res.NumTurns, "dur", time.Since(start).Round(time.Millisecond),
		"in", res.Usage.InputTokens, "out", res.Usage.OutputTokens,
		"cache_read", res.Usage.CacheReadInputTokens, "cost_usd", res.TotalCostUSD)

	return buildResponse(res, adapter)
}

// GenerateStream satisfies the interface with a buffered single-shot: the
// full result is computed, then replayed as chunks. Real incremental
// streaming (--output-format stream-json) is a follow-up; the fallback
// decorator's "fail over only before first token" rule is preserved either
// way, since any failure here happens before a chunk is emitted.
func (b *Backend) GenerateStream(ctx context.Context, req model.GenerateRequest) (<-chan model.StreamChunk, error) {
	resp, err := b.Generate(ctx, req)
	if err != nil {
		return nil, err
	}
	ch := make(chan model.StreamChunk, len(resp.ToolCalls)+2)
	if resp.Text != "" {
		ch <- model.StreamChunk{Delta: resp.Text}
	}
	for i := range resp.ToolCalls {
		tc := resp.ToolCalls[i]
		ch <- model.StreamChunk{ToolCallDelta: &tc}
	}
	ch <- model.StreamChunk{FinishReason: resp.FinishReason, Usage: &resp.Usage}
	close(ch)
	return ch, nil
}

// TokenCount estimates via chars/4, same as the anthropic backend: there is
// no local tokenize endpoint, and the budgeting layer only needs stability.
func (b *Backend) TokenCount(_ context.Context, text string) (int, error) {
	n := len(text) / 4
	if n == 0 && len(text) > 0 {
		return 1, nil
	}
	return n, nil
}

// ----- prompt construction -----

// buildPrompt splits req into (system, stdin transcript). System messages and
// the toolfmt fragment go to system; the conversation flattens into a
// role-tagged transcript because `claude -p` accepts exactly one user prompt.
func buildPrompt(req model.GenerateRequest, adapter toolfmt.Adapter) (system, prompt string, err error) {
	var sys strings.Builder
	for _, m := range req.Messages {
		if m.Role == "system" && m.Content != "" {
			if sys.Len() > 0 {
				sys.WriteString("\n\n")
			}
			sys.WriteString(m.Content)
		}
	}
	if len(req.Tools) > 0 {
		frag, ferr := adapter.FormatToolPrompt(req.Tools)
		if ferr != nil {
			return "", "", ferr
		}
		if frag != "" {
			if sys.Len() > 0 {
				sys.WriteString("\n\n")
			}
			sys.WriteString(frag)
		}
	}

	var tr strings.Builder
	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			continue
		case "tool":
			fmt.Fprintf(&tr, "[tool result for %s]\n%s\n\n", m.ToolCallID, m.Content)
		case "assistant":
			if m.Content != "" {
				fmt.Fprintf(&tr, "[assistant]\n%s\n\n", m.Content)
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&tr, "[assistant tool call %s]\n%s(%s)\n\n", tc.ID, tc.Name, string(tc.Arguments))
			}
		default: // user
			if m.Content != "" {
				fmt.Fprintf(&tr, "[user]\n%s\n\n", m.Content)
			}
		}
	}
	p := strings.TrimSpace(tr.String())
	if p == "" {
		p = "(no prompt)"
	}
	return sys.String(), p, nil
}

// ----- result parsing -----

// cliResult is the --output-format json envelope emitted by `claude -p`.
type cliResult struct {
	Type         string  `json:"type"`
	Subtype      string  `json:"subtype"`
	IsError      bool    `json:"is_error"`
	Result       string  `json:"result"`
	SessionID    string  `json:"session_id"`
	NumTurns     int     `json:"num_turns"`
	DurationMS   int64   `json:"duration_ms"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Usage        struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

// parseResult decodes the CLI's JSON envelope, tolerating non-JSON noise
// (deprecation warnings, update notices) before the object.
func parseResult(stdout string) (*cliResult, error) {
	s := strings.TrimSpace(stdout)
	if i := strings.Index(s, "{"); i > 0 {
		s = s[i:]
	}
	var res cliResult
	if err := json.Unmarshal([]byte(s), &res); err != nil {
		snippet := s
		if len(snippet) > 512 {
			snippet = snippet[:512]
		}
		return nil, fmt.Errorf("%w: claude -p output was not the expected JSON envelope: %v (got: %s)",
			model.ErrInternal, err, snippet)
	}
	return &res, nil
}

// buildResponse maps a parsed CLI result onto the model contract, running the
// toolfmt safety-net parse so text-format tool calls become structured calls.
func buildResponse(res *cliResult, adapter toolfmt.Adapter) (*model.GenerateResponse, error) {
	out := &model.GenerateResponse{
		FinishReason: "stop",
		Usage: model.Usage{
			PromptTokens:     res.Usage.InputTokens + res.Usage.CacheReadInputTokens + res.Usage.CacheCreationInputTokens,
			CompletionTokens: res.Usage.OutputTokens,
		},
	}
	out.Usage.TotalTokens = out.Usage.PromptTokens + out.Usage.CompletionTokens

	raw := res.Result
	calls, err := adapter.ParseToolCalls(raw)
	if err != nil {
		// A malformed tool block degrades to plain text rather than failing
		// the turn — same posture as the vllm safety net.
		calls = nil
	}
	if len(calls) > 0 {
		out.ToolCalls = calls
		out.FinishReason = "tool_calls"
		raw = adapter.StripToolCalls(raw)
	}
	out.Text = strings.TrimSpace(adapter.CleanText(raw))
	return out, nil
}

// ----- failure classification -----

// classifyCLIFailure maps a nonzero exit onto Tenant's error taxonomy so the
// FallbackLLM chain does the right thing: plan limits fail over (and cool
// down), auth problems surface as actionable config errors, everything else
// is ErrInternal with the stderr snippet.
func classifyCLIFailure(stderr, stdout string) error {
	msg := strings.TrimSpace(stderr)
	if msg == "" {
		msg = strings.TrimSpace(stdout)
	}
	snippet := msg
	if len(snippet) > 512 {
		snippet = snippet[:512]
	}
	lower := strings.ToLower(msg)
	switch {
	case containsAny(lower,
		"usage limit", "rate limit", "hit your limit", "limit reached",
		"weekly limit", "5-hour", "out of extra usage", "try again at"):
		return fmt.Errorf("%w: claude subscription limit: %s", model.ErrRateLimited, snippet)
	case containsAny(lower,
		"credit balance", "billing", "payment", "out of credits", "insufficient"):
		return fmt.Errorf("%w: %s", model.ErrInsufficientBalance, snippet)
	case containsAny(lower,
		"not logged in", "please log in", "please run /login", "authenticate",
		"oauth", "invalid api key", "login required"):
		return fmt.Errorf("%w: claude CLI is not authenticated — run `claude` interactively and log in (details: %s)",
			model.ErrInvalidRequest, snippet)
	default:
		return fmt.Errorf("%w: claude -p failed: %s", model.ErrInternal, snippet)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// ----- environment scrubbing -----

// scrubbedEnv drops every variable that could re-route or re-bill the spawned
// CLI away from the operator's subscription login: API keys (would bill
// metered), auth tokens, base-url overrides (the GLM-in-claude-code trick
// must never leak into the escalation path), and provider-routing toggles.
// CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC is added so a daemon spawn never
// blocks on update checks.
func scrubbedEnv(env []string) []string {
	drop := []string{
		"ANTHROPIC_API_KEY=", "ANTHROPIC_AUTH_TOKEN=", "ANTHROPIC_BASE_URL=",
		"ANTHROPIC_MODEL=", "ANTHROPIC_SMALL_FAST_MODEL=",
		"CLAUDE_CODE_USE_BEDROCK=", "CLAUDE_CODE_USE_VERTEX=",
	}
	out := make([]string, 0, len(env)+1)
outer:
	for _, kv := range env {
		for _, d := range drop {
			if strings.HasPrefix(kv, d) {
				continue outer
			}
		}
		out = append(out, kv)
	}
	return append(out, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")
}

// ----- binary discovery -----

func (b *Backend) resolveBin() (string, error) {
	b.binOnce.Do(func() {
		b.bin, b.binErr = discoverBinary(b.profile.Endpoint)
		if b.binErr == nil {
			b.log.Info("claudecode backend using binary", "path", b.bin)
		}
	})
	return b.bin, b.binErr
}

// DiscoverBinary resolves the claude executable exactly as the backend will
// at call time. Exported so cmd/tenant's add-provider flow can probe at
// configuration time and tell the operator immediately whether the CLI is
// installed, instead of deferring the failure to the first escalation.
func DiscoverBinary(endpoint string) (string, error) { return discoverBinary(endpoint) }

// discoverBinary resolves the claude executable. Precedence: explicit
// (non-sentinel) Profile.Endpoint > TENANT_CLAUDE_BIN > PATH > the desktop
// app's managed install (%APPDATA%\Claude\claude-code\<newest>\claude.exe) >
// the native installer (~/.local/bin). Everything is stat-checked so a stale
// config fails with an actionable ErrEndpointDown, not a spawn error later.
func discoverBinary(endpoint string) (string, error) {
	if endpoint != "" && endpoint != AutoEndpoint && !strings.Contains(endpoint, "://") {
		if fileExists(endpoint) {
			return endpoint, nil
		}
		return "", fmt.Errorf("%w: no claude binary at configured path %s", model.ErrEndpointDown, endpoint)
	}
	if p := os.Getenv(binEnv); p != "" {
		if fileExists(p) {
			return p, nil
		}
		return "", fmt.Errorf("%w: %s=%s does not exist", model.ErrEndpointDown, binEnv, p)
	}
	if p, err := exec.LookPath("claude"); err == nil {
		return p, nil
	}
	if p := newestManagedInstall(); p != "" {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		p := filepath.Join(home, ".local", "bin", exeName("claude"))
		if fileExists(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("%w: claude CLI not found (PATH, %%APPDATA%%\\Claude\\claude-code, ~/.local/bin) — install Claude Code or set %s",
		model.ErrEndpointDown, binEnv)
}

// newestManagedInstall finds the desktop app's per-version CLI install and
// returns the highest version's claude.exe ("" when absent). The directory
// is version-named (e.g. 2.1.221) and changes on every app update, which is
// why this is discovery, not config.
func newestManagedInstall() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	appdata := os.Getenv("APPDATA")
	if appdata == "" {
		return ""
	}
	root := filepath.Join(appdata, "Claude", "claude-code")
	entries, err := os.ReadDir(root)
	if err != nil {
		return ""
	}
	var versions []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if fileExists(filepath.Join(root, e.Name(), "claude.exe")) {
			versions = append(versions, e.Name())
		}
	}
	if len(versions) == 0 {
		return ""
	}
	sort.Slice(versions, func(i, j int) bool { return versionLess(versions[j], versions[i]) })
	return filepath.Join(root, versions[0], "claude.exe")
}

// versionLess compares dotted numeric versions ("2.1.221" < "2.1.1000");
// non-numeric segments fall back to string compare.
func versionLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var av, bv string
		if i < len(as) {
			av = as[i]
		}
		if i < len(bs) {
			bv = bs[i]
		}
		ai, aerr := strconv.Atoi(av)
		bi, berr := strconv.Atoi(bv)
		if aerr == nil && berr == nil {
			if ai != bi {
				return ai < bi
			}
			continue
		}
		if av != bv {
			return av < bv
		}
	}
	return false
}

func exeName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
