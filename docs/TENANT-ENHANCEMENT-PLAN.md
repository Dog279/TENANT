# Tenant Enhancement Plan: Competitive Analysis & Roadmap

**Generated:** 2026-06-09  
**Sources:** Tenant DESIGN.md, MEMORY-DESIGN.md, README.md, wiki research on Hermes/OpenClaw/Odysseus, live web verification, adversarial QA audit, strategic debate

---

## Part 1: Competitive Landscape (Verified)

| Dimension | Tenant | OpenClaw | Hermes Agent | Odysseus |
|---|---|---|---|---|
| **Language** | Go (CGO-free) | Node.js | Python | Python (web/desktop) |
| **GitHub Stars** | N/A (private) | 250K–378K | 186.7K | 64.5K |
| **License** | None (blocker) | MIT | MIT | MIT |
| **Deploy Model** | Single static binary | Node.js service + Gateway | Python service, Docker, Modal | Desktop app (Docker/native) |
| **Skills/Plugins** | 8 built-in tools | 10,700+ on ClawHub | 85 built-in + community | Built-in only |
| **Messaging Channels** | Discord relay only | 24+ (Telegram, Slack, Discord, WhatsApp, Signal, Teams, Matrix, iMessage, WeChat…) | 6 (Telegram, Discord, Slack, WhatsApp, Signal, Email) | None |
| **Memory Architecture** | 6-tier (Soul/Working/Episodic/Semantic/Archive/Procedural) | Flat file (MEMORY.md, SOUL.md, USER.md) + Task Brain (SQLite ledger) | 3-tier + Honcho dialectic + 8 external memory providers | ChromaDB vector + keyword |
| **Self-Improvement** | Designed but not shipped | None | GEPA loop (evaluates every ~15 tasks, auto-writes skills) | None |
| **Sub-Agent Model** | 5 built-in specialists + live bus | None | Isolated subagents, no bus | None |
| **Enterprise** | None | NemoClaw (NVIDIA), DigitalOcean one-click, AWS Marketplace AMIs | Docker/Singularity/Modal sandbox backends | None |
| **Security Track Record** | No audit, no disclosures | 9 CVEs in 4 days (Mar 2026), including CVSS 9.9; actively patching | 20 CVEs in 3 months (4 Critical, 9 High); vendor non-response to disclosure | No audit, no security policy, 493 open issues |
| **Auth / Multi-User** | Single-operator only | Operator scopes, gateway auth (broken then patched) | Single-user focus | Single admin account, no RBAC |
| **Interfaces** | TUI, web dashboard, MCP memory server, Discord relay | CLI, Gateway WebSocket, web | CLI, desktop app (v0.15.2, Jun 2026), web | Desktop web UI, PWA |

### Verified Facts (Corrected from Original Research)

1. **OpenClaw stars:** 250K+ (GitHub), up to 378K reported. Original research cited 247K — understated.
2. **OpenClaw skills:** 10,700+ on ClawHub (not 5,700). However, 1,184 skills were confirmed malicious in the Feb 2026 ClawHavoc campaign (Reversing Labs). Vetted useful subset is a fraction of the headline number.
3. **CVE-2026-25253 belongs to OpenClaw** (CVSS 8.8), not Hermes. The original research misattributed it.
4. **Hermes stars:** 186.7K (star-history.com live), not unreported. Desktop app shipped June 2, 2026.
5. **Odysseus stars:** 64.5K (star-history.com live), not 23K. Launched May 2026, not earlier.
6. **Hermes has 20 CVEs** (Apr–Jun 2026), including RCE via `prompt_builder.py` (CVE-2026-9366) and authorization bypass (CVE-2026-11461). Vendor did not respond to disclosure in any case.

---

## Part 2: Where Tenant Wins

These are advantages no competitor currently replicates.

### W1: Single Static Binary Deployment

No Node.js runtime, no Python virtualenv, no Docker, no desktop environment. One `scp` and the agent runs. This isn't convenience — it's a deployment philosophy with real security implications (smaller attack surface, no npm/pip supply chain, no runtime dependency drift). Cross-compile with `GOOS=linux GOARCH=arm64 go build`. OpenClaw needs Node.js + npm + dependency resolution. Hermes needs Python + pip + venv. Odysseus needs a desktop environment. **No competitor matches this.**

### W2: 6-Tier Layered Memory Architecture

Soul → Working → Episodic → Semantic → Archive → Procedural. Each tier has distinct storage, retention, retrieval, and token-budget policies. Compaction fires at 60% context budget. The assembler is budget-aware across 128K–1M token windows. OpenClaw has three flat markdown files. Hermes has a 3-tier system + Honcho (good, but fewer tiers). Odysseus has ChromaDB search. **Tenant has the most sophisticated memory architecture in the comparison.**

