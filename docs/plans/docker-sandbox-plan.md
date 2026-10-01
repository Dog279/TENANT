# Docker sandbox: drop bubblewrap, one hardened-container tier on every OS

**Status:** proposal, 2026-09-30. **Direction:** remove bubblewrap for now, and plan Docker
across the OSes, for Tenant's general open-source releases. Docker is better known and friendlier to homelab and open-source users
than a niche tool, and a hardened container is the industry-standard sandbox.
**Builds on:** [sandboxing-plan.md](sandboxing-plan.md). The seam, the policy vocabulary,
escalation and the escape suite all stay. This plan builds the `container` tier that plan
sketched in §4.3 (its Phase 4), and removes its Linux and Windows `restricted` backends.
**Hardening:** [docker-sandbox-hardening.md](docker-sandbox-hardening.md) has what a live probe of
§3.3 on Windows found, and the epic that Phase 1 depends on.

## 0. TL;DR

- **Phase 0: remove bubblewrap.** One PR, mostly deletion (about 2.5k lines). It covers:
  - the Linux bubblewrap backend;
  - the Windows backend, which runs bubblewrap inside WSL2, and its sh-in-WSL plumbing;
  - the CI job that installs bubblewrap.

  Linux and Windows run commands on the host, which is what they do today on any box without
  WSL or bubblewrap. macOS keeps Seatbelt (decision 1).
- **Phase 1: the `container` backend.** The seam already reserves it: `sandbox.For("container")`
  returns "not implemented", and there is an `IsolationContainer` level. It is one backend for
  all three OSes, driven through the **`docker` CLI**. That means no new Go dependency, and it
  works with Docker Engine, Docker Desktop, OrbStack, Colima and Podman's docker-compatible CLI.
- **Each command gets a fresh, hardened container.** The root filesystem is read-only and the
  container runs as a non-root user. All capabilities are dropped, and `no-new-privileges` and
  Docker's default seccomp filter apply. There is no network by default, and pid, memory and
  CPU are capped. Only the work dir is mounted: never the Docker socket, never a credential
  store, never Tenant's own dirs.
- **Phases 2–4:**
  - Phase 2: surfaces and defaults, and setup docs for each OS.
  - Phase 3: local MCP servers in containers (the TEN-318 follow-up).
  - Phase 4: an egress allowlist, plus a proxy that injects credentials.

## 1. Why Docker, and what it costs

**For it**
- **Known by everyone who runs a homelab.** Docker Engine is usually already on a Linux box,
  and every desktop OS has a one-installer path to it.
- **One implementation instead of three OS-specific ones:** Seatbelt, bubblewrap, and
  bubblewrap-in-WSL. Each of those had its own attribution quirks: the Linux dry-run problem,
  and Windows programs through interop.
- **Denied by default through mounts.** Only what is mounted exists inside the container. That
  is stronger than bubblewrap's masking of deny roots, where a read of a masked root failed with
  nothing attributed (TEN-436).
- **It's the tier the peers use:**
  - Hermes (`terminal.backend: docker`, the `docker_forward_env` allowlist Tenant already adopted);
  - OpenClaw (per-agent docker sandbox);
  - OpenHands and devcontainers.

**Costs, stated up front**
- **A runtime to install.** Docker is the only new dependency. Tenant stays a single binary, and
  without Docker it runs as today: the host backend, and `container` reported as unavailable.
- **Cold start per command.** Roughly 0.3–1 s on Linux and 0.5–2 s under Docker Desktop.
  Phase 1 measures it on Linux, macOS and Windows. It's fine for cron and MCP servers, and
  noticeable in a chatty interactive turn.
- **The model writes Linux `sh` inside the container on every OS**, and it is told so.
  Windows-native programs need the escalation to the host, as they did under WSL.
- **Windows runs Linux containers on WSL2 or Hyper-V underneath.** Docker Desktop hides that.
  A Tenant running as a system service needs an engine its account can reach (§6).

## 2. Phase 0: remove bubblewrap (done: TEN-444)

**Deleted:**
- the Linux backend: `restricted_linux.go`, its bubblewrap argv and red-team suite;
- the Windows backend: `wsl_windows.go`, which ran bubblewrap inside WSL2;
- helpers only they used: `bwrapLimitArgv`, `headBuffer`, `WindowsProgramRefusal`, and the
  "outside twin", `sandbox.Outside`;
- in `cmd/tenant`, all the plumbing for running Linux `sh` in WSL on Windows: the shell notes
  and the `os_exec` description override, the WSL path mapping, and the cron-sh notices in
  doctor and at startup;
- the CI step that installed bubblewrap.

**Kept:**
- The Job Object limit mapping moved to an untagged `joblimits.go`.
- The four `restrictedOrSkip` tests stay. They skip on Linux and Windows and still cover the
  Seatbelt paths on macOS.

**What changes behaviour**

