# Change: a cron job can pre-approve sensitive egress for named tools (egress_allow)

**Branch:** `feat/cron-egress-allow` · **Status:** implemented, tested, not yet deployed.

## What changed
A scheduled turn has nobody to approve sensitive egress, so on the cron surfaces every
outbound tool call that repeats sensitive data is refused: a word of 8 or more characters,
or a whole line, from a recent sensitive result. That is right for most jobs, and it makes
some impossible. A job that follows a runbook from the wiki with `os_exec`, or writes a
ticket note from an email through an MCP server, repeats its sources by design.

A prompt job can now name the tools it may do that with, in `config.json`:

```json
{"id": "tickets", "kind": "prompt", "exec": true, "spec": "*/20 * * * *",
 "prompt": "...", "egress_allow": ["os_exec", "sdp_*"]}
```

- `egress_allow` holds tool-name globs (`path.Match`). A call to a matching tool whose
  arguments carry sensitive data runs without the confirmation that would have refused it.
  Every other outbound tool is checked as before.
- The flow is still recorded, so the data's provenance follows the call, and each such call
  is logged as `turns 4001` (warning, Egress).
- It is set only in `config.json`. A job added at runtime (`/cron add`, the dashboard, the
  agent's `cron_*` tools) never has it, so the agent cannot grant itself egress.
- It changes only the egress check. The tool's own permission (the exec auto-approver,
  connector trust, tool rules) is still checked at dispatch.

This trades a guard for a stated purpose: a listed job can move what it reads (email,
runbooks, files) into the named tools, and what it reads can try to steer it. Keep the
list to the tools the job exists to use.

## Behavior changes
- None for a job without `egress_allow`.
- Jobs cross between `config.json` and the engine through one pair of functions
  (`cronDefsFromConfig`, `cronJobsFromDefs`) instead of four field-by-field copies, which a
  new field could slip past and be dropped at the next save.

## Evidence
- `internal/agent`: `TestSensitiveEgress_JobAllowlist`. A listed tool runs without asking;
  an unlisted one is still blocked and still asks; no list behaves as before.
- `cmd/tenant`: `TestCronJobConversionKeepsEveryField`. Every job field survives
  config → engine → config.
- `internal/config`: the stable keys gain `cron.jobs[].egress_allow`.
- `docs/EVENTS.md`: `turns 4001`.
