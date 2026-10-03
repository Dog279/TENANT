# Data Flow and Network Exposure

This document inventories Tenant's network, process, storage, credential, and
approval boundaries from the implementation. It describes the code as it is,
not an intended future posture.

State labels used below:

- **Default-on**: active on the ordinary path without an additional opt-in.
- **Opt-in (default-off)**: requires a command, flag, setup choice, saved
  configuration, or enabled plugin.
- **Conditional default-on**: automatically active after its parent feature is
  enabled; the row says how to configure it off.
- **Configured endpoint**: inactive until a provider/integration is selected;
  the operator may replace the listed default URL.

`<data>` and `<config>` can be overridden with `--data` and `--config`. On
Windows, `<data>` defaults to `%LOCALAPPDATA%\tenant`; `<config>` is the Go OS
user-config directory plus `tenant`. The other OS defaults are implemented in
the cited functions (`cmd/tenant/wiring.go:74-75`,
`cmd/tenant/wiring.go:88-106`, `internal/memory/archive/archive.go:65-94`,
`internal/memory/soul/soul.go:71-78`).

## Cloud model data flow

For every configured generation provider, Tenant sends the selected model ID,
the assembled system/soul instructions, conversation messages (including user
content and prior assistant content), tool definitions, and tool results. The
OpenAI-compatible request exposes `messages` and `tools` directly
(`internal/model/backend/vllm/vllm.go:74-86`,
`internal/model/backend/vllm/vllm.go:210-225`); the Anthropic request sends the
same material as top-level `system`, `messages`, and `tools`
(`internal/model/backend/anthropic/anthropic.go:65-74`,
`internal/model/backend/anthropic/anthropic.go:171-192`). Streaming uses the
same payload (`internal/model/backend/vllm/stream.go:60-74`,
`internal/model/backend/anthropic/stream.go:64-71`). Provider replies return to
the agent loop and can be persisted in the episode/archive/transcript stores
listed below.

Embedding calls are a separate flow: raw text chunks are sent as the `input`
array to the configured OpenAI-compatible `/v1/embeddings` endpoint
(`internal/model/backend/vllm/vllm.go:376-395`). This can include conversation,
memory, wiki, skill, or routing text. For any non-echo generation backend the
default embedding endpoint is local Ollama, but it remains an HTTP disclosure
boundary if the operator changes that URL (`cmd/tenant/wiring.go:222-228`).

## Inbound listeners and published ingress

| Listener / exposure | Bind and data accepted | State and protection | Code evidence |
| --- | --- | --- | --- |
| Dashboard HTTP, REST, SSE, and WebSocket | `127.0.0.1:8770` by default; chat, control actions, approvals, memory and configuration administration | **Default-on; configurable off** for TUI/`serve`. Every listener, loopback included, requires the bearer token (TEN-323); loopback may be plaintext. A non-loopback bind additionally requires TLS. Requests must carry an allowlisted Host: the listener address (any loopback name, when it is loopback), `dashboard.allowed_hosts`, or the tailnet name while `/tailscale serve` is on (TEN-332). | `cmd/tenant/commands.go:2884-2903`; `cmd/tenant/serve.go:49-58`; `internal/dashboard/server.go:211-237`; `internal/dashboard/auth.go:137-170`; `internal/dashboard/auth.go:232-252` |
| MCP memory HTTP+SSE gateway | Configured `--sse-addr`; MCP requests can read memory and, with flags, tools or memory writes | **Opt-in (default-off)**; setup defaults to local stdio. It has no authentication. Non-loopback binds are rejected unless `--insecure-lan` is explicitly supplied. | `cmd/tenant/setup.go:272-300`; `cmd/tenant/commands.go:342-370`; `cmd/tenant/commands.go:382-392`; `cmd/tenant/commands.go:450-468` |
| Peer Streamable-HTTP server | Standalone default `127.0.0.1:9100`; the TUI `/peer serve` empty-address default is `0.0.0.0:9100` | **Opt-in (default-off), persisted once enabled**. Non-loopback requires TLS or declared overlay. MCP calls require a per-peer bearer; `/pair` is unauthenticated but rate-limited and operator-approved. | `cmd/tenant/peerctl.go:108-122`; `cmd/tenant/commands.go:2721-2744`; `internal/peering/listener.go:312-336`; `internal/peering/listener.go:363-398`; `internal/peering/listener.go:148-162` |
| Atlassian OAuth callback | `127.0.0.1:8765` by default, configurable; accepts OAuth code/error/state | **Temporary opt-in** during native Atlassian login; off otherwise. State is verified. A configured callback is passed directly to `net.Listen`, so operators should keep it loopback-only. | `internal/plugins/atlassian/oauth.go:33-39`; `internal/plugins/atlassian/oauth.go:227-262` |
| Remote MCP OAuth callback | `127.0.0.1:8765` by default, configurable; accepts OAuth code/error/state | **Temporary opt-in** during an interactive remote-MCP connection; cached reconnects do not bind. A configured callback is passed directly to `net.Listen`. | `internal/plugins/mcpremote/mcpremote.go:36-40`; `internal/plugins/mcpremote/mcpremote.go:63-74`; `internal/plugins/mcpremote/fetcher.go:25-53` |
| Google Workspace OAuth callback | `127.0.0.1:0` by default (ephemeral port); accepts OAuth code/error/state | **Temporary opt-in** during Workspace OAuth; off otherwise. PKCE and state are checked. | `internal/plugins/gsuite/oauth_flow.go:112-138`; `internal/plugins/gsuite/oauth_flow.go:140-151`; `internal/plugins/gsuite/oauth_flow.go:165-201` |
| X OAuth callback | `localhost:8723/callback` by default, or configured redirect host; accepts OAuth code/error/state | **Temporary opt-in** during X PKCE login; off otherwise. State is checked. The configured redirect host becomes `http.Server.Addr`. | `internal/plugins/x/auth.go:213-220`; `internal/plugins/x/auth.go:251-287` |
| Tailscale-published dashboard | Tailscale's daemon publishes the loopback dashboard port to the tailnet; Tenant does not create another socket | **Opt-in (default-off), persisted once enabled**. Exposure and identity policy are provided by Tailscale. While serving, the dashboard trusts the tailnet DNS name as a Host, since Tailscale's proxy forwards it (TEN-332); the bearer token is still required. | `cmd/tenant/tailscalemgr.go:139-194`; `cmd/tenant/dashboardmgr.go` (`SetTailnetHost`) |

