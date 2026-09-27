# Change: third-party MCP servers run out of process, in the sandbox (EXT-003)

**Branch:** `feat/ten-318-isolated-mcp` · **Status:** implemented and tested on Windows
(host, Job Object, and WSL bubblewrap); Linux and macOS paths are built and vetted, not run
here. Not yet deployed.
**Ticket:** EXT-003 / TEN-318.

## What changed
- **Local MCP servers (`mcp_servers` in `config.json`).** Each entry names a server, the
  command that starts it, and its boundary, in the fields `exec_profiles` uses:
  - `sandbox`: `restricted` (the default), `jobobject` (Windows) or `host`;
  - `network`: `none` (the default), `full` or `inherit`;
  - `read_roots`, `write_roots` and `deny_roots`;
  - `env_pass` and `env_set`;
  - `cpu_seconds`, `memory_mb` and `max_pids`;
  - `work_dir`, which defaults to `<data>/mcp/<name>`.

  The server appears as connector `mcp:<name>`. It starts in the background at launch, as
  remote connectors reconnect, or on `/enable`. Its tools are gated like a remote
  connector's:
  - deny-by-default unless the operator trusts its annotations;
  - trust `ask` / `allow` / `deny`, kept under `stdio:<name>` in `mcp_trust`;
  - tool rules;
  - the approval gate.

  `/mcp remove` and the model's `mcp_remove` refuse a local server: it is removed from
  `config.json`.
- **Its defaults are those of a confined process:**
  - the restricted backend, with no network;
  - a work dir of its own;
  - Tenant's config and data dirs, its binary and the local credential stores denied;
  - a scrubbed environment, plus what `env_pass` and `env_set` add;
  - caps when set.

  Refused at load: a network allowlist (no backend enforces one), relative roots, an unknown
  sandbox, and bad names.
- **The sandbox can run a long-lived process.** `sandbox.Start` returns a `Process`: its pipes,
  the tail of its stderr, and `Close`, which follows the stdio transport's shutdown (close
  stdin, wait, kill). Every backend implements it:
  - `host`;
  - bubblewrap on Linux;
  - Seatbelt on macOS;
  - WSL bubblewrap on Windows;
  - the Windows Job Object, now selectable as `jobobject`.

  Caps:
  - Bubblewrap, Seatbelt and WSL set memory and CPU with `ulimit` in the sandbox before the
    server runs. A cap that cannot be set fails the start.
  - The Job Object caps memory, CPU and processes.
  - `StartLimits` names which caps a backend applies.

  Under WSL, the environment allowlist crosses through `WSLENV`, never on a command line.
- **`mcpremote.OpenStdio`** connects to a server over its pipes and reuses the remote
  connector's dispatcher, gating and result labels. It bounds only the handshake and the
  tool listing, never the server's life. **`toolmux.RegisterMCPStdio`** and
  **`StartMCPStdioSilently`** mirror the remote connector's registration and launch
  reconnect, keeping the operator's disabled tools off.
- **The effective-policy view shows isolation.** `tenant permissions effective` lists every
  integration and where its code runs:
  - first-party plugins: in Tenant's process;
  - remote MCP servers: on another host;
  - local ones: out of process under their backend. The line gives the isolation level,
    what this host enforces for filesystem, network (and its policy) and environment, and
    which caps apply.

  `tenant security audit` warns about a local server with no confinement, or one that
  cannot start.
- **Only first-party code runs in Tenant's process.** A test holds that every in-process
  plugin is first-party (the plugin registry) or the host's own. Anything else is an MCP
  connector, and so out of process.

## Security fix: WSL deny roots were not masked
The WSL restricted backend never masked a deny root. `bwrapMask` stat'ed each root at its
translated Linux path (`/mnt/c/…`) from Windows, where that path does not exist. So every
translated deny root was skipped. Whenever the host was readable, a sandboxed command could
read:
- Tenant's config dir, including `credentials.json`;
- its data dir;
- the credential stores.

The host is readable by default: empty read roots. This affected `exec_profiles` on Windows
with `"backend": "restricted"`.

Now:
- **Deny roots are stat'ed at their Windows path**, so they are masked.
- **The Windows drives are hidden** when the host is readable, and bound back only at the
  work dir and the write roots. WSL resolves Windows names case-insensitively, so
  `/mnt/c/users/…` reaches what a mask on `/mnt/c/Users/…` hides: a mask cannot guard a
  Windows path.
- **A deny root inside a root the operator grants** is masked at its own path, and is
  best-effort against other letter case.

## Why
Tenant could reach third-party MCP servers only over HTTP. ADR 0001 makes MCP one of two
extension boundaries, and says a local server runs out of process under the execution
sandbox. TEN-318 asks for exactly that: the environment, filesystem, egress and resource
bounds; in-process reserved for first-party code; and isolation visible in the effective
policy.

## Behavior changes
- **WSL hides Windows files.** Under the WSL restricted backend, a command no longer sees
  Windows files outside its work dir and write roots unless `read_roots` lists them. The
  Linux distro stays readable.
- **Gate wording.** An MCP tool's approval prompt says "MCP tool", not "remote MCP tool".
- **The container backend** is still rejected, now without naming TEN-318.

## Still open
- **Backends:** no container backend, and no backend enforces a network allowlist (a
  server's network is `none` or `full`).
- **Caps (TEN-346):** process caps on bubblewrap, Seatbelt and WSL.
- **Native Windows confinement (TEN-346):** `jobobject` caps resources but confines no
  filesystem or network.
- **The environment under WSL** also carries the distro's login environment (`PATH`,
  `HOME`, …).
- **Lifetime:** a server lives as long as the command that started it. Each command that
  builds tools (`tui`, `serve`, `chat`, …) starts its own.

## Evidence
- **`internal/sandbox`:**
  - `Start` serves and closes;
  - it scopes the environment (a parent secret does not pass);
  - `Close` kills a stuck server;
  - the context ends the process;
  - `Start` refusals;
  - the `ulimit` wrapper and the stderr tail;
  - the Job Object serves, and a 1 GiB allocation fails under a 128 MB cap;
  - WSL: a server streams through bubblewrap, the environment crosses by `WSLENV`, and the
    caps read back;
  - deny roots stay hidden with the host readable, in any letter case, and inside a
    granted root;
  - the drive-mount edge case (an automount root of `/`).
- **`internal/plugins/mcpremote`:** a stdio server's tools are gated like a remote
  connector's (a denied call never reaches the server; deny trust blocks), and a failed
  start is reported.
- **`cmd/tenant`:**
  - the plan's defaults and refusals;
  - a real MCP server runs out of process through the whole path: gated, then allowed by
    trust. Under `host`, under `jobobject`, and confined under the restricted backend. On
    this Windows machine that is a Linux build in WSL: it works, cannot reach the network,
    and cannot read `credentials.json` or a denied root in any letter case;
  - only first-party code is in-process;
  - the effective view and the audit finding;
  - `/mcp remove` refuses a local server.
- **By hand:** `tenant permissions effective` and `tenant security audit` against a scratch
  config with a restricted and a host server.
- **Mutation testing:** 18 mutants, all caught. They cover the plan's defaults (backend,
  network, the Tenant and credential-store denies, the environment), the WSL hiding, deny
  stat, caps and environment, the Job Object assignment, `Close`'s kill, the host
  environment, the connector's gating and trust key, the removal guard, the effective view,
  the audit finding, and registration.
- **Suite:** `go test ./...`, `go vet` (Windows, and `GOOS=linux`/`darwin` for the sandbox),
  smoke eval.
