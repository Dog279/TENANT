# Sensitive-data information-flow labels

Status: implemented v1 design for TEN-311 / SEC-002.

## Security objective

Tenant must treat moving information as a security-relevant action. A tool that
only reads can return a local secret, and a later tool that appears read-only can
put that secret in a URL, search query, message, peer request, or remote MCP
argument. Mutation policy alone does not stop that composition.

The v1 objective is therefore:

1. attach sensitivity and provenance to successful tool-result envelopes;
2. retain a bounded set of matchable fragments from recent sensitive results;
3. before dispatching an outbound-capable tool, detect those fragments in its
   arguments; and
4. route a detected flow through Tenant's existing approval broker. No second
   approval queue or decision model is introduced.

## Label model

`model.ToolSpec` carries internal-only result metadata and an outbound marker:

- `ResultLabels`: zero or more labels whose `Sensitivity` is `sensitive` for
  data that must not leave an existing trust boundary implicitly, and whose
  `Provenance` identifies `local_file`, `memory`, `wiki`, `local_service`,
  `service`, `remote_mcp`, or `peer`.
- `Outbound`: true when tool arguments may cross a process, host, account, or
  human boundary.

After a successful dispatch, the agent copies the spec's sensitivity and
provenance into `ToolCallResult.Labels`. Labels also accompany the working-set
and archive copies of the result, so the envelope remains inspectable after the
immediate dispatch. Errors are not retained as sensitive source values because
they do not contain a successful read result.

Initial sensitive sources include:

| Source | Tools | Provenance |
| --- | --- | --- |
| Local files | `os_read_file` | `local_file` |
| Memory | `memory_search`, `memory_recall`, `memory_history`, `recall` | `memory` |
| Wiki/notes | wiki read/search/list/link tools | `wiki` |
| Local services | read-only SQL schema/query results | `local_service` |
| Private integrations | Google, Jira, Discord, and iMessage service reads | `service` / `local_service` |
| Remote MCP | dynamically discovered remote MCP tools | `remote_mcp` |
| Agent peers | team inbox/history reads | `peer` |

Credential-adjacent service integrations opt into the same metadata, and future
plugins must declare their source and sink behavior as the catalog evolves.
Unknown/unlabelled results remain unclassified; this is not a claim that they
are safe.

## Dispatch policy hook

The hook runs in the agent after schema/capability validation and immediately
before the outbound tool reaches the dispatcher. It examines only specs marked
`Outbound`, including web navigation/search/form actions, messaging/service
calls, peer-backed searches, and every dynamically discovered remote MCP tool.

The matcher compares normalized outbound argument strings with bounded
fragments derived from recent sensitive results. V1 normalization is
case-insensitive, decodes JSON and URL/query escaping, and considers whole
results, lines, and sufficiently long tokens. This intentionally catches the
common direct-copy and URL-encoding exfiltration shapes while remaining
deterministic. Tracking is bounded by result count, fragment count, and bytes.

On a match the hook calls the configured approval callback with action
`sensitive_egress`. The existing approval broker maps this to its distinct
`egress` category, so operators can approve once/session/always or deny through
the same TUI/dashboard machinery used by other capabilities. A missing callback,
cancelled context, or denial fails closed and returns a normal tool error without
calling the underlying dispatcher. Approval permits that one dispatch; it does
not remove or relabel the source result.

## Trust boundaries and lifecycle

- Labels and match fragments are per `Agent`, not global between agents.
- Recent sensitive fragments survive turn boundaries for the life of that agent
  because prior tool results can remain in its working context.
- Sub-agents and offsite surfaces use their own configured approval callback and
  therefore preserve the existing origin-aware approval routing.
- Parallel calls in one batch cannot consume each other's results: a result is
  labelled only after that batch finishes. A later iteration is checked.

## V1 limitations and follow-ups

This is explicit-label, direct-flow enforcement, not full taint propagation.
Model transformations that substantially rewrite, summarize, encrypt, split, or
encode sensitive text may evade substring matching; conversely, a long shared
token can cause a conservative approval prompt. Future versions should carry
field-level taint through structured tool results and model-generated values,
add content-aware declassification, expose labels in operator traces, and define
a stable plugin declaration contract. Prompt instructions are defense in depth,
not the enforcement boundary.
