# Change: container sandbox hardening, wave 1 close-out and part of wave 2 (TEN-453)

**Pull requests:** #64 (`feat/sandbox-wave1-probe-and-load-checks`), #65 (`feat/sandbox-stop-all`)
and #66 (`feat/sandbox-run-record`), stacked in that order. **Written:** 2026-11-10.
**Tickets:** TEN-455, TEN-457, TEN-458, TEN-460 (wave 1) and TEN-462, TEN-463, TEN-464, TEN-466,
TEN-468, TEN-469 (wave 2), all in part or in full as listed below. The plan and its status table
are [docker-sandbox-hardening.md](../plans/docker-sandbox-hardening.md) (§7 decisions, §9 status).
The backend itself is described in [docker-sandbox-container.md](docker-sandbox-container.md).

**Update, later on 2026-11-10: it has now run on a real engine.** Docker Desktop was quit and
started again after the merge. The image builds on trixie, 14 of 14 live cases pass and the
selftest contains 4 of 4. The first live run found one defect, fixed in a follow-up pull
request: an approved re-run of a write into a folder granted read-only mounted that folder twice,
and the engine refused the container. See "Verified on a real engine" below. The paragraph that
follows describes the state at the time of the merge.

**Status at the merge.** Everything here passes `go build ./...`, `go vet ./...` and
`go test ./...` on Windows, and #64's commits passed CI on Ubuntu, Windows and macOS. **None of it
has run against an engine that starts containers.** On the development machine Docker Desktop
(engine 29.9.3, WSL 3.0.5) was stuck in `starting` for the whole of this work: `docker version`,
`docker info` and `docker images` answered, `docker create`, `docker run` and a filtered
`docker ps` never returned. The container tests here use a fake `docker` CLI (the test binary
re-executed). The live cases exist (`TENANT_SANDBOX_LIVE=1 go test ./internal/sandbox/ -run
TestContainerLive`) and are the first thing to run on a working engine, after
`tenant sandbox image build`. See "Not verified" below for what that leaves open.

## Why

Three things drove this work.

1. A live run on Windows found that the backend called itself available while the engine could
   not start anything, so every command waited out its whole time limit, and `tenant serve` hung
   at start-up on the container sweep.
2. An audit of the wave-1 tickets against the code found items that were ticked but not done
   (a `tcp://` endpoint launched without TLS, the sweep never matching its own containers) and
   wave-2 items that were already in the code while their tickets said To Do.
3. A review of what the operator is shown found that the terminal app drew a model's and a
   tool's text, and the command in an approval prompt, with its control characters.

## What changed

### The backend knows whether it can run a command (TEN-455, TEN-458)

- **A container is started before first use.** `Available` now runs one check container through
  the same hardened arguments, with a 20-second limit (`containerCheck`, cached for 10 minutes,
  retried with back-off). An engine that answers but starts nothing is reported as that, and the
  surface's commands are refused with the reason instead of timing out one by one. The check
  reads what only a container shows: the uid map (a user namespace or ECI) and the running kernel.
- **The kernel floor is checked against the running kernel**, not only the installed one
  (`checkRunningKernel`): after a WSL update the VM keeps the old kernel until it restarts.
- **Every refusal has a fix.** `sandbox.Unavailable{Err, Fix}` carries the one thing to do
  (install Docker, start it, update it, build the image, restart a stuck engine).
  `tenant doctor` has a "container sandbox" row; `tenant sandbox run` and
  `selftest --backend container` are gated on the same check.