## Outbound network matrix

Plugin reads are network actions too: unless a row says otherwise, read/search/
navigate calls do not ask for per-request approval once the parent plugin or
provider is enabled.

### Models and routing

| Destination | Data sent / purpose | State | Code evidence |
| --- | --- | --- | --- |
| Operator-configured vLLM endpoint | Full generation payload to `/v1/chat/completions`; model discovery/health checks to `/v1/models`; optional tokenization | **Configured endpoint; default-off** (`echo` is the no-network CLI default) | `cmd/tenant/wiring.go:72-82`; `internal/model/backend/vllm/vllm.go:365-371`; `internal/model/backend/vllm/vllm.go:410-431`; `cmd/tenant/bootstrap.go:120-145` |
| Local Ollama `http://localhost:11434` | Full generation payload when selected; embedding input is sent here by default for any real generation backend | **Configured generation; conditional default-on for embeddings; configurable off/change** | `internal/config/providers.go:38-42`; `cmd/tenant/wiring.go:222-228`; `internal/model/backend/vllm/vllm.go:388-395` |
| Local llama.cpp `http://localhost:8080` | Full generation payload and configured model requests | **Configured endpoint; default-off** | `internal/config/providers.go:43-47`; `internal/model/backend/vllm/vllm.go:410-431` |
| OpenAI `https://api.openai.com` | Full prompt/message/tool/tool-result generation payload; bearer API key | **Configured cloud provider; default-off** | `internal/config/providers.go:48-53`; `internal/model/backend/vllm/vllm.go:410-431` |
| xAI/Grok `https://api.x.ai` | Full prompt/message/tool/tool-result generation payload; bearer API key | **Configured cloud provider; default-off** | `internal/config/providers.go:54-59`; `internal/model/backend/vllm/vllm.go:410-431` |
| Z.ai global coding plan `https://api.z.ai/api/coding/paas/v4` | Full generation payload to `/chat/completions`; bearer API key | **Configured cloud provider; default-off** | `internal/config/providers.go:60-82`; `internal/model/backend/vllm/vllm.go:365-371`; `internal/model/backend/vllm/vllm.go:410-431` |
| Z.ai China coding plan `https://open.bigmodel.cn/api/coding/paas/v4` | Full generation payload to `/chat/completions`; bearer API key | **Configured cloud provider; default-off** | `internal/config/providers.go:83-88`; `internal/model/backend/vllm/vllm.go:410-431` |
| Z.ai metered `https://api.z.ai/api/paas/v4` | Full generation payload to `/chat/completions`; bearer API key | **Configured cloud provider; default-off** | `internal/config/providers.go:89-98`; `internal/model/backend/vllm/vllm.go:410-431` |
| Sakana AI `https://api.sakana.ai` | Full generation payload to `/v1/chat/completions`; bearer API key | **Configured cloud provider; default-off** | `internal/config/providers.go:99-112`; `internal/model/backend/vllm/vllm.go:410-431` |
| Anthropic `https://api.anthropic.com` | System prompt, messages, tools, and tool results to `/v1/messages`; `x-api-key` credential | **Configured cloud provider; default-off** | `internal/config/providers.go:113-117`; `internal/model/backend/anthropic/anthropic.go:171-192`; `internal/model/backend/anthropic/anthropic.go:315-348` |
| Configured embedding endpoint | Raw text arrays to `/v1/embeddings`, with a bearer key when configured | **Conditional default-on after a real backend; default local Ollama; configurable endpoint/off via echo** | `cmd/tenant/wiring.go:188-200`; `cmd/tenant/wiring.go:222-228`; `internal/model/backend/vllm/vllm.go:376-395`; `internal/model/backend/vllm/vllm.go:424-431` |
| Configured local route classifier (Ollama API) | The user's query embedded in a routing prompt to `/api/generate` | **Opt-in (default-off)**; configuration rejects non-loopback classifier endpoints | `internal/route/supra.go:47-55`; `internal/route/supra.go:76-81`; `internal/route/supra.go:107-124`; `internal/config/route.go:240-245`; `internal/config/route.go:278-292` |
| Configured judge/provider during eval and self-improvement | Evaluation prompts, candidate outputs, tool traces, and scoring context use the same generation provider payload above | **Opt-in/default-off for eval cadence and explicit eval; `serve` self-improvement is default-on but only calls a configured real model** | `cmd/tenant/serve.go:53-56`; `cmd/tenant/eval.go:294-348`; `cmd/tenant/wiring.go:664-680` |

### Plugins, peers, and control clients

