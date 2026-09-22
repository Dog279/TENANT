# Change: approvals over iMessage — TEN-230 Phase 2, reconciled with TEN-267

**Branch:** `feat/ten-344-reconcile-main` (first landed on `feat/discord-approval-buttons`, 7ae89f1)
· **Status:** implemented, tested, not yet deployed.

## What changed
- The approval rules are main's TEN-267 `imessageApprover` (`imessageconfirm.go`). A gated action
  texts a prompt with a nonce to the chat the turn came from. Only `Y <nonce>` (or `deny <nonce>`)
  from the configured **operator handle** (`imessage.operator`) resolves it. Nonce-bound,
  approve-once, fail-closed, no session/always grants. This branch's chat-bound `APPROVE-<code>`
  approver (`imessageapprove.go`) is gone.
- Kept from this branch: the responder runs each turn **off** its poll loop (one active turn; a
  second text mid-turn gets a "busy" reply). This is load-bearing: a turn blocked on an approval
  needs the loop to keep draining so the reply can reach the approver. With turns inline, as on
  main alone, the reply is only read after the approval has timed out.
- The responder is the approver's send path, so prompts and echoes are serialized with its
  replies and land in the watcher's echo cache.
- The host mirror carries the broker's own origin ("waiting in imessage", not "discord").

## Why
Approvals go back out the channel they came in on, and only the operator can grant one: an
allowlisted contact can chat but not approve.

## Evidence
main's approver suite; the blocked-turn-released-by-a-later-reply regression, ported to the
operator rule (it fails in ~6s if turns run inline again); mirror origin; responder tests wait for
the async turn. Suites green, `-race` included.

## Operator notes
- Set `imessage.operator` in `config.json` to your phone/email handle. Unset, an `ask` category
  over iMessage prompts at the host (TUI or dashboard) instead, and nothing can be approved by text.
- iMessage categories default to **deny**. Enable per category: `/imessage permissions set exec ask`.
- Live test: text a gated action from an allowed handle; reply `Y <nonce>` from the operator handle.