### W3: Built-In Specialist Sub-Agents with Live Bus

Five specialists (Programmer, Researcher, Writer, QA, Strategist) coordinate through a live message bus. The Strategist delegates to Programmer, Programmer hands off to QA, QA escalates back. This models how real teams work. OpenClaw has no sub-agent concept. Hermes has isolated subagents without coordinated bus. Odysseus has none. **This is architecturally unique.**

### W4: Archive-Sourced Reversible Compaction

Compaction summaries are reversible and auditable (not lossy folds). The append-only archive (T5) is the source of truth — all other tiers can be rebuilt from it. OpenClaw's memory is opaque to the agent (it reads files but can't reason about its own memory). Hermes's Honcho decides what to remember with no user-facing audit trail. **Tenant is the only framework where memory compaction is lossless-by-design.**

### W5: Multi-Surface in One Binary

TUI (streaming chat + activity feed), server-rendered web dashboard (no JS build step), MCP memory server over stdio, Discord relay — all compiled into a single artifact. No competitor ships this breadth of interface without external dependencies.

---

## Part 3: Where Tenant Loses (Critical Gaps)

### G1: No Ecosystem / Marketplace

**The gap:** OpenClaw's ClawHub has 10,700+ skills. Even after removing malicious ones, the vetted catalog dwarfs what any competitor offers. A user can `openclaw skill install seo-auditor` and have a working tool in seconds. Tenant has 8 built-in plugin tools with no community extension path.

**Why it matters:** Ecosystems are moats. A framework without a marketplace is a product, not a platform. The #1 reason developers choose OpenClaw is "someone already built what I need."

**Enhancement:** Build a `tenant skill` CLI (install/search/publish) backed by a Git-based skill registry. Skills are versioned markdown + optional Go plugin WASM. Start with the 8 built-in tools extracted as first-party skills. Target: 50 community skills within 3 months of launch.

---

### G2: No Multi-Channel Gateway

**The gap:** OpenClaw supports 24+ messaging platforms. Hermes supports 6. Tenant has Discord relay only. A personal AI assistant that can't reach you on Telegram, Slack, or iMessage is a tool, not an assistant.

**Why it matters:** Meeting the user wherever they are is table stakes for a "personal AI assistant." Multi-channel also enables the future platform play — other users need to reach the agent through their preferred surface.

**Enhancement:** Build a Gateway abstraction layer (`internal/gateway/`) with a uniform message → agent → reply pipeline. Implement Telegram and Slack adapters first (highest ROI, well-documented APIs). Each adapter is a Go interface:

```go
type ChannelAdapter interface {
    Name() string
    Listen(ctx context.Context) <-chan InboundMessage
    Send(ctx context.Context, msg OutboundMessage) error
}
```

Target: Telegram + Slack within 6 weeks. Then WhatsApp (via WhatsApp Business API) and email (IMAP/SMTP — Odysseus proved this is viable).

---

### G3: No Self-Improving Loop (The Biggest Gap)

**The gap:** Hermes's GEPA loop evaluates performance every ~15 tasks, extracts patterns, and auto-writes skills. This is the only framework where the agent *actually compounds knowledge over time*. Tenant has the best memory *container* (6 tiers) but no learning *mechanism*. The refrigerator is world-class; there's no food in it.

**Why it matters:** The DESIGN.md identifies this as core to the vision. Procedural memory (T4) and skill promotion are deferred to v1.1, but they're the features that would make Tenant genuinely superior to every competitor. A memory architecture without learning is an expensive database.

**Enhancement — GEPA-equivalent for Tenant:**

1. **Phase 1 (4 weeks): Outcome logging.** After every agent turn, log `(prompt, tool_sequence, outcome, user_reaction, tokens_used, latency_ms)` to T5 archive. Add `tenant ack` / `tenant undo` CLI commands as explicit success/failure signals. Already designed in DESIGN.md §Continual Learning — just needs to ship.

2. **Phase 2 (4 weeks): Pattern extraction.** Background distillation job (runs every N turns): "From these successful traces, extract reusable tool sequences. Name them. Register as candidate skills." Candidate skills land in a review queue (`~/.config/tenant/skills/proposed/`). User approves with `tenant skill accept <name>`. This is the "loud self-improvement" pattern from DESIGN.md — visible, reversible, never silent.

3. **Phase 3 (6 weeks): GEPA loop.** Every ~15 completed tasks, trigger an evaluation cycle: "Rate your performance on these tasks. Which patterns succeeded? Which failed? Write or refine skills accordingly." The loop writes to the proposed-skill queue — same approval gate. This matches Hermes's GEPA loop but with Tenant's 6-tier memory as the substrate (richer context for evaluation).

