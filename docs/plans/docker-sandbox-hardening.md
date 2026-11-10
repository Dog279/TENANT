# Docker sandbox hardening: the seams around the container, Windows first

**Status:** proposal, 2026-09-30.

**Scope:** the `container` backend of [docker-sandbox-plan.md](docker-sandbox-plan.md).
- Windows 11 with Docker Desktop (WSL2 backend) comes first.
- The macOS and Linux builds pick up wave 3.

**Evidence** comes from two sources:
- A live probe of the plan's §3.3 argv, driven from Go on the target machine: Docker Desktop 4.92.0, Engine 29.8.0, runc 1.5.1, WSL kernel 6.18.33.2.
- A research pass over the removed bubblewrap backends, OWASP, container and Docker Desktop CVEs, the CIS Docker Benchmark, and peer agent sandboxes.

The full report and its notes are local, untracked research: `reports/Docker agent sandbox hardening.md` and `research_notes/Docker agent sandbox hardening/`. The probe harness is in `tenant-work/dockerprobe`.

## 0. TL;DR

- **The argv is a sound inner boundary.** On this box it contained all 21 attacks, and all 13 legitimate twins passed. Nothing below adds a flag for its own sake.
- **The seams are Tenant code around it.** The probe exposed five:
  - killing `docker.exe` orphans the container;
  - a missing pinned image makes the argv contact Docker Hub;
  - `HOME` in the work dir persists dotfiles onto the host;
  - a comma in a path breaks `--mount`;
  - `full` networking reaches every Windows `127.0.0.1` service, Tenant's unauthenticated MCP gateway included.

  The current code adds five traps of its own (§1.3).
- **Behind the container sits Docker Desktop's VM.** It has:
  - no user namespace without ECI;
  - no AppArmor or SELinux;
  - a WSL kernel shared with every distro;
  - an Engine API that is root in the VM, with every drive mounted.

  Its patch level is part of the boundary, so versions gate the run.
- **The work is 22 tickets in three waves:**
  - wave 1 (7 tickets) blocks Phase 1;
  - wave 2 (9) blocks making `container` the cron default;
  - wave 3 (6) ships with the Linux and macOS builds and Phases 3–4.
- **Most fixes are cross-platform code** in `internal/sandbox` and `cmd/tenant`. The Windows-only pieces are:
  - mount-source canonicalisation;
  - reading the Desktop version from the registry;
  - WSL checks;
  - the shell used on escalation and for cron.

## 1. What the probe settled

### 1.1 Held (keep, and pin in the golden test)

| Check | Result |
|---|---|
| Identity | uid/gid 65532. CapPrm, CapEff, CapBnd and CapAmb are all 0. NoNewPrivs 1, Seccomp 2 |
| Namespaces | `unshare -Ur` and `unshare -n` fail with EPERM |
| Filesystem | Only the mounts exist. `~/.ssh` and a canary beside the work dir are ENOENT. Writes to the rootfs, a read-only root and the dir beside the work dir all fail with EROFS; the host is untouched |
| Kernel surfaces | `/sys/fs/cgroup` and `/proc/sysrq-trigger` are read-only. kcore, keys, timer_list, sched_debug, acpi, scsi and firmware are masked. There is no `docker.sock`, `/run/host-services`, `/run/desktop` or TTY |
| `--network none` | Only `lo`, no DNS; the internet and 192.168.65.254 are unreachable |
| Limits | A fork bomb was held at 256 PIDs. The engine still answered in 156 ms, and `docker kill` ended it in 368 ms. The memory bomb was OOM-killed (137) within 1 s. `/tmp` hit ENOSPC at 256m |
| Env | Only the `-e` names cross, plus the image's own PATH, HOME, HOSTNAME, PWD and TMPDIR |
| Streams | `--log-driver none` still streams attached output (8 MiB at 16 MB/s). stdin, the exit status, and separate stdout/stderr all work |
| Setuid | With `no-new-privileges`, a setuid-root binary stays uid 65532. The control run kept `--cap-drop ALL` but dropped NNP: it reached euid 0 and read `/etc/shadow`. **NNP is load-bearing.** |
| Latency | 526 ms median cold start (p90 542), 568 ms with a bridge, 186 ms for a warm `docker exec`, 154 ms for `docker version` |

### 1.2 Exposed