| Feature / destination | Data sent / purpose | State | Code evidence |
| --- | --- | --- | --- |
| Web navigation: **arbitrary operator/model-selected HTTP(S) destinations**, including public, private, loopback, and configured internal hosts | Chrome sends ordinary browser requests, URL paths/query strings, cookies/page state, and any approved form interactions to each destination. The dispatcher restricts schemes to HTTP(S), but does not impose a public-address allowlist. | **Opt-in plugin (default-off)**; `tenant research` enables a browser. Read/navigate is ungated; interaction/transact gates are separate. | `cmd/tenant/plugin_web.go` (`launchWeb`); `cmd/tenant/research.go:1278-1281`; `internal/plugins/web/web.go:443-478`; `internal/plugins/web/dispatch.go:428-437` |
| DuckDuckGo HTML `https://html.duckduckgo.com/html/` | Search query in the URL; receives result HTML | **Conditional default-on** when web search is used without Tavily/Brave keys | `internal/plugins/web/search.go:40-48`; `internal/plugins/web/search.go:110-124` |
| Tavily `https://api.tavily.com/search` | Search query, result limit/depth, bearer key | **Configured upgrade; default-off**; takes precedence when a Tavily key exists | `internal/plugins/web/search.go:40-48`; `internal/plugins/web/search.go:263-298` |
| Brave Search `https://api.search.brave.com/res/v1/web/search` | Search query in URL and subscription token header | **Configured upgrade; default-off**; used when Brave but no Tavily key exists | `internal/plugins/web/search.go:40-48`; `internal/plugins/web/search.go:198-220` |
| **Jina Reader `https://r.jina.ai/<targetURL>`** | The entire failed target URL is concatenated into the Jina request, including host, path, query, signed URLs, or query tokens; optional Jina bearer key. Jina returns page markdown. | **Opt-in (default-off).** On when `skills.web.settings.jina_reader` is set (`tenant setup`, `/configure web`) or `TENANT_WEB_READER=true`; `TENANT_WEB_READER=false` forces it off. Once on, it is used only after local browsing is blocked, times out, or returns empty text. It works without a key. | `internal/plugins/web/reader.go:3-12`; `internal/plugins/web/reader.go:23-49`; `internal/plugins/web/web.go:256-259`; `internal/plugins/web/dispatch.go:307-316`; `cmd/tenant/plugin_web_lazy.go:82-103` |
| Google OAuth `accounts.google.com` / `oauth2.googleapis.com`, then `gmail.googleapis.com`, `www.googleapis.com/calendar/v3`, and `www.googleapis.com/drive/v3` | OAuth authorization/code exchange or service-account assertion; mail, events, files, queries, drafts, and approved mutations | **Opt-in plugin (default-off)**. Read-only scopes are default; broader scopes follow the send/write opt-in. | `internal/plugins/gsuite/oauth_flow.go:75-84`; `internal/plugins/gsuite/auth.go:125-166`; `internal/plugins/gsuite/gsuite.go:107-129`; `internal/plugins/gsuite/scopes.go:3-25` |
| Atlassian configured Jira site (API-token path), or `auth.atlassian.com`, `api.atlassian.com`, and `api.atlassian.com/ex/jira/<cloudID>` (OAuth path) | Account email/token or OAuth authorization/tokens; accessible-resource lookup; Jira search/read data and approved issue/comment/transition writes | **Opt-in plugin (default-off)** | `internal/plugins/atlassian/atlassian.go:20-36`; `internal/plugins/atlassian/oauth.go:120-169`; `internal/plugins/atlassian/oauth.go:186-205`; `cmd/tenant/plugin_atlassian.go` (`launchAtlassian`, `activateAtlassian`) |
| X/Twitter `api.twitter.com/2` and OAuth `twitter.com` | Bearer or user token; user/timeline/search reads and approved posts/deletes; OAuth code/token exchange | **Opt-in plugin (default-off)** | `internal/plugins/x/x.go:14-30`; `internal/plugins/x/x.go:85-106`; `internal/plugins/x/auth.go:30-31`; `cmd/tenant/plugin_x.go` (`launchX`, `activateX`) |
| Discord REST `https://discord.com/api/v10` and provider-returned Gateway WebSocket | Bot token; guild/channel/message reads, inbound gateway events, and approved messages/reactions | **Opt-in plugin/relay (default-off)** | `internal/plugins/discord/discord.go:39-59`; `internal/plugins/discord/discord.go:117-139`; `internal/plugins/discord/client.go:66-80`; `internal/plugins/discord/gateway.go:103-121` |
| Configured BlueBubbles HTTP(S) server | Password is placed in every request's query string; conversation/message reads and approved sends | **Opt-in iMessage transport (default-off)** | `internal/plugins/imessage/bluebubbles.go:30-68`; `internal/plugins/imessage/bluebubbles.go:89-114`; `cmd/tenant/plugin_imessage.go` (`launchIMessage`) |
| Configured remote MCP server and its discovered OAuth authorization, token, and registration endpoints | OAuth metadata/DCR/client credentials/tokens; MCP tool lists, arguments, and results | **Opt-in (default-off)**; cached servers reconnect non-interactively once configured | `internal/plugins/mcpremote/mcpremote.go:18-31`; `internal/plugins/mcpremote/mcpremote.go:50-83`; `internal/plugins/mcpremote/persist.go:185-219` |
| Configured Tenant peers | Pairing identity and PIN to `<peer>/pair`; later bearer-authenticated MCP tool calls, queries, grants, and results | **Opt-in (default-off)**; saved dial peers are monitored/reconnected | `internal/peering/pair.go:216-243`; `cmd/tenant/peerheartbeat.go:275-290`; `cmd/tenant/commands.go:2706-2709` |
| Dashboard attach client | Configured dashboard URL plus optional bearer; receives status/events and sends chat/control requests over the dashboard API | **Opt-in command (default-off)** | `cmd/tenant/attach.go:118-138` |
| Setup, model listing, and doctor probes | GET/POST probes to configured generation/embedding/dashboard/serve endpoints and enabled Discord/X/Brave integrations; credentials may be attached | **Explicit setup/doctor/model actions**, plus short configured-endpoint health checks at launch | `cmd/tenant/bootstrap.go:112-150`; `cmd/tenant/modelprobe.go:39-61`; `cmd/tenant/modelprobe.go:103-133`; `cmd/tenant/doctor.go` (`checkGenEndpoint`, `checkEmbedEndpoint`, `checkDashboard`, `checkServe`); `cmd/tenant/plugin_discord.go` (`checkDiscord`); `cmd/tenant/plugin_x.go` (`checkX`) |
| Tailscale control via local CLI | Tenant invokes the local Tailscale CLI; the daemon may contact Tailscale infrastructure and publishes/resets dashboard ingress | **Opt-in (default-off), persisted if enabled** | `cmd/tenant/tailscalemgr.go:89-98`; `cmd/tenant/tailscalemgr.go:139-186` |

