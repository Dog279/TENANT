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

## backup

Writes the `backup` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1001](#backup-1001) | info | Backup | `Back up to {path}` |
| [1002](#backup-1002) | info | Backup | `Backup written to {path}: {files} files, {bytes} bytes` |
| [1003](#backup-1003) | info | Verify | `Verify backup {path}` |
| [1004](#backup-1004) | info | Verify | `Backup {path} checks out: {files} files, made {created} by Tenant {version}` |
| [1005](#backup-1005) | info | Restore | `Restore backup {path}` |
| [1006](#backup-1006) | info | Restore | `Restored backup {path}` |
| [2001](#backup-2001) | warning | Backup | `Backup {path}: {warning}` |
| [3001](#backup-3001) | error | Verify | `Backup {path} failed verification with {count} problems, the first: {problem}` |

### backup 1001

`Back up to {path}`

- **Level:** info · **Channel:** operational · **Task:** Backup · **Log:** backup
- **Fields:** `path`
- **Find it:** `provider:backup id:1001`

**What it means.** A backup of the config and data directories: its start, then its stop with the duration, or its failure with the error.

**Why it happens.** The operator ran tenant backup create.

**What to do.** None when it stops. On a failure, read the error; nothing is left at the path.

### backup 1002

`Backup written to {path}: {files} files, {bytes} bytes`

- **Level:** info · **Channel:** operational · **Task:** Backup · **Log:** backup
- **Fields:** `path`, `bytes`, `files`, `databases`, `logs`, `secrets`, `skipped`, `encrypted`, `version`
- **Find it:** `provider:backup id:1002`

**What it means.** A backup file was written. skipped counts what was left out, such as credentials in an unencrypted backup.

**Why it happens.** A backup finished.

**What to do.** Check it now and then with tenant backup verify, and keep it somewhere other than this machine.

### backup 1003

`Verify backup {path}`

- **Level:** info · **Channel:** operational · **Task:** Verify · **Log:** backup
- **Fields:** `path`
- **Find it:** `provider:backup id:1003`

**What it means.** A backup file was checked: its start, then its stop with the duration, or its failure with the error. Its findings follow as their own event.

**Why it happens.** The operator ran tenant backup verify.

**What to do.** None when it stops. A failure means the file could not be read at all: a wrong path, or a missing or wrong passphrase or key.

### backup 1004

`Backup {path} checks out: {files} files, made {created} by Tenant {version}`

- **Level:** info · **Channel:** operational · **Task:** Verify · **Log:** backup
- **Fields:** `path`, `files`, `created`, `version`, `encrypted`, `newer`, `notes`
- **Find it:** `provider:backup id:1004`

**What it means.** Every checksum in the backup matches, every database passes its integrity check and every log parses. newer counts schema from a newer Tenant, which a restore refuses unless forced.

**Why it happens.** A verify or a restore read the backup.

**What to do.** None.

### backup 1005

`Restore backup {path}`

- **Level:** info · **Channel:** operational · **Task:** Restore · **Log:** backup
- **Fields:** `path`, `dry_run`
- **Find it:** `provider:backup id:1005`

**What it means.** A restore of a backup over the config and data directories: its start, then its stop with the duration, or its failure with the error. A dry run only verifies.

**Why it happens.** The operator ran tenant backup restore.

**What to do.** None when it stops. On a failure nothing was changed; read the error.

### backup 1006

`Restored backup {path}`

- **Level:** info · **Channel:** operational · **Task:** Restore · **Log:** backup
- **Fields:** `path`, `aside`, `kept_secrets`
- **Find it:** `provider:backup id:1006`

**What it means.** The backup replaced the config and data directories. aside names where the previous ones are kept.

**Why it happens.** A restore finished.

**What to do.** Start Tenant and check it; delete the directories named in aside once you are happy. Don't run this install alongside the machine the backup came from.

### backup 2001

`Backup {path}: {warning}`

- **Level:** warning · **Channel:** operational · **Task:** Backup · **Log:** backup
- **Fields:** `path`, `warning`
- **Find it:** `provider:backup id:2001`

**What it means.** A backup was written, but something about it needs attention.

**Why it happens.** Usually the destination cannot restrict access to the file (a FAT or exFAT drive), and the backup is not encrypted.

**What to do.** Keep the file somewhere private, or back up again with --encrypt.

### backup 3001

`Backup {path} failed verification with {count} problems, the first: {problem}`

- **Level:** error · **Channel:** operational · **Task:** Verify · **Log:** backup
- **Fields:** `path`, `count`, `problem`, `problems`
- **Find it:** `provider:backup id:3001`

**What it means.** The backup is damaged: a checksum does not match, the file is cut short, a database fails its integrity check or a log does not parse. It cannot be restored.

**Why it happens.** The file was changed or cut short after it was written, or the disk or the copy damaged it.

**What to do.** Use another backup, and make a fresh one with tenant backup create.

## improve

Writes the `improve` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1001](#improve-1001) | info | Job run | `Self-improvement job {job}` |
| [1002](#improve-1002) | info | Job run | `{job}: {summary}` |
| [2001](#improve-2001) | warning | Job run | `{job}: {step} failed; the run went on: {err}` |
| [2002](#improve-2002) | warning | Consolidation | `Consolidation could not tell which facts are protected, so it may merge any of {facts} facts: {err}` |
| [2003](#improve-2003) | warning | Reflection | `Reflection wrote nothing: the synthesis failed: {err}` |
| [2004](#improve-2004) | warning | Reflection | `Reflection cut SME section {section} to {cap} tokens; it was about {tokens}` |
| [2005](#improve-2005) | warning | Soul nudge | `Soul nudge proposed nothing: the candidate could not be scored: {err}` |
| [2006](#improve-2006) | warning | Configuration | `improve.profile {profile} is not usable, so the jobs stay on the main model: {reason}` |

### improve 1001

`Self-improvement job {job}`

- **Level:** info · **Channel:** operational · **Task:** Job run · **Log:** improve
- **Fields:** `job`
- **Find it:** `provider:improve id:1001`

**What it means.** A self-improvement job ran: its start, then its stop with the duration, or its failure with the error.

**Why it happens.** The scheduler found it due (serve, or the TUI with self-improvement on), or the operator asked for a run.

**What to do.** None when it stops. On a failure, read the error, and the job's warnings under the same activity.

### improve 1002

`{job}: {summary}`

- **Level:** info · **Channel:** operational · **Task:** Job run · **Log:** improve
- **Fields:** `job`, `summary`, `changed`, `source_runs`
- **Find it:** `provider:improve id:1002`

**What it means.** What one run of a self-improvement job did. source_runs are the turns (runs.db trace ids) it learned from.

**Why it happens.** Every run that does not fail reports its result, whether it changed anything or not.

**What to do.** None. To see what a run wrote, follow its activity to the memory and skills events it caused.

### improve 2001

`{job}: {step} failed; the run went on: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Job run · **Log:** improve
- **Fields:** `job`, `step`, `err`, `fact_id`, `section`, `size`
- **Find it:** `provider:improve id:2001`

**What it means.** A best-effort step of a self-improvement job failed, and the job skipped it.

**Why it happens.** The model, or a memory database the step writes to, returned an error.

**What to do.** None if it is rare: the next run tries again. If it repeats, check the model and the memory databases with tenant doctor.

### improve 2002

`Consolidation could not tell which facts are protected, so it may merge any of {facts} facts: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Consolidation · **Log:** improve
- **Fields:** `facts`, `err`
- **Find it:** `provider:improve id:2002`

**What it means.** Consolidation ran without leaving out pinned and protected facts, so one of them may be merged into a broader fact.

**Why it happens.** The fact signals table in facts.db could not be read.

**What to do.** Check facts.db with tenant doctor. A fact merged by mistake can be restored from its page in the dashboard.

### improve 2003

`Reflection wrote nothing: the synthesis failed: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Reflection · **Log:** improve
- **Fields:** `err`
- **Find it:** `provider:improve id:2003`

**What it means.** The reflection job did not update the project's SME document; the previous version stays in use.

**Why it happens.** The proposer model failed, or answered with something that is not the expected JSON.

**What to do.** Check the model set by improve.profile (or the main model). The next run tries again.

### improve 2004

`Reflection cut SME section {section} to {cap} tokens; it was about {tokens}`

- **Level:** warning · **Channel:** operational · **Task:** Reflection · **Log:** improve
- **Fields:** `section`, `tokens`, `cap`
- **Find it:** `provider:improve id:2004`

**What it means.** A section of the SME document was longer than its cap, so its end was cut off.

**Why it happens.** The model wrote more than the per-section limit allows.

**What to do.** None usually. If details go missing from the section, raise its token cap.

### improve 2005

`Soul nudge proposed nothing: the candidate could not be scored: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Soul nudge · **Log:** improve
- **Fields:** `err`
- **Find it:** `provider:improve id:2005`

**What it means.** The soul-nudge job had a candidate, but could not run the fitness check that must pass before it is proposed.

**Why it happens.** There is no fitness baseline yet, or the eval run failed.

**What to do.** Capture a fitness baseline (tenant eval) if there is none; otherwise read the error. Nothing is proposed until it passes.

### improve 2006

`improve.profile {profile} is not usable, so the jobs stay on the main model: {reason}`

- **Level:** warning · **Channel:** operational · **Task:** Configuration · **Log:** improve
- **Fields:** `profile`, `reason`, `err`
- **Find it:** `provider:improve id:2006`

**What it means.** The self-improvement jobs run on the main model instead of the profile improve.profile names.

**Why it happens.** The profile is not in config.json, or its model could not be set up.

**What to do.** Fix the profile's entry in config.json, or clear improve.profile.

## memory

Writes the `memory` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1001](#memory-1001) | info | Fact | `Fact {fact_id} added by {job}` |
| [1002](#memory-1002) | info | Fact | `Fact {fact_id} superseded by fact {by}` |
| [1003](#memory-1003) | info | Fact | `Fact {fact_id} removed` |
| [1004](#memory-1004) | info | Fact | `Fact {fact_id} restored` |
| [1101](#memory-1101) | info | Compaction | `Compacted a conversation from {before} messages to {after}` |
| [2001](#memory-2001) | warning | Distillation | `Distillation skipped {episodes} episodes from #{first_episode} at {step}: {err}` |
| [2002](#memory-2002) | warning | Distillation | `Distillation: {step} failed for fact {fact_id}: {err}` |
| [2003](#memory-2003) | warning | Distillation | `Could not tell whether a new fact repeats fact {fact_id}; added it anyway: {err}` |
| [9001](#memory-9001) | info | Retrieval | `Retrieved {facts} facts and {episodes} episodes for the turn` |
| [9002](#memory-9002) | info | Retrieval | `memory_recall found {facts} facts, {episodes} episodes and {archived} archive events` |

### memory 1001

`Fact {fact_id} added by {job}`

- **Level:** info · **Channel:** operational · **Task:** Fact · **Log:** memory
- **Fields:** `fact_id`, `job`, `agent`, `source_runs`, `fact` (sensitive, masked: `fact`)
- **Find it:** `provider:memory id:1001`

**What it means.** A fact was written to long-term memory (facts.db). source_runs are the turns it was learned from.

**Why it happens.** Distillation or consolidation learned it, the agent or the operator saved it, an import brought it, or an edit or restore made a new version.

**What to do.** None. To see where it came from, open `/memory/facts/<fact_id>` in the dashboard.

### memory 1002

`Fact {fact_id} superseded by fact {by}`

- **Level:** info · **Channel:** operational · **Task:** Fact · **Log:** memory
- **Fields:** `fact_id`, `by`
- **Find it:** `provider:memory id:1002`

**What it means.** A fact was replaced by a newer one. It stays in facts.db for its history, but recall no longer uses it.

**Why it happens.** Distillation learned something that contradicts it, consolidation merged it, or it was edited or resolved as a duplicate.

**What to do.** None. Its versions are on `/memory/facts/<fact_id>` in the dashboard, where an earlier one can be restored.

### memory 1003

`Fact {fact_id} removed`

- **Level:** info · **Channel:** operational · **Task:** Fact · **Log:** memory
- **Fields:** `fact_id`
- **Find it:** `provider:memory id:1003`

**What it means.** A fact was removed from memory: recall no longer uses it, but it can still be restored.

**Why it happens.** The operator or the agent forgot it.

**What to do.** None. To undo it, restore it from the dashboard's removed-facts page.

### memory 1004

`Fact {fact_id} restored`

- **Level:** info · **Channel:** operational · **Task:** Fact · **Log:** memory
- **Fields:** `fact_id`
- **Find it:** `provider:memory id:1004`

**What it means.** A removed fact was brought back, and recall uses it again.

**Why it happens.** The operator restored it.

**What to do.** None.

### memory 1101

`Compacted a conversation from {before} messages to {after}`

- **Level:** info · **Channel:** operational · **Task:** Compaction · **Log:** memory
- **Fields:** `session`, `before`, `after`, `origin`, `source_messages`
- **Find it:** `provider:memory id:1101`

**What it means.** The oldest turns of a conversation were summarized into one message, so it fits the model's context again. origin says whether the summary was made from the archive's raw events or from the working set.

**Why it happens.** The conversation filled most of its share of the context window, or the operator ran /compress.

**What to do.** None. If answers lose the thread after this, lower the model's context use or raise its context length.

### memory 2001

`Distillation skipped {episodes} episodes from #{first_episode} at {step}: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Distillation · **Log:** memory
- **Fields:** `step`, `first_episode`, `episodes`, `agent`, `err`
- **Find it:** `provider:memory id:2001`

**What it means.** Distillation could not learn facts from a batch of turns. It moved past them, so their facts are not extracted.

**Why it happens.** The summarizer model failed or answered with something that is not the expected JSON (extract), or embedding or writing the facts failed (persist).

**What to do.** Check the summarizer model (tenant doctor). If it keeps happening, the batch size may be too large for the model.

### memory 2002

`Distillation: {step} failed for fact {fact_id}: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Distillation · **Log:** memory
- **Fields:** `step`, `fact_id`, `by`, `err`
- **Find it:** `provider:memory id:2002`

**What it means.** A best-effort write after distilling a fact failed; the fact itself was kept.

**Why it happens.** facts.db refused the write: it is busy, full or damaged.

**What to do.** If it repeats, check facts.db with tenant doctor.

### memory 2003

`Could not tell whether a new fact repeats fact {fact_id}; added it anyway: {err}`

- **Level:** warning · **Channel:** operational · **Task:** Distillation · **Log:** memory
- **Fields:** `fact_id`, `err`
- **Find it:** `provider:memory id:2003`

**What it means.** Distillation added a fact that may repeat or contradict an existing one, because the model could not judge them.

**Why it happens.** The summarizer model failed or gave an answer that could not be read.

**What to do.** None usually: consolidation merges duplicates later. If it repeats, check the summarizer model.

### memory 9001

`Retrieved {facts} facts and {episodes} episodes for the turn`

- **Level:** info · **Channel:** diagnostic · **Task:** Retrieval · **Log:** memory
- **Fields:** `facts`, `episodes`, `fact_ids`, `episode_ids`, `truncations`
- **Find it:** `provider:memory id:9001`

**What it means.** What long-term memory put into a turn's context, after its budget cut: the facts and past turns by id.

**Why it happens.** Every turn retrieves memory.

**What to do.** None. When an answer ignores something memory holds, check whether its fact was retrieved here.

### memory 9002

`memory_recall found {facts} facts, {episodes} episodes and {archived} archive events`

- **Level:** info · **Channel:** diagnostic · **Task:** Retrieval · **Log:** memory
- **Fields:** `query`, `scope`, `facts`, `episodes`, `archived`, `err` (sensitive, masked: `query`)
- **Find it:** `provider:memory id:9002`

**What it means.** The agent searched its memory with memory_recall. err holds a store's search error, which the agent never sees.

**Why it happens.** The agent decided it needed something from earlier conversations.

**What to do.** None; on an err, check the memory databases with tenant doctor.

## privacy

Writes the `privacy` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1001](#privacy-1001) | info | Retention | `Retention sweep` |
| [1002](#privacy-1002) | info | Deletion | `Privacy deletion ({reason})` |
| [1003](#privacy-1003) | info | Deletion | `{action} {count} {data}` |
| [2001](#privacy-2001) | warning | Deletion | `A privacy deletion did not fully finish: {warning}` |

### privacy 1001

`Retention sweep`

- **Level:** info · **Channel:** operational · **Task:** Retention · **Log:** privacy
- **Fields:** `classes`
- **Find it:** `provider:privacy id:1001`

**What it means.** The daily retention sweep of a running Tenant: its start, then its stop with the duration, or its failure with the error. classes counts the data classes that have a retention period; a sweep with none removes nothing.

**Why it happens.** Serve and the TUI sweep a few minutes after they start, then daily.

**What to do.** None when it stops. On a failure, read the error: usually config.json could not be read, and nothing was removed.

### privacy 1002

`Privacy deletion ({reason})`

- **Level:** info · **Channel:** operational · **Task:** Deletion · **Log:** privacy
- **Fields:** `reason`
- **Find it:** `provider:privacy id:1002`

**What it means.** Data was deleted for good: its start, the counts removed, then its stop with the duration, or its failure with the error. reason is forget (the operator asked) or retention (a class aged out).

**Why it happens.** tenant privacy forget or prune, or the retention sweep.

**What to do.** None when it stops. After a failure, the stores done before it stay deleted and the rest are unchanged; run the command again.

### privacy 1003

`{action} {count} {data}`

- **Level:** info · **Channel:** operational · **Task:** Deletion · **Log:** privacy
- **Fields:** `data`, `action`, `count`, `reason`
- **Find it:** `provider:privacy id:1003`

**What it means.** One kind of data a deletion changed: rows or files deleted, archive files rewritten, or records unlinked from what was removed.

**Why it happens.** A privacy deletion ran.

**What to do.** None.

### privacy 2001

`A privacy deletion did not fully finish: {warning}`

- **Level:** warning · **Channel:** operational · **Task:** Deletion · **Log:** privacy
- **Fields:** `warning`, `reason`
- **Find it:** `provider:privacy id:2001`

**What it means.** The data was deleted, but a clean-up step after it did not finish, so copies may linger until later.

**Why it happens.** A running Tenant was reading the store, so its WAL still holds old pages, or its search index could not be compacted.

**What to do.** None usually: the store's next checkpoint clears its WAL. A deletion run while Tenant is stopped leaves no copies.

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

## skills

Writes the `skills` log.

| ID | Level | Task | Message |
|---|---|---|---|
| [1001](#skills-1001) | info | Skill | `Skill {skill} saved ({source})` |
| [1002](#skills-1002) | info | Skill | `Skill {skill} is now {trust} (was {prior})` |
| [1003](#skills-1003) | info | Skill | `Skill {skill} accepted` |
| [1004](#skills-1004) | info | Skill | `Skill {skill} {state}` |
| [1005](#skills-1005) | info | Skill | `Skill {skill} removed` |
| [1006](#skills-1006) | info | Package | `Skill {skill} imported from {from}` |
| [1007](#skills-1007) | info | Package | `Skill {skill} exported to {dir}` |

### skills 1001

`Skill {skill} saved ({source})`

- **Level:** info · **Channel:** operational · **Task:** Skill · **Log:** skills
- **Fields:** `skill`, `skill_id`, `source`, `status`, `trust`, `source_runs`
- **Find it:** `provider:skills id:1001`

**What it means.** A skill was added or changed. source says how: operator, induction, auto-accept, import, seed or revert. Its earlier version is in its history (/skills history).

**Why it happens.** The operator or the agent saved it, the skill-induction job proposed it, a package was imported, or a version was restored.

**What to do.** None. /skills show names what it does; /skills diff compares it with its earlier version.

### skills 1002

`Skill {skill} is now {trust} (was {prior})`

- **Level:** info · **Channel:** operational · **Task:** Skill · **Log:** skills
- **Fields:** `skill`, `skill_id`, `trust`, `prior`, `source`
- **Find it:** `provider:skills id:1002`

**What it means.** A skill's trust changed. An untrusted skill is not offered to the agent.

**Why it happens.** The operator trusted or distrusted it, or a change widened the tools it expects to use, which makes it untrusted until reviewed.

**What to do.** If it became untrusted by a change, review it (/skills show) and trust it again if it is fine.

### skills 1003

`Skill {skill} accepted`

- **Level:** info · **Channel:** operational · **Task:** Skill · **Log:** skills
- **Fields:** `skill`, `skill_id`
- **Find it:** `provider:skills id:1003`

**What it means.** A proposed skill went live: it is enabled and offered to the agent.

**Why it happens.** The operator accepted a skill the skill-induction job proposed.

**What to do.** None.

### skills 1004

`Skill {skill} {state}`

- **Level:** info · **Channel:** operational · **Task:** Skill · **Log:** skills
- **Fields:** `skill`, `skill_id`, `state`
- **Find it:** `provider:skills id:1004`

**What it means.** A skill was enabled or disabled. A disabled skill is not offered to the agent.

**Why it happens.** The operator switched it on or off.

**What to do.** None.

### skills 1005

`Skill {skill} removed`

- **Level:** info · **Channel:** operational · **Task:** Skill · **Log:** skills
- **Fields:** `skill`, `skill_id`
- **Find it:** `provider:skills id:1005`

**What it means.** A skill was forgotten: it is disabled and hidden, and its row and history stay until a privacy forget deletes them.

**Why it happens.** The operator or the agent forgot it.

**What to do.** None. Saving a skill of the same name brings it back.

### skills 1006

`Skill {skill} imported from {from}`

- **Level:** info · **Channel:** operational · **Task:** Package · **Log:** skills
- **Fields:** `skill`, `from`, `digest`, `trusted`, `unchanged`
- **Find it:** `provider:skills id:1006`

**What it means.** An Agent Skills package (SKILL.md) was imported as a skill. It is untrusted unless the import said otherwise.

**Why it happens.** The operator ran tenant skills import or /skills import.

**What to do.** Review it (/skills show) and trust it (/skills trust) before the agent may use it.

### skills 1007

`Skill {skill} exported to {dir}`

- **Level:** info · **Channel:** operational · **Task:** Package · **Log:** skills
- **Fields:** `skill`, `dir`
- **Find it:** `provider:skills id:1007`

**What it means.** A skill was written out as an Agent Skills package (SKILL.md).

**Why it happens.** The operator ran tenant skills export or /skills export.

**What to do.** None.

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
