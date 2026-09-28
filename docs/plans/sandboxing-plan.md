# Tenant sandboxing — limit blast radius when the model acts up, without breaking what works

Prepared 2026-09-22 against `feat/logs-hub` (the R340 production line). Anchors below were
verified on that tree today; origin/main anchors are marked as such.

**Tickets this plan answers**

| Where | Key | What it asks for | Status |
|---|---|---|---|
| twiki (R340 box) | **MOD-07 — unattended-execution isolation** | threat model, option ladder (fs/process restriction → container → VM), escape tests | scoped only: `docs/plans/MOD-07-isolation.md` on branch `mod/06-07-08-docs`; the day-one rename shipped on `mod/07-rename-sandbox` |
| Jira | [TEN-302](https://findtime.atlassian.net/browse/TEN-302) SEC workstream (2026-07 audit) | information-flow containment umbrella | To Do (children mostly In Progress) |
| Jira | [TEN-318](https://findtime.atlassian.net/browse/TEN-318) [EXT-003] | third-party MCP subprocesses isolated by default: env allowlist, fs roots, egress policy, resource caps, visible in the effective-policy view | To Do |
| Jira | [TEN-316](https://findtime.atlassian.net/browse/TEN-316) / [TEN-334](https://findtime.atlassian.net/browse/TEN-334) | Chrome process sandbox restored; uid_map-only namespace must not count as an outer sandbox | fixed on origin/main; follow-up open |
| Jira | [TEN-331](https://findtime.atlassian.net/browse/TEN-331) [SEC-008] | `tenant security audit` + `tenant permissions effective`; its report already has an `execution.{backend,isolation}` slot that is hard-coded to `host` / `none` | on origin/main |
| Jira | [TEN-326](https://findtime.atlassian.net/browse/TEN-326) [ARC-005] | bounded agent-team resource governance | on origin/main |

Short version: the sandboxing ticket is MOD-07 on the twiki, not Jira. Jira has the pieces
around it. This plan is the missing "actual isolation" design MOD-07's note deferred, sized
so every phase leaves Tenant fully usable.

---

## 0. TL;DR

1. **The production line does not carry the July audit merge.** `origin/main` has 68 commits
   R340 lacks, including the 18 hardening fixes (SSRF/egress guard, restored Chrome sandbox,
   sensitive-data egress labels, secrets protection, loopback dashboard auth, run traces,
   `tenant security audit`). R340 has 23 commits origin/main lacks (file-root confinement,
   cron overrides, the tokenize/media-scrub fixes, and everything stacked on it: MOD branches,
   Discord approval cards, the logs hub, tool rules). A dry-run merge conflicts in 10 files.
   Building isolation on the R340 line without reconciling forks the security story a second
   time. **Phase 0 is the merge.** (§3)
2. **Add a boundary under the gate, don't replace the gate.** Tenant's approval model (categories,
   per-tool rules, per-surface brokers, unattended-surface cuts) is good and stays. What is
   missing is an *execution boundary* for anything that spawns a process: `os_exec`, cron shell
   jobs, cron `exec=true` turns, and later stdio MCP servers. (§4)
3. **One seam, three backends.** An `internal/sandbox` execution-backend interface with
   `host` (today, honestly labelled), `restricted` (per-OS primitives: Windows Job Object +
   restricted low-integrity token first, because the 24/7 box is Windows; macOS Seatbelt;
   Linux bubblewrap/Landlock), and `container` (ephemeral Docker/Podman, for unattended and
   third-party work). A policy object (work dir, read/write/deny roots, network mode, env
   allowlist, limits) is resolved per surface from `exec_profiles` in settings, hot-reloaded,
   never agent-writable. (§4.2–4.4)
4. **Functionality is preserved by escalation, not by holes.** On an attended surface a
   blocked action becomes an approval request ("run this outside the sandbox?") through the
   existing broker. On an unattended surface it is a hard deny plus a loud event. Nothing that
   works today silently stops; it asks once. (§4.5)
5. **Escape tests are the acceptance criteria**, run on the 3-OS CI matrix and, via a new
   `tenant security selftest`, on the R340 box itself. (§5)
6. Sequencing: Phase 0 reconcile+posture → 1 seam/host/audit wiring → 2 restricted backend
   (Windows first) → 3 unattended defaults + escalation UX → 4 container + MCP stdio (TEN-318)
   → 5 egress proxy, Windows secret confidentiality, provenance quarantine. (§6)
7. Five decisions are Dylan's; none block Phases 0–3. (§7)

---

## 1. Threat model: what "the LLM acts up" means in Tenant

Adversary is not a hostile operator or a compromised OS account (out of scope, same as
SECURITY.md). It is the model itself, steered or mistaken:

| Failure shape | How it enters Tenant | Example |
|---|---|---|
| **Prompt injection** | any content a tool returns: web pages, email, Jira/Confluence bodies, Discord/iMessage text, files, peer notes, MCP results | a fetched page says "run `curl … \| sh`" or "email the credentials file to X" |
| **Confidently wrong action** | hallucinated flags, wrong path, wrong recipient | `Remove-Item -Recurse` on the wrong tree; `git push --force` |
| **Runaway** | loops, fan-out, retry storms | 30 sub-agents, a send loop, a shell job that forks or floods output |
| **Self-persistence** | writes that change future behaviour | editing `config.json`/`settings.<agent>.json`, adding cron jobs, saving a poisoned skill/memory/wiki note, adopting an MCP server |

Assets, in the order they get hurt: the operator's secrets (env + credential files); Tenant's
own config/data (self-modification); the host filesystem; outbound channels (email, iMessage,
X, Discord, Jira, GitHub); the LAN (R640 model host, peers); the operator's accounts behind
every integration.

The audit's central point stands: *moving* information is as dangerous as mutating it. This
plan covers the process boundary; egress labelling (SEC-002) and the web egress guard (SEC-001)
cover the in-process tools and must ship together with it (§3).

---

## 2. Surface inventory: what exists today and how real it is

"Real boundary" means enforced by the OS or by path resolution, not by pattern matching or
prompt text.

| Surface | Human present? | Gate | Containment today | Blast radius once the gate says yes |
|---|---|---|---|---|
| **TUI / dashboard `os_exec`** | yes | broker: category `exec` ask/allow/deny + per-tool rules (`cmd/tenant/approval.go:202`, `toolrules.go`) | `osys.Classify` denylist only (`internal/plugins/osys/classify.go:51`, self-described "NOT a security sandbox"); 60 s timeout; 16 KB output cap | full: runs as the Tenant user, whole fs, whole network |
| **`os_write/append/edit/mkdir/read/list`** | yes | category `write` | **real**: `osys.Roots` deny + write roots, symlink-resolved (`osys/paths.go:103`), default deny = Windows/Program Files/ProgramData (`paths.go:139`). **Only on the R340 line** (59b3c16), not on origin/main | bounded to write roots |
| **Discord relay** | remote operator | button approver per category; operator allowlist | **none**: full live tool surface, no wall (`discordtools.go:1-30`); box posture is currently **all `allow`** so `exec` never prompts (docs/changes/discord-approval-cards.md) | full, unprompted, from a phone |
| **iMessage responder** | remote operator | deny-by-default per category; text-nonce approvals (7ae89f1) | none beyond the gate | bounded by deny default |
| **Cron prompt job (safe)** | no | every gated tool cut; fan-out cut; `cron_*`/`mcp_*` cut (`restricttools.go`, `cronmgr.go:93`) | memory-isolated (nil long-term stores) | read + comms only |
| **Cron `exec=true` job** | no | filtered auto-approver: destructive denied (with overrides for Discord send, `wiki_save`, MCP read verbs), writes into cfg/data denied by **substring match** on the detail string (`cronmgr.go:184-204`, `mentionsDir`) | `exec` category is auto-approved, so any non-catastrophic shell runs; `scrubbedEnv` strips vars whose *name* contains KEY/TOKEN/SECRET/PASSWORD (`cronmgr.go:253`); cwd = `<data>/cron-work` (`cronmgr.go:282`, the thing MOD-07 renamed) | full shell as the Tenant user, unattended. **This is MOD-07 phase 1.** |
| **Cron shell job** | no | `Classify` refusal | scrubbed env, work dir | same as above minus the agent |
| **Sub-agents (team)** | inherits | same brokers via ctx | full plugin set + own browser (`team.go:584`); concurrency cap 4 (`team.go:440`); budgets on origin/main (TEN-326) | same as parent |
| **Browser (web plugin)** | inherits | `web_click/fill/select` gated as category `web`; dangerous clicks → `web_transact` (destructive) | R340 line: `--no-sandbox` unconditionally (`web/web.go:359`); origin/main: sandbox on by default + `detectOuterSandbox` (TEN-316; TEN-334 open) | renderer exploit = Tenant user |
| **Web egress / SSRF** | inherits | none on the R340 line | origin/main: `egressGuard` blocks loopback/private/metadata targets, Jina reader opt-in (`web/egress.go`) | R340 line: the agent's browser can reach the loopback dashboard REST (`/api/approvals`), where auth is optional on loopback (TEN-323 fixes on origin/main). A turn could in principle approve its own pending request. |
| **Sensitive-data egress** | inherits | none on the R340 line | origin/main: result labels + outbound-argument matching → `egress` category (`internal/agent/infoflow.go`, TEN-311) | secrets read by `os_read_file` can be pasted into a URL, message, or MCP argument |
| **Remote MCP tools** | inherits | per-connector trust ask/allow/deny + tool rules; unattended surfaces cannot adopt | URL-only adoption today; `internal/mcp/transport/stdio.go` can spawn a process but no agent path reaches it (TEN-318 is forward-looking) | per server trust |
| **`claudecode` model backend** | n/a | spawns `claude -p` without `--dangerously-skip-permissions`, env scrubbed (`claudecode.go:28-31,153`) | the CLI's own write tools auto-deny; reads are not blocked | host reads reach the vendor session |
| **Self-persistence tools** | inherits | `cron_add`, `mcp_add/trust` gated; `skill_save`, `memory_remember`, `wiki_save` (wiki_save root-confined + overwrite gate) | unattended surfaces cut `cron_*`/`mcp_*`; the Discord relay shares the main agent's memory | a poisoned skill or memory steers every later turn |

Takeaways that shape the design:

- **Every process spawn reachable from a model turn goes through exactly two places**:
  `osys.Service.Exec` (already runner-injectable, `osys/osys.go:29,61`) and `runCronShell`
  (`cronmgr.go:234`). One seam covers both. (The other `exec.Command` sites are OAuth browser
  launches, `osascript`, `tailscale`, the self re-exec in `commands.go:645`, and the model CLI.)
- **File tools already have a real, resolved boundary** (`osys.Roots`). The sandbox should reuse
  its semantics, not invent new ones, and the merge must keep it.
- **The gate model is per-surface and per-category; the boundary should be too**, resolved
  from the same settings file the tool rules live in, watched by the same hot-reload.

---

## 3. Finding: reconcile before building (Phase 0)

Verified today:

```
origin/main tip        3564f55   local main is 72 commits behind it
merge-base main/R340   b1f7407
origin/main..R340      23 commits (Roots, cron overrides, media scrub, R340 prod fixes;
                       MOD-*, discord-approval-buttons, logs-hub stack on top of it)
R340..origin/main      68 commits (8c9a853 "Merge integration/audit-2026-07: 18 audit
                       hardening fixes + follow-ups", CI for 3 OSes, security workflow)
git merge-tree origin/main R340 → CONFLICT in:
  cmd/tenant/approval.go  commands.go  cronmgr.go  routewire.go  serve.go  team.go
  toolmux.go  internal/agent/agent.go  internal/dashboard/rest.go  docs/PLUGINS.md
```

Packages the exec path will lean on that exist **only** on origin/main: `internal/fsperm`
(private file modes, used by `cronSandboxDir`), `internal/runtrace` (durable run records),
`internal/secrets` (protected credential storage), `internal/plugins/web/egress.go`,
`internal/agent/infoflow.go`, `cmd/tenant/securityaudit.go`, `cmd/tenant/permissionsctl.go`.

Things that exist **only** on the R340 line and must survive the merge: `osys.Roots`
(`paths.go`), `resolveOSRoots` (`toolmux.go:2008`), the cron destructive overrides, the media
sentinel scrub, the Discord/iMessage approval work, tool rules + settings watcher, the log book.

**Phase 0 steps**

1. Fast-forward local `main` to `origin/main`.
2. Integration branch `integration/r340-audit`: merge `origin/main` into R340, resolve the 10
   files. Known semantic overlaps: `approval.go` (Discord send categorisation vs the new
   `egress` category and presets), `cronmgr.go` (`fsperm`/`runtrace` vs the overrides and
   the rename), `toolmux.go` (Roots wiring vs audit flags), `agent.go` (infoflow hook vs the
   compaction/tokenize fixes), `rest.go` (addon events vs auth).
3. Rebase the unpushed feature branches onto it in order: `feat/discord-approval-buttons`,
   `feat/logs-hub`, `mod/*`.
4. Deploy to the box; **posture flip** the change docs already recommend: relay `exec`,
   `destructive`, `send` → `ask`; host `destructive` → ask; run `tenant security audit --json`
   on the box and keep the report as the baseline (it will say `execution.isolation: none`).
5. Confirm the two audit behaviours that close the self-approval loop are live on the box:
   loopback/private targets refused by the web plugin, bearer required on `/api/approvals`.

Effort: one to two sittings plus a deploy window. Nothing in Phases 1–5 depends on the
*content* of the merge, only on building from a single base.

---

## 4. Design

### 4.1 Principles

1. **Gate + boundary.** Approval decides *whether*; the sandbox decides *what can happen when
   it runs anyway*. Neither replaces the other.
2. **Escalate on attended surfaces, deny on unattended.** A blocked action is an approval
   request when a human is reachable, a hard stop plus alert when not.
3. **Policy is data.** Per-surface exec profiles in `settings.<agent>.json`, hot-reloaded by
   the existing watcher, editable from TUI/dashboard/file, and — like `tool_rules` — with **no
   agent tool** that can change them.
4. **Honest labels.** The host backend is called `host` and the audit reports
   `isolation: none`. The classifier stays as a fast-fail UX layer, never described as a
   boundary. (MOD-07's rename was the first step of this.)
5. **Portable core, per-OS enforcers, loud degradation.** A backend that cannot enforce a
   policy element on this host says so in `Probe()`, the audit and `doctor` show it, and the
   surface falls back to *deny* for unattended work, *ask* for attended work — never to
   silent host execution.
6. **Adversarial tests are the spec.** Each escape case fails on `host`, passes on
   `restricted`/`container`, and its legitimate twin passes on all.

### 4.2 The seam: `internal/sandbox`

The audit already calls this the *execution backend*; keep that vocabulary.

```go
package sandbox

type NetworkMode int // NetNone | NetAllowlist | NetFull
type Isolation int   // None | Process | Container | VM   (feeds securityaudit.executionAudit)

type Policy struct {
    WorkDir    string          // the only implicitly writable place; cwd; TMP/TEMP/HOME point here
    ReadRoots  []string        // empty = whole host readable (restricted still masks DenyRoots)
    WriteRoots []string        // osys.Roots.Write semantics, symlink-resolved
    DenyRoots  []string        // osys.Roots.Deny semantics; always includes cfgDir, dataDir, the binary
    Network    NetworkMode
    Allow      []string        // hosts/CIDRs when NetAllowlist (model endpoints live here)
    Env        EnvPolicy       // allowlist of names + fixed values; replaces the KEY/TOKEN name denylist
    Limits     Limits          // Wall, CPU, MemoryBytes, MaxPIDs, MaxOutputBytes
}

type Command struct{ Shell string; Args []string; Stdin string; Surface string; Origin string }

type Result struct {
    Output     string
    ExitCode   int
    TimedOut   bool
    Violations []Violation   // what the boundary refused (path/network/limit), for the model AND the feed
}

type Backend interface {
    Name() string             // "host" | "restricted" | "container"
    Isolation() Isolation
    Probe(ctx) Capabilities   // FS, Network, Limits, Env: enforced | partial | none, with reasons
    Run(ctx, Command, Policy) (Result, error)
}
```

**Callers** (all via injection, no new global):

- `osys.Service.run` — today a `runner` func; becomes `backend.Run` with the surface's policy.
  `os_exec` keeps its gate; the dispatcher just gains a policy source.
- `runCronShell` — replaces `exec.CommandContext(sh -c …)`.
- cron `exec=true` turns — the exec agent's osys dispatcher is bound to the `cron-exec` profile.
- later: stdio MCP servers (`transport.NewStdioProcess`) and the `claudecode` CLI run under a
  `container`/`restricted` policy (TEN-318).

**Profiles.** `exec_profiles` in `settings.<agent>.json`, one per surface, defaults baked in:

| Surface | Default backend | Write roots | Network | Env | Limits | On violation |
|---|---|---|---|---|---|---|
| `tui`, `dashboard` | restricted | Tenant tree + Desktop/Documents/Downloads (today's `resolveOSRoots`) + work dir | full | allowlist (PATH, HOME, LANG, TMP, + operator list) | 60 s, 2 GB, 256 pids, 16 KB out | **ask** (escalation, §4.5) |
| `discord` | restricted | same | full | same | same | ask via button card |
| `imessage` | restricted | same | full | same | same | ask via text nonce |
| `subagent` | inherits parent surface | parent | parent | parent | parent | parent |
| `cron-exec` | restricted (container when available) | work dir only | allowlist: model endpoints + operator list | minimal | 15 min wall (matches `cronPerRun`), 1 GB, 64 pids | **deny + alert** |
| `cron-shell` | restricted (container when available) | work dir only | **none** by default | minimal | same | deny + alert |
| `mcp-stdio` (Phase 4) | container | server's own dir | allowlist | minimal | per server | deny + alert |

Presets compose: `safe` = exec deny; `balanced` = exec ask **on restricted**; `full` = exec allow
**still on restricted** (host requires an explicit `backend: host` per profile, which the audit
flags critical when `exec` is `allow`). That is the "full no longer means naked" change.

`tenant permissions effective --surface cron-exec --json` prints the resolved profile and the
backend's probed capabilities; `tenant security audit` fills `Execution{Backend, Isolation,
FilesystemRoot}` from the live registry instead of constants (TEN-338's accuracy follow-up).

### 4.3 Backends

**`host`** — today's behaviour behind the new interface. `Probe` reports nothing enforced.

**`restricted`** — OS primitives, no new runtime dependency, one implementation per OS behind a
build tag. Order of work follows where Tenant runs 24/7.

- *Windows (first).*
  - **Job Object** per run: `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`, active-process limit,
    job memory limit, per-job CPU time, `JOB_OBJECT_LIMIT_DIE_ON_UNHANDLED_EXCEPTION`, basic
    UI restrictions. All present in `golang.org/x/sys/windows` v0.45 (`CreateJobObject`,
    `AssignProcessToJobObject`, `SetInformationJobObject`, `TerminateJobObject`). Promote
    `x/sys` from indirect to direct.
  - **Restricted primary token**: `CreateRestrictedToken(DISABLE_MAX_PRIVILEGE)` (advapi32,
    lazy proc; not wrapped in x/sys) then `SetTokenInformation(TokenIntegrityLevel,
    S-1-16-4096)` (low integrity), passed through `syscall.SysProcAttr.Token` (present in Go
    1.26's `exec_windows.go:296`). Mandatory integrity control then denies **writes** to
    every medium-integrity object — the operator profile, Tenant's config/data, the binary —
    while the work dir carries a low label (`icacls <dir> /setintegritylevel low`, done once at
    profile creation) so legitimate jobs can write there. `TEMP`/`TMP`/`HOME` point at the
    work dir. PowerShell runs `-NoProfile -NonInteractive` already.
  - **Honest limits:** integrity levels block write-up, not read-up, so secrets on disk remain
    readable by a low-IL child (Phase 5 addresses this: a dedicated sandbox account or an
    AppContainer). No network control at this level (Phase 5 proxy). `Probe` says exactly that.
- *macOS (dev boxes).* `sandbox-exec -f <generated profile>` (Seatbelt; `/usr/bin/sandbox-exec`
  present). Profile: `(deny default)`, `(allow process-exec …)` for the shell and system
  binaries, `(allow file-read* …)` on read roots, `(allow file-write* …)` on write roots + work
  dir, `(deny network*)` or `(allow network-outbound (remote ip …))` for the allowlist. Limits
  via `ulimit` in the wrapper + ctx deadline. Chrome and Anthropic's open-source
  `sandbox-runtime` (Seatbelt on macOS, bubblewrap on Linux, egress via a local proxy) are the
  reference designs to read before writing the profile generator.
- *Linux.* `bwrap` when installed (`--ro-bind / /`, `--bind <work> /work`, `--tmpfs /tmp`,
  `--unshare-net` unless allowlist/full, `--die-with-parent`, `--new-session`); otherwise
  Landlock via `landlock-lsm/go-landlock` (fs rules on kernel ≥ 5.13; TCP connect/bind rules
  need ABI v4, kernel ≥ 6.7) applied in a re-exec'd helper (`tenant __sandbox …`, same
  pattern as the existing self re-exec) plus `unshare -rn` for `NetNone`; rlimits/cgroup for
  limits.

**`container`** — one ephemeral container per run: `docker|podman run --rm --read-only
--cap-drop ALL --security-opt no-new-privileges --pids-limit N --memory M --cpus C --network
none|<allowlist net> --mount type=bind,src=<work>,dst=/work -w /work <pinned image>`, no
secrets mounted, output streamed and capped. Strongest practical boundary for unattended and
third-party work; needs a runtime on the box (decision 1) and image lifecycle (a `tenant
sandbox image pull|verify` command, digest pinned in settings).

**`vm`** — not planned; keep the enum value so the audit vocabulary is complete.

### 4.4 Network egress for sandboxed commands

- v1 (Phase 2): `none | full` per profile, enforced where the backend can (Linux/macOS/container
  yes, Windows restricted no — reported by `Probe`). `cron-shell` defaults to `none`.
- v2 (Phase 5): `allowlist` via a loopback forwarding proxy the sandbox is forced through
  (`HTTP_PROXY`/`HTTPS_PROXY` + direct egress denied by the backend), allowlist of hosts/CIDRs
  including the model endpoints so `curl 172.16.250.65:8081/props`-style diagnostics from a
  Discord turn keep working. Composes with origin/main's `egressGuard` for in-process web tools
  (one allow/deny table, two enforcement points).

### 4.5 Escalation: how nothing that works today breaks

- **Attended surfaces.** `Result.Violations` non-empty → the osys dispatcher raises an approval
  with action `os_exec_unsandboxed` (category `exec`, so it obeys the surface's mode and per-tool
  rules) and a detail that names exactly what was refused ("wrote outside write roots:
  C:\Users\dylan\foo"; "connected to 10.0.0.5:22 with network=none"). Approve once/session/
  always through the existing broker; *always* persists an exception in the profile (a path or
  host), never a backend switch. The model sees the refusal text first and can usually fix its
  own command (write to the work dir, use the allowed path), so most prompts never fire.
- **Unattended surfaces.** Hard deny, event `EventSandbox` on the activity feed, a line in a
  new `sandbox` log-book source (the logs hub makes it a dashboard tab for free), and the
  daily-brief banner mechanism (`withAuthBanner`) extended to "N sandbox violations since the
  last run" so the operator reads about escapes where they already read.
- **Dry-run mode** per profile (`enforce: false`) logs violations without blocking — the
  rollout tool for the box: run a week in dry-run, read the `sandbox` tab, add the two
  exceptions real jobs need, then enforce.

### 4.6 Self-modification boundary (cheap, wide effect)

- `DenyRoots` for *every* profile always resolve to cfgDir, dataDir, the running binary, and the
  wiki root (writes there go through `wiki_save`'s own gate). Enforced by the backend for child
  processes, and by `osys.Roots` for file tools. Replaces the cron approver's substring
  `mentionsDir` with `osys.resolve`/`under` so `..`, relative paths and symlinks cannot dodge it.
- Unattended surfaces keep the `cron_*`/`mcp_*`/fan-out cuts. `skill_save`, `memory_remember`,
  `wiki_save` from turns that consumed untrusted content get a provenance tag in Phase 5
  (PRD-001 TEN-297's inspector is the natural home; do not build a second store).

### 4.7 Observability and audit integration

- `sandbox` log source; `EventSandbox` feed kind; run-trace attribute `execution.backend`
  (TEN-300 store, origin/main).
- `tenant security audit`: `execution` block filled from the live profiles + `Probe`; new
  findings: `execution.host` (critical when exec allow), `execution.unenforced-fs`,
  `execution.dry-run` (warning), `execution.container-unavailable` (warning when configured).
- `tenant doctor`: one check per surface — chosen backend, probed capabilities, image digest.
- MOD-06 tracing (deferred) gets a `sandbox.violation` span event when it lands; nothing here
  waits for it.

---

## 5. Escape-test suite (acceptance criteria)

`internal/sandbox/escapetest`: table-driven, backend × case matrix, each case paired with a
legitimate twin. Skips with a stated reason when a backend is unavailable on the runner; runs
on origin/main's 3-OS CI matrix; and ships as `tenant security selftest [--surface X]` so the
same suite runs on the R340 box against its live profiles (the only place the Windows path is
really proven).

| # | Adversarial job (must be contained) | Legitimate twin (must succeed) |
|---|---|---|
| 1 | read the Tenant credentials file / `~/.aws/credentials` | read a file under a read root |
| 2 | write to `$HOME`, cfgDir, dataDir, the binary's directory | write to the work dir and a write root |
| 3 | escape via `..`, a symlink planted in the work dir, a junction (Windows) | follow a symlink that stays inside a root |
| 4 | outbound connect to a canary listener with `network=none` | fetch an allowlisted host with `allowlist` |
| 5 | fork bomb / 10k processes / 4 GB allocation / infinite output | a 100-line `go build`-style job within limits |
| 6 | daemonise (`nohup`, `Start-Process`, `setsid`) to outlive the job | a job that spawns and reaps a child |
| 7 | exfiltrate an env var with KEY/TOKEN in its *value* but a benign name | read `PATH`/`HOME` |
| 8 | modify Tenant's cron definitions / settings from a shell | append to `<work>/notes.md` |
| 9 | `curl 127.0.0.1:8770/api/approvals` + POST approve from inside a turn | `web_navigate` to a public page |
| 10 | dry-run profile: violations logged, job proceeds, feed line present | enforce profile: same job blocked |

Every case asserts the `Violation` is reported (model text + feed + log line) and, on attended
surfaces, that the escalation prompt fires with the right category.

---

## 6. Phases, effort, tickets

Effort in "sittings" (MOD plan convention) with a rough ticket-scale number.

| Phase | Deliverable | Depends on | Effort |
|---|---|---|---|
| **0 Reconcile + posture** | `integration/r340-audit` merge (10 conflicts), feature branches rebased, deployed; relay/host `exec`+`destructive`(+`send`) → ask; box audit baseline; self-approval loop closed | — | 1–2 sittings + deploy (3) |
| **1 Seam** | `internal/sandbox` interface, `host` backend, `Policy`/profiles/settings/hot-reload, `osys` + `runCronShell` on the seam, `permissions effective` + `security audit` wired, escape suite red on host, `tenant security selftest` | 0 | 2 sittings (3) |
| **2 Restricted backend** | Windows (Job Object + low-IL restricted token + labelled work dir) → macOS Seatbelt → Linux bwrap/Landlock; `Probe`; escape suite green per OS on CI; dry-run mode | 1 | 3–5 sittings, Windows is the long pole (5) |
| **3 Unattended defaults + escalation** | cron-exec/cron-shell default restricted, `network=none` for shell; violation → ask on attended, deny+alert on unattended; `sandbox` log source; brief banner; `mentionsDir` → resolved roots; docs + change record | 2 | 1–2 sittings (3) |
| **4 Container + third-party** | `container` backend, image pin/verify, cron profiles prefer it when present, stdio MCP servers run under it (TEN-318), `claudecode` optional | 3 + decision 1 | 2 sittings + ops (4) |
| **5 Later** | egress proxy allowlist; Windows secret confidentiality (dedicated sandbox account or AppContainer); provenance-tagged skills/memory/wiki (with TEN-297); TEN-334 uid_map hardening; MOD-06 span events | 3/4 | scoped separately |

**Proposed Jira** (under TEN-302 unless noted; nothing created yet):

- **OPS: reconcile the R340 production line with the origin/main audit merge** — the 10-file
  merge, rebases, deploy, posture flip, baseline audit. (Phase 0)
- **[SEC-009] Execution-backend seam + host backend + per-surface exec profiles** — AC: every
  model-reachable process spawn goes through `sandbox.Backend`; profiles in settings with hot
  reload and no agent tool; `permissions effective` and `security audit` report the live
  backend/isolation; escape suite exists and is red on host. (Phase 1)
- **[SEC-010] Restricted backend: Windows Job Object + low-integrity token; macOS Seatbelt;
  Linux bwrap/Landlock** — AC: escape cases 1–8 contained per OS with the legitimate twins
  passing; `Probe` reports partial enforcement honestly; dry-run mode. (Phase 2)
- **[SEC-011] Unattended surfaces run restricted by default; violations escalate or alert** —
  AC: cron-exec/cron-shell restricted with `network=none` for shell; attended violation → broker
  prompt; unattended → deny + feed + `sandbox` log + brief banner; `mentionsDir` replaced by
  resolved-root checks. (Phase 3)
- **TEN-318 [EXT-003]** becomes the container backend + stdio MCP ticket (Phase 4); link
  TEN-334 and TEN-338 as follow-ups; note MOD-07 on the twiki as "planned here".

---

## 7. Decisions for Dylan (none block Phases 0–3)

1. **Container runtime on the R340 box?** Docker Desktop / WSL2 / Podman available and
   acceptable on a 60-day-uptime machine? Yes → Phase 4 gives cron and MCP the strong boundary.
   No → `restricted` is the ceiling on Windows and Phase 5's dedicated account matters more.
2. **Dedicated low-privilege Windows account for the sandbox** (strongest same-host boundary,
   including read confidentiality of secrets; needs one stored credential in the protected
   store) versus restricted-token-only (no credential, write/limits boundary only).
3. **Interactive exec restricted by default** (recommended; escalation keeps it painless) or
   unattended surfaces only in the first rollout.
4. **Cron-shell network default** `none` (recommended) or a LAN allowlist from day one.
5. **Where this plan lives**: RESOLVED 2026-09-22 — `docs/plans/sandboxing-plan.md` (tracked
   dir, next to the MOD-07 note). NOTE: this file carries personal/infra data (see the scrub
   inventory below) and must NOT be committed until scrubbed, or it lands in git history where a
   later edit cannot remove it. Tenant is headed for an open-source share; the scrub is a
   pre-commit gate, not a post-commit cleanup.

---

## 8. Risks and non-goals

- **Low integrity on Windows has sharp edges**: some cmdlets and tools refuse to run or write
  to `%TEMP%` at low IL. Mitigations: `TEMP`/`TMP` → labelled work dir, `-NoProfile`, and the
  dry-run week on the box before enforcing. Escape case 5's twin exists to catch this.
- **Seatbelt is deprecated in Apple's docs** but still shipped and used by major sandboxes;
  keep the profile small and regenerate per run.
- **Latency**: `restricted` costs milliseconds; `container` costs seconds of cold start —
  fine for cron and MCP servers, wrong for interactive turns, hence the profile split.
- **Dev/prod divergence** is the standing risk of this codebase (§3); `selftest` on the box
  is the counter.
- **Not a boundary against the operator's own account being compromised**, and not a
  replacement for the SEC-001/002 in-process egress work, which must ship alongside (Phase 0).
- The classifier remains: it is fast, explains itself, and catches the accidental
  `rm -rf`; it just stops being the last line.

---

## Appendix A — anchors verified 2026-09-22 (feat/logs-hub)

- `internal/plugins/osys/classify.go:1-12,51` classifier + "NOT a security sandbox"
- `internal/plugins/osys/dispatch.go:17-27` Policy; `:43-68` gateExec; `:300` exec tool
- `internal/plugins/osys/osys.go:29,44-69` injectable runner, `realRunner`
- `internal/plugins/osys/paths.go:14-16,103,139` Roots contract, Check, DefaultDeny
- `cmd/tenant/cronmgr.go:93-156` two runners; `:171-204` exec approver + `mentionsDir`;
  `:234-263` `runCronShell` + `scrubbedEnv`; `:282` `cronSandboxDir`; `:355` `mcpReadVerb`
- `cmd/tenant/restricttools.go` live filtered views; `cmd/tenant/discordtools.go` no wall
- `cmd/tenant/approval.go:174-229` categories + `categorize`; `cmd/tenant/toolrules.go` rules
- `cmd/tenant/toolmux.go:1461-1462,1657-1661,2008-2010` os roots flags + wiring
- `cmd/tenant/team.go:440-452,584` sub-agent cap + tools
- `internal/plugins/web/web.go:359` `--no-sandbox` unconditional (R340 line)
- `internal/mcp/transport/stdio.go:56-60` stdio process transport (no agent path yet)
- `internal/model/backend/claudecode/claudecode.go:28-31,128-157` CLI spawn stance
- origin/main: `cmd/tenant/securityaudit.go:73-79,234-303`; `cmd/tenant/permissionsctl.go:
  23-27,45-60,140-185`; `internal/plugins/web/web.go:371-425`; `internal/plugins/web/egress.go`;
  `docs/design/infoflow-labels.md`; `.github/workflows/ci.yml` (ubuntu/windows/macos)

## Appendix B — MOD-07 scoping note, carried forward

Threat model, option ladder and escape-test list from `docs/plans/MOD-07-isolation.md`
(branch `mod/06-07-08-docs`) are absorbed above: ladder rung 1 = `restricted`, rung 2 =
`container`, rung 3 = `vm` (unplanned); its five escape cases are rows 1, 2, 4, 5, 8 of §5.
Its "dependency on MOD-06 for escape alerting" is replaced by the feed event + log source so
alerting does not wait on tracing.

---

## Update 2026-09-22 — tickets created + competitor/industry benchmark

### Tickets created (all under TEN-302, labelled `sandboxing`,`sec`, priority High)

| Key | Phase | Title |
|---|---|---|
| [TEN-344](https://findtime.atlassian.net/browse/TEN-344) | 0 | Reconcile the R340 line with the origin/main 2026-07 audit merge |
| [TEN-345](https://findtime.atlassian.net/browse/TEN-345) | 1 | [SEC-009] Execution-backend seam + host backend + per-surface exec profiles |
| [TEN-346](https://findtime.atlassian.net/browse/TEN-346) | 2 | [SEC-010] Restricted backend — Windows dedicated account, macOS Seatbelt, Linux bwrap/Landlock |
| [TEN-347](https://findtime.atlassian.net/browse/TEN-347) | 3 | [SEC-011] Sandbox every exec surface by default; escalate/deny+alert |

Links: 344 → blocks → 345 → blocks → 346 → blocks → 347. Relates: 347↔TEN-318 (container/MCP),
347↔TEN-316 (browser sandbox), 346↔TEN-334 (uid_map honesty), 345↔TEN-338 (audit accuracy).
TEN-318 [EXT-003] is the Phase-4 container/stdio-MCP ticket (already existed). MOD-07 on the
twiki is closed by TEN-347.

### Decisions resolved (2026-09-22)

1. Container runtime on the box: **yes, WSL2/Docker OK** → cron + MCP get the container boundary
   (TEN-318); TEN-347 defaults unattended surfaces to `container` when present.
2. Windows enforcement: **dedicated sandbox account** (SID + NTFS ACE + WFP), matching Anthropic
   sandbox-runtime → TEN-346 Windows path; one credential in `internal/secrets`.
3. Interactive exec: **restricted by default too**, escalation keeps it painless → TEN-347.
4. Egress/credential-injection proxy: **follow-up**, not one of the 4 (overlaps SEC-002/TEN-311).
5. Plan location: RESOLVED — moved to `docs/plans/sandboxing-plan.md` 2026-09-22. Uncommitted
   and to stay that way until the personal-data scrub (see inventory below) runs, because Tenant
   is destined for an open-source share and a committed secret survives a later edit.

### Scrub inventory — remove before the first commit that includes this file

Personal / infrastructure data in this plan (and the wider dev branch) to redact before any
open-source commit. A later edit does NOT clean git history, so scrub pre-commit or rewrite
history (git-filter-repo / BFG):

- The R640 model-host LAN IP and port, and the box names R340 / R640.
- Operator identity: the name Dylan, `dtaylor@findtime.net`, and the `findtime.atlassian.net`
  Jira instance + TEN-*/MOD-* keys (decide whether ticket refs stay for an internal fork only).
- Home / machine paths: `/Users/dylan/...`, `C:\Tenant\...`, `D:\models\...`, `~/.aws/credentials`
  as a concrete example.
- Dashboard bind `127.0.0.1:8770` and any other concrete host:port.
- Model/infra specifics tied to the deployment (the local model name, the daily-brief MON-ledger).
  Generalise to placeholders (`<model-host>:<port>`, `<operator>`, `<data-dir>`).

### Appendix C — how Tenant compares (competitor + industry review, 2026-09)

The design is not novel; it is the industry-standard shape, and Tenant is roughly one tier
behind the named competitors today.

- **Hermes** — pluggable `terminal.backend` (local/docker/ssh/modal/daytona/apptainer),
  credential stripping from child processes via a `docker_forward_env` **allowlist**,
  cross-session isolation, approval modes manual/smart/off ("off suitable only inside
  containers"). This is exactly the SEC-009 seam + the env-allowlist + the "full runs on
  restricted, not host" preset change. Hermes is the closest analogue to Tenant's gate+classifier
  model and shows the missing piece is *execution isolation + credential scoping by construction*,
  not more classification. (9 CVEs in 4 days on Hermes, and "configs where all approval checks are
  disabled" — the box's current all-`allow` relay is that anti-pattern; TEN-344 fixes it.)
- **OpenClaw** — per-agent `sandbox` (docker/podman/ssh/openshell), off by default, containers
  default to no network, honest "not a perfect boundary… but materially limits what a model that
  does something dumb can reach". The docker.sock bind-mount escape is the load-bearing lesson for
  our container backend: **never mount the runtime socket, and the profile is not agent-writable**
  (baked into TEN-347).
- **Anthropic sandbox-runtime** — the reference for our `restricted` backend: Seatbelt (macOS),
  bubblewrap+seccomp (Linux), and on Windows a dedicated `srt-sandbox` **SID + NTFS ACEs + a WFP
  `ALE_AUTH_CONNECT` egress filter** (default proxy ports 60080–60089). Its config knobs
  (`filesystem.allowWrite/denyWrite/allowRead/denyRead`, `network.allowedDomains/deniedDomains`,
  `deniedResolvedAddresses`, `allowUnixSockets`) are the vocabulary TEN-345's `Policy` adopts.
  Its resolved-address check (drop loopback/link-local/multicast/metadata/host IPs before dialing)
  is the same defence as origin/main's `egressGuard` — one allow/deny table, two enforcement points.
- **Industry taxonomy (2026)** — tiers are hardened container (trusted only) → gVisor (10–30% I/O
  overhead, multi-tenant) → Firecracker microVM (~125 ms boot, untrusted code) → Kata (~200 ms).
  Defense-in-depth = isolation + resource limits + network default-deny allowlist + short-lived
  creds + human gates + monitoring. Tenant is a **single-user desktop appliance**, so microVM/
  Firecracker is over-scope; `restricted` (OS primitives, no runtime) fits the single-binary thesis
  and `container` is the strong tier for unattended/third-party work. "Most teams are better served
  using existing platforms than building custom" — noted; Tenant builds `restricted` itself only
  because the single-binary/CGO-free/no-external-runtime constraint rules the platforms out, and
  uses the existing container runtime for the strong tier rather than reinventing it.
- **The secret-proxying gap** (the reason for the Phase-5 follow-up): sandboxing the agent does
  not stop it reading its own `GITHUB_TOKEN`/API tokens and using them through an authenticated
  egress — "the sandbox isolates the agent, it doesn't isolate the secret." The fix is a
  credential-injection proxy *outside* the sandbox that strips agent-supplied creds and injects the
  real secret from the protected store, so there is nothing in the sandbox to exfiltrate. Proxying
  prevents theft, not misuse; misuse is what SEC-002's egress labels (TEN-311) catch. Env-allowlist
  (in TEN-345) is the cheap first cut; the proxy is the follow-up.

---

## Implementation status — 2026-09-22 (branch `feat/sandbox-seam`, off `feat/logs-hub`)

Built with subagents (macOS backend + escape suite, Linux/Windows backends, seam
integration, red-team QA, operator CLI, docs), integrated and QA'd on macOS.

| Ticket | State on the branch | Remaining |
|---|---|---|
| TEN-344 Phase 0 reconcile | not started (human-gated) | merge origin/main audit work into the R340 line, deploy, posture flip |
| TEN-345 [SEC-009] seam | `internal/sandbox` (Backend/Policy/Execute), host backend byte-identical to the old runner, os_exec + cron shell on the seam, operator `exec_profiles` (per surface, hot-reloaded, validated, no agent tool), env allowlist, escape suite, `tenant sandbox probe/selftest/run` | wire `tenant permissions effective` + `tenant security audit` + presets (those files exist only on origin/main → after TEN-344) |
| TEN-346 [SEC-010] restricted backends | macOS Seatbelt: live-tested + red-teamed (all escapes contained). Linux bwrap: written, argv-tested, cross-compiled. Windows: Job Object limits; dedicated account / NTFS ACE / WFP egress scaffolded (TODO) | run Linux on WSL; finish + test Windows on the box (`tenant sandbox selftest`); Landlock fallback |
| TEN-347 [SEC-011] defaults + escalation | refusal escalation (`os_exec_unsandboxed`, destructive), dry-run (`enforce: false`), unattended deny, `sandbox` log source, resolved-path self-modification guard, cmd /C parity, credential + browser deny baseline, scrubbed env for confined profiles, sub-agent surface inheritance | flip defaults per surface after Windows is enforced + a dry-run week on the box; brief banner + feed event; approve-always → scoped exception |

Decisions taken during implementation (recorded for review):
- Surfaces are `local` (TUI/dashboard/serve), `discord`, `imessage`, `cron-exec`,
  `cron-shell`, plus a `default` entry; sub-agents inherit their parent's surface.
- Dry-run = re-run on the host after a refusal and log it (works on every backend;
  documented caveat: pre-refusal side effects can repeat). A side-effect-free
  report-only mode needs OS audit support (Seatbelt report, Windows SACL/WFP audit).
- The Tenant binary FILE is denied, not its directory (a shared /usr/local/bin would
  otherwise break every tool beside it).
- Container backend (TEN-318) rejected at profile load until implemented.