| What | Before | After |
|---|---|---|
| Built-in default, Linux | host | host (unchanged) |
| Built-in default, Windows | restricted (WSL), dry run | host: PowerShell, `cmd /C` for cron |
| Built-in default, macOS | Seatbelt, dry run | Seatbelt, dry run (decision 1) |
| An `exec_profiles` entry naming `restricted` on Linux or Windows | runs under bubblewrap | stays valid. An enforcing entry, which is the default for one that names a backend, fails closed and says "restricted is macOS-only; use host, or the container backend once it ships". A dry-run entry runs on the host. |
| `mcp_servers` with the default `restricted` sandbox, on Linux or Windows | runs under bubblewrap | doesn't start: the message names `"sandbox": "host"` to run it unconfined. Phase 3 brings `container`. |

**Jira.**
- TEN-436, TEN-437 and TEN-439 close as Won't Do (bubblewrap only).
- TEN-438, capping output as it arrives, stays: host and container both need it.

## 3. Phase 1: the `container` backend

### 3.1 Driver
- It shells out to the `docker` CLI through `os/exec`. `container.cli` picks the binary:
  `docker`, `podman` or a path.
- It does **not** use the Engine SDK: the dependency tree is heavy, and Windows named pipes need
  extra code. `DOCKER_HOST` and docker contexts work unchanged, so Docker Desktop, a remote
  engine over TLS, Colima and OrbStack need no code.

### 3.2 One container per command
Each command gets its own `docker run --rm`: the strongest isolation between commands, and the
simplest lifecycle. A warm container per session, driven with `docker exec`, only comes in
(Phase 2) if the latency measured in Phase 1 hurts interactive turns.

### 3.3 The run
```text
docker run --rm -i --name tenant-sbx-<run> --label tenant.sandbox=1 --label tenant.trace=<trace>
  --read-only --tmpfs /tmp:rw,nosuid,nodev,size=256m
  --mount type=bind,src=<WorkDir>,dst=<WorkDir as mapped> -w <WorkDir as mapped>
  --mount type=bind,src=<ReadRoot>,dst=<mapped>,readonly   (per ReadRoot)
  --mount type=bind,src=<WriteRoot>,dst=<mapped>           (per WriteRoot / exec_exception)
  --user 65532:65532 --cap-drop ALL --security-opt no-new-privileges
  --pids-limit <MaxPIDs|256> --memory <MemoryMB|1g> --memory-swap <same> --cpus <CPU|2>
  --network none                  (network full: the default bridge; allowlist: Phase 4)
  --init --log-driver none
  -e NAME …                       (the env allowlist, by name; values come from Tenant's env)
  -e HOME=<WorkDir as mapped> -e TMPDIR=/tmp
  <image>@sha256:<digest> sh -c '<command>'
```
**Never:**
- the runtime socket;
- `--privileged` or added capabilities;
- host network, pid or ipc;
- device mounts;
- an unpinned image.

An argv test holds every flag in place.

