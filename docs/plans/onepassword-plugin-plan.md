# 1Password plugin: implementation plan

The agent reads and keeps secrets in 1Password through typed tools, with the
same approval, secret-storage and log-scrubbing rules as every other plugin,
instead of shelling out to `op` through `os_exec`.

## Decisions (operator, 2026-10-28)

1. **The `op` CLI is the first backend; the SDK is optional, later.** The
   official Go SDK (`github.com/1password/onepassword-sdk-go` v0.4.1) builds
   with `CGO_ENABLED=0`, but it embeds a 9.1 MB WASM core and pulls wazero,
   extism, OpenTelemetry and protobuf: a probe binary grows from 1.1 MB to
   21.7 MB. Tenant is one static binary of about 46 MB, so the SDK is not
   the default. The plugin talks to a `Backend` interface; `op` is the one
   that ships, an SDK backend can come behind a build tag.
2. **A service account, never a personal sign-in.** The token is a
   `skill:onepassword:token` secret (or `OP_SERVICE_ACCOUNT_TOKEN`), handed
   to `op` through its environment only. 1Password itself scopes a service
   account: it cannot reach Personal, Private or Employee vaults or the
   default Shared vault, only the vaults granted at creation, with
   `read_items`, `write_items` or `share_items`. The plugin adds a second
   fence: an optional `vaults` allowlist in settings.
3. **A secret reaches the model only through one tool, and it is gated.**
   `op_item` shows an item's shape (titles, usernames, URLs, notes, tags) and
   replaces every concealed value with `[concealed]` plus its reference.
   `op_read` resolves one `op://vault/item/field` reference and is the only
   tool that returns a concealed value. Its action, `op_reveal`, files under
   the `egress` category: with a local model the operator allows it once;
   with a cloud model it asks, because the value leaves the machine.
4. **Every revealed value is scrubbed from the log book.** The hub's
   scrubber learns the value the moment it is read, so a tool result, an
   episode or an export never carries it. Only the reference is logged.
5. **Writes are off until a flag or a setting turns them on.** `op_create`
   and `op_edit` are `write`; `op_delete` (archive) is `destructive`.
   `op_create` can ask 1Password to generate the password, so a credential
   the agent creates for a service never passes through the model.
6. **Rate limits are respected, not hidden.** Teams and Individual plans
   allow 1,000 reads and 100 writes an hour per token and 1,000 requests a
   day per account (Business: 10,000 / 1,000 / 50,000). A 429 is reported as
   the plugin's `RateLimited` event and returned to the model in plain words.

## Files

- `internal/plugins/onepassword/`
  - `backend.go`: the `Backend` interface and the shared types (`Vault`,
    `ItemOverview`, `Item`, `Field`, `CreateParams`, `EditParams`).
  - `opcli.go`: the `op` backend. Finds the binary (`op_path` setting,
    else `PATH`), runs it with `--format json`, the token in the child's
    environment, a 20 s timeout per call, stderr parsed for 429 and auth
    errors. Never puts the token or a value on the command line.
  - `dispatch.go`: `Policy` (AllowWrite, Confirm, Vaults allowlist, Reveal
    hook) and the `Dispatcher` (`Tools`, `Dispatch`).
  - `events.go`: the catalog: `CLINotFound` 3101, `AuthFailed` 3102,
    `RateLimited` 2101, `Revealed` 1101 (the reference, never the value),
    `ItemCreated` 1102, `ItemEdited` 1103, `ItemArchived` 1104.
  - `*_test.go`: a fake backend for the dispatcher; the `op` backend tested
    through its process runner, replaced by a recorder (JSON fixtures, the
    token in the child's environment and not its arguments, a 429, a bad
    token, a missing binary, a timeout).
- `cmd/tenant/plugin_onepassword.go`: the descriptor (`ID: "onepassword"`,
  `Connector: true`, Keys, Flags, Doctor, Actions, Launch and Activate).
- `cmd/tenant/plugin_onepassword_cmd.go`: the CLI,
  `tenant onepassword whoami|vaults|items <vault>` (the file name keeps it
  in the plugin's own namespace, as `TestPluginsAreNamedOnlyInTheirOwnFiles`
  requires).
