# 1Password: sign-in modes and secrets kept out of the model

The 1Password plugin ([onepassword-plugin-plan.md](onepassword-plugin-plan.md))
signs in with a service account token through the `op` CLI. This plan adds the
two other ways 1Password offers (a Connect server, the desktop app), then does
what 1Password itself recommends for agents: use a secret without the model
ever holding it.

## What 1Password's documentation says (read 2026-11-22, www.1password.dev)

- **Desktop app sign-in.** `op` asks the 1Password app, and the app shows its
  own prompt (Touch ID, Windows Hello, the system password). One approval
  lasts until ten idle minutes pass, twelve hours at most, and ends when the
  app locks. It is tied to the terminal on macOS and Linux and to the calling
  process on Windows. It grants the whole account, personal vaults included.
  1Password describes it as for a person at the machine.
- **Connect.** A self-hosted server (two Docker containers, `connect-api` and
  `connect-sync`, default port 8080, TLS optional) with a REST API. It caches
  the vaults, so it has no rate limits, and its tokens are scoped per vault
  with read or write. Through `op` it supports only `read`, `item get`, `run`
  and `inject`: listing and writing need the REST API. The API can delete an
  item but not archive one.
- **Service accounts.** Per token and hour: 10,000 reads and 1,000 writes on
  Business, 1,000 and 100 on the other plans; per account and day: 50,000 on
  Business, 5,000 on Teams, 1,000 otherwise. `op read` and `op item get` cost
  three reads by name and one by id. `op service-account ratelimit` reports
  what is left.
- **Guidance for agents.** Do not pass a raw secret to the model where it can
  be avoided; use secret references resolved at run time, short-lived and
  tightly scoped access, and auditing.
- **Not usable by Tenant today.** Credential Broker (no stored token, the
  workload proves itself with OIDC): Business accounts, the JavaScript SDK and
  Environments only, public preview. The NVIDIA OpenShell integration: beta.
  1Password for Agents (the app fills a sign-in form the agent cannot see):
  individual and family accounts and partner agents only.

## Decisions (operator, 2026-11-22)

1. **Connect is called over REST, with the standard library.** Not through
   `op`, which would give two read-only tools.
2. **Desktop sign-in is for a person at the machine.** A scheduled, Discord
   or iMessage turn is refused before `op` starts. An unattended install uses
   a service account or Connect.
3. **`references only` ships off by default** (phase 2), so an existing
   install keeps working; it is documented as the setting an enterprise turns
   on.
4. **Where a Connect server runs** is the operator's choice per site. The
   R340 has no Docker, so there it means another host or installing Docker.

## Phase 1: sign-in modes (built 2026-11-22)

A new `auth` setting: `service_account` (the default, and what a config from
before the setting means), `connect`, `desktop`. The mode comes from the
settings alone; an environment variable supplies the credential for the mode
chosen and never chooses the mode.

| | service account | Connect | desktop app |
|---|---|---|---|
| Settings | `token` (secret) | `connect_host`, `connect_token` (secret) | `account` (optional), `vaults` (required) |
| Environment | `OP_SERVICE_ACCOUNT_TOKEN` | `OP_CONNECT_HOST` + `OP_CONNECT_TOKEN`, used only as a pair | none |
| Needs the `op` binary | yes | no | yes |
| Works unattended | yes | yes | no: refused |
| Rate limits | yes | no | the account's own |
| `op_delete` (archive) | yes | refused: the API cannot archive | yes |
| Reaches | the vaults granted to the account | the vaults granted to the token | every vault the person can see, fenced by `vaults` |

- `internal/plugins/onepassword/connect.go`: the `Backend` over REST. The
  token travels only in the `Authorization` header; a redirect is reported and
  never followed; an address with a user, password, query or fragment is
  refused; a vault name two vaults share is refused in favour of the id. Items
  and fields are found by id first, then by name. A reference is built the way
  `op` builds one. `op_edit` sends one JSON Patch; a generated password on
  edit is made locally with `crypto/rand`.
- `opcli.go`: `NewDesktopCLI`. No token; `OP_BIOMETRIC_UNLOCK_ENABLED=true`;
  `--account` when one is named; a two-minute bound per call so a person has
  time to answer. `Accounts` (`op account list`) is the probe that never
  raises a prompt. A closed app and a dismissed prompt are named errors.
- `dispatch.go`: `Policy.Present`. With desktop sign-in every tool is refused
  on a turn with nobody at the machine, before the backend is touched. An
  archive over Connect is refused before anyone is asked to approve it.
- `cmd/tenant`: one builder for the three modes, used by launch, `/enable`,
  doctor, the configure probe and `tenant onepassword`. Doctor never raises a
  prompt in desktop mode, and warns when a Connect address is plain http to
  another machine. `/configure` asks only for the chosen mode's fields.
- Events: `AppUnavailable` 3103, `ConnectUnreachable` 3104, `NotApproved`
  2102; `AuthFailed` reworded for all three modes.

### Checked against the real app (Windows 11, op 2.42.0, 2026-11-22)

- Desktop sign-in works end to end: one approval in the app, then the user
  and the vault list come back. The read-only live test passes.
- One approval covers the calls after it: eight more calls from the same
  process ran with no second prompt.
- A call through the app takes about 1.7 s.
- `op whoami` always answers "account is not signed in" with the app, so
  desktop mode asks for the user (`op user get --me`) and never runs it.
- The app refuses a new connection for about a second after a call that
  needs no session (`whoami`, `account list`), and not after a call that used
  one. `op` gives up before sending anything, so the CLI asks again (after
  0.5 s, 1 s and 2 s) and the call goes through.
- With the app's `Integrate with 1Password CLI` setting off, every call fails
  in 0.3 s with a named error and no prompt.

### Still to check against the real thing

- Desktop mode on macOS and Linux, and what happens when Tenant has no
  terminal there (launchd, systemd), where the approval is tied to the tty.
- Desktop mode from the Tenant binary itself in a long session: the ten idle
  minutes and the twelve-hour limit.
- A real Connect server: that it accepts the item ids `op_create` sends for
  custom fields, and the JSON Patch operations `op_edit` sends.
- Live tests for both, gated behind environment variables like the existing
  `TENANT_LIVE_OP_*` test.

## Phase 2: the secret never reaches the model

1. **`os_exec` takes `secret_env`.** `{"NAME": "op://vault/item/field"}`: the
   host resolves each reference through the plugin, puts the value in the
   child's environment and in the log scrubber, and scrubs it from the
   command's output. The exec approval and command rules apply unchanged.
2. **`web_fill` takes a reference.** The host resolves an `op://` value and
   types it; the model sees only that the field was filled.
3. **`references only`.** A setting that turns `op_read` off, so the two
   paths above are the only way a secret is used.

## Phase 3: controls

1. Look items up by id (one request, not three) and cache vault and item ids,
   never values. Doctor shows the remaining quota for a service account.
2. Every audit event carries the sign-in mode and the account.
3. Environments, read-only, once `op` 2.42 or later is everywhere (the R340
   has 2.41.1).

## Not planned

- **The SDK backend.** It embeds a 9 MB WASM core and grows the binary by
  about 20 MB; `op` and REST cover all three modes.
- **Credential Broker, OpenShell, 1Password for Agents.** See above; revisit
  when the Go SDK supports the broker or 1Password opens the agent programme.
