package catalog

import "tenant/internal/logs/event"

// security: approvals (4100s), execution (4200s) and access (4300s)
// (docs/design/logging-v2.md §10.1). Each record is written where the action
// happens, before it returns: with Gate when the action waits on it, else
// Audit. Who acted, through which surface and under which approval are the
// event's own attributes (event.WithActor, event.WithApproval), not fields.

// Approvals. category, action and detail name what was asked; origin is the
// surface that raised it: local, discord, imessage or cron. via says what
// decided: operator (a person; the actor names who), mode, session, rule,
// lock, no_surface or cron (the cron-exec approver).

var ApprovalAsk = Security.Event(4101, event.Info, "Approval", "Approval requested: {action} ({category})", "category", "action", "detail", "origin").
	Keywords("audit.success").
	Doc("A gated action is waiting for the operator's decision.",
		"The agent, a job or a peer asked for an action whose category needs approval.",
		"Approve or deny it where it was asked; it expires when nobody answers.")

var ApprovalYes = Security.Event(4102, event.Info, "Approval", "Approval granted {grant}: {action} ({category})", "category", "action", "detail", "origin", "grant", "via", "nonce").
	Keywords("audit.success").
	Doc("A gated action was allowed, so it went ahead: once, for the session, or always (which sets its category, or its tool rule, to allow).",
		"The operator approved it (via operator; on a Discord card, nonce is the card's), or a standing setting allowed it without asking: the category's mode, a session grant, a tool rule, the cron-exec approver.",
		"None when intended. To be asked again, set the category back to ask (/permissions).")

var ApprovalNo = Security.Event(4103, event.Warning, "Approval", "Approval denied: {action} ({category})", "category", "action", "detail", "origin", "via", "nonce").
	Keywords("audit.failure").
	Doc("A gated action was denied, so it did not run.",
		"The operator denied it (via operator), a standing setting did (the category's mode, a tool rule, the surface's lock: the Discord relay's exec mode, or the cron-exec approver), or no operator could be reached (no_surface).",
		"None when intended; otherwise check the category's approval mode.")

var ApprovalShutdown = Security.Event(4104, event.Warning, "Approval", "Approval denied at shutdown: {action} ({category})", "category", "action", "detail", "origin").
	Keywords("audit.failure").
	Doc("A gated action still waiting for its decision was denied because Tenant was stopping.",
		"Tenant stopped while the prompt was open.",
		"None. The agent can ask again after the restart.")

var ApprovalExpired = Security.Event(4105, event.Warning, "Approval", "Approval expired unanswered: {action} ({category})", "category", "action", "detail", "origin", "nonce").
	Keywords("audit.failure").
	Doc("Nobody answered a gated action's prompt in time, so it did not run.",
		"The prompt timed out (relay.approval_timeout on Discord and iMessage), or the turn that asked ended first.",
		"None when intended. Otherwise answer sooner, or raise the timeout.")

var ModeChanged = Security.Event(4106, event.Info, "Approval", "{broker} approvals for {category} set to {mode}", "broker", "category", "mode", "was").
	Keywords("audit.success").
	Doc("The operator changed how one category of gated actions is approved on one surface (broker: local, discord or imessage): ask, allow or deny.",
		"/permissions, /relay permissions or /imessage permissions in the terminal, or the dashboard's Access page. An Always grant sets allow too; its record is the grant (4102).",
		"None when intended.")

// Execution: every os_exec and cron shell command, on every backend. The
// surface is where it came from; the approval, the one that let it run.

var ExecRan = Security.Event(4201, event.Info, "Execution", "Command ran on {backend}: exit {exit}", "command", "backend", "exit", "timed_out", "err").
	Keywords("audit.success").
	Doc("A shell command ran: the agent's os_exec, or a scheduled shell job. command is scrubbed of known secrets.",
		"The agent called os_exec (approved, or allowed by --allow-exec or its category's mode), or a cron shell job ran.",
		"None when expected. An unexpected command: read the turn (trace) and its approval, and tighten os_exec's approval mode or exec profile.")

var ExecRefused = Security.Event(4202, event.Warning, "Execution", "The sandbox refused a command on {backend}: {violations}", "command", "backend", "violations").
	Keywords("audit.failure").
	Doc("A confining exec backend refused what a command tried to reach, and the command was not re-run outside it.",
		"The command read or wrote outside its profile's roots, or used the network where the profile forbids it.",
		"None when the refusal is right. Otherwise widen the surface's exec profile (exec_profiles in settings), or approve the escalation when asked.")

