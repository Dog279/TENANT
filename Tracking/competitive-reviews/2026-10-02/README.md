# Competitive review: Tenant vs OpenClaw vs Hermes Agent (2026-10-02)

**Overall (average of three judges): Tenant 5.7 · OpenClaw 7.5 · Hermes 7.1.** Tenant is the most carefully engineered
and safest-by-default of the three, but about a generation behind on raw capability and reach.

## What was compared

- **Tenant:** the local build, meaning main (`d2497d6`) plus the unpushed local branches `feat/exec-parity`,
  `feat/imessage-modes`, `chore/drop-token-budget` and `mod/*`.
- **OpenClaw:** v2026.9.7 (2026-09-30), repo checked at `32e30e5`.
- **Hermes Agent** (Nous Research): v0.21.5 (2026-09-24), repo checked at `7200e0f`.

## Method

1. Three researchers each wrote an evidence report on one product:
   [Tenant](dossier-tenant.md) (from the code), [OpenClaw](dossier-openclaw.md) and [Hermes](dossier-hermes.md)
   (from shallow clones of their repos plus primary and critical sources).
2. Three judges each scored all three products 1–10 on 14 capabilities, each from a different angle and without
   seeing the others' scores:
   - [A: power user](judge-A.md), who wants the most capable agent today
   - [B: security and reliability](judge-B.md), trusting it with a home server unattended
   - [C: newcomer's guide](judge-C.md), choosing one to self-host today
3. A referee was planned for any score where the judges were more than 2 points apart. None were, so it did not run.

**Caveat:** the judges all read the same evidence reports, so their agreement mostly shows how consistent those
reports are. It is not three independent investigations, though each judge spot-checked claims against the code.

## Scores (average of judges A, B and C)

| # | Capability | Tenant | OpenClaw | Hermes |
|---|---|---|---|---|
| 1 | Messaging channels | 3.7 | 10 | 9 |
| 2 | Running commands (shell, files, background jobs) | 4.3 | 9 | 9 |
| 3 | Browser & computer control | 3.3 | 9 | 8 |
| 4 | Memory & personalization | 7.7 | 8.3 | 6 |
| 5 | Extensibility (skills, plugins, MCP) | 5.7 | 8.7 | 8.7 |
| 6 | Autonomy & scheduling | 6 | 9 | 8 |
| 7 | Multi-agent / delegation | 5.7 | 8 | 8 |
| 8 | Model choice & local LLMs | 7 | 9 | 8.7 |
| 9 | Security & safe defaults | **6.3** | 3.7 | 4 |
| 10 | Monitoring & operations | **8** | 8 | 7.3 |
| 11 | Install & cross-platform | 4.3 | 8 | 8.3 |
| 12 | Engineering quality & reliability | **7.3** | 5.3 | 4.3 |
| 13 | Docs, community & ecosystem | 2 | 9 | 9 |
| 14 | Self-improvement loop | 5 | 6.7 | 7.7 |
| | **Simple average** | **5.5** | **8.0** | **7.6** |

Each judge's overall score uses their own weighting:

| Judge | Tenant | OpenClaw | Hermes |
|---|---|---|---|
| A: power user | 5.5 | 8.4 | 7.8 |
| B: security & reliability | 6.0 | 6.4 | 6.1 |
| C: newcomer's guide | 5.5 | 7.7 | 7.5 |

## Where Tenant leads

- **Engineering and safety.** 4,149 passing tests with 0 failures, race-checked CI on three OSes, approvals on by
  default, tracking that stops sensitive data leaving without approval, and a tamper-evident security log.
  OpenClaw has 722 published security advisories and its sandbox off by default; Hermes runs commands directly
  on the host by default and has 6+ CVEs in 2026.
- **Small local models.** Tool-call parsers per model family, model profiles, a fallback chain across providers,
  and a small router model.
- **Token cost.** About 8.3K fixed tokens per call, against about 14K for Hermes. OpenClaw also burns background
  tokens on its periodic check-ins.

## Where Tenant trails (now tracked as Jira workstreams)

| Gap | Score gap | Tracked as |
|---|---|---|
| Running commands: background jobs, paged reads, workdir and timeout | 4.3 vs 9 | TEN-488 (existing) |
| Vision: the model never sees images or screenshots | 3.3 vs 8–9 | TEN-493 (GAP-1) |
| Channels: Discord and iMessage only | 3.7 vs 9–10 | TEN-494 (GAP-2) |
| Install and upgrade: no installer, packages, image or self-update | 4.3 vs 8+ | TEN-495 (GAP-3) |
| Skills ecosystem and learning loop | 5–5.7 vs 7–8.7 | TEN-496 (GAP-4) |
| Autonomy and delegation: event triggers, sub-agent workspaces | 5.7–6 vs 8–9 | TEN-497 (GAP-5) |
| Public repo, community, docs | 2 vs 9 | TEN-442 / TEN-449 (existing) |

Security, operations and engineering quality are leads, so they have no gap workstream. The Docker sandbox
(TEN-453) keeps the security lead honest.

## Next review

Re-run the same method after the gap workstreams land, scoring against the same 14 capabilities so the history
lines up. Add a new dated folder next to this one and a row to [the index](../README.md).
