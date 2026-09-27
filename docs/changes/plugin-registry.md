# Change: each plugin declared once, and every surface reads the registry (ARC-003)

**Branch:** `feat/ten-321-plugin-registry` · **Status:** implemented and tested; not yet
deployed.
**Tickets:** ARC-003 / TEN-321.

## What changed
- **Each first-party plugin has one descriptor.** It is in `cmd/tenant/plugin_<id>.go`, and
  the plugins are sql, wiki, web, gsuite, atlassian, github, x, imessage, discord, os, crm
  and simplefin. The descriptor declares:
  - identity: id, title and settings version;
  - tools;
  - config schema: settings and secret fields, validators and the liveness probe;
  - secret files: at rest, and the ones the operator points it at;
  - permission classes: every approval action it raises, with its category, and what its
    allow-flags pre-approve;
  - doctor checks;
  - lifecycle hooks: flags, saved config into flags, launch, activator, first-run prepare,
    eval auto-enable, and `/configure`'s per-plugin steps;
  - its privacy connector, backup exclusions, keys-page rows and CLI commands.

  `docs/PLUGINS.md` §5 lists every field.
- **`internal/pluginreg`** checks the descriptors against each other and indexes them. It
  refuses:
  - a duplicate id, or an id that is one of the host's own labels;
  - a tool two plugins declare;
  - an approval action two plugins declare, or one filed under an unknown category.

  It indexes tool to plugin and action to category. It is a leaf, generic over the
  descriptor type. `cmd/tenant/plugindesc.go` builds the registry, and the order there is
  the launch order.
- **Every surface reads the registry instead of its own switchboard:**
  - **Launch:** `buildToolMux` builds each plugin through its `Launch`, then registers a
    stub and its activator for each plugin not built. It no longer imports a plugin
    package except wiki's, whose index it hands its callers.
  - **Flags:** each plugin binds and owns its launch flags. `pluginFlags` holds one struct
    per plugin, plus the remote-MCP family's fields. Saved config, `~` expansion, first run
    and eval auto-enable are loops over `ApplyConfig`, `ExpandPaths`, `Prepare` and `Eval`.
  - **Approvals:** categories come from each plugin's `Actions`, and flag grants from its
    `Grants`. The host keeps only its own actions: sandbox escalation, `mcp_*` and egress.
  - **Configuration:** `/configure`, the dashboard's Integrations page and `tenant setup`
    list the plugins with a config schema, in their declared setup order. `/configure`
    applies each plugin's own steps:
    - atlassian's probe goes through this session's MCP connector;
    - gsuite hides the OAuth client field when one is installed;
    - atlassian's MCP mode doesn't also enable the native plugin;
    - discord hands the relay its token.
  - **Doctor:** each plugin's checks moved into its own file. The secret-file audit and
    `doctor --fix` read `ConfigSecrets` and `DataSecrets`.
  - **Other surfaces:**
    - the CLI's plugin commands;
    - `permissions effective` and the security audit, by each plugin's `On` and the exact
      tool index;
    - privacy's connectors and `forget --connector`;
    - backup's "not covered" list;
    - the keys page;
    - the file tools' secret guards.
- **Adding a plugin takes three things:** its package, its `plugin_<id>.go`, and one line in
  `plugins()`. There is no switchboard to edit.
- `lazyweb.go` is now `plugin_web_lazy.go`, with web's other files.

## Why
Plugin behavior was spread across central switchboards: the launch branches, the stub
catalog, the approval table, the `/configure` catalog, the setup order, doctor, the CLI,
the tool groups, privacy, backup, the keys page and the secret paths. Adding a plugin meant
editing all of them. They had drifted: each switchboard held its own copy of a plugin's
facts, and several copies were wrong (below). A plugin could be built in up to four places,
and those copies disagreed too.

## Fixed
- **Tool groups (D8).** `permissions effective` matched gsuite by a `gsuite_` prefix that
  matches no tool, and it left out github, crm and simplefin. It now uses the registry's
  exact tool index.
- **Doctor (D9).** `doctor --fix` skipped `github-token.json` when tightening secret files.
  The list now comes from the descriptors.
- **File guards (D10).** The file tools did not refuse gsuite's OAuth client JSON, which
  holds a client secret. They do now.
- **GitHub's launch (D2).** A GitHub turned on at launch, by `tenant setup` or saved config,
  ignored the token that `/configure` and the keys page store. It looked only at
  `$GITHUB_TOKEN`, `$GITHUB_PAT` and a token cache that nothing writes, so the launch failed.
  The launch and the activator now share one resolver: the stored token, then the
  environment, then the cache.