**Success metric:** Within 30 days of daily use, the agent should have auto-created ≥5 skills that measurably reduce task completion time on repeated intent patterns.

---

### G4: No License

**The gap:** No `LICENSE` file. All rights reserved by default. No corporation will touch an unlicensed repo. No developer will contribute. No one can legally use, modify, or distribute it.

**Why it matters:** This is a hard blocker, not a nice-to-have. It's a 5-minute fix.

**Enhancement:** Add MIT license immediately (consistent with OpenClaw, Hermes, Odysseus — all MIT). If dual-licensing for enterprise is desired later, add Apache-2.0 as an option. Do this today.

---

### G5: No Enterprise Security Story

**The gap:** No audit, no CVE disclosure process, no sandboxing documentation, no compliance narrative. OpenClaw survived 9 CVEs and came out stronger (hardened, NemoClaw). Hermes has 20 CVEs and is ignoring them (worse). Tenant has no security posture at all — which paradoxically makes it both safer (no known vulns) and riskier (no evidence of hardening).

**Why it matters:** Even for a solo founder's personal assistant, if the agent has OS shell access, SQL access, GSuite OAuth tokens, and iMessage access, a vulnerability means total compromise. The blast radius is everything.

**Enhancement:**

1. **Week 1:** Add `SECURITY.md` with disclosure policy and contact.
2. **Week 2:** Document the permission/gating model (already exists: "dangerous actions require explicit approval") as a formal security architecture.
3. **Week 4:** Run `gosec` + `govulncheck` + `nancy` (dependency scanning) in CI. Publish results.
4. **Week 8:** Commission an external security review (even a lightweight one). Publish findings.
5. **Ongoing:** Adopt a responsible disclosure process. When vulns are found (they will be), publish CVEs and patches. This is how OpenClaw built credibility post-breach.

---

### G6: Single-Operator / No Multi-User Path

**The gap:** All four frameworks are primarily single-user, but OpenClaw at least has the scaffolding (operator scopes, gateway auth). Tenant is explicitly single-operator with no visible path to multi-user.

**Why it matters:** The README calls it "Tenant" — the name implies multi-tenancy. If the vision includes a platform play, multi-user is on the critical path.

**Enhancement:** Defer to v2 but document the architecture now. Add an `agent_id` + `namespace` column to every memory table (already designed in MEMORY-DESIGN.md with `visibility` field). Design the auth boundary as a middleware layer between the Gateway (G2) and the agent runtime. Ship single-operator in v1, but ensure the data model doesn't need a migration when multi-user lands.

---

## Part 4: Enhancement Roadmap

### Priority Matrix

| Priority | Enhancement | Effort | Impact | Depends On |
|----------|------------|-------|--------|-----------|
| **P0** | G4: Add MIT license | 5 min | Unblocks everything | Nothing |
| **P0** | G5.1-3: Security basics (SECURITY.md, doc permissions, CI scanning) | 2 weeks | Trust foundation | Nothing |
| **P1** | G3.1: Outcome logging (ack/undo + trace logging) | 3 weeks | Prerequisite for learning | Nothing |
| **P1** | G2 Phase 1: Telegram + Slack adapters | 6 weeks | Multi-channel reach | Gateway abstraction |
| **P2** | G1 Phase 1: `tenant skill` CLI + Git registry scaffold | 4 weeks | Community extension path | Nothing |
| **P2** | G3.2: Pattern extraction + proposed-skill queue | 4 weeks | First learning behavior | G3.1 |
| **P3** | G3.3: GEPA evaluation loop (every ~15 tasks) | 6 weeks | Full self-improvement | G3.2 |
| **P3** | G2 Phase 2: WhatsApp + Email adapters | 4 weeks | Broader reach | G2 Phase 1 |
| **P3** | G5.4: External security review | 8 weeks | Enterprise credibility | G5.1-3 |
| **P4** | G1 Phase 2: Community skill publishing flow | 4 weeks | Marketplace viability | G1 Phase 1 |
| **P4** | G6: Multi-user data model (documentation + schema prep) | 3 weeks | Future platform path | Nothing (schema only) |

### 12-Week Sprint Plan

**Weeks 1–2: Foundation**
- [ ] Add MIT LICENSE file
- [ ] Add SECURITY.md with disclosure policy
- [ ] Document permission/gating model formally
- [ ] Add `gosec` + `govulncheck` + `nancy` to CI pipeline
- [ ] Begin outcome logging: append `(prompt, tools, outcome, reaction, tokens, latency)` to T5 archive on every turn
- [ ] Implement `tenant ack` / `tenant undo` CLI commands

