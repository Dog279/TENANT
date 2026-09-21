# Change: Discord approval cards (adc22e9)

**Branch:** `feat/discord-approval-buttons` · **Status:** implemented, tested, not yet deployed.

## What changed
The Discord approve/deny flow (TEN-117 buttons) now behaves like a proper card:
- Components-v2 container card — green **Approve** / red **Deny**, emoji on the button's `emoji`
  field, no nonce clutter. Falls back to the legacy button row if Discord rejects the v2 post,
  then to a plain-text nonce prompt; every degradation is logged and the last one is a feed line.
- Every resolution edits the card **in place**: verdict + who/when, action kept as the record,
  buttons disabled. Expiry edits the original (no orphaned live buttons). Text replies freeze it.
- Clicks are ACKed **first** under a bounded ctx (Discord's 3 s window); non-operator and unknown
  clicks get an ephemeral reply instead of "This interaction failed"; the gateway dispatches
  `INTERACTION_CREATE` off the read loop.
- Timeout 5 m → **10 m**, tunable: `relay.approval_timeout` / `/relay timeout <dur>` (30 s–24 h).
  `relay.card_style: legacy` forces the old layout.
- The broker logs every approval decision (auto-allowed / pending / resolved with origin,
  category, outcome, latency) — approval history now survives a restart.
- The TUI's mirror of a Discord-owned approval renders as "⏳ waiting in discord — resolve it
  there" instead of a dead Approve/Deny card.

## Why
Buttons already existed; the *behavior* was the problem: prompts expired before a tap, live
buttons lingered, resolved cards lost their context, un-ACKed clicks showed "interaction failed",
and the TUI showed an unlabeled, unresolvable mirror card — which read as "the prompt went to the
TUI instead of Discord."

## Evidence
Tests fail-before/pass-after: card layout, expiry-freeze, fallback chain (+ feed line),
ACK-before-release, text-reply freeze, timeout knob, ephemeral ACKs, gateway off-read-loop
(proven by stashing `gateway.go`). Full `cmd/tenant`, `discord`, `tui` suites green.

## Operator notes
- The box's `relay.permissions` is currently ALL "allow" — buttons only fire for a category in
  "ask". After deploy: `/relay permissions set exec ask` (recommended also `destructive`, `send`).
- Live test: trigger a gated exec from Discord; the card should post in the originating channel.