The OS, SQL, wiki, and cron plugins have no fixed network service of their own.
Their file/command actions may themselves cause network traffic if an approved
command, arbitrary SQL database path, saved content, or enabled tool invokes a
network-capable external component.

## Local process matrix

| Process / relationship | Data and authority | State | Code evidence |
| --- | --- | --- | --- |
| Chrome, Chromium, or Edge child process | Started by chromedp with `--no-sandbox`; receives target URLs and browser actions over the DevTools control channel; inherits the local user's browser-process privileges | **Opt-in web plugin (default-off); lazy per agent in team/TUI paths** | `internal/plugins/web/web.go:301-339`; `internal/plugins/web/web.go:342-396`; `cmd/tenant/plugin_web_lazy.go:105-128` |
| Ollama | **Not spawned by Tenant.** Tenant expects the operator-run HTTP service at `localhost:11434`; setup instructions tell the operator to run `ollama serve`. | **Conditional default endpoint for embeddings; external process remains operator-managed** | `internal/config/providers.go:38-42`; `cmd/tenant/modelprobe.go:152-167` |
| vLLM / llama.cpp servers | **Not spawned by Tenant.** Tenant connects to the configured HTTP endpoint. | **Configured; default-off** | `cmd/tenant/wiring.go:76-80`; `internal/config/providers.go:34-47` |
| `tenant mcp-memory` child | `tenant mcp-selftest` spawns the current executable, passes backend/data/config endpoints as arguments, and exchanges MCP JSON-RPC over stdin/stdout | **Explicit self-test only; default-off** | `cmd/tenant/commands.go:480-508`; `internal/mcp/transport/stdio.go:56-78` |
| Local MCP server child (`mcp_servers` in config.json, TEN-318) | Tenant starts each configured server and speaks MCP JSON-RPC over its stdin/stdout. Under the default restricted backend (macOS only: Seatbelt) it runs with the network off, Tenant's config and data dirs and the local credential stores denied, a scrubbed environment plus what the entry passes, and memory/CPU caps when set. On Linux and Windows a server whose backend is unavailable doesn't start until the operator names one (`"sandbox": "host"` runs it unconfined). The `jobobject` (Windows: caps and env only) and `host` backends are the operator's explicit choice and confine no filesystem or network. | **Opt-in: only servers the operator lists; started at launch** | `cmd/tenant/mcpstdio.go` (`planMCPServer`, `mcpStdioOpener`, `registerMCPServers`); `internal/sandbox/process.go` (`Start`); `internal/plugins/mcpremote/stdio.go` (`OpenStdio`) |
| Tenant as an MCP stdio subprocess | An external MCP host may spawn `tenant mcp-memory`; in that direction Tenant is the child and consumes/produces JSON-RPC on its inherited stdio | **Opt-in by the external MCP host** | `internal/mcp/transport/stdio.go:44-47`; `internal/mcpserver/server.go:124-130` |
| sh (PowerShell on Windows) for `os_exec` | Arbitrary approved command and its output; runs as the Tenant OS user, through the execution sandbox | **OS plugin and exec approval/allow opt-in; default-off** | `internal/plugins/osys/osys.go:52-69`; `internal/plugins/osys/dispatch.go:38-59` |
| `tasklist` or `ps aux` | Fixed process-list command; output returns to the agent/model | **OS plugin opt-in; the process-list action itself is ungated** | `internal/plugins/osys/osys.go:268-283` |
| `sh -c` for scheduled shell jobs (`cmd /C` on Windows) | Stored cron command; environment allowlist (safe base set plus the profile's `env_pass`); working directory `<data>/cron-work`; output returns to cron history/agent | **Two opt-ins: job marked shell/exec and global cron-exec enabled; default-off** | `cmd/tenant/cronmgr.go:106-124`; `cmd/tenant/cronmgr.go:161-176`; `cmd/tenant/cronmgr.go:187-214` |
| `osascript` on macOS | Approved iMessage recipient/text passed as argv; controls Messages under the local user's Automation permission | **Opt-in iMessage send; default-off** | `internal/plugins/imessage/native_darwin.go:155-178` |
| `gcloud auth application-default print-access-token` | Reads an access token from gcloud's local credential context; token remains in process memory and is used for Google APIs | **Opt-in Workspace `gcloud` auth mode; plugin default-off** | `internal/plugins/gsuite/auth.go:208-220`; `cmd/tenant/skillctl_gsuite.go:430-441` |
| OS URL opener (`rundll32`, `open`, or `xdg-open`) | OAuth/consent URL appears in child arguments and opens the user's browser | **Temporary during explicit OAuth/setup actions** | `internal/plugins/gsuite/oauth_flow.go:225-237`; `cmd/tenant/setup_oauth.go:109-118` |
| `tailscale` CLI | Status/serve/reset arguments and command output; changes Tailscale daemon configuration | **Opt-in; default-off** | `cmd/tenant/tailscalemgr.go:74-98`; `cmd/tenant/tailscalemgr.go:139-186` |

## Data stores

Unless noted otherwise, these stores are plaintext and readable by the local
user account. SQLite also creates adjacent `-wal` and `-shm` files while open.

| Store / path | Contents | State | Code evidence |
| --- | --- | --- | --- |
| `<data>/episodes.db` | Conversation episodes, summaries, feedback, tags, and embeddings | **Default-on** for normal agent operation | `cmd/tenant/wiring.go:897-920`; `cmd/tenant/commands.go:159` |
| `<data>/facts.db` | Semantic facts, provenance/signals, what made each fact (`fact_origin`: job and model), removed-source records (`fact_source_tombstones`), and embeddings | **Default-on** for normal agent operation | `cmd/tenant/wiring.go:905-919`; `internal/memory/semantic/semantic.go:146-156` |
| `<data>/archive/YYYY-MM/<session>.jsonl` | Append-only raw session events, including messages, tool calls, tool results, and errors | **Default-on** where the agent archive writer is wired | `internal/memory/archive/archive.go:97-123`; `internal/memory/archive/archive.go:150-164`; `cmd/tenant/wiring.go:919` |
| `<data>/skills.db` | Learned skill text/metadata and embeddings; each skill's trust and origin; for an imported Agent Skills package, its source path, SKILL.md digest, license and metadata (TEN-298). The package's other files are not copied. | **Default-on in TUI/serve/research** | `internal/memory/skills/skills.go` (`Open`, `UpsertWithSource`); `internal/memory/skills/trust.go`; `cmd/tenant/skills_wire.go` (`skillTool`, `skillControl`); `cmd/tenant/skills_package.go` (`importSkillPackage`) |
| `<data>/usage.db` | Model token/cost usage accounting | **Default-on in TUI/serve; absent in simpler commands** | `cmd/tenant/commands.go:1659`; `cmd/tenant/serve.go:136`; `internal/memory/usage/usage.go:39-49` |
| `<data>/tenant_meta.db` | Self-improvement/distillation cursors and job metadata | **Default-on where self-improvement is active; configurable off with the parent feature** | `cmd/tenant/commands.go:1801`; `cmd/tenant/serve.go:53-56`; `internal/improve/meta.go:43-53` |
| `<config>/config.json` | Model endpoints/models, plugin settings, enabled integrations, gateway/dashboard/peer/relay/cron configuration, and other non-secret runtime settings | **Default-on after setup/configuration** | `internal/config/config.go:17-142`; `internal/config/persist.go:13-122` |
| `<config>/settings.<agent>.json` | Persisted local approval category modes (`ask`/`allow`/`deny`) | **Default-on when modes are saved; removable/configurable** | `internal/config/settings.go:12-36`; `internal/config/settings.go:63-104` |
| `<config>/soul/<agent>.toml` and `<config>/soul/proposed/*` | Agent identity/soul and model-proposed revisions awaiting acceptance | **Default soul created on first run; proposals conditional** | `internal/memory/soul/soul.go:81-93`; `cmd/tenant/bootstrap.go:68-76`; `internal/memory/soul/propose.go:45-63` |
| `<data>/profile.<agent>.md` | Synthesized user profile used in prompts | **Conditional default-on** in TUI/serve self-improvement; operator can disable parent self-improvement | `internal/memory/userprofile/userprofile.go:94-100`; `internal/memory/userprofile/userprofile.go:119-134`; `cmd/tenant/serve.go:53-55` |
| `<data>/transcript.txt`, `<data>/tui.log`, `<data>/screenshots/*.png` | Explicitly saved/rendered transcript, runtime log, and web screenshots | Transcript/screenshot **on action**; TUI log **default-on in TUI** | `cmd/tenant/commands.go:1610`; `cmd/tenant/commands.go:2791`; `internal/tui/tui.go:5436`; `internal/plugins/web/web.go:548-561` |
| Configured wiki directory plus `<data>/wiki/<hash>.json` | User Markdown/text notes in an operator-chosen directory and a generated search/index sidecar under `<data>` | **Opt-in plugin (default-off)** | `cmd/tenant/plugin_wiki.go` (`launchWiki`); `internal/plugins/wiki/wiki.go:266-276` |
| Configured SQL database (fallback for standalone SQL command: `~/Desktop/tenant.db`) | Arbitrary operator-selected SQLite contents | **Opt-in plugin/command (default-off)** | `cmd/tenant/plugin_sql.go` (`launchSQL`); `cmd/tenant/bootstrap.go:35-40`; `internal/plugins/sql/sql.go:50-61` |
| macOS `~/Library/Messages/chat.db` | iMessage conversations/messages, opened read-only by Tenant; owned by Messages/macOS | **Opt-in native iMessage plugin (default-off)** | `internal/plugins/imessage/native_darwin.go:40-60`; `internal/plugins/imessage/chatdb.go:24-47` |
| `<data>/research/<run>/` (`run.json`, `events.jsonl`, `findings/*.md`, `report.md`) | Research question, agent findings, event history, sources, and final report | **On explicit research run; default-off otherwise** | `internal/research/store.go:118-125`; `internal/research/store.go:195-196`; `internal/research/store.go:234-265`; `internal/research/store.go:307-375` |
| Cron state: jobs inside `<config>/config.json`, `<data>/cron-history.json`, `<data>/cron-work/` | Scheduled prompts/commands and status/output history; sandbox working directory for shell jobs | Cron subsystem is present in TUI/serve; jobs and exec are **opt-in/default-off** | `cmd/tenant/commands.go:2100-2101`; `cmd/tenant/cronmgr.go:210-243` |
| `<data>/route-decisions.jsonl` and explicit route export output | User-query routing decisions/justifications; optional exported training dataset | Route logging depends on configured routing; export **explicit/default-off** | `internal/route/log.go:23-29`; `internal/route/log.go:154-166`; `cmd/tenant/routeexport.go:102-118`; `cmd/tenant/routeexport.go:184-195` |
| `<data>/eval-artifacts/` (`eval-*.json`, trend files) | Evaluation prompts/results, tool traces, scores, and trends | **Explicit eval or configured cadence; default-off cadence** | `cmd/tenant/eval_job.go:59-69`; `cmd/tenant/eval_job.go:128-139`; `cmd/tenant/eval_trend.go:44-64` |
| `<data>/.tenant.lock` | Empty lock file; every Tenant process holds it shared, `backup restore` exclusively | **Default-on** | `internal/datalock/datalock.go`; `cmd/tenant/datalock.go` |
| `<data>/privacy-ledger.jsonl` | The path and time of each backup `tenant backup` writes, and the time of each forget that removed data, so `privacy status` can count backups made before the last forget | **Explicit `tenant backup` / `tenant privacy forget`** | `internal/privacy/ledger.go` |
| Backup file (operator-chosen path) and `<data>/.backup-staging-*` | Every file in the config and data dirs; credential and key files only when age-encrypted; staging holds the snapshots while a backup is written and is removed after | **Explicit `tenant backup`; default-off** | `internal/backup/create.go`; `cmd/tenant/backupctl.go` |
| `tenant privacy export --out DIR` (operator-chosen path) | Every store table as JSON Lines (no embeddings), the log book's events with their sensitive fields, profiles, souls, research runs, the raw archive unless `--no-archive`, and connector names | **Explicit; default-off** | `internal/privacy/export.go`; `cmd/tenant/privacyctl.go` |
| `<dir>.restore-<time>` and `<dir>.pre-restore-<time>` beside the config and data dirs | A backup being restored (removed on failure), and the directories a restore replaced (kept for the operator to delete) | **Explicit `tenant backup restore`** | `internal/backup/restore.go` |

OS file tools and wiki/research output flags can read or write additional
operator/model-selected paths. Those paths are not bounded to `<data>` or
`<config>`; their gates are documented below (`internal/plugins/osys/osys.go:84-98`,
`internal/plugins/osys/osys.go:120-196`, `cmd/tenant/research.go:1384-1391`).

## Credentials and secret storage

In this section, “mode `0600`” is the mode Tenant requests when creating the
file. It provides owner-only mode bits on Unix; on Windows it does not create a
new owner-only ACL, so the containing profile directory's ACL remains the
effective protection (`internal/config/credentials.go:52-62`).

| Credential | Sources and storage | State | Code evidence |
| --- | --- | --- | --- |
| Model-provider API keys | `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `XAI_API_KEY`, `ZAI_API_KEY`, or `SAKANA_API_KEY`; an operator-named environment reference; `--api-key` in process arguments; or plaintext `<config>/credentials.json` under the provider ID. Environment references win over stored provider secrets. | **Only when a keyed provider is configured; storage is operator choice** | `cmd/tenant/keyscatalog.go:49-58`; `internal/config/config.go:310-317`; `internal/config/credentials.go:75-96`; `cmd/tenant/wiring.go:82` |
| Web-service API keys | Tavily: `TAVILY_API_KEY`/`TAVILY_KEY` or `credentials.json[tavily]`; Brave: `BRAVE_SEARCH_API_KEY`/`BRAVE_API_KEY` or `credentials.json[brave_search]` (also the namespaced web skill key); Jina: `JINA_API_KEY`/`JINA_KEY` or `credentials.json[jina]` | **Optional; absent by default.** DuckDuckGo and Jina fallback still work keylessly. | `cmd/tenant/plugin_web.go:23-27`; `cmd/tenant/plugin_web_lazy.go:30-49`; `cmd/tenant/plugin_web_lazy.go:51-80` |
| Shared secret file | Plaintext `<config>/credentials.json`, keyed by provider/skill ID, requested mode `0600`. On Windows, those Unix mode bits do not create an owner-only ACL. | **Created only if the operator stores a secret** | `internal/config/credentials.go:12-72`; `cmd/tenant/skillctl.go:65-67` |
| Google Workspace OAuth refresh/access token | Serialized token stored as a secret in `<config>/credentials.json`; client metadata is `<config>/oauth_client.json`, written and read through the secrets package (TEN-337). Service-account JSON remains at the configured operator path. gcloud mode uses gcloud's own credential store. | **Opt-in auth mode** | `cmd/tenant/skillctl_gsuite.go` (`gsuiteOAuthClient`, `gsuiteCachedToken`); `cmd/tenant/plugin_gsuite.go` (`gsuiteRuntimeOAuth`); `internal/plugins/gsuite/embedded_creds.go:55-90`; `cmd/tenant/setup_oauth.go:158-183`; `internal/plugins/gsuite/auth.go:208-220` |
| Atlassian API token/client secret and OAuth token | API token/client secret may be environment/flag or `<config>/credentials.json`; OAuth refresh/access token is `<config>/atlassian-token.json` mode `0600` | **Opt-in plugin/auth mode** | `cmd/tenant/skillctl_atlassian.go:133-171`; `internal/plugins/atlassian/oauth.go:42-70`; `internal/plugins/atlassian/oauth.go:80-96` |
| Remote MCP OAuth cache | `<config>/mcp/<URL-hash>.json`, mode `0600`; contains server URL, DCR client ID/secret, discovered endpoints/scopes, and OAuth token | **Created after an interactive remote connection; default absent** | `cmd/tenant/mcpctl.go:45`; `internal/plugins/mcpremote/token.go:13-35`; `internal/plugins/mcpremote/token.go:37-63` |
| X credentials | Bearer may be flag/environment/`credentials.json`; PKCE access/refresh token is plaintext `<data>/x-token.json`, stored through the secrets package (0600, or a current-user-only Windows DACL) and audited by `tenant doctor` (TEN-337) | **Opt-in plugin/login** | `cmd/tenant/skillctl_x.go:18-32`; `internal/plugins/x/auth.go` (`loadStore`, `save`); `cmd/tenant/secretpaths.go` |
| Discord and BlueBubbles credentials | Discord bot token: `DISCORD_BOT_TOKEN`, flag, or `credentials.json[skill:discord:token]`. BlueBubbles password: `BLUEBUBBLES_PASSWORD`, flag, or `credentials.json[skill:imessage:password]`; the password is then placed in each BlueBubbles request URL query. | **Opt-in integrations; absent by default** | `cmd/tenant/plugin_discord.go` (`Keys`, `applyDiscordConfig`, `launchDiscord`); `cmd/tenant/plugin_imessage.go` (`applyIMessageConfig`, `launchIMessage`); `internal/plugins/imessage/bluebubbles.go:89-97` |
| Peer credentials and TLS identity | `<config>/peers.json` mode `0600` stores per-peer current/pending bearer secrets and trust/share metadata; `<config>/peer-cert.pem` and `peer-key.pem` are mode `0600` | **Created by opt-in pairing/listening; default absent** | `internal/peering/peers.go:27-61`; `internal/peering/peers.go:137-151`; `internal/peering/tls.go:28-46`; `internal/peering/tls.go:63-69` |
| Dashboard bearer and TLS key path | An explicit bearer (`dashboard.auth`) and the TLS certificate/key paths are fields in plaintext `<config>/config.json`; the private key itself remains at the configured path. Without `dashboard.auth`, the generated bearer lives in `<config>/dashboard-auth-token`, created exclusively on first use with owner-only protection (0600 or a current-user-only Windows DACL) so racing processes agree on one token (TEN-332) | **Default-on: every dashboard listener needs a bearer (TEN-323); TLS only for non-loopback exposure** | `internal/config/config.go:325-340`; `cmd/tenant/dashboardauth.go`; `internal/secrets/store.go` (`CreateExclusive`) |
| Security log chain key | `<config>/logbook.key`: 32 random bytes, created once through the secrets package (0600, or a current-user-only Windows DACL, and sealed with DPAPI on Windows); it keys the HMAC that links every Security log record. The chain's head is anchored beside it in `<config>/logbook.anchor`. Both are sandbox deny roots; only an encrypted backup carries the key | **Created at the first serve or TUI start with the log book on** | `cmd/tenant/logchain.go` (`chainKey`, `anchorChain`); `internal/logs/store/chain.go`; `cmd/tenant/secretpaths.go` |

## Approval and gate paths

The local broker maps actions into `exec`, `write`, `destructive`, `web`, and
`send`. Local and Discord modes default to `ask`; iMessage/offsite modes default
to `deny`. Decisions may be once, session, always, or deny, and saved `always`
modes persist in `settings.<agent>.json` (`internal/approval/broker.go`: `NewBroker`, `NewOffsiteBroker`,
`NewDiscordBroker`, `Confirm`, `SetPermission`). Unknown action IDs map to
`destructive`, which is the fail-closed category (`internal/approval/categories.go`
`Categorize`; the host's and the plugins' tables in `cmd/tenant/approval.go`
`categorize`).

| Action path | Actual gate | Default / bypasses | Code evidence |
| --- | --- | --- | --- |
| Read/search/navigate across web, Google, Atlassian, X, Discord, iMessage, SQL, wiki, and local files | No per-action approval after the plugin is enabled; credentials/scopes and plugin enablement are the boundary | **Allowed reads**; enabling a plugin/provider is the opt-in. Browser navigation can reach arbitrary HTTP(S) hosts. | each plugin's `Launch` in `cmd/tenant/plugin_<id>.go`; `internal/plugins/web/dispatch.go:44-50`; `internal/plugins/sql/dispatch.go:27-32`; `internal/plugins/atlassian/dispatch.go:31-32` |
| OS file writes | `AllowWrite` or broker `Confirm` for each write/edit/append/mkdir | **Default deny/ask in interactive TUI**; `--os-allow-write` or persisted `write=allow` bypasses prompts | `internal/plugins/osys/dispatch.go:18-35`; `cmd/tenant/approval.go` (`launchFlagGrants`); `cmd/tenant/plugin_os.go` (`launchOS`, `osGrants`) |
| OS command execution | Non-dangerous command: `AllowExec` or broker confirm. Classifier-marked dangerous command always uses the destructive confirmation category. | **Default deny/ask**; `--os-allow-exec` can blanket-allow only non-dangerous commands | `internal/plugins/osys/dispatch.go:38-59`; `internal/approval/categories.go` (`Categorize`) |
| SQL writes | `INSERT`/`UPDATE`/`DELETE` require `--sql-allow-write`; they do **not** consult the confirm hook. DDL/destructive statements always require `Confirm`. | **Default-off**. Ordinary writes are flag-only; DDL is per-action and fail-closed without a broker. | `internal/plugins/sql/dispatch.go:19-42`; `cmd/tenant/plugin_sql.go` (`launchSQL`) |
| Web interaction and transactions | Click/fill/select require `AllowInteract` or `Confirm`; transact/irreversible actions always require destructive `Confirm` | **Default ask/deny** after web opt-in. `--web-allow-interact` bypasses interaction prompts but not transactions. | `internal/plugins/web/dispatch.go:44-64`; `cmd/tenant/plugin_web.go` (`launchWeb`, `webGrants`) |
| Google Workspace mutations | Send/create/modify/trash actions require `AllowSend` or broker `Confirm`; OAuth scopes are separately widened when allowed | **Default ask/deny**; `--gsuite-allow-send` or persisted send allow bypasses prompts | `internal/plugins/gsuite/dispatch.go:19-42`; `internal/plugins/gsuite/dispatch.go:447-646`; `internal/plugins/gsuite/scopes.go:23-36` |
| Jira board writes | Create/comment/transition requires `AllowWrite` or broker `Confirm` | **Default ask/deny**; allow flag/mode can bypass | `internal/plugins/atlassian/dispatch.go:19-39`; `cmd/tenant/plugin_atlassian.go` (`launchAtlassian`) |
| X posts/deletes, Discord sends/reactions, and iMessage sends | Each dispatcher requires its allow flag or `Confirm`; public/destructive action IDs that do not map explicitly fall into the destructive broker category | **Default ask/deny**; plugin-specific allow flags can bypass dispatcher prompts where wired | `internal/plugins/x/dispatch.go:17-40`; `internal/plugins/discord/dispatch.go:21-47`; `internal/plugins/imessage/dispatch.go:17-40`; `cmd/tenant/approval.go` (`categorize`) |
| Remote and local MCP tools | Deny-by-default: every tool is gated unless trusted server annotations mark it read-only (or a fixed peer allowlist ungates it); otherwise `AllowWrite` or `Confirm` is required | **Default ask/deny**. `--mcp-trust-annotations` and `--mcp-allow-write` relax the boundary. | `internal/plugins/mcpremote/dispatch.go:38-70`; `internal/plugins/mcpremote/dispatch.go:120-139`; `cmd/tenant/toolmux_build.go` (`buildToolMux`); `internal/plugins/mcpremote/stdio.go` (`OpenStdio`: a local server's tools are gated the same way) |
| Wiki save | Calls broker `Confirm` when a broker exists; refuses overwrite unless the call explicitly sets `overwrite=true` | **Interactive ask**. In headless construction with `confirm=nil`, the operator launch is treated as authorization and no per-write prompt occurs. | `cmd/tenant/wikisave.go:90-105`; `cmd/tenant/plugin_wiki.go` (`launchWiki`) |
| MCP memory contribution | `memory_fact_add` is not registered unless `--allow-writes` is set | **Flag-only, default-off**; no per-call prompt | `cmd/tenant/commands.go:364-370`; `cmd/tenant/commands.go:417-424`; `internal/mcpserver/server.go:53-56`; `internal/mcpserver/handler.go:160-163` |
| Cron mutations and unattended exec | Cron mutating tools carry `ToolSpec.Gated=true`, but the dispatcher itself calls the manager directly and has no confirmation callback. Shell/exec jobs additionally require the persisted global cron-exec switch; catastrophic commands and config/data writes are refused by the unattended approver. | Cron mutation metadata says gated, but there is **no dispatcher-side per-action approval**. Exec remains **double opt-in/default-off**. | `internal/plugins/cron/dispatch.go:57-61`; `internal/plugins/cron/dispatch.go:80-105`; `internal/plugins/cron/dispatch.go:109-123`; `cmd/tenant/cronmgr.go:106-124`; `cmd/tenant/cronmgr.go:138-176` |
| Peer pairing and peer data/tool access | Every inbound `/pair` asks the operator and fails closed; accepted peers start with all-deny shares. Later calls require bearer auth and are limited by per-peer share policy. | **Opt-in/default deny**; pairing cannot be blanket auto-allowed | `internal/approval/broker.go` (`AskPairing`); `internal/peering/pair.go:104-165`; `internal/peering/listener.go:148-170`; `internal/peering/peers.go:15-37` |
| Offsite relays | iMessage broker starts deny-all; Discord uses origin-scoped button approvals; local approval modes do not silently authorize an offsite-origin action. While the relay's exec mode (`relay.allow_exec`) is off, the Discord broker refuses exec, write, destructive, sandbox and send (Discord's own posts/reactions excepted) before tool rules or modes are consulted. | **iMessage default deny; Discord default ask; Discord exec mode default off**. `tenant security audit` evaluates each live surface. | `cmd/tenant/imessagerelay.go:13-15`; `cmd/tenant/imessagerelay.go:70-72`; `cmd/tenant/discordconfirm.go:44-57`; `internal/approval/broker.go` (`NewOffsiteBroker`, `NewDiscordBroker`, `SetLock`, `askDecision`); `cmd/tenant/discordtools.go:27-46` |

## Boundary summary

- Tenant implements no telemetry or analytics path in this inventory. That does
  not mean “no unconfigured destinations”: after web is enabled, keyless
  DuckDuckGo search and Jina fallback can be contacted without service keys,
  and browser navigation can contact arbitrary HTTP(S) destinations.
- The ordinary TUI/serve posture includes a loopback dashboard listener. Other
  listeners are feature- or authentication-flow opt-ins.
- “Read by default” is not “local only.” Reads disclose queries, URLs, resource
  identifiers, and authentication metadata to configured or fallback services.
- A cloud model receives the prompt context and tool traffic required for the
  turn; a separately configured embedding service receives raw text inputs.