- **Eval's Jira (D14).** Eval turned on a Jira set up with an API token but never supplied
  the token; it copied a `client_id` secret that does not exist. Without `$ATLASSIAN_TOKEN`
  the whole eval failed to build its tools. The launch now reads the stored API token or
  client secret, as the activator does.
- **Approval table.** `jira_write`, `github_write`, `wiki_save` and the crm actions reached
  "destructive" only because unknown actions default to it. They are now declared, with
  the same category. The `web_` prefix guess is gone.
- **Docs.**
  - `docs/PLUGINS.md` had drifted: stale line numbers and "how to add a plugin" steps.
  - The keys catalog said X's and iMessage's secrets were env-only. Both now resolve
    through `credentials.json`.
  - `docs/DATAFLOW.md` said the Jina reader was default-on with an opt-out variable that
    does not exist. It is opt-in, and `TENANT_WEB_READER` overrides. Its code citations now
    point at each plugin's own file.

## Behavior changes
- **The wiki stub lists `wiki_save`.** An unconfigured wiki used to show only search and
  read.
- **Tool groups.** github and crm count in `permissions effective` when they are on.
- **Order.** Lists derived from the registry follow its order, which is the launch order.
  These are the stub catalog, doctor's plugin checks, the CLI's plugin commands and the
  keys page's plugin rows. Some entries moved relative to one another, and none was added
  or dropped except as noted here.
- **Memory tools register first.** The host's memory tools register before any plugin.
- **Stored secrets win at launch.** A GitHub or Jira with both a stored secret and an
  environment variable now launches with the stored one, as its activator already did.
- **Unchanged:**
  - `tenant tui -h`, `tenant serve -h` and `tenant eval -h` are byte-identical before and
    after.
  - Every approval category is unchanged.

## Still open
- **Security (D4):** the cron tools are marked gated, but nothing prompts. Filed separately.
- **Approval gaps:**
  - allow-flags cannot pre-approve destructive-class plugin actions on broker surfaces
    (D3);
  - web never uses the approval broker (D5);
  - SQL `INSERT`/`UPDATE`/`DELETE` never prompt (D6);
  - the retry deny-list is stale (D7).
- **Configuration:**
  - `chrome_path` works only in doctor (D11);
  - `/configure` never sets `skills.<id>.enabled` (D12);
  - `/configure discord` switches on stub tools (D13);
  - env and stored secret precedence differ between surfaces (D15);
  - the dashboard's "Run /enable github" does nothing mid-session (D17).
- **Launch and eval:**
  - one plugin's launch failure stops the whole launch (D18);
  - the nightly eval mutates the host's plugin flags (D19).
- **Runtime health:** each plugin declares its checks and probe, but there is no
  `mux.Health()` for a running plugin yet.
- **Not descriptors:** remote MCP connectors and cron stay host-owned.

## Evidence
- **`internal/pluginreg`:** its refusal rules and indexes.
- **`cmd/tenant`**, in `plugindesc_test.go`:
  - every plugin declares identity, version, tools, the permission class of every gated
    tool, and a stub hint;
  - host and plugin actions are disjoint, and `categorize` agrees with the table;
  - the shared mux built with nothing configured holds exactly the registry's stubs with
    their declared tools;
  - the configure surfaces and the setup order match the registry;
  - every plugin command and check is wired;
  - tool groups use the tool index;
  - no file names a plugin's settings, secrets or config schema outside the plugin's own
    files;
  - the old switchboard files import no plugin package;
  - the at-rest secrets are managed, and pointed secrets are guarded;
  - connectors and keys come from the registry;
  - each activator runs on first `/enable`;
  - `/configure` applies atlassian's hooks, and runs discord's follow-up;
  - each plugin's paths are expanded.
- **Mutation testing:** each of 15 mutants of the registry wiring was caught. The mutants
  do one of three things:
  - drop a declaration: a connector, a secret file or a pointed secret;
  - put a plugin's name back in a switchboard;
  - disable one loop or hook: os registering disabled, `ApplyConfig`, grants, activators,
    the three configure hooks, `Prepare`, `Eval`, `ExpandPaths` or the tool groups.

  Four tests were added to catch the last five survivors.
- **Suite:** `go test ./...` (67 packages), `go vet`, smoke eval 5/5 against the baseline.