| Finding | Ticket |
|---|---|
| Killing `docker.exe` (Go's CommandContext uses TerminateProcess) leaves the container running: "Up 6 seconds" three seconds later | W1-4 |
| When the pinned image isn't local, the §3.3 argv contacts Docker Hub: "pull access denied" after 1.1 s. With `--pull never` it fails offline in 160 ms | W1-2 |
| `HOME=<WorkDir>` writes dotfiles and caches into the host work dir, and the next "fresh" container reads them | W2-2 |
| A comma in a path breaks an unquoted `--mount` (exit 125). CSV-quoting the fields fixes it | W1-2 |
| `full` (the default bridge) reaches services bound to Windows `127.0.0.1` through `host.docker.internal` / 192.168.65.254. That includes the MCP SSE gateway on 127.0.0.1:8765, which has no auth (TEN-451). `--add-host` can't stop it, because the IP answers directly | W1-5, W2-4, W2-9 |
| A Windows program exits 127 "not found", and §3.9 has no attribution for it | W1-6, W2-5 |
| Docker Desktop resolves a junction in a mount *source* and follows symlinks in `/mnt/host/...` sources. `/mnt/c/...` as a source lands on the default WSL distro's disk. The §3.5 mapping is right only as a *destination* | W1-3 |
| Existing host files show `0:0` mode 777 inside, so POSIX modes protect nothing. The work dir is case-insensitive inside | W1-3 |
| Symlinks the container creates land on NTFS as WSL reparse points that the host can't open (Lstat reports them irregular) | W2-3 |
| `--cpus` is a rate (`cpu.max 200000 100000`), so there is no CPU-time budget | W2-1 |

### 1.3 Traps in the current code (checked at 160ec8f)

| Trap | Where | Ticket |
|---|---|---|
| A profile that names a backend resolves to `Enforce=true, Network=NetInherit` | `cmd/tenant/execprofiles.go:320-325` | W1-5 |
| Escalation re-runs `Command.Shell` on `Host().Run`, so on Windows a model-written `sh` line runs under PowerShell | `internal/sandbox/sandbox.go:393-405` | W1-6 |
| `os_exec` resolves its profile with an empty work dir | `cmd/tenant/execprofiles.go:544` | W1-3 |
| Cron's shell is `cmd /C` on Windows, whatever the backend | `cmd/tenant/execprofiles.go:879-884` | W2-6 |
| `safeEnv` forwards the host's `PATH`, `HOME`, `SHELL`, `TMPDIR`, `TEMP` and `TMP`, and `safeEnvWindows` adds the Windows set. None of these is right inside a Linux container | `cmd/tenant/execprofiles.go:424-431` | W2-5 |

## 2. Overlap with the bubblewrap backends

All 63 controls of the removed Linux and WSL backends (spec.go, process.go, wsl_windows.go at 6c9db0a) were audited against §3.3:

| Verdict | Count |
|---|---|
| Covered | 30 |
| Stronger | 16 |
| Weaker | 2 |
| Mixed | 1 |
| Missing | 13 |
| New | 1 |

Docker inverts the old model. Only what is mounted exists, so deny roots, `/run`, the host's `/tmp` and the Windows drives are absent rather than masked. The WSL case-folding bypass of the masks goes away with them.

| bubblewrap / WSL control | Docker counterpart | Verdict |
|---|---|---|
| `--die-with-parent` | `--rm` plus a kill and sweep | **Weaker** (W1-4) |
| `--new-session` (TIOCSTI) | `-i` with no `-t` | Covered, if `-t` is forbidden (W1-2) |
| An unprivileged user namespace | `--user 65532`, and no userns on Desktop without ECI | **Mixed**: stronger inside, but an escape lands as root in the VM (W3-1) |
| No seccomp (TEN-437 closed) | Docker's default allowlist profile | Stronger; add a delta (W2-8) |
| `--ro-bind / /` plus masks | Nothing but the mounts | Stronger |
| `ulimit -v` | `--memory`, on the whole tree | Stronger |
| `ulimit -t`, a CPU-time budget | `--cpus`, a rate | **Missing** (W2-1) |
| `headBuffer` (TEN-438) | "capped as it arrives", but the code was deleted | **Missing** (W1-4) |
| The `program` refusal and the outside twin | "Windows-native programs need the escalation" | **Missing** (W1-6, W2-5) |
| `allowlist` mapped to `--unshare-net` (fails closed) | "allowlist: Phase 4" | **Missing** (W1-5) |
| Env allowlist; Set values kept off command lines | `-e NAME` | Covered. Host-only names are unstated (W2-5) |

Lessons that carry over:
- **Test deny roots at the Windows path.** Commit 7aa5735 fixed a WSL bug where every deny root was stat'ed at its translated path and silently skipped.
- **Make stray writes fail loudly.** The old backend remounted its layers read-only so a stray write failed with EROFS instead of landing in a throwaway layer; `--read-only` does the same.
- **No dry-run default for `container`.** TEN-436 showed that a dry run is lossless only if every refusal is attributed, and §3.9 already makes this choice.
- **Mount sources can't be pinned by fd.** Flatpak's CVE-2024-42472 was fixed by fd-based binds, and Docker has no equivalent, so canonicalise on every run (W1-3).

## 3. The boundary on Windows

- **Inner layer: the container.** Everything in §1.1.
- **Outer layer: Docker Desktop's VM.**
  - No user namespace: ECI is Business-tier and is off here (`uid_map` reads `0 0 4294967295`).
  - No LSM: the security options are only `seccomp,profile=builtin` and `cgroupns`.
  - The WSL kernel is shared with every distro and is patched through WSL updates, not Desktop updates.
- **The Engine API.** The named pipes belong to the signed-in user: the user has read/write, and SYSTEM and Administrators have full control. API access means root in the VM plus read/write on every shared drive. So the host user, and every process running as that user, is trusted. The sandbox protects the host from the container, not from the user's other processes. SECURITY.md should say this (D8).
- **The residual risk** is a kernel bug that needs no capabilities and no namespaces, of the Dirty Pipe class. It lands as root in the VM, and root in the VM can mount the user's drives.
  - CVE-2026-53362 may be a live one. It has been on CISA's KEV list since 2026-08-27. The kernel CNA describes an unprivileged UDPv6 trigger, while Red Hat says it needs namespace creation.
  - This box's WSL kernel, 6.18.33.2, predates the fix. `wsl --update` should bring it to 6.18.40 or later; `wsl --version` confirms.

  Three mitigations:
  - the version floors below;
  - the seccomp delta (W2-8), which drops the 32-bit ABIs the Copy Fail `socketcall` bypass used;
  - a stronger tier later (W3-6).

**Version floors** (Desktop floor: D1)

| Component | Refuse below | Warn below | Read it from |
|---|---|---|---|
| Docker Desktop | 4.44.3 | 4.76.0, then latest (4.93.0) | the `DisplayVersion` value of the `Docker Desktop` uninstall key (HKCU, then HKLM), cross-checked against `docker version` Server.Platform. Never use `docker desktop version`, which prints the CLI plugin's version (v0.4.4) |
| WSL | 2.7.10 | latest (2.7.14) | `wsl --version`, whose output is UTF-16 |
| WSL kernel | one with the CVE-2026-53362 fix (upstream 6.18.38; Microsoft's first is `linux-msft-wsl-6.18.40.1`) | latest | the kernel line of `wsl --version`, cross-checked with `uname -r` in a container |
| Docker Engine | 29.4.3 | latest 29.8.x | `docker version` |
| runc | 1.2.8 / 1.3.3 / 1.4.0, by release line | latest | `docker version` |
| containerd | 1.6.38 / 1.7.27 / 2.0.4 | latest | `docker version` |
| NVIDIA Container Toolkit | never pass `--gpus` or a runtime other than runc, and never forward `NVIDIA_*` variables | 1.17.8, if GPUs are ever added | the engine's registered runtimes (`nvidia` is registered on this box) |

## 4. Wave 1: ship-blockers for Phase 1 (Windows first)

The Phase 1 backend PR does not merge without all seven.

### W1-1 Fail closed on a pinned, patched engine (H1, H2, H3, H11)
- **No way out of the container:**
  - `container` never falls back to the host;
  - `enforce: false` is rejected at load for `container`;
  - no unsandboxed retry is selectable by the model, and escalation goes only through the Escalator, on attended surfaces.
- **Clean launch:** start `docker` from an absolute path resolved once, with an environment Tenant builds. Strip every ambient `DOCKER_*`, and pass an explicit `--context` or `-H` for the vetted endpoint (`npipe:////./pipe/dockerDesktopLinuxEngine` on Desktop).
- **Endpoint checks:** refuse `tcp://` without TLS, and refuse to run while `127.0.0.1:2375` answers. A remote or headless engine connects only over `ssh://` or mutual TLS.
- **Gates:** enforce the floors in §3, and require the `seccomp` security option.
- **Probe:**
  - records the Desktop, WSL, Engine, runc and kernel versions, ECI (from `uid_map`), the LSM, the seccomp profile and the image store;
  - caches only success, and re-probes with back-off, because Docker Desktop starts after Tenant at login (the TEN-439 lesson);
  - reports Limits as `partial` until W2-1 lands.
- **Done when:**
  - unit tests cover endpoint resolution (precedence of `-H`, `DOCKER_HOST` and context; `tcp://` refused);
  - unit tests cover version parsing: the registry value, `docker version` JSON, and UTF-16 `wsl --version`;
  - every refusal reason has a doctor row.

### W1-2 One argv schema, held by a golden test (H6)
- **A typed builder** with no pass-through arguments from config.
- **Always:**
  - `--pull never`;
  - `--mount` only, never `-v`, with every field CSV-quoted;
  - the command as one argv element after the image (`sh -c <line>`);
  - `no-new-privileges` pinned by name;
  - `--runtime runc` (`runsc` only through W3-2).
- **Never:** `-t`.
- **Rejected:** control characters, quotes and newlines in mount values.
- **Env:** names must match `[A-Za-z_][A-Za-z0-9_]*` and are never `DOCKER_*` or `NVIDIA_*`. Under the NVIDIA runtime, `NVIDIA_VISIBLE_DEVICES` requests GPUs by itself. Values travel only as `-e NAME` from the environment W1-1 builds, so they never appear on a command line.
- **Forbidden, and the golden test fails on any of them:**
  - `--privileged` and `--cap-add`;
  - `unconfined` seccomp, AppArmor or systempaths, and `label=disable`;
  - `host` or `container:` for net, pid, ipc, uts, userns or cgroupns;
  - `--cgroup-parent`, `--device*`, `--gpus` and any other `--runtime`;
  - `-p`/`-P`, `--restart`, `--use-api-socket` and `--oom-kill-disable`;
  - a bind of any socket or pipe, and `docker.sock` anywhere.
- **Done when:**
  - argv golden tests exist per OS, with paths containing a comma, a space and a quote;
  - a forbidden-flag table test passes;
  - the mount-value quoting is fuzz-tested.

### W1-3 Mount sources: derived, canonical, and re-checked (H5)
- **Where the work dir comes from:** only session or operator config, never the model's cwd or Tenant's own. `os_exec`'s empty work dir gets a fresh scratch dir that Tenant owns, or the run is refused.
- **On Windows:**
  - Pass native canonical paths only, never `/mnt/c/...` or `/mnt/host/...` as a source.
  - Resolve the final path, and reject a reparse point in any component: junction, symlink or mount point.
  - Reject UNC and device paths, alternate data streams, 8.3 short names, and trailing dots or spaces.
  - Compare deny roots case-insensitively.
- **On Linux and macOS:** use realpath, and reject symlinked components under writable roots.
- **Everywhere:**
  - re-check the sources immediately before each `docker run`;
  - fail closed when canonicalisation fails;
  - refuse, at load, any grant that contains a deny root (§3.4), tested at the Windows path.
- **Done when:**
  - fixtures cover a junction, a symlink, UNC, an ADS, an 8.3 name and a trailing dot (junctions need no elevation, so the tests run on any Windows box);
  - selftest shows that a junction given as a source is refused.

### W1-4 Reap every container, and cap output as it arrives (H4, H8, TEN-438)
- **Kill path:** `cmd.Cancel` runs `docker kill <name>`, then waits. It fires on timeout, cancellation, the output cap and shutdown.
- **Labels:** `tenant.sandbox`, `tenant.trace` and a per-instance `tenant.instance`.
- **Startup sweep:** kill this instance's leftovers, and never another live instance's.
- **In-container deadline:** `timeout -s KILL <wall+grace>`, so an orphan dies even if Tenant does.
- **Output:** rebuild `headBuffer` for both streams, and kill on the cap.
- **Done when:**
  - selftest shows no container left 2 s after a cancel (the probe's orphan, inverted);
  - 100 MB of output stops at the cap, and the container is gone;
  - the sweep is tested.

### W1-5 Map network modes fail-closed, and treat `full` as host access (H7)
- **Until W2-9 lands:** `NetInherit` and `NetAllowlist` both map to `--network none`. This is the bubblewrap precedent, where spec.go mapped allowlist to `--unshare-net`.
- **`full`** can be set two ways:
  - by the operator on a profile;
  - on a cron job at `cron_add` time, where the gated approval shows it.

  The model can't select it outside one of those approvals.
  - Cron jobs may use it (D2), and W2-9 narrows it per run.
  - It is refused while the MCP SSE gateway is enabled.
  - Doctor and the audit warn that `full` reaches the host's loopback, the LAN and tailnet, and other containers.
- **Done when:**
  - profile-resolution tests show that a profile naming the backend resolves to `none`;
  - selftest records loopback as reachable under `full` and unreachable under `none`.

### W1-6 Attribution and escalation that can't misfire (H9, H10)
- **Attribute conservatively:**
  - *filesystem:* only EROFS, or ENOENT on a path that exists on the host but isn't mounted (map it back through the mount table);
  - *network:* only under `none`;
  - *limit, never escalated:* exit 137 / OOMKilled, "Cannot fork", the timeout and the output cap;
  - *program:* on Windows, exit 127 for a known Windows program (finished in W2-5);
  - *unattributed:* everything else, which fails closed.
- **Escalation keeps the language (D3):**
  - it escalates within the tier first: the same `sh` line in a container, with the one refused path granted or with network for that one command;
  - only a program refusal goes to the host, run verbatim as the operator saw it;
  - an `sh` line never reaches PowerShell.
- **The prompt:**
  - shows the exact interpreter and command, untruncated, with control characters escaped;
  - carries a risk summary that Tenant writes, not the model.
- **The approval:** it is bound to a hash of interpreter, argv, mounts, network and image digest, and it expires.
- **Done when:**
  - attribution tests run on the captured stderr of the probe cases;
  - an escalation test shows no `sh` line ever reaching PowerShell;
  - an approval whose hash doesn't match is refused.

### W1-7 Make `tenant sandbox selftest` the Windows test bed (H12)
- **Why it matters:** GitHub's Windows runners can't run Linux containers, so on Windows selftest is the security test of record.
- **Build it from the probe harness** (`tenant-work/dockerprobe`):
  - retarget escapecore case (b) at the host's real home, since `HOME` is no longer the host's;
  - add a hook that maps host paths to container paths;
  - keep fixtures out of `/tmp`.
- **Live cases to add:**
  - a junction as a mount source, and a comma in a path;
  - the CLI-kill orphan;
  - host loopback under `full`;
  - `cmd.exe` exiting 127, and OOM attribution;
  - the setuid control (NNP) and the exact env set;
  - 32-bit `socketcall(AF_ALG)`, after W2-8;
  - a planted Windows executable (WSL interop, untested).
- **Done when:**
  - selftest on this box reproduces the probe table with every attack contained;
  - any failed case fails the command;
  - the release checklist says to run it on the Windows box.

## 5. Wave 2: before `container` becomes the cron default (Phase 2)

- **W2-1 Resource gaps (H13).**
  - Add `--ulimit core=0` and `nofile=1024:4096`. Never use `nproc`: it counts per user, and every sandbox runs as 65532.
  - Set the CPU budget (D5).
  - Cap concurrent sandboxes, and the launches per turn and per job.
  - Watch the work dir's growth.
  - Document that `/tmp` and `/dev/shm` count toward memory.
- **W2-2 HOME on a per-run tmpfs, and protected paths (H14, H16).**
  - Mount `--tmpfs /home/sandbox:size=64m,mode=0700` with HOME there (D4).
  - Add nested read-only binds over `.git/hooks`, `.git/config`, `.vscode`, `.idea`, `.claude`, `.cursor`, `.mcp.json` and Tenant's project config. Create any that are missing on the host first.
  - Refuse a work dir laid out as a bare repo.
  - Verify nested read-only binds on 9p; they are untested.
- **W2-3 The host treats the work dir as hostile (H17).**
  - File tools and host-side git never follow links the container created. On Windows those are irregular reparse points; on Linux and macOS they are live symlinks.
  - Fail closed when canonicalisation fails.
  - Neutralise `core.fsmonitor` and `core.hooksPath`, and never execute work-dir artifacts.
  - This is mandatory before the Linux and macOS builds.
- **W2-4 Take `full` off the default bridge, and authenticate the MCP gateway (H18, TEN-451).**
  - Run `full` on a Tenant-owned `tenant-sbx` bridge with `com.docker.network.bridge.enable_icc=false`.
  - Add token auth to the SSE gateway, or keep refusing `full` while it is on.
- **W2-5 Finish attribution, exceptions and env (H19, H20).**
  - Add the `program` refusal kind.
  - Add ENOENT parsing and container-to-host mapping in `refusedPath` and `exceptionScope`. A reverse-mapped folder must pass W1-3 before "always" is offered.
  - For `container`, drop the host-only names (PATH, HOME, SHELL, TMPDIR, TEMP, TMP and the Windows set).
  - Forward the log-join IDs, and warn on secret-looking names.
  - Selftest asserts the exact env.
- **W2-6 Cron under `container` on Windows (H21).**
  - Container jobs run `sh -c`.
  - Jobs in cmd syntax stay on the host by declaration, or fail with a message.
  - A circuit breaker pauses a surface after 5 consecutive refusals or timeouts.
- **W2-7 Audit, kill switch and quarantine (H22, H23).**
  - Record every run in the hash-chained Security log. The record holds:
    - trace, surface, and argv with env values redacted;
    - image digest, mounts and their modes, network, limits;
    - approval id, policy version and times;
    - exit, any OOM or timeout kill, output bytes and whether the cap hit;
    - attribution, and a digest of changed files on critical paths.
  - Strip control characters before output reaches logs or the UI (LLM10:2026).
  - Add `tenant sandbox stop-all`.
  - One setting disables both `container` and escalation.
  - An opt-in quarantine keeps the container (no `--rm`) and snapshots the work-dir diff.
- **W2-8 The seccomp delta and the image supply chain (H15, H24).**
  - *Seccomp:*
    - start from a pinned moby/profiles release;
    - drop `SCMP_ARCH_X86` and `SCMP_ARCH_X32` (`SCMP_ARCH_ARM` on arm64), and optionally deny ptrace and `process_vm_*`;
    - keep EPERM;
    - regenerate it every release and record its hash;
    - verify that the Windows CLI reads the profile file on the client side.
  - *Image:*
    - base it on `debian:trixie-slim@sha256:…`, since bookworm left regular support on 2026-07-12, or on a Docker Hardened Images Debian base (D6);
    - strip setuid and setgid bits, and use `--no-install-recommends`;
    - record the image ID for whichever store is in use;
    - scan with Trivy or Grype in CI and in `image verify`;
    - rebuild on a cadence, with a doctor warning on image age;
    - verify base signatures with cosign or Notation, since Docker Content Trust retires and notary.docker.io shuts down on 2026-12-08.
- **W2-9 A network scope per run (D2).**
  - *Declared with the work:* a profile, or a cron job at `cron_add` time, lists `host:port` or `cidr:port` destinations. The gated approval shows the list. `config.CronJob` gains the field.
  - *From an escalation:* a D3 network escalation grants one destination, for one command, when the refusal names it.
  - *`full` means the internet only.* These are denied unless a destination on the list names them:
    - the host gateway (192.168.65.254 on Desktop) and 192.168.65.0/24;
    - loopback, link-local and metadata addresses;
    - RFC 1918 and 100.64/10 (the tailnet);
    - other containers.

    A local model server is listed like `host:11434`.
  - *No general DNS.* Tenant resolves declared names at run start through the egress guard (`internal/plugins/web/egress.go`), then pins them into the container with `--add-host`. This also closes the DNS channel the peer incidents used.
  - *Enforcement,* chosen in a spike on Docker Desktop:
    - L4: a per-run network namespace whose egress rules are set by a helper container Tenant owns, before the command container joins it. The command container keeps zero capabilities. This works for any protocol, down to the port.
    - L7: the Phase 4 proxy (W3-5) on an `--internal` network. Rules can name hosts, but only clients that use a proxy work.
  - *The golden-test exception:* the only `container:` network mode W1-2 allows is the backend's own per-run helper, named by the backend, never by config.
  - *Audit:* the run's record (W2-7) holds the scope and every denied connection.
  - *Done when:* selftest shows a scoped run reaching its listed destination, while an unlisted port, the host gateway and DNS all fail.

## 6. Wave 3: the Linux and macOS builds, and Phases 3–4

- **W3-1 A user namespace wherever the host offers one (H25).**
  - Detect ECI.
  - Support Podman machine on Windows: rootless by default, on the same API pipe.
  - On Linux, support rootless Docker or Podman, with an ownership strategy (`--userns=keep-id`).
  - Avoid userns-remap, which Engine 29 can't combine with the containerd store.
- **W3-2 Linux host specifics and gVisor (H26, H28).**
  - Detect SELinux and relabel with `-v …:z`, or use Podman. Record AppArmor.
  - Scan declared roots for socket files. Never mount `/run`, `/var/run` or `XDG_RUNTIME_DIR`: that is the TEN-437 residual.
  - Attribute EACCES from a uid mismatch.
  - Check cgroup v2 delegation.
  - Keep Podman in parity, and above its floor.
  - Offer `runtime: runsc`, with its overhead on git and pip measured.
- **W3-3 macOS specifics (H27).**
  - Verify the Desktop VM's LSM state.
  - W2-3 is mandatory here, because symlinks are live on the host.
  - Share less than all of `/Users`.
  - Seatbelt stays the dependency-free tier, and selftest compares it with `container` side by side.
- **W3-4 MCP servers in containers, Phase 3 (H29).**
  - Use pre-built, pinned images, with no `npx` or `uvx` installs over `full`.
  - Keep trust-on-first-use hashes of tool definitions, and re-approve on any change.
  - Consent shows the exact argv and digest.
  - No writable mounts by default.
  - Add limits, a restart budget, a `container` entry in `StartLimits`, and a `tenant.mcp` label in the sweep.
- **W3-5 The egress proxy, Phase 4 (H30).**
  - Use an `--internal` network whose only exit and only resolver is a proxy container.
  - An empty allowlist denies everything.
  - Hostnames must be canonical: reject NUL, `%`, CR/LF, trailing dots and IP literals.
  - Resolve, then deny loopback, 169.254/16, RFC 1918, 100.64/10 (the tailnet), 192.168.65.0/24 and the host gateway.
  - Prefer narrow names over multi-tenant domains, and document domain fronting.
  - The policy core is `internal/plugins/web/egress.go` (TEN-307).
  - The proxy injects credentials, configured by the operator only.
- **W3-6 Keep a stronger tier in view (H31).**
  - A warm container is allowed only if it is keyed on the full policy hash and resets state for each command. It also brings `docker exec` back into scope for runc bugs such as CVE-2019-5736.
  - Benchmark a microVM tier against Docker Sandboxes (`sbx`) on the Windows Hypervisor Platform.

## 7. Decisions

| # | Decision | Recommended | Alternative |
|---|---|---|---|
| D1 | Docker Desktop floor | **Decided 2026-09-30.** Refuse below 4.44.3 (CVE-2025-9074). Warn below 4.76.0, and below the latest release. The CVE spot-check removed two of the reasons for refusing below 4.76.0 | — |
| D2 | Network for cron and other unattended work | **Decided 2026-09-30.** `full` is allowed, cron included, given the work cron dispatches. W2-9 scopes each run to the ports and destinations the work declares | — |
| D3 | Escalation shape | **Decided 2026-09-30.** Within the tier, as the least friction: one path granted, or network for one command, still in a container. The host only for a program refusal, run verbatim | — |
| D4 | HOME | **Decided 2026-11-10: the recommended option.** Its own `--tmpfs /home/sandbox` (64m, 0700, owned by the sandbox uid) | `/tmp/home` inside the `/tmp` tmpfs |
| D5 | CPU budget | **Decided 2026-11-10: the recommended option.** A per-process `--ulimit cpu` plus the wall clock, with Limits reported as `partial` | Document the wall × `--cpus` bound only |
| D6 | Base image | **Decided 2026-11-10: the recommended option.** `debian:trixie-slim`, pinned and built locally | A Docker Hardened Images Debian base |
| D7 | Engines on Windows | **Decided 2026-11-10: the recommended option.** Docker Desktop in wave 1, Podman machine in wave 3 | Both now |
| D8 | Trust statement | **Decided 2026-11-10: the recommended option.** SECURITY.md says the host user and their processes are trusted | Leave it implicit |

## 8. Tickets

| Ticket | Title | Requirements | Size |
|---|---|---|---|
| W1-1 | Fail closed on a pinned, patched engine | H1 H2 H3 H11 | L |
| W1-2 | One argv schema, held by a golden test | H6 | M |
| W1-3 | Mount sources: derived, canonical, re-checked | H5 | M |
| W1-4 | Reap every container; cap output as it arrives | H4 H8 | M |
| W1-5 | Network modes fail closed; `full` is host access | H7 | S |
| W1-6 | Attribution and escalation that can't misfire | H9 H10 | L |
| W1-7 | `tenant sandbox selftest` as the Windows test bed | H12 | M |
| W2-1 | Resource gaps | H13 | S |
| W2-2 | Per-run HOME tmpfs; protected paths | H14 H16 | M |
| W2-3 | Host treats the work dir as hostile | H17 | M |
| W2-4 | `full` off the default bridge; MCP gateway auth | H18 | S |
| W2-5 | Attribution, exceptions and env, finished | H19 H20 | M |
| W2-6 | Cron under `container` on Windows | H21 | S |
| W2-7 | Audit records, kill switch, quarantine | H22 H23 | M |
| W2-8 | Seccomp delta; image supply chain | H15 H24 | M |
| W2-9 | A network scope per run | H7, and H30 brought forward | M |
| W3-1 | User namespaces where offered | H25 | M |
| W3-2 | Linux host specifics; gVisor | H26 H28 | M |
| W3-3 | macOS specifics | H27 | S |
| W3-4 | MCP servers in containers (Phase 3) | H29 | M |
| W3-5 | Egress proxy (Phase 4) | H30 | L |
| W3-6 | A stronger tier in view | H31 | S |

**Jira.**
- File this as a workstream under TEN-302, with one ticket per row.
- The Phase 1 backend ticket, not filed yet, is blocked by W1-1 through W1-7.
- Existing tickets fold in:
  - TEN-438 into W1-4;
  - TEN-451 into W2-4;
  - the TEN-437 residual into W3-2.

**Not in scope:**
- Windows containers;
- requiring ECI, which is Business-tier only (it is detected and recorded instead);
- gVisor as the default;
- building a VM tier (W3-6 only benchmarks one).

## 9. Status, 2026-11-10

Where wave 1 stands after PR #35 (Phase 1), PR #60 (wave-1 close), PR #63 (sweep deadline, gateway token) and the PR that adds this section. Waves 2 and 3 have not started. "Live" means it needs a machine with a working Docker engine and the built image (`TENANT_SANDBOX_LIVE=1 go test ./internal/sandbox/ -run TestContainerLive`, or `tenant sandbox selftest --backend container`).

### What is still owed

| Ticket | Item | State | Where |
|---|---|---|---|
| W1-1 | A doctor row per refusal reason | done in part: one `container sandbox` row whose fix is the one for the reason found | `cmd/tenant/doctor.go` `checkContainerSandbox`; `sandbox.Unavailable` |
| W1-2 TEN-456 | Check the ticket's own description against the code before closing | open | nothing known outstanding |
| W1-3 TEN-457 | The live junction case | open (live) | `TestContainerLive_JunctionRefusedCommaRuns` |
| W1-4 TEN-458 | The live cap and sweep cases | open (live) | `TestContainerLive_OutputCapKills`, `TestContainerLive_SweepLeftovers` |
| W1-4 | The terminal app sets no instance id and runs no sweep | open, needs a decision | serve and a terminal app can share one data dir (the data lock is shared), so a sweep by install id at one's start would remove the other's running containers |
| W1-6 | Attribution tests on captured stderr from real runs | open (live) | needs the captures |
| W1-6 | The live refusal-past-the-cap case | open (live) | `TestContainerLive_RefusalPastOutputCap` |
| W1-7 TEN-461 | Setuid/NNP control, 32-bit `socketcall(AF_ALG)`, a planted Windows executable | open | needs image or harness work (W2-8 for `socketcall`) |

### Closed by the PR that adds this section

| Ticket | Item | Where |
|---|---|---|
| W1-1 | The kernel floor against the kernel that is running, not only the one `wsl --version` prints | `container_wsl.go` `checkRunningKernel` |
| W1-1 | A container run before first use: liveness, the uid map (ECI or a user namespace), the kernel a container sees | `container_engine.go` `containerCheck`; `Available` |
| W1-1 | A fix for every refusal reason | `sandbox.Unavailable`, `fix*` constants |
| W1-3 | A container profile whose grant overlaps a deny root is refused at load | `sandbox.GrantOverlapsDeny`; `execprofiles.go` `containerGrantError` |
| W1-4 | Commands are labelled with the install's id, so the sweep finds a previous run's containers | `sandbox.go` `For` |
| W1-1 | An operator setting for the endpoint: `container.endpoint` and `container.tls_cert_dir` in `config.json`, applied to every command | `internal/config` `Container`; `cmd/tenant/wiring.go` `containerOptions` |
| W1-1 | A `tcp://` endpoint is launched with `--tlsverify` and its three files, or refused. It passed vetting on `DOCKER_TLS_VERIFY` alone and was then launched with `-H` only and a stripped environment, which is plain HTTP | `container_engine.go` `tlsArgs` |
| W1-6 | The approval is bound to the run: a digest before the prompt and again before the run, with a ticket that expires; a mismatch runs nothing. Held in `Execute`, so the approval broker carries no ticket | `sandbox.EscalationBinder`, `approvalStillCovers`; `container.go` `GrantedRunHash` |

### A live run on Windows, 2026-11-10

Docker Desktop (engine 29.9.3, WSL 3.0.5, installed kernel 6.18.58, running kernel 6.18.54) was stuck in `starting` after a WSL update: `docker version`, `docker info` and an unfiltered `docker ps` answered, but `docker create`, `docker run` and `docker ps --filter label=…` never returned. The probe called the backend available, and the live tests then failed by timing out with empty output (`Identity`, `ReadOnlyRootAndSecret`, `LoopbackUnderNoneVsFull`, `OutputCapKills`; `SweepLeftovers` hung the suite). The two tests that only need a container to be gone passed. That run is why the container check exists. **No live case has yet been seen to pass on this machine**: they need a restarted Docker Desktop.

### A live run on Windows, 2026-11-10, on a working engine

After Docker Desktop was quit and started again (engine 29.9.3, running kernel 6.18.58.1), on `main` at the merge of #66:

- `tenant sandbox image build` built the image from the pinned `debian:trixie-slim` (image id `sha256:a282e8fc…f43da8`, Debian 13.7).
- `TENANT_SANDBOX_LIVE=1 go test ./internal/sandbox/ -run TestContainerLive`: 13 of 13 pass, the two new cases among them (`HomeIsATmpfsOfItsOwn`, `CPULimitStopsARunawayProcess`).
- `tenant sandbox selftest --backend container`: 4 of 4 escape cases contained, every legitimate twin succeeded.
- A new live case for the approved re-run, `TestContainerLive_ApprovedReRunStartsByImageID`, **failed and found a defect**: when the refused folder was already granted read-only, the re-run mounted it twice (read-only and writable) and the engine refused the container with "Duplicate mount point". The refusal stood, so nothing ran unconfined, but approving a write into a read-only grant had never worked on a real engine; the fake CLI accepts any argv. `specFor` now mounts each destination once, by its widest grant. With that, 14 of 14 live cases pass.

Not run on a real engine: `tenant sandbox stop-all` (not run against this machine's engine) and a `tcp://` endpoint with TLS (no such engine here).

### Nested read-only binds on 9p, tried by hand on 2026-11-10

W2-2 asks to verify nested read-only binds on 9p before building them. Tried with `docker run` and the backend's own flags (read-only root, uid 65532, no capabilities, no network) on Docker Desktop for Windows, engine 29.9.3, with a work dir on NTFS:

- **They work.** With read-only binds over `.git/hooks`, `.git/config`, `.vscode` and `.mcp.json` nested inside the writable work mount, a write to each is refused (`Read-only file system`), the rest of the work dir stays writable, and a bind's own mount point can be neither renamed nor removed (`Device or resource busy`). A single file (`.git/config`, `.mcp.json`) can be a bind.
- **The §5 list is not enough.** With only those binds, `mv .git .git2` succeeded, and the rename was there on the host afterwards. A command can move the repository aside and make a new `.git` with hooks and a config of its own, which is exactly what the binds are for.
- **`.git` as a mount point closes that.** With `.git` bound onto itself, writable, under the same two read-only binds, renaming or removing `.git` fails (`Device or resource busy`) while `.git/objects` and the rest stay writable, so a commit inside the container still works. `rm -rf .git` then deletes what is writable inside it (objects, refs, `HEAD`) and stops at the mount points: the repository can be destroyed, as anything in a writable work dir can, but not given hooks.
- **Not covered by any of this:** a repository the command creates elsewhere in the work dir (`git init sub` worked, and `sub/.git/hooks` is the command's), submodule git dirs under `.git/modules/*/`, a work dir whose `.git` is a file, and a protected folder that does not exist yet (the command can create it).

So the build is three layers per repository (the work dir, `.git` onto itself, then the read-only binds), plus answers to the open questions in the table below. W2-3 stays necessary whatever is bound: the host must not run what it finds in a work dir.

### Wave 2, checked against the code on 2026-11-10

Some of wave 2 was already in the code when its tickets were still To Do. Rows that say "live" were run on a real engine on 2026-11-10 (the section above); the rest have unit tests only.

| Ticket | Item | State |
|---|---|---|
| W2-1 TEN-462 | `--ulimit core=0` and `nofile=1024:4096` | in the code (`buildRunArgv`) |
| W2-1 | The CPU budget | done (D5): a profile's `cpu_seconds` is `--ulimit cpu=N:N`, a cap per process (`buildRunArgv`); the run record's limits say `cpu=Ns/process`. Passed live (`TestContainerLive_CPULimitStopsARunawayProcess`) |
| W2-1 | A cap on concurrent sandboxes and on launches per turn and per job; watching the work dir's growth | open: the numbers, and whether a launch over the cap waits or is refused, are not decided |
| W2-2 TEN-463 | HOME on its own tmpfs | done (D4): `--tmpfs /home/sandbox:rw,nosuid,nodev,size=64m,mode=0700,uid=65532,gid=65532` and `HOME=/home/sandbox` (`buildRunArgv`, `sandboxHome`); the image creates the folder. Passed live (`TestContainerLive_HomeIsATmpfsOfItsOwn`) |
| W2-2 | Refusing a bare-repo work dir | done: `bareRepoLayout` in `specFor` refuses a work dir or write root that is itself a git directory (HEAD beside `objects` and `refs`); read-only it is allowed |
| W2-2 | Read-only binds over `.git/hooks`, `.git/config` and the editor and agent folders | not built; tried by hand on a real engine on 2026-11-10 (below). Nested read-only binds work on 9p, but the list in §5 is not enough on its own: `.git` has to be a mount point too. Open: whether to create missing folders in the operator's work dir, and what to do for a work dir with no `.git`, a `.git` file (a worktree), submodule git dirs and repos the command creates itself |
| W2-3 TEN-464 | The host treats the work dir as hostile | audited, part open. Already so: the file tools resolve every link, a dangling one by hand, and check the place it points at against the roots (`internal/plugins/osys/paths.go` `resolve`), so a link a container made does not move a host tool outside its roots; Tenant itself runs no git on the host, so there is no `core.fsmonitor` or `core.hooksPath` for it to neutralise. `resolve` now refuses a path it cannot relate to its resolved ancestor (it returned the path unresolved). Open: the check and the open are two steps, so a link swapped in between is followed; reparse points a container leaves on NTFS are not tried on a live engine |
| W2-4 TEN-465 | Token auth on the SSE gateway | done (PR #63) |
| W2-4 TEN-465 | `full` on a Tenant-owned bridge with inter-container traffic off | done: `dockerNetwork(NetFull)` is `tenant-sbx`; `ensureNetwork` creates it on first use (`enable_icc=false`), checks it, and refuses a network of that name that is open (`container_network.go`). Passed live (`TestContainerLive_FullIsOffTheDefaultBridge`): on the default bridge a sandboxed command fetched another container's page; on `tenant-sbx` it reaches neither a default-bridge container nor a neighbour on its own network. The host's loopback through `host.docker.internal` is still reachable under `full` (W2-9) |
| W2-5 TEN-466 | The `program` refusal kind; ENOENT mapped back through the mounts | in the code (wave 1) |
| W2-5 | Host-only names dropped from a container's environment; the log-join ids forwarded | in the code (`envNamesFor`) |
| W2-5 TEN-466 | A warning on secret-looking names | done: `secretLookingEnv` on a surface's `env_pass`, shown by `tenant sandbox probe` (and `env_pass_secret_looking` in its JSON) and `tenant doctor`. A hint: the name is still passed |
| W2-5 | Selftest asserting the exact env | done: the live test `TestContainerLive_ExactEnv` passes |
| W2-6 TEN-467 | Container jobs run `sh -c` | in the code |
| W2-6 | Jobs in cmd syntax stay on the host by declaration; a circuit breaker after 5 refusals or timeouts | open: how a paused surface is shown and resumed is not decided |
| W2-7 TEN-468 | `tenant sandbox stop-all` | done with the PR that adds this table (`sandbox.StopAllContainers`) |
| W2-7 | The run record in the Security log | done in part with the PR after that one: record `4201` holds the image and its id, mounts and modes, network, limits, output bytes, whether the cap hit, and the time (`sandbox.Confinement`, `confinementFields`); `4203` says where a re-run ran. The names of the environment variables passed in were added after (`Confinement.Env`). Not in the record yet: the policy version, an OOM kill as its own field, and a digest of changed files |
| W2-7 TEN-468 | Control characters kept from the logs and the UI | done: the log files quote them (`internal/logs/entry.go`), the dashboard shows hidden characters as markers (`control.VisibleText`), and the terminal app now does too: `termText` on every agent, sub-agent, system and research line, `promptText` on an approval prompt, and `termText`/`styledText` on what a slash command prints (`internal/tui/safetext.go`) |
| W2-7 | One setting that disables `container` and escalation; quarantine | open: the setting's name and place are not decided |
| W2-8 TEN-469 | The image on trixie | done (D6): `baseImageRef` is `debian:trixie-slim` pinned to the index digest of 2026-11-04. Built live, and the live suite and the selftest pass on the new image |
| W2-8 | The seccomp delta; the image scanned and signed | open |
| W2-9 TEN-470 | A network scope per run | open; needs the enforcement spike on Docker Desktop |

The approval binding, as it stands: the re-run is started by the backend from the description it hashes (`PinnedEscalator`, `containerBackend.RunGrantedAs`). The spec is built once, its digest is compared with the approved one, and that spec is what runs, with the image named by the id in the digest. This closed the earlier limit, where the second digest was taken before the run and the run resolved its mounts and the image tag once more. What is left: the engine itself resolves a mount source when it starts the container, so a folder replaced by a link in that instant is followed by the engine. Passed live (`TestContainerLive_ApprovedReRunStartsByImageID`), after the duplicate-mount fix above.

Decisions D4 to D8 were taken on 2026-11-10, each as the recommended option (§7). D4, D5, D6 and D8 are in the code and in SECURITY.md; D7 changes nothing now (Podman on Windows stays in wave 3). No decision is open. Still undecided, and not numbered in §6: the name and place of the setting that disables `container` and escalation, what a launch over the concurrency cap does, how a paused surface is shown and resumed, and whether the read-only binds create missing folders in the operator's work dir.
