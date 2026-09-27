# Event reference

<!-- Generated from the event catalog: go test ./cmd/tenant -run TestEventsDoc -update. Do not edit by hand. -->

Every catalogued event Tenant writes: what it means, why it happens and what to
do about it. The definitions live in the code, in `internal/logs/catalog` and in
each package that declares a provider. This file, the dashboard's Help tab and
`tenant logs catalog` are all rendered from them. To declare an event, see
[LOGGING.md](LOGGING.md#event-ids-the-reference-and-coverage).

IDs are unique within their provider and never reused: a retired ID stays
here, marked retired. They are assigned in blocks: 1–999 lifecycle, 1000s
operations, 2000s warnings, 3000s errors, 4000s security and audit, 5000s
configuration, 9000s diagnostics.

## plugin.atlassian

Writes the `plugin.atlassian` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1](#pluginatlassian-1) | info | Lifecycle | `Jira started` |
| [2](#pluginatlassian-2) | info | Lifecycle | `{target} enabled` |
| [3](#pluginatlassian-3) | info | Lifecycle | `{target} disabled` |
| [1101](#pluginatlassian-1101) | info | Auth | `Jira OAuth token refreshed` |
| [2001](#pluginatlassian-2001) | warning | API | `Jira rate limit reached on {op}: HTTP {status}` |
| [2101](#pluginatlassian-2101) | warning | Auth | `Jira's rotated refresh token not saved: {err}` |
| [3001](#pluginatlassian-3001) | error | Lifecycle | `Jira could not start: {err}` |
| [3002](#pluginatlassian-3002) | error | API | `Jira refused the credentials on {op}: HTTP {status}` |
| [3003](#pluginatlassian-3003) | error | API | `Jira API call {op} failed: {err}` |
| [3101](#pluginatlassian-3101) | error | Auth | `Jira OAuth token refresh failed: {err}` |

### plugin.atlassian 1

`Jira started`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.atlassian
- **Find it:** `provider:plugin.atlassian id:1`

**What it means.** The plugin was built and its tools can run.

**Why it happens.** Tenant started with the plugin configured, or the operator enabled it for the first time since.

**What to do.** None.

### plugin.atlassian 2

`{target} enabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.atlassian
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.atlassian id:2`

**What it means.** The operator turned the plugin, or one of its tools, on.

**Why it happens.** An /enable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.atlassian 3

`{target} disabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.atlassian
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.atlassian id:3`

**What it means.** The operator turned the plugin, or one of its tools, off. The agent can't call it until it is enabled again.

**Why it happens.** A /disable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.atlassian 1101

`Jira OAuth token refreshed`

- **Level:** info · **Channel:** operational · **Task:** Auth · **Log:** plugin.atlassian
- **Find it:** `provider:plugin.atlassian id:1101`

**What it means.** The OAuth access token expired and was renewed; the rotated refresh token was saved.

**Why it happens.** Atlassian access tokens last an hour.

**What to do.** None.

### plugin.atlassian 2001

`Jira rate limit reached on {op}: HTTP {status}`

- **Level:** warning · **Channel:** operational · **Task:** API · **Log:** plugin.atlassian
- **Fields:** `op`, `status`, `retry`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.atlassian id:2001`

**What it means.** The service refused a call because the account used up its request quota.

**Why it happens.** The agent made many calls in a short time, or something else shares the account's quota.

**What to do.** Wait until the quota resets (retry holds the service's Retry-After, when it sent one). If it keeps happening, use the tool less or raise the account's quota.

### plugin.atlassian 2101

`Jira's rotated refresh token not saved: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Auth · **Log:** plugin.atlassian
- **Fields:** `err`
- **Find it:** `provider:plugin.atlassian id:2101`

**What it means.** Atlassian rotated the refresh token, and the new one could not be written, so the call that refreshed it failed and the saved token may no longer work.

**Why it happens.** The token cache or its directory can't be written: permissions or a full disk.

**What to do.** Fix what the error names. If Jira calls then fail to refresh, sign in again with `tenant atlassian login`.

### plugin.atlassian 3001

`Jira could not start: {err}`

- **Level:** error · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.atlassian
- **Fields:** `err`
- **Find it:** `provider:plugin.atlassian id:3001`

**What it means.** The plugin could not be built. At launch, Tenant stops with this error; on an /enable, the plugin stays off.

**Why it happens.** Its credentials are missing, expired or revoked, its service or program can't be reached, or a setting is wrong.

**What to do.** Read the error. Run `tenant doctor`, fix the setting or sign in again with /configure, then /enable the plugin.

### plugin.atlassian 3002

`Jira refused the credentials on {op}: HTTP {status}`

- **Level:** error · **Channel:** operational · **Task:** API · **Log:** plugin.atlassian
- **Fields:** `op`, `status`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.atlassian id:3002`

**What it means.** The service refused a call as unauthorized, so the tool failed.

**Why it happens.** The token or key expired or was revoked, or it lacks the scope or access this call needs.

**What to do.** Check the plugin's credentials: /configure it again, or paste a new key on the dashboard's Keys page.

### plugin.atlassian 3003

`Jira API call {op} failed: {err}`

- **Level:** error · **Channel:** operational · **Task:** API · **Log:** plugin.atlassian
- **Fields:** `op`, `status`, `err`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.atlassian id:3003`

**What it means.** A call to the service failed on its side or on the way, so the tool failed. status is 0 when no answer came.

**Why it happens.** The service had an outage or an error, or the network, DNS or a proxy failed.

**What to do.** Retry later. If it persists, check the service's status page and this machine's connection.

### plugin.atlassian 3101

`Jira OAuth token refresh failed: {err}`

- **Level:** error · **Channel:** operational · **Task:** Auth · **Log:** plugin.atlassian
- **Fields:** `err`
- **Find it:** `provider:plugin.atlassian id:3101`

**What it means.** The access token could not be renewed, so Jira calls fail.

**Why it happens.** The refresh token was revoked, expired after 90 days unused, or the OAuth app's secret changed.

**What to do.** Sign in again with `tenant atlassian login`, then /enable atlassian.

## plugin.crm

Writes the `plugin.crm` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1](#plugincrm-1) | info | Lifecycle | `CRM started` |
| [2](#plugincrm-2) | info | Lifecycle | `{target} enabled` |
| [3](#plugincrm-3) | info | Lifecycle | `{target} disabled` |
| [3001](#plugincrm-3001) | error | Lifecycle | `CRM could not start: {err}` |
| [3101](#plugincrm-3101) | error | Tool | `crm-tool {subcommand} could not run: {err}` |

### plugin.crm 1

`CRM started`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.crm
- **Find it:** `provider:plugin.crm id:1`

**What it means.** The plugin was built and its tools can run.

**Why it happens.** Tenant started with the plugin configured, or the operator enabled it for the first time since.

**What to do.** None.

### plugin.crm 2

`{target} enabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.crm
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.crm id:2`

**What it means.** The operator turned the plugin, or one of its tools, on.

**Why it happens.** An /enable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.crm 3

`{target} disabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.crm
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.crm id:3`

**What it means.** The operator turned the plugin, or one of its tools, off. The agent can't call it until it is enabled again.

**Why it happens.** A /disable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.crm 3001

`CRM could not start: {err}`

- **Level:** error · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.crm
- **Fields:** `err`
- **Find it:** `provider:plugin.crm id:3001`

**What it means.** The plugin could not be built. At launch, Tenant stops with this error; on an /enable, the plugin stays off.

**Why it happens.** Its credentials are missing, expired or revoked, its service or program can't be reached, or a setting is wrong.

**What to do.** Read the error. Run `tenant doctor`, fix the setting or sign in again with /configure, then /enable the plugin.

### plugin.crm 3101

`crm-tool {subcommand} could not run: {err}`

- **Level:** error · **Channel:** operational · **Task:** Tool · **Log:** plugin.crm
- **Fields:** `subcommand`, `err`
- **Find it:** `provider:plugin.crm id:3101`

**What it means.** The external crm-tool did not run to completion, so the CRM tool failed. A run that exits with an error is the tool's answer, and is not logged.

**Why it happens.** The binary is missing or not executable, or it ran past its timeout.

**What to do.** Check --crm-tool-path (or $CRM_TOOL_PATH), and run the tool by hand to see where it hangs.

## plugin.github

Writes the `plugin.github` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1](#plugingithub-1) | info | Lifecycle | `GitHub started` |
| [2](#plugingithub-2) | info | Lifecycle | `{target} enabled` |
| [3](#plugingithub-3) | info | Lifecycle | `{target} disabled` |
| [2001](#plugingithub-2001) | warning | API | `GitHub rate limit reached on {op}: HTTP {status}` |
| [3001](#plugingithub-3001) | error | Lifecycle | `GitHub could not start: {err}` |
| [3002](#plugingithub-3002) | error | API | `GitHub refused the credentials on {op}: HTTP {status}` |
| [3003](#plugingithub-3003) | error | API | `GitHub API call {op} failed: {err}` |

### plugin.github 1

`GitHub started`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.github
- **Find it:** `provider:plugin.github id:1`

**What it means.** The plugin was built and its tools can run.

**Why it happens.** Tenant started with the plugin configured, or the operator enabled it for the first time since.

**What to do.** None.

### plugin.github 2

`{target} enabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.github
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.github id:2`

**What it means.** The operator turned the plugin, or one of its tools, on.

**Why it happens.** An /enable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.github 3

`{target} disabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.github
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.github id:3`

**What it means.** The operator turned the plugin, or one of its tools, off. The agent can't call it until it is enabled again.

**Why it happens.** A /disable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.github 2001

`GitHub rate limit reached on {op}: HTTP {status}`

- **Level:** warning · **Channel:** operational · **Task:** API · **Log:** plugin.github
- **Fields:** `op`, `status`, `retry`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.github id:2001`

**What it means.** The service refused a call because the account used up its request quota.

**Why it happens.** The agent made many calls in a short time, or something else shares the account's quota.

**What to do.** Wait until the quota resets (retry holds the service's Retry-After, when it sent one). If it keeps happening, use the tool less or raise the account's quota.

### plugin.github 3001

`GitHub could not start: {err}`

- **Level:** error · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.github
- **Fields:** `err`
- **Find it:** `provider:plugin.github id:3001`

**What it means.** The plugin could not be built. At launch, Tenant stops with this error; on an /enable, the plugin stays off.

**Why it happens.** Its credentials are missing, expired or revoked, its service or program can't be reached, or a setting is wrong.

**What to do.** Read the error. Run `tenant doctor`, fix the setting or sign in again with /configure, then /enable the plugin.

### plugin.github 3002

`GitHub refused the credentials on {op}: HTTP {status}`

- **Level:** error · **Channel:** operational · **Task:** API · **Log:** plugin.github
- **Fields:** `op`, `status`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.github id:3002`

**What it means.** The service refused a call as unauthorized, so the tool failed.

**Why it happens.** The token or key expired or was revoked, or it lacks the scope or access this call needs.

**What to do.** Check the plugin's credentials: /configure it again, or paste a new key on the dashboard's Keys page.

### plugin.github 3003

`GitHub API call {op} failed: {err}`

- **Level:** error · **Channel:** operational · **Task:** API · **Log:** plugin.github
- **Fields:** `op`, `status`, `err`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.github id:3003`

**What it means.** A call to the service failed on its side or on the way, so the tool failed. status is 0 when no answer came.

**Why it happens.** The service had an outage or an error, or the network, DNS or a proxy failed.

**What to do.** Retry later. If it persists, check the service's status page and this machine's connection.

## plugin.gsuite

Writes the `plugin.gsuite` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1](#plugingsuite-1) | info | Lifecycle | `Google Workspace started` |
| [2](#plugingsuite-2) | info | Lifecycle | `{target} enabled` |
| [3](#plugingsuite-3) | info | Lifecycle | `{target} disabled` |
| [1101](#plugingsuite-1101) | info | Auth | `Google access token refreshed; valid until {expires}` |
| [2001](#plugingsuite-2001) | warning | API | `Google Workspace rate limit reached on {op}: HTTP {status}` |
| [2101](#plugingsuite-2101) | warning | Auth | `Rotated Google OAuth token not saved; a restart may need a new sign-in: {err}` |
| [3001](#plugingsuite-3001) | error | Lifecycle | `Google Workspace could not start: {err}` |
| [3002](#plugingsuite-3002) | error | API | `Google Workspace refused the credentials on {op}: HTTP {status}` |
| [3003](#plugingsuite-3003) | error | API | `Google Workspace API call {op} failed: {err}` |
| [3101](#plugingsuite-3101) | error | Auth | `Google access token refresh failed: {err}` |

### plugin.gsuite 1

`Google Workspace started`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.gsuite
- **Find it:** `provider:plugin.gsuite id:1`

**What it means.** The plugin was built and its tools can run.

**Why it happens.** Tenant started with the plugin configured, or the operator enabled it for the first time since.

**What to do.** None.

### plugin.gsuite 2

`{target} enabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.gsuite
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.gsuite id:2`

**What it means.** The operator turned the plugin, or one of its tools, on.

**Why it happens.** An /enable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.gsuite 3

`{target} disabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.gsuite
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.gsuite id:3`

**What it means.** The operator turned the plugin, or one of its tools, off. The agent can't call it until it is enabled again.

**Why it happens.** A /disable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.gsuite 1101

`Google access token refreshed; valid until {expires}`

- **Level:** info · **Channel:** operational · **Task:** Auth · **Log:** plugin.gsuite
- **Fields:** `expires`
- **Find it:** `provider:plugin.gsuite id:1101`

**What it means.** A new access token was minted for the Gmail, Calendar and Drive calls.

**Why it happens.** The previous one was about to expire, or this is the first call since the plugin started.

**What to do.** None.

### plugin.gsuite 2001

`Google Workspace rate limit reached on {op}: HTTP {status}`

- **Level:** warning · **Channel:** operational · **Task:** API · **Log:** plugin.gsuite
- **Fields:** `op`, `status`, `retry`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.gsuite id:2001`

**What it means.** The service refused a call because the account used up its request quota.

**Why it happens.** The agent made many calls in a short time, or something else shares the account's quota.

**What to do.** Wait until the quota resets (retry holds the service's Retry-After, when it sent one). If it keeps happening, use the tool less or raise the account's quota.

### plugin.gsuite 2101

`Rotated Google OAuth token not saved; a restart may need a new sign-in: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Auth · **Log:** plugin.gsuite
- **Fields:** `err`
- **Find it:** `provider:plugin.gsuite id:2101`

**What it means.** Google rotated the refresh token, and the new one could not be written to the credentials. The plugin keeps working until Tenant restarts.

**Why it happens.** The credentials file or its directory can't be written: permissions, a full disk, or a lock another process holds.

**What to do.** Fix what the error names; the save is retried on the next refresh. After a restart that finds the old token refused, run /configure gsuite again.

### plugin.gsuite 3001

`Google Workspace could not start: {err}`

- **Level:** error · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.gsuite
- **Fields:** `err`
- **Find it:** `provider:plugin.gsuite id:3001`

**What it means.** The plugin could not be built. At launch, Tenant stops with this error; on an /enable, the plugin stays off.

**Why it happens.** Its credentials are missing, expired or revoked, its service or program can't be reached, or a setting is wrong.

**What to do.** Read the error. Run `tenant doctor`, fix the setting or sign in again with /configure, then /enable the plugin.

### plugin.gsuite 3002

`Google Workspace refused the credentials on {op}: HTTP {status}`

- **Level:** error · **Channel:** operational · **Task:** API · **Log:** plugin.gsuite
- **Fields:** `op`, `status`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.gsuite id:3002`

**What it means.** The service refused a call as unauthorized, so the tool failed.

**Why it happens.** The token or key expired or was revoked, or it lacks the scope or access this call needs.

**What to do.** Check the plugin's credentials: /configure it again, or paste a new key on the dashboard's Keys page.

### plugin.gsuite 3003

`Google Workspace API call {op} failed: {err}`

- **Level:** error · **Channel:** operational · **Task:** API · **Log:** plugin.gsuite
- **Fields:** `op`, `status`, `err`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.gsuite id:3003`

**What it means.** A call to the service failed on its side or on the way, so the tool failed. status is 0 when no answer came.

**Why it happens.** The service had an outage or an error, or the network, DNS or a proxy failed.

**What to do.** Retry later. If it persists, check the service's status page and this machine's connection.

### plugin.gsuite 3101

`Google access token refresh failed: {err}`

- **Level:** error · **Channel:** operational · **Task:** Auth · **Log:** plugin.gsuite
- **Fields:** `err`
- **Find it:** `provider:plugin.gsuite id:3101`

**What it means.** No access token could be minted, so every Gmail, Calendar and Drive call fails.

**Why it happens.** The OAuth refresh token was revoked or expired, the service account's key or delegation changed, or gcloud is signed out.

**What to do.** Sign in again with /configure gsuite (for gcloud mode, `gcloud auth login`); `tenant doctor` checks the setup.

## plugin.os

Writes the `plugin.os` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1](#pluginos-1) | info | Lifecycle | `OS started` |
| [2](#pluginos-2) | info | Lifecycle | `{target} enabled` |
| [3](#pluginos-3) | info | Lifecycle | `{target} disabled` |
| [3001](#pluginos-3001) | error | Lifecycle | `OS could not start: {err}` |

### plugin.os 1

`OS started`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.os
- **Find it:** `provider:plugin.os id:1`

**What it means.** The plugin was built and its tools can run.

**Why it happens.** Tenant started with the plugin configured, or the operator enabled it for the first time since.

**What to do.** None.

### plugin.os 2

`{target} enabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.os
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.os id:2`

**What it means.** The operator turned the plugin, or one of its tools, on.

**Why it happens.** An /enable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.os 3

`{target} disabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.os
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.os id:3`

**What it means.** The operator turned the plugin, or one of its tools, off. The agent can't call it until it is enabled again.

**Why it happens.** A /disable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.os 3001

`OS could not start: {err}`

- **Level:** error · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.os
- **Fields:** `err`
- **Find it:** `provider:plugin.os id:3001`

**What it means.** The plugin could not be built. At launch, Tenant stops with this error; on an /enable, the plugin stays off.

**Why it happens.** Its credentials are missing, expired or revoked, its service or program can't be reached, or a setting is wrong.

**What to do.** Read the error. Run `tenant doctor`, fix the setting or sign in again with /configure, then /enable the plugin.

## plugin.simplefin

Writes the `plugin.simplefin` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1](#pluginsimplefin-1) | info | Lifecycle | `SimpleFIN started` |
| [2](#pluginsimplefin-2) | info | Lifecycle | `{target} enabled` |
| [3](#pluginsimplefin-3) | info | Lifecycle | `{target} disabled` |
| [2001](#pluginsimplefin-2001) | warning | API | `SimpleFIN rate limit reached on {op}: HTTP {status}` |
| [3001](#pluginsimplefin-3001) | error | Lifecycle | `SimpleFIN could not start: {err}` |
| [3002](#pluginsimplefin-3002) | error | API | `SimpleFIN refused the credentials on {op}: HTTP {status}` |
| [3003](#pluginsimplefin-3003) | error | API | `SimpleFIN API call {op} failed: {err}` |

### plugin.simplefin 1

`SimpleFIN started`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.simplefin
- **Find it:** `provider:plugin.simplefin id:1`

**What it means.** The plugin was built and its tools can run.

**Why it happens.** Tenant started with the plugin configured, or the operator enabled it for the first time since.

**What to do.** None.

### plugin.simplefin 2

`{target} enabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.simplefin
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.simplefin id:2`

**What it means.** The operator turned the plugin, or one of its tools, on.

**Why it happens.** An /enable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.simplefin 3

`{target} disabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.simplefin
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.simplefin id:3`

**What it means.** The operator turned the plugin, or one of its tools, off. The agent can't call it until it is enabled again.

**Why it happens.** A /disable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.simplefin 2001

`SimpleFIN rate limit reached on {op}: HTTP {status}`

- **Level:** warning · **Channel:** operational · **Task:** API · **Log:** plugin.simplefin
- **Fields:** `op`, `status`, `retry`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.simplefin id:2001`

**What it means.** The service refused a call because the account used up its request quota.

**Why it happens.** The agent made many calls in a short time, or something else shares the account's quota.

**What to do.** Wait until the quota resets (retry holds the service's Retry-After, when it sent one). If it keeps happening, use the tool less or raise the account's quota.

### plugin.simplefin 3001

`SimpleFIN could not start: {err}`

- **Level:** error · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.simplefin
- **Fields:** `err`
- **Find it:** `provider:plugin.simplefin id:3001`

**What it means.** The plugin could not be built. At launch, Tenant stops with this error; on an /enable, the plugin stays off.

**Why it happens.** Its credentials are missing, expired or revoked, its service or program can't be reached, or a setting is wrong.

**What to do.** Read the error. Run `tenant doctor`, fix the setting or sign in again with /configure, then /enable the plugin.

### plugin.simplefin 3002

`SimpleFIN refused the credentials on {op}: HTTP {status}`

- **Level:** error · **Channel:** operational · **Task:** API · **Log:** plugin.simplefin
- **Fields:** `op`, `status`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.simplefin id:3002`

**What it means.** The service refused a call as unauthorized, so the tool failed.

**Why it happens.** The token or key expired or was revoked, or it lacks the scope or access this call needs.

**What to do.** Check the plugin's credentials: /configure it again, or paste a new key on the dashboard's Keys page.

### plugin.simplefin 3003

`SimpleFIN API call {op} failed: {err}`

- **Level:** error · **Channel:** operational · **Task:** API · **Log:** plugin.simplefin
- **Fields:** `op`, `status`, `err`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.simplefin id:3003`

**What it means.** A call to the service failed on its side or on the way, so the tool failed. status is 0 when no answer came.

**Why it happens.** The service had an outage or an error, or the network, DNS or a proxy failed.

**What to do.** Retry later. If it persists, check the service's status page and this machine's connection.

## plugin.sql

Writes the `plugin.sql` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1](#pluginsql-1) | info | Lifecycle | `SQL started` |
| [2](#pluginsql-2) | info | Lifecycle | `{target} enabled` |
| [3](#pluginsql-3) | info | Lifecycle | `{target} disabled` |
| [3001](#pluginsql-3001) | error | Lifecycle | `SQL could not start: {err}` |
| [3101](#pluginsql-3101) | error | Database | `SQL database schema unreadable: {err}` |

### plugin.sql 1

`SQL started`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.sql
- **Find it:** `provider:plugin.sql id:1`

**What it means.** The plugin was built and its tools can run.

**Why it happens.** Tenant started with the plugin configured, or the operator enabled it for the first time since.

**What to do.** None.

### plugin.sql 2

`{target} enabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.sql
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.sql id:2`

**What it means.** The operator turned the plugin, or one of its tools, on.

**Why it happens.** An /enable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.sql 3

`{target} disabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.sql
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.sql id:3`

**What it means.** The operator turned the plugin, or one of its tools, off. The agent can't call it until it is enabled again.

**Why it happens.** A /disable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.sql 3001

`SQL could not start: {err}`

- **Level:** error · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.sql
- **Fields:** `err`
- **Find it:** `provider:plugin.sql id:3001`

**What it means.** The plugin could not be built. At launch, Tenant stops with this error; on an /enable, the plugin stays off.

**Why it happens.** Its credentials are missing, expired or revoked, its service or program can't be reached, or a setting is wrong.

**What to do.** Read the error. Run `tenant doctor`, fix the setting or sign in again with /configure, then /enable the plugin.

### plugin.sql 3101

`SQL database schema unreadable: {err}`

- **Level:** error · **Channel:** operational · **Task:** Database · **Log:** plugin.sql
- **Fields:** `err`
- **Find it:** `provider:plugin.sql id:3101`

**What it means.** The database's schema could not be read, so the agent can't see its tables.

**Why it happens.** The file was moved, deleted, locked by another program, or is corrupt.

**What to do.** Check the database file named by --sql-db or /configure sql; `sqlite3 <file> 'PRAGMA integrity_check'` tests it.

## plugin.web

Writes the `plugin.web` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1](#pluginweb-1) | info | Lifecycle | `Web started` |
| [2](#pluginweb-2) | info | Lifecycle | `{target} enabled` |
| [3](#pluginweb-3) | info | Lifecycle | `{target} disabled` |
| [2001](#pluginweb-2001) | warning | API | `Web rate limit reached on {op}: HTTP {status}` |
| [3001](#pluginweb-3001) | error | Lifecycle | `Web could not start: {err}` |
| [3002](#pluginweb-3002) | error | API | `Web refused the credentials on {op}: HTTP {status}` |
| [3003](#pluginweb-3003) | error | API | `Web API call {op} failed: {err}` |

### plugin.web 1

`Web started`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.web
- **Find it:** `provider:plugin.web id:1`

**What it means.** The plugin was built and its tools can run.

**Why it happens.** Tenant started with the plugin configured, or the operator enabled it for the first time since.

**What to do.** None.

### plugin.web 2

`{target} enabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.web
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.web id:2`

**What it means.** The operator turned the plugin, or one of its tools, on.

**Why it happens.** An /enable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.web 3

`{target} disabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.web
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.web id:3`

**What it means.** The operator turned the plugin, or one of its tools, off. The agent can't call it until it is enabled again.

**Why it happens.** A /disable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.web 2001

`Web rate limit reached on {op}: HTTP {status}`

- **Level:** warning · **Channel:** operational · **Task:** API · **Log:** plugin.web
- **Fields:** `op`, `status`, `retry`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.web id:2001`

**What it means.** The service refused a call because the account used up its request quota.

**Why it happens.** The agent made many calls in a short time, or something else shares the account's quota.

**What to do.** Wait until the quota resets (retry holds the service's Retry-After, when it sent one). If it keeps happening, use the tool less or raise the account's quota.

### plugin.web 3001

`Web could not start: {err}`

- **Level:** error · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.web
- **Fields:** `err`
- **Find it:** `provider:plugin.web id:3001`

**What it means.** The plugin could not be built. At launch, Tenant stops with this error; on an /enable, the plugin stays off.

**Why it happens.** Its credentials are missing, expired or revoked, its service or program can't be reached, or a setting is wrong.

**What to do.** Read the error. Run `tenant doctor`, fix the setting or sign in again with /configure, then /enable the plugin.

### plugin.web 3002

`Web refused the credentials on {op}: HTTP {status}`

- **Level:** error · **Channel:** operational · **Task:** API · **Log:** plugin.web
- **Fields:** `op`, `status`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.web id:3002`

**What it means.** The service refused a call as unauthorized, so the tool failed.

**Why it happens.** The token or key expired or was revoked, or it lacks the scope or access this call needs.

**What to do.** Check the plugin's credentials: /configure it again, or paste a new key on the dashboard's Keys page.

### plugin.web 3003

`Web API call {op} failed: {err}`

- **Level:** error · **Channel:** operational · **Task:** API · **Log:** plugin.web
- **Fields:** `op`, `status`, `err`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.web id:3003`

**What it means.** A call to the service failed on its side or on the way, so the tool failed. status is 0 when no answer came.

**Why it happens.** The service had an outage or an error, or the network, DNS or a proxy failed.

**What to do.** Retry later. If it persists, check the service's status page and this machine's connection.

## plugin.wiki

Writes the `plugin.wiki` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1](#pluginwiki-1) | info | Lifecycle | `Wiki started` |
| [2](#pluginwiki-2) | info | Lifecycle | `{target} enabled` |
| [3](#pluginwiki-3) | info | Lifecycle | `{target} disabled` |
| [2101](#pluginwiki-2101) | warning | Index | `Wiki embedder unreachable at start; using the declared dimension {declared_dim}: {err}` |
| [2102](#pluginwiki-2102) | warning | Index | `Wiki initial reindex failed; it reindexes on the first search: {err}` |
| [3001](#pluginwiki-3001) | error | Lifecycle | `Wiki could not start: {err}` |

### plugin.wiki 1

`Wiki started`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.wiki
- **Find it:** `provider:plugin.wiki id:1`

**What it means.** The plugin was built and its tools can run.

**Why it happens.** Tenant started with the plugin configured, or the operator enabled it for the first time since.

**What to do.** None.

### plugin.wiki 2

`{target} enabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.wiki
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.wiki id:2`

**What it means.** The operator turned the plugin, or one of its tools, on.

**Why it happens.** An /enable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.wiki 3

`{target} disabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.wiki
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.wiki id:3`

**What it means.** The operator turned the plugin, or one of its tools, off. The agent can't call it until it is enabled again.

**Why it happens.** A /disable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.wiki 2101

`Wiki embedder unreachable at start; using the declared dimension {declared_dim}: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Index · **Log:** plugin.wiki
- **Fields:** `declared_dim`, `err`
- **Find it:** `provider:plugin.wiki id:2101`

**What it means.** The embedding model did not answer when the wiki started, so the index uses the dimension its profile declares and reindexes on the first search.

**Why it happens.** The embedder's server is down, still loading, or unreachable.

**What to do.** Check the embedder role's backend (`tenant doctor`). The wiki heals on its own once it answers.

### plugin.wiki 2102

`Wiki initial reindex failed; it reindexes on the first search: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Index · **Log:** plugin.wiki
- **Fields:** `err`
- **Find it:** `provider:plugin.wiki id:2102`

**What it means.** The notes changed since the index was saved, and they could not be embedded at start, so searches may miss them until the first search reindexes.

**Why it happens.** The embedder is down, or a note could not be read.

**What to do.** Check the embedder role's backend; or run `tenant wiki reindex` once it answers.

### plugin.wiki 3001

`Wiki could not start: {err}`

- **Level:** error · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.wiki
- **Fields:** `err`
- **Find it:** `provider:plugin.wiki id:3001`

**What it means.** The plugin could not be built. At launch, Tenant stops with this error; on an /enable, the plugin stays off.

**Why it happens.** Its credentials are missing, expired or revoked, its service or program can't be reached, or a setting is wrong.

**What to do.** Read the error. Run `tenant doctor`, fix the setting or sign in again with /configure, then /enable the plugin.

## plugin.x

Writes the `plugin.x` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1](#pluginx-1) | info | Lifecycle | `X started` |
| [2](#pluginx-2) | info | Lifecycle | `{target} enabled` |
| [3](#pluginx-3) | info | Lifecycle | `{target} disabled` |
| [1101](#pluginx-1101) | info | Auth | `X user token refreshed` |
| [2001](#pluginx-2001) | warning | API | `X rate limit reached on {op}: HTTP {status}` |
| [3001](#pluginx-3001) | error | Lifecycle | `X could not start: {err}` |
| [3002](#pluginx-3002) | error | API | `X refused the credentials on {op}: HTTP {status}` |
| [3003](#pluginx-3003) | error | API | `X API call {op} failed: {err}` |
| [3101](#pluginx-3101) | error | Auth | `X user token refresh failed: {err}` |

### plugin.x 1

`X started`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.x
- **Find it:** `provider:plugin.x id:1`

**What it means.** The plugin was built and its tools can run.

**Why it happens.** Tenant started with the plugin configured, or the operator enabled it for the first time since.

**What to do.** None.

### plugin.x 2

`{target} enabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.x
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.x id:2`

**What it means.** The operator turned the plugin, or one of its tools, on.

**Why it happens.** An /enable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.x 3

`{target} disabled`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.x
- **Fields:** `target`, `tools`
- **Find it:** `provider:plugin.x id:3`

**What it means.** The operator turned the plugin, or one of its tools, off. The agent can't call it until it is enabled again.

**Why it happens.** A /disable in the terminal, or the Tools page of the dashboard.

**What to do.** None.

### plugin.x 1101

`X user token refreshed`

- **Level:** info · **Channel:** operational · **Task:** Auth · **Log:** plugin.x
- **Find it:** `provider:plugin.x id:1101`

**What it means.** The user-context access token expired and was renewed with the refresh token, which X rotates; both were saved.

**Why it happens.** X access tokens last about two hours.

**What to do.** None.

### plugin.x 2001

`X rate limit reached on {op}: HTTP {status}`

- **Level:** warning · **Channel:** operational · **Task:** API · **Log:** plugin.x
- **Fields:** `op`, `status`, `retry`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.x id:2001`

**What it means.** The service refused a call because the account used up its request quota.

**Why it happens.** The agent made many calls in a short time, or something else shares the account's quota.

**What to do.** Wait until the quota resets (retry holds the service's Retry-After, when it sent one). If it keeps happening, use the tool less or raise the account's quota.

### plugin.x 3001

`X could not start: {err}`

- **Level:** error · **Channel:** operational · **Task:** Lifecycle · **Log:** plugin.x
- **Fields:** `err`
- **Find it:** `provider:plugin.x id:3001`

**What it means.** The plugin could not be built. At launch, Tenant stops with this error; on an /enable, the plugin stays off.

**Why it happens.** Its credentials are missing, expired or revoked, its service or program can't be reached, or a setting is wrong.

**What to do.** Read the error. Run `tenant doctor`, fix the setting or sign in again with /configure, then /enable the plugin.

### plugin.x 3002

`X refused the credentials on {op}: HTTP {status}`

- **Level:** error · **Channel:** operational · **Task:** API · **Log:** plugin.x
- **Fields:** `op`, `status`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.x id:3002`

**What it means.** The service refused a call as unauthorized, so the tool failed.

**Why it happens.** The token or key expired or was revoked, or it lacks the scope or access this call needs.

**What to do.** Check the plugin's credentials: /configure it again, or paste a new key on the dashboard's Keys page.

### plugin.x 3003

`X API call {op} failed: {err}`

- **Level:** error · **Channel:** operational · **Task:** API · **Log:** plugin.x
- **Fields:** `op`, `status`, `err`, `path` (sensitive, masked: `path`)
- **Find it:** `provider:plugin.x id:3003`

**What it means.** A call to the service failed on its side or on the way, so the tool failed. status is 0 when no answer came.

**Why it happens.** The service had an outage or an error, or the network, DNS or a proxy failed.

**What to do.** Retry later. If it persists, check the service's status page and this machine's connection.

### plugin.x 3101

`X user token refresh failed: {err}`

- **Level:** error · **Channel:** operational · **Task:** Auth · **Log:** plugin.x
- **Fields:** `err`
- **Find it:** `provider:plugin.x id:3101`

**What it means.** The user-context token could not be renewed or saved, so posting and user reads fail.

**Why it happens.** The refresh token was revoked or already used, the app's client id changed, X is unreachable, or the token file can't be written.

**What to do.** Sign in again with `tenant x --login`.

## security

Writes the `security` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [4101](#security-4101) | info | Approval | `Approval {approval} requested: {action}` |
| [4103](#security-4103) | warning | Approval | `Approval {approval} denied: {action}` |

### security 4101

`Approval {approval} requested: {action}`

- **Level:** info · **Channel:** operational · **Task:** Approval · **Log:** security
- **Fields:** `approval`, `category`, `action`, `surface`
- **Find it:** `provider:security id:4101`

**What it means.** A gated action is waiting for the operator's decision.

**Why it happens.** The agent, a job or a plugin asked for an action whose category needs approval.

**What to do.** Approve or deny it where it was asked; it expires when nobody answers.

### security 4103

`Approval {approval} denied: {action}`

- **Level:** warning · **Channel:** operational · **Task:** Approval · **Log:** security
- **Fields:** `approval`, `action`, `surface`, `actor`
- **Keywords:** `audit.failure`
- **Find it:** `provider:security id:4103`

**What it means.** A gated action was denied, so it did not run.

**Why it happens.** The operator denied it, or Tenant denied it while shutting down.

**What to do.** None when intended; otherwise check the category's approval mode.

## system

Writes the `system` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1](#system-1) | info | Lifecycle | `Tenant {version} started as {role}` |
| [2](#system-2) | info | Lifecycle | `Tenant stopped cleanly after {uptime}` |
| [3](#system-3) | critical | Lifecycle | `The previous run ended unexpectedly; its last event was at {last}` |
| [3101](#system-3101) | error | Component | `{component} failed: {err}` |

### system 1

`Tenant {version} started as {role}`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** system
- **Fields:** `version`, `role`
- **Find it:** `provider:system id:1`

**What it means.** Tenant started.

**Why it happens.** The operator or a service manager started serve or the TUI.

**What to do.** None.

### system 2

`Tenant stopped cleanly after {uptime}`

- **Level:** info · **Channel:** operational · **Task:** Lifecycle · **Log:** system
- **Fields:** `uptime`
- **Find it:** `provider:system id:2`

**What it means.** Tenant finished its shutdown.

**Why it happens.** It was asked to stop: Ctrl-C, the service manager, or an update.

**What to do.** None.

### system 3

`The previous run ended unexpectedly; its last event was at {last}`

- **Level:** critical · **Channel:** operational · **Task:** Lifecycle · **Log:** system
- **Fields:** `last`, `boot`
- **Find it:** `provider:system id:3`

**What it means.** The previous run of Tenant never stopped cleanly.

**Why it happens.** It crashed or was killed, or the machine lost power or restarted.

**What to do.** Read the events before its last one, and any crash file filed at this start.

### system 3101

`{component} failed: {err}`

- **Level:** error · **Channel:** operational · **Task:** Component · **Log:** system
- **Fields:** `component`, `err`
- **Find it:** `provider:system id:3101`

**What it means.** A subsystem stopped with an error.

**Why it happens.** Something it depends on failed: a connection, a file or its configuration.

**What to do.** Open the component's log; the event before this one usually names the cause.

## toolmux

Writes the `toolmux` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1001](#toolmux-1001) | info | Connectors | `MCP connector {server} re-authorized` |
| [2001](#toolmux-2001) | warning | Connectors | `Local MCP server {connector} did not start: {err}` |
| [2002](#toolmux-2002) | warning | Connectors | `Remote MCP connector {server} did not reconnect: {err}` |
| [2003](#toolmux-2003) | warning | Connectors | `MCP connector {server} needs re-authorization: {err}` |
| [2004](#toolmux-2004) | warning | Peers | `peers.json unreadable; no peer is reconnected: {err}` |
| [2005](#toolmux-2005) | warning | Peers | `Peer {peer} did not reconnect: {err}` |
| [2006](#toolmux-2006) | warning | Stats | `Tool stats not saved to {path}; retrying every flush: {err}` |

### toolmux 1001

`MCP connector {server} re-authorized`

- **Level:** info · **Channel:** operational · **Task:** Connectors · **Log:** toolmux
- **Fields:** `server`
- **Find it:** `provider:toolmux id:1001`

**What it means.** A connector that had lost its sign-in has a working one again, and its tools run.

**Why it happens.** The operator reconnected it, or its token refreshed after all.

**What to do.** None.

### toolmux 2001

`Local MCP server {connector} did not start: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Connectors · **Log:** toolmux
- **Fields:** `connector`, `err`
- **Find it:** `provider:toolmux id:2001`

**What it means.** A local MCP server registered for this machine failed to start at launch, so its tools stay a stub.

**Why it happens.** Its command is missing or fails, or the sandbox refused its profile.

**What to do.** Fix what the error names, then /enable the connector to start it again.

### toolmux 2002

`Remote MCP connector {server} did not reconnect: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Connectors · **Log:** toolmux
- **Fields:** `server`, `err`
- **Find it:** `provider:toolmux id:2002`

**What it means.** A remote connector could not come back at launch from its cached sign-in, so its tools stay a stub.

**Why it happens.** The server is down or unreachable, or refused the cached token.

**What to do.** /enable the connector to sign in again, or check the server's status.

### toolmux 2003

`MCP connector {server} needs re-authorization: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Connectors · **Log:** toolmux
- **Fields:** `server`, `url`, `err`, `fix`
- **Find it:** `provider:toolmux id:2003`

**What it means.** A connector's sign-in expired and could not be refreshed, so its tools fail until it is reconnected.

**Why it happens.** The server revoked the refresh token, or it expired unused.

**What to do.** Run the command in fix (`tenant mcp connect <url>`) at the host, or use the dashboard's Remote services page.

### toolmux 2004

`peers.json unreadable; no peer is reconnected: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Peers · **Log:** toolmux
- **Fields:** `err`
- **Find it:** `provider:toolmux id:2004`

**What it means.** The paired peers could not be read at launch, so federated search runs on this machine alone.

**Why it happens.** peers.json is corrupt, or can't be read.

**What to do.** Fix or restore peers.json in the config directory (`tenant peer list` reads it), then restart.

### toolmux 2005

`Peer {peer} did not reconnect: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Peers · **Log:** toolmux
- **Fields:** `peer`, `err`
- **Find it:** `provider:toolmux id:2005`

**What it means.** A paired peer could not be reached at launch, so searches leave it out.

**Why it happens.** The peer is offline, its address changed, or it revoked this machine's token.

**What to do.** Check the peer is up (`tenant peer show <name>`); pair again if its token was revoked.

### toolmux 2006

`Tool stats not saved to {path}; retrying every flush: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Stats · **Log:** toolmux
- **Fields:** `path`, `err`
- **Find it:** `provider:toolmux id:2006`

**What it means.** The per-tool usage counts could not be written. They are kept in memory and retried, and logged again only after a success.

**Why it happens.** The data directory can't be written: permissions or a full disk.

**What to do.** Fix what the error names; nothing else is affected.
