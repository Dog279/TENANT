# An operations center that runs 24/7: plan

A self-hosted model runs Tenant around the clock with a budget, a 1Password
vault and a runbook in the wiki, toward a goal the operator sets, with as
little human involvement as the runbook allows. Tokens are not the
constraint (the model is self-hosted); exposure and money are.

## What serve offers today, and what it lacks

- **Real-time inputs that exist:** the Discord relay (a DM or a channel
  message starts a turn at once), iMessage, the dashboard chat, approvals
  with a timeout, and the log book as an event bus: `POST /api/events` and
  `POST /api/addon/event` file an outside event; an alert rule matches it
  and its `cron` action runs a one-off prompt at once.
- **Gaps that block an unattended agent:**
  1. `/goal` runs only in the terminal app (`cmd/tenant/serve.go`: serve
     has no Autopilot). The loop under serve is cron.
  2. The cron exec agent sets no `ConfirmSensitiveEgress`
     (`cmd/tenant/cronmgr.go`), and `internal/agent/agent.go` denies a
     sensitive flow when it is nil. A cron turn that reads the runbook and
     then sends anything out is blocked. The fix is the approver the cron
     surface already has (`cronExecApprover`) answering egress too.
  3. An alert's `cron` action runs on the safe runner, with no exec tools
     (`setAlertCron`). A reactive turn that must act needs an `exec` option
     on the rule, running on the exec agent with the same approver.
  4. A cron job's prompt is static. A reactive job needs the event that
     woke it (`alerts.Alert.Facts` already builds that text).

## Design

**The loop.** Two kinds of turn drive the agent:

- **Reactive:** an outside signal (a payment, a form, a reply, a failed
  job) reaches the log book through `POST /api/events` from a small
  webhook receiver, or through the Discord relay. Alert rules route each
  kind to a prompt job with `exec: true`; the prompt names the runbook page
  to read first and the standing order for that signal.
- **Heartbeat:** a cron tick (`@every 15m`, `exec: true`) reads the runbook
  and the journal, does the next step of the plan, writes the ledger and
  the journal back, and posts a one-line status to the operator channel.
  The tick is the fallback when nothing arrives; it is what makes the agent
  make progress, not only react.

**The runbook (wiki).** Three pages the agent reads at every turn and
writes through `wiki_save`:

- `ops/runbook.md`: the mission, the budget (total, per-day cap, what
  needs a human), the approval boundary (what asks: any spend above a
  line, any new account under the operator's identity, any signature, any
  outbound mail to a new party), the standing orders per signal, and the
  stop rule.
- `ops/ledger.md`: every spend and every credential created, appended, with
  the running total against the budget. The agent refuses its own next
  spend when the ledger says the cap is reached; a capped virtual card is
  the enforcement behind the page.
- `ops/journal.md`: dated decisions and their reasons, so a human (or the
  next tick) can follow the thread.

**Models.** The generation provider is the self-hosted GLM server (vLLM,
`glm` tool format, found by Tenant's own server check); no cloud fallback
for the main agent, so an outage stops the loop instead of switching
models. The Judge (Clef on its own server) checks every interactive task
turn. Helpers for research run on the same vLLM server's second model.

**Secrets.** A 1Password service account scoped to one vault for this
work, read and write, no share. The plugin's `vaults` setting names that
vault again. Credentials the agent creates go in through `op_create` with
a generated password; a secret reaches the model only through `op_read`,
under egress, logged by reference.

**Guardrails, per surface** (the cron exec surface is the one that matters):

| Category | Setting | Why |
|---|---|---|
| exec | allow, command rules deny `rm -r`, `format`, service control | the runbook's work is commands |
| write | allow, inside the write roots | the ledger and journal |
| send | allow for the operator channel; ask for mail to a new address | outbound is the business |
| destructive | ask (Discord approval, 30 m timeout) | one human click |
| egress | allow (local model) | the runbook's own facts are sensitive by label |
| mcp, sandbox | deny | nothing adopts tools unattended |

The spending cap is a virtual card with a hard limit, in the vault. The
kill switch is three things that already exist: `/cron exec off`,
`/disable onepassword`, and `stop` in the relay.

## Phases

1. **Provider swap** (config): the GLM provider active, the old cloud
   provider removed, the Judge on. No code.
2. **The 1Password plugin** (its own plan and PR).
3. **Unattended egress and reactive exec** (one PR): the cron approver
   answers egress; `logs.alerts[].action.exec` runs the rule's prompt on
   the exec agent with the event's facts; docs for `dashboard/cron.md`,
   `dashboard/logs.md` and `reference/config.md`.
4. **Ingress**: a webhook receiver that files `POST /api/events` with a
   provider name per source, and the alert rules for each.
5. **Runbook and go-live**: the three wiki pages; a week in shadow (every
   category `ask`, the operator approving from Discord), then the table
   above.

## Tests

- Phase 3: a cron exec turn whose outbound call quotes a wiki read is
  approved by the cron approver's egress rule and refused by a `deny`
  rule; an alert with `exec: true` runs on the exec agent and its prompt
  carries the event's facts; without `exec` the safe runner is unchanged.
- Phase 4: the receiver files one event per request with the source's
  provider name and refuses an unknown source.
