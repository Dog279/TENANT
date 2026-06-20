# Hermes → Tenant: deploy + rollback runbook (TEN-263 / TEN-264)

Operational runbook for the phased cutover. The iMessage **front door** (which
agent answers the Dylan number) is swung by toggling exactly **one** launchd unit
at a time; Hermes stays installed + bootable for the one-week rollback window.

> **Hard rules** (carried over from the Hermes environment):
> - All test/verification sends go to **Dylan (+15302208314)** — *never* Tyler (+19164121156).
> - No path the launchd unit touches may live under `~/Desktop`, `~/Documents`, `~/Downloads` (TCC).
> - Draft-first on all email; never auto-send.
> - **Never boot a unit out mid-turn.** Drain first (see below).

Placeholders: `$U = $(id -u)`. Units: `com.tyclaw.hermes` (incumbent),
`com.tyclaw.tenant` (new — see `deploy/launchd/com.tenant.serve.plist`).

---

## 0. One-time: install the Tenant unit (TEN-263) — does NOT cut over yet

Tenant runs in **parallel, with no iMessage channel** first, so the loop + memory
are validated on real inference before it touches the front door (TEN-262).

```sh
# Build + place the binary outside the TCC dirs
go build -o tenant ./cmd/tenant && cp tenant {{TENANT_BIN}}

# Install the LaunchAgent (edit the {{PLACEHOLDERS}} first)
cp deploy/launchd/com.tenant.serve.plist ~/Library/LaunchAgents/com.tyclaw.tenant.plist
launchctl bootstrap gui/$U ~/Library/LaunchAgents/com.tyclaw.tenant.plist
launchctl kickstart -k gui/$U/com.tyclaw.tenant
```

Verify it's alive and survives a reboot, **without** touching Hermes:

```sh
launchctl print gui/$U/com.tyclaw.tenant | grep -E 'state|pid|last exit'
tail -f {{LOG_DIR}}/tenant.serve.log          # setup/health/fallback lines
# dashboard up?
curl -fsS http://127.0.0.1:8770/healthz && echo OK
# OR from another box on the tailnet, or:  tenant attach --follow
```

Reboot the Mac → confirm `com.tyclaw.tenant` comes back `RunAtLoad` AND
`com.tyclaw.hermes` still boots independently. **Acceptance (TEN-263):** survives
reboot, restarts on crash, logs to a known path; Hermes unaffected.

---

## 1. Drain check (run before ANY front-door swing)

A turn must not be killed mid-flight. Confirm the *currently-serving* agent is idle:

- **Tenant:** `tenant attach` (or `GET /api/status`) → `turn_active=false`. If true,
  wait for it to clear (the daemon finishes the turn; it won't start a new one if
  you're about to boot it out). Honor the Hermes-side "idle 5+ min, never mid-turn"
  policy when Hermes is the one serving.
- **Hermes:** confirm no in-flight turn per its own status/log before booting it out.

---

## 2. CUTOVER — swing the front door to Tenant

Only after TEN-262 (live loop) + TEN-265/266/267 (iMessage hardening) + the
TEN-287 canary pass on the Dylan number.

```sh
# 2a. Drain (section 1). Then take Hermes off the iMessage front door:
launchctl bootout gui/$U/com.tyclaw.hermes          # or disable just its imsg responder

# 2b. Enable Tenant's iMessage channel (it was parallel/no-channel until now).
#     Restart the Tenant unit with the iMessage responder enabled (per your
#     serve flags / config), then:
launchctl kickstart -k gui/$U/com.tyclaw.tenant

# 2c. Verify on the DYLAN number only:
#     send a test message → Tenant answers (tapback ack within ~1s, no echo loop).
```

---

## 3. ROLLBACK — swing the front door back to Hermes (TEN-264)

Target: **< 2 min, no message loss** on the Dylan number.

```sh
# 3a. Drain Tenant (section 1): tenant attach → turn_active=false.
# 3b. Take Tenant off the front door (leave the unit installed for re-cutover,
#     OR bootout to fully stop it):
launchctl bootout gui/$U/com.tyclaw.tenant
# 3c. Bring Hermes back as the responder:
launchctl bootstrap gui/$U ~/Library/LaunchAgents/com.tyclaw.hermes.plist
launchctl kickstart -k gui/$U/com.tyclaw.hermes
# 3d. Verify: send a Dylan-number test → Hermes answers.
```

**No message loss:** inbound iMessages persist in `chat.db` regardless of which
agent is up; the responder that's live reads from its ROWID cursor on start, so a
message that arrives during the ~seconds-long swap is picked up by whichever agent
comes up — it is queued in chat.db, not dropped. Keep the swap inside one drain
window so a single inbound isn't answered by *both*.

**Dry-run the revert before cutover** and time it — this is the TEN-264 acceptance
(< 2 min, zero message loss on the Dylan number).

---

## 4. Decommission (after the 1-week rollback window holds)

```sh
launchctl bootout gui/$U/com.tyclaw.hermes        # stop Hermes
# (optionally) rm ~/Library/LaunchAgents/com.tyclaw.hermes.plist
```
Keep the Hermes install + its `state.db` archived until you're certain; per
**TEN-280** the migration is a cold-start, so Hermes history is your only copy of
the pre-cutover sessions.

---

### Status notes
- **TEN-263 / TEN-264 are operator-execution tickets.** This repo provides the
  launchd template + this runbook; the acceptance (survives reboot / dry-run revert
  < 2 min, no message loss) must be **verified live on the Mac** before cutover.
