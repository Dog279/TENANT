# Claude subscription auth — research findings & decision

**Date:** 2026-06-09 · **Status:** DECIDED — do **not** implement Claude subscription
auth; keep the `anthropic` API-key provider. **Re-check: after 2026-06-15.**

## Question
Can Tenant use a **Claude Pro/Max subscription** as its LLM provider (OAuth /
"Sign in with Claude") instead of a pay-per-token Anthropic API key — i.e. a flat
subscription used like a coding plan, the way we use the Z.ai GLM coding plan?

**The Z.ai analogy does not hold.** Z.ai *sells* a flat coding plan intended for
programmatic use via an OpenAI-compatible key. Anthropic *explicitly prohibits*
using a consumer Claude subscription this way.

---

## Pass 1 — Subscription OAuth token reuse (deep research, 102 agents, 20 sources)

**Mechanically feasible, but a ToS violation with account-ban risk.**

- **How Claude Code does it:** OAuth 2.0 authorization-code + PKCE — authorize at
  `claude.ai/oauth/authorize`, token at `platform.claude.com/v1/oauth/token`,
  `client_id 9d1c250a-e61b-44d9-88ed-5944d1962f5e`, `localhost:54545` callback.
  Produces `sk-ant-oat01-*` access + `sk-ant-ort01-*` refresh tokens (macOS
  Keychain or `~/.claude/.credentials.json` 0600, `claudeAiOauth` object). Sent as
  **`Authorization: Bearer`** (NOT `x-api-key`) **+ `anthropic-beta: oauth-2025-04-20`**.
  `claude setup-token` mints a 1-year inference-only token.
- **Prohibited:** Consumer Terms §3(7) bar automated/non-human access except via an
  API key (since ~Feb 2024). The **2026-02-19** legal update explicitly bans
  reusing Free/Pro/Max OAuth tokens "in any other product, tool, or service —
  including the Agent SDK," restricting them to Claude Code + native apps.
- **Enforced:** token blocks / account action already applied to OpenClaw,
  OpenCode, Roo Code, Goose.
- **Fragile:** access tokens are short-lived (single-digit hours; exact TTL
  disputed — ~6/8/24h), and auto-refresh frequently fails on headless servers → 401s.
- **OSS bridges exist** (auth2api, LiteLLM Max tutorial, claude-max-api-proxy) —
  feasibility proof, but they operate in violation.

Primary sources: code.claude.com/docs/en/authentication, .../legal-and-compliance;
anthropic.com/legal/consumer-terms; anthropics/claude-code issues
#2586/#42904/#12447/#50743; The Register 2026-02-20.

## Pass 2 — Agent SDK "credit pools" (focused research, 4 agents)

The **sanctioned** alternative — but **not cleanly adoptable by a Go framework today.**

- **What:** from **2026-06-15**, programmatic usage (Agent SDK, `claude -p`, apps
  that authenticate *through the Agent SDK*) draws from a **separate per-user
  monthly credit billed at standard API list prices** (Pro $20 / Max-5x $100 /
  Max-20x $200; no rollover; opt-in). Exhaustion stops requests (or overflows to
  pay-as-you-go) — no fall-back to the cheap subscription rate.
- **Why not viable for Tenant now:**
  - **No Go SDK** — Agent SDK is Python/TS only; the credit-pool/OAuth machinery
    lives inside the Claude Code CLI binary, no documented public HTTP contract.
  - **API keys get NO credit** — the only documented credit path is subscription OAuth.
  - **Third-party login is allowlist-gated** — "does not allow third party
    developers to offer claude.ai login … unless previously approved" (OpenClaw/
    Conductor appear pre-approved; no self-serve registration documented).
  - **OAuth wire protocol undocumented**; non-official clients historically
    fingerprinted/blocked.
- **ToS delta:** the OAuth-token-reuse ban is **NOT lifted**. Going *through* the
  SDK = sanctioned; *extracting/reusing the token yourself* = still banned.
- **Only compliant route today:** subprocess the Claude Code CLI (`claude -p`)
  behind Tenant's backend interface and let it own auth — a heavy Node/binary
  dependency for a Go framework.

Primary sources: support.claude.com/en/articles/15036540;
code.claude.com/docs/en/agent-sdk/overview, .../authentication, .../legal-and-compliance.

---

## Decision
**Keep the `anthropic` API-key provider as the Claude path** (Console `x-api-key`,
already wired; configurable live via `/setup` + `/configure`, hot-reloaded per
TEN-147). Do **not** build subscription-OAuth (ToS violation, ban risk, fragile,
undocumented) and do **not** build credit-pool support yet (no Go path, approval
gate unresolved, weak economics — API rates + hard cap, not cheaper than a key).

If we ever want subscription use, the lowest-risk experiment is an opt-in mode
that **subprocesses `claude -p`** (Claude-Code-owned auth) — deferred until the
approval picture is clear.

## Recommended re-check — after 2026-06-15
Policy changed roughly monthly through 2026; a major change lands 2026-06-15.
Re-verify (re-run the focused 4-agent pass):
1. **Approval reality** — self-serve registration for third-party agents, or still
   an allowlist? Can a general-purpose tool like Tenant qualify?
2. **Wire/spec disclosure** — does Anthropic publish the OAuth client/endpoints/
   scopes, or any non-CLI / documented HTTP path?
3. **Go support** — any first-party Go SDK or documented HTTP contract for
   credit-pool inference (today: none).
4. **Exhaustion + balance** — real behavior on credit depletion; any programmatic
   balance/usage API.
5. **Enforcement posture** — how the "ordinary, individual usage" clause is applied
   to general-purpose agent frameworks.

If 1+3 clear (self-serve approval + a Go/HTTP path), reconsider an opt-in Claude
subscription provider. Until then: API keys.
