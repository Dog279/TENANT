# Hermes → Tenant — Operator Cutover Checklist (run on the tyclaw Mac)

Everything that **cannot** be done from the dev machine. The repo-side code is
done (PRs #3–#13 + buildHub #2 — see Phase 0); this is the live work. Work top
to bottom. Tickets: epic **TEN-288**.

> ## 🛑 Hard rules — read first, every time
> - **All test/verification sends go to Dylan (+15302208314). NEVER Tyler (+19164121156).**
> - **Draft-first on all email — never auto-send.**
> - **Nothing a launchd unit touches may live under `~/Desktop`, `~/Documents`, `~/Downloads`** (macOS TCC will silently deny file access).
> - **Never `launchctl bootout` an agent mid-turn** — drain first (status `turn_active=false`).
> - Keep **Hermes installed + bootable** for the full 1-week rollback window.

Reference docs in this repo: `docs/migration/deploy-and-rollback.md` (launchd +
rollback detail), `deploy/launchd/com.tenant.serve.plist` (the unit template).

Live topology (from the audit): local vLLM-MLX `http://127.0.0.1:8000/v1`
(planner) · DGX `http://192.168.1.229:8000/v1` (aux/vision) · Z.ai GLM-5.1
(frontier). crm-tool `/Users/tyclaw/bin/crm-tool` → `~/.assistant/assistant.db`.
SA key `automation.json` impersonating `tylerx@hundred.com`. PA repo
`~/Projects/personal-assistant` (NOT `~/Desktop/...`).

---

## Phase 0 — merge the dev-machine PRs + build

- [ ] Review + merge the open PRs on `github.com/Dog279/TENANT` (12 open). Suggested order — **merge the big buildHub refactor first**, then the rest, re-running the suite after each (several touch `cmd/tenant/commands.go` / `toolmux.go` / the iMessage pkg, so expect the odd conflict GitHub will flag):
  1. **#2** buildHub (TEN-247) — biggest `commands.go`/`serve.go` refactor.
  2. #10 (TEN-283 cosine+BLOB), #7 (TEN-282 health), #5 (TEN-284 MCP), #4 (TEN-286 pass^k), #3 (TEN-281 docs) — F/G internal.
  3. #8 (TEN-265), #6 (TEN-268), #12 (TEN-267) — iMessage pkg.
  4. #11 (TEN-269 crm), #13 (TEN-279 memory import) — cmd/tenant.
  5. #9 (TEN-263/264 deploy docs).
- [ ] `go build -o tenant ./cmd/tenant && go test ./...` on the merged `main` → all green.
- [ ] Install the binary outside the TCC dirs: `cp tenant /Users/tyclaw/bin/tenant`.

---

## Phase 1 — parallel bring-up (no iMessage front door yet)

### TEN-262 · serve headless against the live endpoints (P0)
- [ ] Configure `config.json` role routing: planner→vLLM-MLX, executor/aux→DGX, frontier→Z.ai GLM-5.1, embedder→your embeddings endpoint. Mirror the Hermes fallback order (vllm-dgx, zai) — Tenant's `FallbackLLM` (TEN-246) already does 429/outage failover.
- [ ] (optional, recommended) enable the proactive **health-gating** (TEN-282) once you've eyeballed each backend's latency — it demotes a slow-but-200 endpoint before it thrashes.
- [ ] `tenant serve` (no iMessage channel). Drive a multi-turn tool session via the dashboard (`http://127.0.0.1:8770`) or `tenant attach --follow`.
- [ ] Confirm the `qwen` toolfmt parses `<tool_call>`/`<think>` cleanly against live Qwen 3.6.
- [ ] **Acceptance:** force a local-vLLM outage → DGX/Z.ai failover fires; multi-turn tool session completes end-to-end. No echo backend in the path.

### TEN-263 · production launchd unit (P0)
- [ ] Copy `deploy/launchd/com.tenant.serve.plist` → `~/Library/LaunchAgents/com.tyclaw.tenant.plist`; fill the `{{PLACEHOLDERS}}` (binary, `--config`, `--data`, logs — all OUTSIDE the TCC dirs).
- [ ] `launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.tyclaw.tenant.plist && launchctl kickstart -k gui/$(id -u)/com.tyclaw.tenant`.
- [ ] **Acceptance:** reboot → `com.tyclaw.tenant` returns AND `com.tyclaw.hermes` still boots independently; logs at your chosen path; crash → restarts.

---

## Phase 2 — memory + core integrations config

### TEN-279 · import Hermes memory content (P0)  — *tool shipped (#13)*
- [ ] `tenant memory import ~/.hermes/MEMORY.md --protected` (feedback/correction items → protected). `--dry-run` first to preview.
- [ ] Import the durable user facts from USER.md / FOUNDER_PROFILE.md the same way (`--protected`). **Soul** (T0 operating instructions) is hand-curated — copy the operative directives into Tenant's soul via `/memory soul`, don't auto-import.
- [ ] **Acceptance:** `tenant memory search` recalls the operative facts (test sends→Dylan, the iMessage echo rule, local-first default, etc.) in context.

### TEN-270 · Google Workspace service-account (P0)
- [ ] **Relocate `automation.json` out of `~/Desktop`** (TCC) → e.g. `~/.config/tenant/automation.json` (0600).
- [ ] Configure the gsuite plugin for SA + domain-wide delegation impersonating `tylerx@hundred.com`; turn on the read-only scope-downgrade when send is off; enforce **draft-first**.
- [ ] **Acceptance:** Calendar read + Gmail **draft** create work via SA; sending requires explicit confirmation; key no longer under `~/Desktop`.

### TEN-269 · CRM wrapper config (P0)  — *gated tool shipped (#11)*
- [ ] `--crm-tool-path /Users/tyclaw/bin/crm-tool` (or `$CRM_TOOL_PATH`). Leave `--crm-allow-mutate` off (gated ops go through Confirm).
- [ ] Sanity-check the arg contract: the wrapper passes one positional `query` per subcommand — if a real subcommand wants flags, adjust `toolToSubcommand` arg-building in `internal/plugins/crm/dispatch.go` (allowlist/gating/no-shell are unaffected).
- [ ] **Acceptance:** `tenant` answers a CRM lookup **via crm-tool** with zero raw-SQL fallback attempts.

---

## Phase 3 — iMessage hardening + live verification (riskiest surface)

Code for all of these is shipped (#6/#8/#12); these are the live checks. **Dylan number only.**

### TEN-265 · dedup / BOM strip (P0)  — *code #8; layers 1+2 already existed*
- [ ] 24h **shadow run on the Dylan number** → zero echo loops, zero duplicate responses.

### TEN-267 · text-confirm handshake (P1)  — *code #12 (security-reviewed)*
- [ ] Set the iMessage **operator handle** in config (the field is already provisioned). Set a category to `ask` via `/imessage permissions` for what you want phone-approvable.
- [ ] Needs **Full Disk Access** for chat.db.
- [ ] **Acceptance:** a Drive/Gmail/SQL-write requested over iMessage prompts `Approve … reply: Y <nonce>`; only your `Y <nonce>` from the operator handle executes it; a stranger / wrong nonce / timeout denies.

### TEN-266 · Tahoe tapback liveness (P1)  — *needs decision*
- [ ] Typing indicators are dead on Tahoe. Confirm the `imsg`/`imsg.real` binary's `react` works with the binary in **Accessibility** (not just AppleEvents — else Funk-sound TCC PostEvent denial).
- [ ] Tenant's native transport is AppleScript-send only → tapback needs that external `react` path. **Once you confirm the transport, ping me and I'll wire the inbound-ack** (small task).
- [ ] **Acceptance:** inbound message gets a tapback ack within ~1s on Tahoe (Dylan number).

### TEN-268 · outbound chunking (P2)  — *code #6*
- [ ] **Acceptance:** send a 2000+ char reply → arrives as clean multi-bubble output, not truncated.

---

## Phase 4 — re-bridge the personal-assistant jobs

### TEN-272 · daily founder brief (P1)
- [ ] Fix the path bug: cron must hit `~/Projects/personal-assistant` (NOT `~/Desktop/personal-assistant`). Keep `daily_founder_brief.py` external; have Tenant's cron trigger/relay it.
- [ ] **Acceptance:** the 07:00 brief generates; `~/.assistant/daily-founder-brief-status.jsonl` shows real success (no worked-around tool failures).

### TEN-274 · re-bridge launchd jobs (P1)
- [ ] Inventory every `com.tyclaw.*` job; repoint any that relay **through the Hermes gateway** to Tenant instead (leave DB-only ones). Keep `_llm.chat_with_fallback` as the LLM path.
- [ ] **Acceptance:** no launchd job depends on the Hermes gateway being up once cutover completes.

### TEN-271 · Cluely transcript bridge (P1)
- [ ] Keep `com.tyclaw.cluely-watcher` external (app is now "Cluely (New)"); point Tenant's wiki/sql tools at the same Obsidian dir + `assistant.db`.
- [ ] **Acceptance:** a new Cluely transcript is queryable by Tenant within one watcher cycle.

### TEN-273 · X content engine (P2)
- [ ] Wrap/trigger the existing X engine (Dylan's X app + Tyler's tokens). **Acceptance:** content seeds generate on demand.

---

## Phase 5 — capability follow-ups (optional, off the cutover path)

### TEN-275 · strategic-research parity (P2)  — *Tenant's `research` already exceeds Hermes*
- [ ] Add the HUNDO_STRATEGY anchoring as a `strategy` soul fragment / agent profile; run `tenant research "<a real strategy question>"` and compare to a Hermes baseline.

### TEN-277 · vision (P2)
- [ ] Route an image to the DGX aux vision model. **Ping me and I'll build the vision tool** once the live vision endpoint is confirmed.

### TEN-276 · voice (P2)
- [ ] New whisper-STT + multi-backend TTS plugin. Significant build, needs audio I/O — schedule separately if wanted.

---

## Phase 6 — eval baseline (de-risks the cutover)

### TEN-285 · migration baselines (P1)  — *harness + pass^k ready (#4)*
- [ ] Build fixture tasks for the real workloads (CRM lookup, calendar/email draft, daily brief, deep research); capture a baseline in `baselines/{smoke,full}.json` with live models. Use `Rollouts: k` on flaky tasks → real pass^k.
- [ ] **Acceptance:** a green baseline exists; the preflight gate blocks regressions on any pre-cutover change.

---

## Phase 7 — canary → cutover → (rollback if needed)

### TEN-287 · cutover canary (P0 — the go/no-go)
- [ ] Run Tenant + Hermes in parallel for a fixed window; route the **Dylan number** to Tenant; compare reply quality / latency / failures. **No Tyler-number traffic until this passes.**
- [ ] **Acceptance:** your pass criteria met over the window → cutover approved.

### Cutover + rollback (TEN-264)
- [ ] Follow `docs/migration/deploy-and-rollback.md`: drain → `bootout` Hermes → enable Tenant's iMessage channel → verify on Dylan.
- [ ] **Pre-flight the rollback once and time it** — must be **< 2 min, no message loss** (§3 of that doc). Keep Hermes bootable for 1 week.

---

## Quick map: ticket → phase

| Ticket | Phase | P |
|---|---|---|
| TEN-262 serve live | 1 | P0 |
| TEN-263 launchd | 1 | P0 |
| TEN-279 memory import | 2 | P0 |
| TEN-270 gsuite SA | 2 | P0 |
| TEN-269 crm config | 2 | P0 |
| TEN-265 dedup shadow | 3 | P0 |
| TEN-267 text-confirm | 3 | P1 |
| TEN-266 tapback | 3 | P1 |
| TEN-268 chunking | 3 | P2 |
| TEN-272 founder brief | 4 | P1 |
| TEN-274 PA jobs | 4 | P1 |
| TEN-271 Cluely | 4 | P1 |
| TEN-273 X engine | 4 | P2 |
| TEN-275 research | 5 | P2 |
| TEN-277 vision | 5 | P2 |
| TEN-276 voice | 5 | P2 |
| TEN-285 eval baseline | 6 | P1 |
| TEN-287 canary/cutover | 7 | P0 |
| TEN-264 rollback dry-run | 7 | P1 |

When a ticket's live acceptance passes, move it to **Done** in Jira. Ping me to
build: the TEN-266 tapback-ack wiring (once the `imsg react` path is confirmed)
and the TEN-277 vision tool (once the DGX vision endpoint is confirmed).
