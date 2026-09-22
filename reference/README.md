# reference/ — copy-paste code and how to adapt it

Working, tested code you copy into other programs or into new Tenant
subsystems, with short tutorials on adapting it. Everything here compiles and
is tested with the rest of the tree (`go build ./...`, `go test ./reference/...`),
so a copy is always current with the running binary.

| Folder | What you copy | Tutorials |
|---|---|---|
| [`logging/`](logging/) | [`tenantlog.go`](logging/tenantlog.go) — one stdlib-only file that logs any Go program into Tenant's log book (its own folder on disk + its own dashboard tab, live when a hub is up, file fallback when it is not, drop-in `slog` handler) | [01 quickstart](logging/01-quickstart.md) · [02 augmenting the file](logging/02-augmenting.md) · [03 hot-patch playbook](logging/03-hotpatch-playbook.md) · [04 inside the binary](logging/04-inside-the-binary.md) |

Ground rules for anything in `reference/`:

- **One file, no Tenant imports.** A copy must compile alone in a stranger's
  module. Tests beside it may import `tenant/internal/...` to pin behavior;
  the copyable file never does.
- **Stdlib only, conservative Go** (1.21+), so it drops into older programs.
- **Tests pin the contract** to the real subsystem (here: the log book's
  on-disk format byte-for-byte, its name rules, its data-dir rule, and the
  discovery round trip). If the hub changes, `go test ./reference/...` fails
  and the copy gets updated with it.
- **The tutorials assume the box, not the repo:** the reader has a running
  `tenant`, a shell, and a program they want to plug in — no rebuild.