- **Engine calls cannot hang Tenant.** Every engine-management call is held to 15 seconds and the
  start-up sweep to 20 (merged earlier in #63); containers are matched by label on the client
  side because a filtered `docker ps` was the call that hung.
- **The sweep finds its own containers.** `sandbox.For("container")` built a backend without the
  install's id, so the label the sweep looked for was never on the containers. It now uses the
  defaults set at start-up (`SetContainerDefaults`).

### The engine endpoint is a setting, and TLS is really used (TEN-455)

- `config.json` gains `container.endpoint` and `container.tls_cert_dir`.
- **Fixed:** a `tcp://` endpoint passed the TLS check on `DOCKER_TLS_VERIFY` alone and was then
  launched with `-H` only, in an environment stripped of that variable, which is plain HTTP. A
  `tcp://` endpoint is now always launched with `--tlsverify` and its three files, or refused.

### A profile's grants are checked when the settings load (TEN-457)

- A `container` profile whose `read_roots` or `write_roots` contain, equal or sit inside a folder a
  container may never mount (Tenant's config or data dir, its binary, a credential store, the
  entry's own `deny_roots`) is skipped at load with the grant and the folder named
  (`GrantOverlapsDeny`, `containerGrantError`). A container sees exactly its mounts; it cannot
  mount a folder and hide part of it.
- **Fixed:** the overlap rule compared a folder that does not exist yet without resolving the
  links and short names on the way to it (`/var` on macOS, `RUNNER~1` on Windows), so it missed
  the overlap. `canonForCompare` now resolves through the nearest ancestor that exists. CI on
  macOS and Windows found this.
- **A writable folder that is itself a git directory is refused** (`bareRepoLayout`): a bare
  repository's `hooks` and `config` would be the command's to write, and git runs both on the
  host the next time it is used there. Read-only it is allowed.

### An approval covers the run it was asked for (TEN-460)

- Before it asks, `Execute` takes a digest of the re-run: the command, every mount, the network
  and the id the image tag resolves to (`EscalationBinder`, `GrantedRunHash`). After the approval
  it checks the digest again, within a time limit; a difference runs nothing and the refusal says
  to ask again.
- **The re-run is started from the description that was hashed** (`PinnedEscalator`,
  `RunGrantedAs`). The spec is built once, its digest is compared with the approved one, and that
  spec runs, with the image named by the id in the digest and never by its tag. Before this the
  run resolved its mounts and the tag once more after the last check.
- Left: the engine itself resolves a mount source when it starts the container.

### What a container is held to: decisions D4, D5, D6 (TEN-462, TEN-463, TEN-469)

Decisions D4 to D8 were taken on 2026-11-10, each as the plan's recommended option.

- **D4, HOME.** `HOME` was the work dir, so dotfiles, caches and history stayed on the host for
  the next "fresh" container and for host tools. It is now `/home/sandbox`, a tmpfs of the
  container's own: `--tmpfs /home/sandbox:rw,nosuid,nodev,size=64m,mode=0700,uid=65532,gid=65532`.
  The image creates the folder.
- **D5, CPU.** A profile's `cpu_seconds` becomes `--ulimit cpu=N:N`, a kernel limit on each
  process. It stops one runaway process. A command that spreads its work over many processes is
  bounded by the wall clock and `--cpus 2`, so the backend keeps reporting Limits as `partial`.
  With no `cpu_seconds` set nothing is added.
- **D6, base image.** `debian:trixie-slim`, pinned to the index digest published 2026-11-04
  (`sha256:a72b4433…63b6b2`). bookworm left regular security support on 2026-07-12. **Operators
  must run `tenant sandbox image build` again**; the backend never pulls or rebuilds.
- **D7.** Docker Desktop is the engine on Windows now; Podman stays in wave 3. No code.
- **D8.** SECURITY.md states that the local user account and every process running as it is
  trusted: the sandbox protects the host from a command, not Tenant or the engine from them.

### Stopping, and the record (TEN-468)

- **`tenant sandbox stop-all`** removes every Tenant sandbox container on the engine, running or
  not, without a running hub (`StopAllContainers`).
- **The Security log says what boundary a command had.** A container run's record (`4201`) holds
  the image and the id its tag resolved to, every mount and its mode, the network, the caps, the
  names (never the values) of the host's environment variables passed in, the bytes written,
  whether the output cap was hit, and the time taken (`sandbox.Confinement`).
- **Corrected:** the record of an approved re-run (`4203`) said the command "ran outside" the
  sandbox when it had stayed in a container with one more grant. It now says where it ran.
- **`env_pass` names that look like credentials are pointed out** by `tenant sandbox probe`
  (`env_pass_secret_looking` in its JSON) and `tenant doctor` (TEN-466). A hint: the name is
  still passed, and a secret under a plain name is not noticed.

### What the operator is shown (TEN-468)

- **The terminal app no longer draws outside text as it came.** Every agent event, sub-agent
  event, system line and research update passes through `termText`: whole escape sequences
  removed, a carriage return made a line break, any other control character shown as `⟨U+XXXX⟩`.
- **An approval prompt removes nothing** (`promptText`): every hidden character, bidi and
  zero-width included, shows as a marker. The rule is the dashboard's, moved to
  `internal/control` (`VisibleText`) so both surfaces agree.
- **What a slash command prints from stored text** goes through the same rule; where the app has
  styled it, color sequences stay except one that conceals (`styledText`).
- Left: stored text inside an already-styled message can still change a color.

### The host and the work dir (TEN-464)

- Audited, one change. The file tools already resolve every link and check where it points
  against the roots, and Tenant runs no git on the host. `resolve` now returns an error where it
  returned a path unresolved.
- Left: the check and the open are two steps, so a link swapped in between is followed.

## Verified on a real engine

Run on 2026-11-10 after the merge, on Windows 11 with Docker Desktop (engine 29.9.3, kernel
6.18.58.1), on `main` at the merge of #66.

| What | Check | Result |
|---|---|---|
| The check container before first use | `tenant sandbox selftest --backend container`, which is gated on it | passes |
| HOME tmpfs with `uid`/`gid`/`mode` options | `TestContainerLive_HomeIsATmpfsOfItsOwn` | passes: writable by the sandbox user, a tmpfs, mode 700, nothing in the work dir, empty in the next container |
| `--ulimit cpu` | `TestContainerLive_CPULimitStopsARunawayProcess` | passes: a busy loop under a 2-second cap is killed in about 3 seconds |
| The image on trixie | `tenant sandbox image build` | builds (Debian 13.7); the live suite and the selftest pass on it |
| A re-run started by image id | `TestContainerLive_ApprovedReRunStartsByImageID`, added for this | **failed first**, then passes. The fault was not the image id: a folder granted read-only and then granted writable by the approval was mounted twice, and the engine refused "Duplicate mount point". `specFor` now mounts each destination once, by its widest grant |
| The escape suite | `tenant sandbox selftest --backend container` | 4 of 4 contained, every legitimate twin succeeded |
| The rest of the live suite | `TestContainerLive` | 14 of 14 pass |

Still not run on a real engine: `tenant sandbox stop-all`, and a `tcp://` endpoint with TLS.

The lesson for this backend's tests: the fake CLI accepts any argv, so it cannot see what an
engine refuses. A change to mounts or flags needs its live case run before it merges.

## Not done

Open in wave 2: the cap on concurrent sandboxes; read-only binds over `.git/hooks`, `.git/config`
and the editor folders (nested read-only binds are untested on 9p); `full` on a Tenant-owned
bridge; the circuit breaker for cron; the kill-switch setting and quarantine; the policy version,
an OOM kill as its own field and a digest of changed files in the run record; the seccomp delta;
the image scanned and signed; the network scope per run. Wave 3 (Linux and macOS builds, Podman)
is not started.

Four questions have no decision yet: the name and place of the setting that disables `container`
and escalation, what a launch over the concurrency cap does, how a paused surface is shown and
resumed, and whether the read-only binds create missing folders in the operator's work dir.