var ExecEscalated = Security.Event(4203, event.Warning, "Execution", "A command the sandbox refused ran outside it ({why}): {violations}", "command", "backend", "violations", "why").
	Keywords("audit.success").
	Doc("A command the sandbox refused was re-run on the host: approved by the operator (why approved; the approval is the event's), or in a dry-run profile (why dry_run), which only reports.",
		"The operator approved os_exec_unsandboxed, or the surface's exec profile has enforce off.",
		"None when intended. Turn enforce on to make refusals final.")

var ExecMode = Security.Event(4204, event.Info, "Execution", "Exec mode for {target} turned {mode}", "target", "mode").
	Keywords("audit.success").
	Doc("The operator turned a surface's exec mode on or off (target: relay, the Discord relay's; cron, cron-exec's). Off refuses exec, write, destructive and send actions there whatever the category modes say.",
		"/relay exec or /cron exec in the terminal, or the dashboard.",
		"None when intended.")

// Access: the dashboard's sign-in and the requests it refuses, and peering.

var SignedIn = Security.Event(4301, event.Info, "Access", "Dashboard sign-in from {remote}", "remote").
	Keywords("audit.success").
	Doc("Someone signed in to the dashboard with its token.",
		"The sign-in form at /settings was sent the right token.",
		"None when it was you. An address you don't know means the token has leaked: rotate it.")

var SignedOut = Security.Event(4302, event.Info, "Access", "Dashboard sign-out from {remote}", "remote").
	Keywords("audit.success").
	Doc("A browser signed out of the dashboard: its sign-in cookie was cleared.",
		"The sign-out link.",
		"None.")

var AccessRefused = Security.Event(4303, event.Warning, "Access", "Request from {remote} refused: {reason}", "reason", "remote", "method", "route").
	Keywords("audit.failure").
	Doc("A request was refused before it reached anything (reason: token, a wrong dashboard or peer token; sign_in, a wrong token on the sign-in form; csrf; host; origin). A request with no token at all is not recorded here: the dashboard log has it.",
		"A token that changed, a script with an old one, a page on another site, a name the dashboard doesn't answer to, or someone probing.",
		"Many from an address you don't know mean someone is guessing: keep the dashboard and the peer listener off public networks. See the dashboard or peering log's event for the same request.")

var PeerPaired = Security.Event(4311, event.Info, "Peering", "Paired with peer {peer}", "peer", "instance", "direction").
	Keywords("audit.success").
	Doc("A peer record now holds a live token: a pairing approved here (direction inbound), this instance pairing with another (outbound), or an invite code minted for one (inbound, until it expires unused).",
		"tenant peer invite, /peer invite, or another instance pairing with this one.",
		"None when intended. Otherwise tenant peer remove the peer.")

var PeerUnpaired = Security.Event(4312, event.Info, "Peering", "Unpaired peer {peer} ({how})", "peer", "how").
	Keywords("audit.success").
	Doc("A peer can no longer authenticate: its record was removed (how remove) or its tokens revoked (how revoke).",
		"tenant peer remove or revoke, or /peer remove.",
		"None when intended. Pair again to restore it.")

// system: the Security log's own health (4000s).

var AuditFailed = System.Event(4001, event.Critical, "Audit", "A Security record was not written: {err}", "event", "policy", "err").
	Doc("An audit record (a security event) could not be committed to logbook.db, so the Security log is missing it. The text mirror still has its line. policy is logs.security.on_failure: with continue the action went ahead; with deny, gated actions are refused until records are written again.",
		"The store is failing (a full disk, a locked or corrupt database), or it has no Security log key.",
		"Read the error and the logbook log; fix the disk or the key, then check that later records land (a system/4002 follows). tenant logs verify security checks the chain.")

var AuditRecovered = System.Event(4002, event.Info, "Audit", "Security records are written again, after {failed} were not", "failed").
	Doc("An audit record committed after one or more failed, so the Security log is being written again.",
		"Whatever stopped the store's commits was fixed or went away.",
		"None. The records that failed (system/4001) are only in the text mirror.")
