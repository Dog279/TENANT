# Tenant Roadmap

This roadmap records public product direction without presenting design intent
as shipped capability. Current implementation facts belong in
[ARCHITECTURE.md](ARCHITECTURE.md); operator verification is dated there.

## Current focus

Near-term work prioritizes information-flow security, browser containment,
Windows secret handling, dashboard authentication, reproducible tests, and
public releases. The next product investments are execution isolation, hard
resource budgets, durable traces, backup/privacy lifecycle, standards-based MCP
and skills extensibility, a provenance-first memory UI, and published
benchmarks.

## Explicitly deferred

The following are intentionally outside the near-term roadmap. They are not
implemented product claims or implied commitments.

| Deferred area | Current decision | Reconsider when |
|---|---|---|
| Mobile nodes and device control | Defer native mobile companion nodes and remote device capabilities. | The core runtime has stable releases, authenticated control-plane boundaries, and an audited device permission model. |
| Voice and wake-word operation | Defer always-listening voice, wake words, and real-time speech surfaces. | Privacy controls, resource budgets, and unattended-action safety are proven. |
| Broad channel coverage | Defer pursuing “20 channels”; maintain the shipped integrations instead of optimizing for channel count. | A concrete operator need justifies a channel and its authentication, moderation, and loop-safety costs. |
| Canvas / A2UI | Defer rich agent-generated canvases and A2UI surfaces. | The provenance UI and existing TUI/dashboard workflows are stable. |
| Hostile multi-tenancy | Defer untrusted multi-user tenancy; Tenant remains a single-operator product. | Process isolation, tenant-scoped storage, authorization, quotas, and adversarial tests exist. |
| Kubernetes deployment | Defer Kubernetes packaging and control-plane work. | A demonstrated multi-node operational need outweighs the single-binary deployment advantage. |
| Large plugin marketplace | Defer a broad public marketplace and marketplace-scale governance. | The external plugin contract, sandboxing, signing, provenance, review, and update policy are stable. |
| PostgreSQL SQL-plugin driver | Defer PostgreSQL support; SQLite is the only shipped SQL-plugin driver. | A supported deployment requires it and can fund driver tests plus operator verification against real PostgreSQL. |

Roadmap changes should name the implemented prerequisite and add dated operator
verification where the capability depends on an external system.
