# Change: per-tool permission rules — `/permissions add`, hand-editable `tool_rules`, dashboard card

**Branch:** `feat/logs-hub` · **Status:** implemented, tested, not yet deployed.

## What changed
A per-tool **rule layer** on top of the safety categories, so the operator never has to
pre-program a category for every tool people (or the agent) might add. A rule maps a pattern
to `ask` / `allow` / `deny` and is checked **before** the category on every broker (host,
Discord relay, iMessage) and inside the connector gate:

| pattern | matches |
|---|---|
| `write_note` (bare name) | that tool on **any** connector (`mcp:…:write_note`) or the local tool of that name |
| `mcp:127.0.0.1:9000:write_note` (exact id) | one tool on one server |
| `mcp:127.0.0.1:9000:*`, `delete_*`, `*:search_*` (glob) | many; `*` any run, `?` one char |

Most specific wins: exact, then bare name, then glob (longer literal first). Semantics:
`allow` runs silently even in an ask or destructive-class category; `deny` refuses silently
even under a trusted connector; `ask` always prompts, even past a category allow or a session
grant — and "approve always" on such a prompt flips **that rule** to allow, never the category.
Connector trust `deny` still hides a server's tools regardless of rules.

Three ways to manage them, all live:
- **TUI:** `/permissions add <tool|pattern> <mode>`, `/permissions rm <pattern>`; `/permissions`
  (and the relay / iMessage views) list the rules under the categories.
- **Dashboard:** a "Tool rules" card on the Tools page (add / remove).
- **The file:** `settings.<agent>.json` → `"tool_rules": {"delete_*": "deny", "write_note": "allow"}`.
  The running hub polls the file and applies edits within ~3 s (rules **and** the host broker's
  category modes) — a feed line reports what was reloaded. The hub's own saves never re-trigger.

Every rule decision and every rule change is an audit line in the log book's `approvals`
folder. There is deliberately **no agent tool** for rules: the model cannot grant itself
permissions.

## Why
With live tool-server adoption, tools the category table has never heard of arrive at runtime
and fall to `destructive` (always prompt). Per-connector trust answers "this whole server";
rules answer "this tool" — allow-list the safe ones on an `ask` server, deny one dangerous tool
on an `allow` server, or force a prompt on one tool in an `allow` category — from a shell, the
phone, or a text editor, with no restart.

## Evidence
`cmd/tenant`: matching + precedence (exact > name > glob, longer literal), bare names against
connector ids, glob edge cases, validation (catch-all rejected), persist / Load / order; broker:
allow beats ask + destructive, deny beats category allow, ask forces a prompt past category
allow, approve-always flips the rule not the category, bare-name deny on a connector tool,
shared with the relay broker; settings watcher: hand edit applied (rules + modes, live object
kept in sync), unchanged file is a no-op, corrupt file reported not applied.
`mcpremote`: rules before trust (deny beats allow, ask prompts under allow, allow skips under
ask, connector deny still wins). `dashboard`: card hidden without a control, add / remove
forms, validation. Full suites green.

## Operator notes
- Nothing changes until you add a rule. Existing prompts behave exactly as before.
- Rules are shared by every surface; category modes stay per surface (host / relay / iMessage).
- `tool_rules` is plain JSON in `settings.<agent>.json`; a typo'd entry is skipped and counted
  in the feed line, never fatal.
