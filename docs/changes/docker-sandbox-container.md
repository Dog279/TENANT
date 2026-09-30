# Change: the Docker `container` sandbox backend, Windows first (TEN-453/454)

**Branch:** `feat/docker-sandbox-hardening` · **Status:** implemented and tested; the backend is
live-tested and passes the escape suite on Windows 11 with Docker Desktop (WSL2, engine 29.8.0,
kernel 6.18.40.1). macOS and Linux builds are wave 3, not exercised here. Not yet deployed.
**Tickets:** TEN-454 (Phase 1 backend) and, from the hardening epic, TEN-455 (W1-1), TEN-456
(W1-2), TEN-457 (W1-3), TEN-458 (W1-4), TEN-459 (W1-5), TEN-460 (W1-6, in part) and TEN-461
(W1-7). The plan is [docker-sandbox-hardening.md](../plans/docker-sandbox-hardening.md); it builds
on [docker-sandbox-plan.md](../plans/docker-sandbox-plan.md).

## What changed

The `container` backend (`internal/sandbox/container*.go`) is now implemented. It runs each
model-written command in a fresh, hardened `docker run --rm` and is one backend for every OS,
driven through the `docker` CLI, so it needs no new Go dependency. `sandbox.For("container")` now
returns it instead of "not implemented", and an `exec_profiles` entry may name `container`.

The wave-1 hardening the live Windows probe motivated is baked in:

- **A fixed argv schema (W1-2), held by a golden test.** `buildRunArgv` emits exactly the §3.3
  flags and never takes an argument from config: `--read-only`, a `nosuid,nodev` tmpfs `/tmp`,
  `--user 65532:65532`, `--cap-drop ALL`, `--security-opt no-new-privileges`, `--runtime runc`,
  `--pids-limit`, `--memory`/`--memory-swap` (no swap), `--cpus`, `core`/`nofile` ulimits,
  `--init`, `--log-driver none`, and `--pull never` so a missing image never reaches a registry.
  Bind mounts go only through `--mount` with CSV-quoted fields (a comma in a path broke an
  unquoted `--mount` in the probe), and the command is one argv element after the image, so no
  value is ever interpolated into a shell string. A forbidden-flag table test pins that
  `--privileged`, `--cap-add`, host namespaces, the socket, `-v`, `--gpus` and the rest never
  appear. Environment crosses by name only (`-e NAME`), dropping host-only, `DOCKER_*` and
  `NVIDIA_*` names.
- **Canonical mount sources (W1-3).** Every source is resolved and, on Windows, rejected if any
  component is a junction or symlink (Docker Desktop follows a junction named as a mount source),
  along with UNC, device paths, alternate data streams, and trailing dots or spaces. A source is
  always a native Windows path; the destination is the `/mnt/<drive>/…` form, and `C:\x → /mnt/c/x`
  is only ever a destination. A grant that names a deny root is refused at spec time.
- **A reaper (W1-4).** Killing the CLI leaves the container running, so a cancel or timeout runs
  `docker kill` by name, a deferred cleanup sweeps it, and `SweepLeftovers` clears this instance's
  containers at startup. Every container carries `tenant.sandbox`, `tenant.trace` and
  `tenant.instance` labels. Output is capped as it streams, and reaching the cap kills the run.
- **Network fail-closed (W1-5).** `inherit` and `allowlist` both map to `--network none`; only an
  operator's explicit `full` opens the bridge. The model can never select it.
- **Conservative attribution (W1-6, in part).** Only EROFS is a filesystem refusal; a network
  error counts only under `none`; OOM and "cannot fork" are limits and never escalate; a Windows
  program is a `program` refusal (exit 127) rather than a mangled failure. Anything else stays
  unattributed and so fails closed.
- **Fail-closed availability (W1-1).** `Available` checks the CLI, a reachable engine and a built
  image; `profileBackend` treats a not-ready backend exactly as an unknown one (an enforcing
  profile fails closed, a dry run falls back to the host). `Probe` reports the boundary honestly
  (Limits is `partial`: `--cpus` is a rate, not a CPU-time budget) and warns below the engine and
  runc floors, and below the Docker Desktop floor on Windows (read from the registry, not
  `docker desktop version`).
- **The image (W1-2/W1-7).** `tenant sandbox image build` builds `tenant-sandbox:latest` from a
  Dockerfile embedded in the binary, on a digest-pinned `debian:bookworm-slim` base, with a
  uid-65532 user and setuid/setgid bits stripped. `tenant sandbox image verify` checks it.
- **The Windows test bed (W1-7).** `tenant sandbox selftest --backend container` runs the escape
  suite against the live backend — the security test CI cannot provide, since GitHub's Windows
  runners have no Linux containers. `escapecore` gained a `PathMapper` hook so the suite names
  files by their container paths, and its write case no longer depends on `$HOME` (which a
  container repoints into the work dir). Live `container_live_test.go` cases (gated on
  `TENANT_SANDBOX_LIVE=1`) cover identity, the read-only root, network none, and the reaper.

## What did not change

The seam, the policy vocabulary, escalation, dry-run, the audit and the host and Seatbelt
backends are untouched. `container` is opt-in per surface; the built-in default stays `host` on
Linux and Windows and Seatbelt on macOS.

## Not yet done (later waves)

Per-run CPU-time budget, a per-run HOME tmpfs and protected `.git`/config binds, host-side
work-dir hardening, taking `full` off the default bridge and the per-run network scope, the
seccomp delta, cron-under-container, the audit record and kill switch, and the Linux/macOS
specifics, MCP-in-containers and the egress proxy are TEN-462 … TEN-476.