**Weeks 3–4: Gateway Abstraction**
- [ ] Define `ChannelAdapter` interface in `internal/gateway/`
- [ ] Implement Discord adapter (refactor existing relay into adapter pattern)
- [ ] Implement Telegram adapter (Bot API, polling or webhook)
- [ ] Begin Slack adapter (Socket Mode for development simplicity)

**Weeks 5–6: First Learning Signal**
- [ ] Build background distillation job: scan successful traces → extract candidate patterns
- [ ] Implement proposed-skill queue (`~/.config/tenant/skills/proposed/`)
- [ ] Implement `tenant skill list/accept/reject` CLI
- [ ] Slack adapter ships

**Weeks 7–8: Skill Registry Scaffold**
- [ ] Design Git-based skill registry format (markdown frontmatter + optional WASM plugin)
- [ ] Implement `tenant skill install/search/publish` CLI
- [ ] Extract 8 built-in tools as first-party skills in the registry
- [ ] Publish registry repo

**Weeks 9–10: GEPA Loop**
- [ ] Implement evaluation cycle trigger (every ~15 completed tasks)
- [ ] Evaluation prompt: rate performance, identify success/failure patterns, write/refine skills
- [ ] Output lands in proposed-skill queue (same approval gate)
- [ ] Integration test: agent should auto-create ≥1 skill from repeated use patterns

**Weeks 11–12: Security Hardening + Polish**
- [ ] Commission external security review
- [ ] Publish review findings and remediation
- [ ] WhatsApp Business API adapter (or email IMAP/SMTP if WA is blocked by API access)
- [ ] Update README with new capabilities, add license badge, add security policy badge
- [ ] Write migration guide for OpenClaw/Hermes users

---

## Part 5: Strategic Positioning

### The Honest Assessment

Tenant has the **best memory architecture** and the **best deployment model** in the comparison. No competitor matches the 6-tier cognitive memory with token-budgeted assembly in a single static binary. The built-in specialist sub-agents with live bus orchestration are architecturally unique.

Tenant is **behind on ecosystem** (no marketplace), **behind on reach** (one messaging channel vs 24+), **behind on learning** (the mechanism is designed but not shipped), and **behind on credibility** (no license, no audit, no community).

### The Positioning Play

Don't compete with OpenClaw on ecosystem breadth. Don't compete with Odysseus on consumer UX. Compete on **cognitive depth**:

> **Tenant: The agent framework with a memory system that actually thinks.**

The pitch to developers: "OpenClaw gives you 10,000 skills but the agent never learns. Hermes learns but has 20 unpatched CVEs. Tenant gives you a 6-tier memory architecture that compounds knowledge over time, specialist sub-agents that coordinate like a real team, and it deploys as a single binary you can `scp` anywhere."

The pitch to yourself: "Build the brain, not the app store. The memory architecture is the moat. Everything else (channels, skills, community) is table stakes that can be added. The 6-tier memory with learning is what makes Tenant worth building instead of just using OpenClaw."

### What Not to Build

- **Don't build a ClawHub clone.** Build a lightweight Git-based skill registry. 50 good skills beat 10,000 unvetted ones.
- **Don't build an enterprise sales motion.** You're a solo founder. Build for power users like yourself.
- **Don't build a desktop app.** TUI + web dashboard + Discord is enough surfaces. Odysseus's desktop app is a liability, not an asset.
- **Don't chase every messaging channel.** Telegram + Slack + Discord covers 80% of the use case. Add more when users ask for them.

---

## Appendix: Source Documents

| Source | What it contributed |
|--------|-------------------|
| Tenant `README.md` | Feature list, current capabilities, repository structure |
| Tenant `docs/DESIGN.md` | Architecture vision, premises, build order, continual learning design |
| Tenant `docs/MEMORY-DESIGN.md` | 6-tier memory architecture, token budgets, multi-agent memory sharing |
| Wiki: `research-hermes-vs-openclaw-vs-odysseus-pros-cons-best-2026-06-09.md` | Original competitive research (with corrections noted above) |
| Petronella Tech — OpenClaw Guide | OpenClaw history, Task Brain, version scheme, CVE details |
| NVD — CVE-2026-25253 | Confirmed CVE belongs to OpenClaw (CVSS 8.8, RCE via WebSocket) |
| CSA Research Note — Hermes CVEs | Hermes security audit: 4 Critical, 9 High, vendor non-response |
| Hermes Agent Official Docs | GEPA loop, Honcho integration, memory providers, desktop app |
| Odysseus GitHub README | ChromaDB memory, Cookbook engine, CalDAV, v1.0 feature set |
| star-history.com | Live star counts: OpenClaw 250K+, Hermes 186.7K, Odysseus 64.5K |
| Reversing Labs — ClawHavoc | 1,184 malicious skills on ClawHub (supply chain attack) |
