# Claude Code backend (`claudecode`) — subscription frontier escalation

The `claudecode` backend runs frontier Claude models (Opus 5, Fable 5, Sonnet)
by spawning the Claude Code CLI (`claude -p --output-format json`) as a
subprocess, billing the operator's **Max/Pro subscription** instead of a
metered API key. It is designed to be the Supra router's **big leg**: local
Qwen carries the volume, complex prompts escalate to the subscription, and
the FallbackLLM chain (TEN-246) degrades gracefully when the weekly plan
limit is hit.

## One-time setup

1. **Standalone CLI login.** The Claude **desktop app's** login does NOT
   create CLI credentials (the app brokers auth internally). From a plain
   terminal, run:

   ```
   claude
   ```

   and complete `/login` with the subscription account. If `claude` is not
   on PATH, use the desktop app's managed binary directly
   (`%APPDATA%\Claude\claude-code\<version>\claude.exe`) or install the
   native CLI. Verify headless auth works before wiring tenant:

   ```
   echo hi | claude -p --model sonnet --output-format json
   ```

2. **Add the provider — one keyless command.** In the TUI:

   ```
   /model add claudecode
   ```

   or from the shell: `tenant model add claudecode`. No API key, no
   endpoint — the catalog fills everything in and the add probes for the
   claude binary immediately. (`/model add-cloud claudecode` also works
   and will refuse to store any key you mistakenly paste.) Model accepts
   Claude Code aliases (`opus` — the default, `sonnet`, `haiku`, `fable`)
   or full model IDs; pin one with `/model use claudecode <model>`.

3. **Route it as the big leg.** `/route configure` → set the big-leg
   provider to `claudecode`. The Supra classifier then escalates prompts
   above `MinComplexityForBig`; start in `shadow` mode to calibrate before
   spending real allowance.

## Picking a model

The default is `opus` (the escalation workhorse — half Fable's burn rate).
Every existing surface works:

- `/model use claudecode <model>` — pin an alias (`opus`, `sonnet`, `haiku`,
  `fable`) or any full Claude model ID.
- `/model pick` and `/model models claudecode` — the picker lists the four
  CLI aliases (there is no HTTP catalog behind the discovery sentinel).
- The route wizard's big-leg model pick offers the same list.

Remember Fable burns the weekly pool ~2× as fast as Opus and is capped at
50% of it — pin `fable` deliberately, not as the default.

## Reasoning effort (`/reasoning`)

`claudecode` supports the same live effort tuning as Sakana Fugu, forwarded
to the CLI's native `--effort` flag. The ladder is Claude's full range —
`low | medium | high | xhigh | max` — and unlike Fugu, `max` is a real
level, not an alias for `xhigh`. `/reasoning` (bare) opens the picker;
`/reasoning medium` sets it directly; `/reasoning off` reverts to the CLI
default. The value persists per-provider in config.json and applies from
the next turn, sub-agents included. Escalation guidance: `medium` is a
strong cost-conscious default for routed turns; reserve `xhigh`/`max` for
prompts you explicitly flag hard.

## Binary discovery

Precedence: explicit `TENANT_CLAUDE_BIN` path → `claude` on PATH → newest
`%APPDATA%\Claude\claude-code\<version>\claude.exe` (desktop-app managed
install; version dir changes on app updates, hence discovery not config) →
`~/.local/bin/claude`.

## Environment knobs

| Env var | Meaning |
|---|---|
| `TENANT_CLAUDE_BIN` | Explicit path to the claude binary (wins over discovery) |
| `TENANT_CLAUDECODE_WEEKLY_BUDGET` | Rolling-7-day token cap for this backend; 0/unset = unlimited. Sized to protect the operator's interactive allowance — 30–40% of the plan's weekly feel is a sane start. |
| `TENANT_CLAUDECODE_ARGS` | Extra flags appended to every spawn (space-split, no quoting) |

## Guardrails

- **Env scrubbing.** Every spawn drops `ANTHROPIC_API_KEY`,
  `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_BASE_URL`, and Bedrock/Vertex toggles
  from the child env, so the call can never silently bill a metered key or
  leak through a GLM base-url override. Subscription login is the only auth
  path.
- **Weekly ledger.** Usage (input + cache-write + output tokens) accrues in
  `<UserConfigDir>/tenant/claudecode-usage.json`. Over budget → the backend
  refuses with `ErrInsufficientBalance` **before** spawning; the fallback
  chain moves to the next provider with a 5-minute cooldown.
- **Plan limits.** CLI "usage limit reached" errors (5-hour window, weekly
  cap) classify as `ErrRateLimited` → failover with a 30-second cooldown,
  so a capped week degrades to GLM/Qwen instead of erroring turns.
- **No permission bypass.** The spawned CLI runs non-interactive, so its
  own permission-gated tools auto-deny. `--dangerously-skip-permissions`
  is never passed: the escalation leg reasons; it does not mutate the host.

## Tool support

`claude -p` has no native tool_calls wire field, so tenant tools ride the
toolfmt text path: profiles default to `ToolFormat: "qwen"`, the adapter
renders ToolSpecs into the appended system prompt, and structured calls are
parsed back out of the result text (same safety-net machinery the vllm
backend uses). Claude follows the format reliably, so escalated turns can
drive the full tenant tool loop.

## Cost model (why this exists)

The subscription is prepaid frontier capacity with a weekly ceiling; the
metered API is unlimited but per-token. Ladder: DGX Qwen (free bulk) →
`claudecode` (frontier, prepaid, budget-railed) → `anthropic`/`zai`
(metered overflow via the fallback chain). Default model is `opus` — half
the burn of `fable`, which is capped at 50% of the weekly pool and better
reserved for the operator's interactive sessions.