- `cmd/tenant/skillctl_onepassword.go`: the configure schema and the
  `whoami` probe.
- `internal/logs/scrub.go`: `Hub.AddSecrets` (additive, runtime values)
  beside `SetSecrets`, so a plugin can register a value it just handled.
- Docs (findtime-net, same PR as the rule in CLAUDE.md): `mcp/integrations.md`
  (the row and a setup note), `mcp/built-in-tools.md` (the row),
  `reference/environment.md` (`OP_SERVICE_ACCOUNT_TOKEN`),
  `reference/cli.md` (the command). Here: CHANGELOG, README's plugin list,
  `docs/ARCHITECTURE.md` facts.

## Tools

| Tool | Does | Returns | Gate |
|---|---|---|---|
| `op_vaults` | lists the vaults the service account can see | id, name | read |
| `op_items` | lists items in one vault, optional `category`/`tag` filter | id, title, category, tags, updated | read |
| `op_item` | one item's structure | fields (concealed ones as `[concealed]` + `op://` reference), URLs, notes, tags | read |
| `op_read` | resolves one `op://vault/item/field` | the value, labelled sensitive | `op_reveal`, egress |
| `op_create` | a login, password, API credential or secure note; `generate_password` | the new item's id and references | `op_write`, write |
| `op_edit` | sets fields on an item | the item's shape | `op_write`, write |
| `op_delete` | archives an item | confirmation | `op_archive`, destructive |

Every result carries `{sensitive, service}` labels, so the flow rules
already follow it: a later outbound call quoting a result asks under egress.

## Configuration

`/configure onepassword`, the Integrations page or `tenant setup`:

| Field | Secret | Meaning |
|---|---|---|
| `token`* | yes (`skill:onepassword:token`) | the service account token (`ops_...`) |
| `op_path` | no | the `op` binary when it is not on the service's `PATH` |
| `vaults` | no | comma-separated vault names or ids the agent may touch; blank means every vault the token can see |
| `allow_write` | no | `true` keeps writes on without the launch flag |

Launch flags: `--onepassword` (on) and `--onepassword-allow-write`. There is
no allow-reveal flag: with an approval broker wired, every gated action goes
through it and the `egress` category's mode decides (`gateAllow`), which is
the one place the operator sets that policy. No `--onepassword-token` flag
either: a token on the command line lands in shell history and the
scheduled task's XML.

The probe runs `op whoami --format json` and shows the account and the
service account's name. Doctor repeats it and lists the vaults it can see.

## Later

- **`os_exec` secret environment** (phase 2): `os_exec` takes
  `secret_env: {"NAME": "op://..."}`; the host resolves each reference through
  the plugin, puts the value in the child's environment and in the scrubber,
  and the model never sees it. The exec approval and command rules apply
  unchanged. This needs a resolver hook on the os plugin, so it is its own
  PR.
- **The SDK backend** (phase 3) behind a `onepassword_sdk` build tag, for an
  install that wants no `op` binary and accepts the size.
- **Environments** (1Password's beta): read-only variables, when the SDK
  backend exists.

## Tests

- Dispatcher over a fake backend: each tool's happy path; the vault
  allowlist refusing a vault by name and by id; `op_item` concealing every
  `CONCEALED` field and keeping its reference; `op_read` denied when the
  confirm says no and the value absent from the error; writes refused with
  `AllowWrite` false; `op_delete` refused without an explicit confirm.
- `op` backend: the process runner replaced by a recorder: JSON decoding of
  `vault list`, `item list`, `item get`, `read` and `whoami`; the token
  present in the child's environment and absent from its arguments, and a
  parent's own `OP_SERVICE_ACCOUNT_TOKEN` or `OP_SESSION_*` never forwarded;
  `item create -` with the template on stdin and no value in the arguments;
  the edit assignments with escaped labels; a 429, a 401 and a not-found
  classified; a missing binary and a timeout named in the error.
- The scrubber: `AddSecrets` values replaced in an entry; a value shorter
  than the scrubber's minimum is ignored (as `SetSecrets` does).
- Descriptor: `plugin_launch_test.go` already walks every descriptor; the
  new one joins it (stub tools advertised, Actions categorised).
