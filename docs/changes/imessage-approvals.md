# Change: approvals over iMessage (7ae89f1) — TEN-230 Phase 2

**Branch:** `feat/discord-approval-buttons` · **Status:** implemented, tested, not yet deployed.

## What changed
- `imessageApprover`: the iMessage broker's "ask" backend. A gated action texts a prompt with a
  short code to the chat the turn came from; the operator replies `APPROVE-<code>` or `deny`.
  Nonce-bound, approve-once, fail-closed, no session/always grants, and **origin-bound** — a
  code from another chat is a normal message.
- The responder runs each turn **off** its poll loop (single active turn; a second text mid-turn
  gets a "busy" reply). This is the load-bearing change: a turn blocked on an approval needs the
  loop to keep draining so the reply can reach the approver.
- Wired in both TUI and daemon paths; the host mirror now carries the broker's own origin
  ("waiting in imessage", not "discord").

## Why
Approvals go back out the channel they came in on. Phase 1 refused every gated tool over
iMessage; with categories set to "ask", prompts went to the host TUI the texter is away from.

## Evidence
Tests: nonce approve-once + origin-bound + deny, timeout/no-chat deny, the blocked-turn-released-
by-a-later-reply regression (with the busy reply), mirror origin; existing responder tests
updated for the async turn. Suites green.

## Operator notes
- iMessage categories default to **deny**. Enable per category: `/imessage permissions set exec ask`.
- Live test: text a gated action from an allowed handle; reply `APPROVE-<code>` in that chat.
