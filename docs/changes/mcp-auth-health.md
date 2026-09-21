# Change: a dead remote-MCP sign-in is loud (69b4f03)

**Branch:** `feat/discord-approval-buttons` · **Status:** implemented, tested, not yet deployed.

## What changed
- `mcpremote` persists `reauth_needed / reauth_error / reauth_since` in the token cache; the
  refreshing source marks it on a hard refresh failure and clears it the moment a valid token is
  in hand (a browser re-auth clears it on the next call). `OnAuthChange` fires **once** per
  dead/recovered transition. `Health`, `ReadHealth` (offline), `TokenCachePath` exported.
- A dead connector's tool error names the condition and the exact fix, so the model — and the
  brief — say it plainly instead of working around an opaque oauth2 error.
- Status surfaces read the live state: TUI `/mcp` ("⚠ NEEDS RE-AUTH since … /mcp add <url>"),
  dashboard Remote services (red badge + **Reconnect**), `tenant doctor` → new "remote MCP auth"
  check (offline, FAIL with the reconnect command).
- Operator notification: `discord.Service.OpenDM` + `NotifyOperator` → a direct DM when a
  connector dies or recovers (plus the feed line).
- Every cron/brief summary carries a re-authorization banner while any connector is dead.

## Why
The Atlassian bridge's refresh token died 2026-09-18. The only trace was one serve.log Warn per
failing call; every status page kept showing "on, N tools" (count frozen from connect time) and
the Daily Brief's Jira section went empty for three days while the operator was remote.

## Evidence
Tests: reauth persisted + announced once + cleared, valid-token clears on the hot path, reauth
text names the fix, `RemoteMCPList` health, late-installed notifier, feed+DM notifier, banner,
doctor check (skip/warn/fail/ok), `OpenDM` wire shape. Suites green.

## Operator notes
- Re-auth still needs the host browser: `tenant mcp connect https://mcp.atlassian.com/v1/mcp`
  (or dashboard → Remote services → Reconnect). Do it once after deploy; the DM confirms recovery.
