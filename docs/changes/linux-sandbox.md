# Change: the Linux sandbox, finished (SEC-010 / TEN-346)

**Branch:** `audit/linux-sandbox` · **Status:** implemented and tested; live-tested in Docker
(Ubuntu 24.04 with bubblewrap 0.9.0 and Ubuntu 22.04 with 0.6.1, as a normal user and as root),
and CI runs the live suite on every push. Not yet deployed.

## What Linux enforces
A command on a `restricted` profile runs inside bubblewrap:

- **Files.** It sees the host read-only, or with `read_roots` set only the system directories
  and those roots. It can write to its work dir, its `write_roots` and a private `/tmp` that is
  gone after the run, and nowhere else. Tenant's config, data and binary are hidden and
  read-only. A write anywhere else fails with "Read-only file system", which Tenant records as a
  refusal, so an attended surface can ask and dry-run works.
- **Host services.** `/run` is hidden. The D-Bus and systemd buses live there (through them,
  `systemd-run` starts a process outside any sandbox), as do the container runtimes' sockets and
  WSL's interop socket. With the network shared, only the resolver files come back, for DNS.
- **Network.** `network: none` gives the command its own empty network: no internet, no DNS, and
  none of the host's loopback services, Tenant's dashboard included.
- **Processes.** The command has its own process namespace. Whatever it leaves running is killed
  when it exits, whether backgrounded, under `nohup` or after `setsid`. It cannot see or signal
  the host's processes.
- **Limits.** `memory_mb` (address space), `cpu_seconds` (per process) and `max_pids` are set as
  rlimits inside the sandbox. `max_pids` counts the sandbox's own processes and threads, its init
  and shell included, not everything the user runs (Linux 5.14+). The output returned to the
  model is capped as it arrives, so a flood cannot fill Tenant's memory. The wall clock still
  stops everything.
- **Privileges.** Every capability is dropped, also when Tenant runs as root. Otherwise a root
  command could unmount a deny mask and read what it hides.

MCP servers on `sandbox: restricted` get the same boundary, `max_pids` included.

## What it needs
- `bwrap` (the `bubblewrap` package) on `PATH`.
- Unprivileged user namespaces. Ubuntu 23.10 and later restrict them with AppArmor
  (`kernel.apparmor_restrict_unprivileged_userns=1`): give `bwrap` an AppArmor profile that
  allows `userns`, or set the sysctl to 0. `kernel.unprivileged_userns_clone=0` (older Debian)
  and `user.max_user_namespaces=0` turn them off entirely.

## What happens without it
On first use Tenant runs an empty command in the sandbox. If bubblewrap is missing or that
check fails, `restricted` is unavailable and says why:

- `sandbox: bubblewrap not installed: restricted unavailable (install the bubblewrap package)`
- `sandbox: bubblewrap cannot create a sandbox on this host: restricted unavailable: <bwrap's
  error> (AppArmor restricts unprivileged user namespaces …)`, naming whichever setting above
  is to blame.

A surface whose profile names `restricted` then fails closed: its commands are not run, and
never run unconfined. `tenant sandbox probe` prints the reason. For scripts,
`tenant sandbox probe --json` has `restricted.available` and `restricted.error`. Only a
passing check is remembered, so installing bubblewrap or lifting the restriction takes effect
without a restart. There is no Landlock fallback (decision 2026-09-29: no new dependency).

## Known limits
- Unix sockets are files. One the sandbox can see stays connectable, through a read-only mount
  and with the network cut. `/run` and `/tmp` are hidden. With `read_roots` empty, a socket
  elsewhere, in the home directory say, is not.
- With the network shared, abstract unix sockets (an X11 display, for one) are reachable.
- The memory cap is address space, per process. The private `/tmp` is held in RAM and does not
  count against it; the wall clock bounds it.
- On kernels before 5.14 `max_pids` counts every process the user runs, so a small cap can
  refuse forks early.
- Run as root, the command is exempt from `max_pids` (the kernel's rule). `probe` then reports
  limits as `partial`.
- A read of a hidden path looks like a missing file, so it is not recorded as a refusal.

## Windows
WSL2 + bubblewrap is Windows' restricted backend (owner decision 2026-09-29). The native path
planned before it (a dedicated sandbox account, NTFS ACEs, a WFP egress filter) is retired and
its stubs are gone. The Job Object stays as the `jobobject` limits backend. The WSL backend
builds its bubblewrap arguments with the same code, so the read-only layers, the hidden `/run`
and the capability drop apply there too. Hiding `/run` also hides WSL's interop socket, through
which a sandboxed command could otherwise start a Windows program outside the sandbox. This is
not yet run under WSL: run the WSL live tests and `tenant sandbox selftest` inside WSL on the box.

## Tests
- Every OS: the argument vector, the rlimit wrapper, the output cap and the setting hint.
- Linux, live: `internal/sandbox/redteam_linux_test.go` and `process_linux_test.go`. The escape
  suite, then the plan's cases 1–8: secret reads (direct, symlink, hardlink, `..`), writes out
  of the roots (symlink, rename, dotfiles), host loopback and sockets, fork, memory, CPU and
  output floods, background survivors, env exfiltration, and Tenant's own dirs. They skip
  without a working bubblewrap, unless `TENANT_SANDBOX_LIVE=1`, which turns that into a failure.
- CI, ubuntu job: installs bubblewrap, checks the refusal while AppArmor's restriction holds,
  lifts it, then runs `TENANT_SANDBOX_LIVE=1 go test -race ./internal/sandbox/...`.