### 3.4 Filesystem semantics
The container denies by default. Only mounts exist inside it, and an empty `ReadRoots` means
nothing extra: not "the whole host", as it does on the host and under Seatbelt. `DenyRoots`
(Tenant's config and data dirs, its binary, the credential stores) hold because they are never
mounted. A grant that contains one is refused at load, and `Probe` says so.

### 3.5 Paths
- **Linux and macOS:** each root is mounted at its own path, so commands, tool output and the
  file tools all name the same paths. Docker Desktop on macOS shares `/Users` by default.
- **Windows:** `C:\x\y` is mounted at `/mnt/c/x/y`, the WSL convention the model already knows.
  The `os_exec` description names the mapping.

### 3.6 Image
- `tenant sandbox image build` builds `tenant-sandbox:<version>` from a Dockerfile embedded in
  the binary. No registry is needed, and the build uses the machine's own architecture.
- The base is `debian:bookworm-slim`, pinned by digest, with a non-root user and `bash`,
  `coreutils`, `curl`, `git`, `jq`, `python3` and `ca-certificates`.
- The backend runs only digest-pinned images.
- `exec_profiles.<surface>.image` names your own image (homelab: bring the tools you use), and
  `tenant sandbox image verify` checks the pin.

### 3.7 Lifecycle
- **Timeouts:** the wall-clock limit runs `docker kill <name>`, because killing the CLI does not
  stop the container.
- **Leftovers:** at start, Tenant sweeps leftover `label=tenant.sandbox` containers. This is the
  TEN-401 lesson: the orphaned headless Chrome.
- **Output:** it is capped as it arrives (TEN-438).

### 3.8 Probe, doctor and audit
- **Probe** runs `docker version`, and checks the image is present and its runtime.
- **Capabilities:**
  - isolation `container`;
  - full enforcement of filesystem, network (`none` / `full`), limits and env;
  - shell `sh`.
- **Doctor** says why when the backend is unavailable (no CLI, no daemon, image not built).
- **Audit:** `execution.isolation=container`.

### 3.9 Refusals
A refusal is attributed from the error. EROFS, or ENOENT on a host path outside the mounts,
counts as filesystem. A name-resolution or unreachable error with `--network none` counts as
network. `Execute` then escalates on an attended surface (`os_exec_unsandboxed`) or denies on
an unattended one.

There is **no dry-run default** for `container`. A dry run cannot attribute every refusal: that
is the Linux lesson. Instead the model is told where it runs and what is mounted, the way
devcontainer agents work.

### 3.10 Tests
- **Argv golden tests** on all three OSes: every hardening flag, no socket, the path mapping
  per OS.
- **A live escape suite** under `TENANT_SANDBOX_LIVE=1`, reusing `escapecore`. Its attacks, each
  with a legitimate twin that must still succeed:
  - read `~/.ssh`;
  - write outside the work dir;
  - reach the network;
  - fork and memory bombs;
  - reach `docker.sock`;
  - escalate through setuid;
  - poke `/proc/sysrq-trigger`.
- **CI:** it runs on `ubuntu-latest`, which has Docker preinstalled, in place of the bubblewrap
  step. GitHub's macOS runners have no Docker, and its Windows runners have no Linux containers.
  Those two are covered by the argv tests and by `tenant sandbox selftest` on each host.

## 4. Phase 2: surfaces and defaults

- `exec_profiles` accepts `backend: container`, with `image`, `cpus` and the existing limits.
- **Defaults (decision 2):**
  - cron (`cron-exec`, `cron-shell`) uses `container` when Docker is up;
  - interactive surfaces stay on the host;
  - an unattended surface set to `container` when Docker is down fails the job and says why
    (fail closed).
- The `os_exec` description says what the model is running in: a Linux container, what is
  mounted, and whether it has a network.
- A warm container per session, only if the Phase 1 latency numbers call for it.

## 5. Phase 3: local MCP servers in containers (TEN-318 follow-up)

- `mcp_servers.<name>.sandbox: container`, with `image` (for example `node:22-slim@sha256:…` or
  `python:3.12-slim@sha256:…`), runs one long-lived `docker run -i --rm` per server. That fits
  `sandbox.Start`'s `Process`.
- The network defaults to none. A server that installs itself at start (`npx`, `uvx`) needs
  `network: full` or a pre-built image; the doc recommends the image.

## 6. Supported setups

Tenant finds the engine the way the `docker` CLI does (`DOCKER_HOST`, docker contexts), so any
of these work without code changes. The docs cover each one, and `tenant sandbox selftest`
checks the result on the host.

| OS | Setups | Notes |
|---|---|---|
| Linux | Docker Engine (rootless recommended), or Podman | The common homelab case. With rootful Docker, the user Tenant runs as must be in the `docker` group. |
| macOS | Docker Desktop, OrbStack or Colima | Seatbelt stays the dependency-free option (decision 1). |
| Windows | Docker Desktop (WSL2 backend); dockerd in a WSL2 distro; or a remote engine through `DOCKER_HOST` | Linux containers need WSL2 or Hyper-V underneath. |

**As a system service** (systemd, launchd or a Windows service), the service's account needs
access to the engine:
- On Linux, the `docker` group, or rootless Docker under that user.
- On Windows, Docker Desktop's engine lives in a signed-in user's session. A headless service
  points `DOCKER_HOST` at a daemon that runs without one.

The service docs gain a short section on this.

## 7. Phase 4: egress allowlist and secrets

- `network: allowlist`: the container joins an internal Docker network whose only way out is a
  proxy container that allows the listed hosts.
- The same proxy can inject credentials. It strips any the agent supplies and adds the real
  secret from Tenant's store, so there is nothing inside the sandbox to exfiltrate. This closes
  the gap sandboxing-plan.md calls "the sandbox isolates the agent, not the secret".

## 8. Decisions

1. **macOS Seatbelt:** keep it as the dependency-free tier on macOS (recommended: it works,
   costs nothing and runs macOS tools), or drop it so Docker is the only sandbox.
2. **Default once `container` ships:** container for cron when Docker is up (recommended), or
   opt-in everywhere.
3. **Image:** build locally from the embedded Dockerfile (recommended), or publish to GHCR once
   the repo is public.
4. **Podman:** support it through `container.cli: podman`. It costs nothing and is rootless by
   default; recommended.

## 9. Size and tickets

| Phase | Size | Ticket |
|---|---|---|
| 0 Remove bubblewrap | one PR, mostly deletion | new, under TEN-302 |
| 1 `container` backend + image + tests | 2–3 sittings | new |
| 2 Surfaces, defaults, setup docs per OS | 1–2 sittings | new |
| 3 MCP servers in containers | 1–2 sittings | TEN-318 follow-up |
| 4 Egress allowlist + credential proxy | 2 sittings | new (overlaps SEC-002 / TEN-311) |

**Not in scope:**
- a VM tier;
- Windows containers;
- gVisor as the default. It's available as an optional `runtime: runsc` on Linux hosts that
  have it installed.
