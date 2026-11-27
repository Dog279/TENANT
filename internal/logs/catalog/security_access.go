package catalog

import "tenant/internal/logs/event"

// security: approvals (4100s), execution (4200s) and access (4300s)
// (docs/design/logging-v2.md §10.1). Each record is written where the action
// happens, before it returns: with Gate when the action waits on it, else
// Audit. Who acted and through which surface are passed as "actor" and
// "surface", which become the event's own attributes; the approval comes from
// the context (event.WithApproval).

// Approvals. category, action and detail name what was asked; origin is the
// surface that raised it: local, discord, imessage or cron. via says what
// decided: operator (a person; the actor names who), mode, session, rule,
// lock, no_surface or cron (the cron-exec approver). For an action that lands
// on a fleet machine, host is the id the master gave the machine and
// host_name its label, escaped to plain ASCII (detail starts with both), and
// via may also be origin (the origin's own rules refused it), fleet (the
// origin may not reach that machine, or the request was not fit to show) or
// rate_limit (the machine asked too often).

var ApprovalAsk = Security.Event(4101, event.Info, "Approval", "Approval requested: {action} ({category})", "category", "action", "detail", "origin", "host", "host_name").
	Keywords("audit.success").
	Doc("A gated action is waiting for the operator's decision.",
		"The agent, a job or a peer asked for an action whose category needs approval. For an action on a fleet machine, host and host_name say which machine, and detail starts with them.",
		"Approve or deny it where it was asked; it expires when nobody answers.")

var ApprovalYes = Security.Event(4102, event.Info, "Approval", "Approval granted {grant}: {action} ({category})", "category", "action", "detail", "origin", "grant", "via", "nonce", "host", "host_name").
	Keywords("audit.success").
	Doc("A gated action was allowed, so it went ahead: once, for the session, or always (which sets its category, or its tool rule, to allow).",
		"The operator approved it (via operator; on a Discord card, nonce is the card's), or a standing setting allowed it without asking: the category's mode, a session grant, a tool rule, the cron-exec approver.",
		"None when intended. To be asked again, set the category back to ask (/permissions).")

var ApprovalNo = Security.Event(4103, event.Warning, "Approval", "Approval denied: {action} ({category})", "category", "action", "detail", "origin", "via", "nonce", "host", "host_name").
	Keywords("audit.failure").
	Doc("A gated action was denied, so it did not run.",
		"The operator denied it (via operator), a standing setting did (the category's mode, a tool rule, the surface's lock: the Discord relay's exec mode, or the cron-exec approver), or no operator could be reached (no_surface). For an action on a fleet machine (host): the origin's own rules refused it (origin), the origin may not reach that machine or the request was not fit to show (fleet), or the machine asked too often (rate_limit).",
		"None when intended; otherwise check the category's approval mode.")

var ApprovalShutdown = Security.Event(4104, event.Warning, "Approval", "Approval denied at shutdown: {action} ({category})", "category", "action", "detail", "origin", "host", "host_name").
	Keywords("audit.failure").
	Doc("A gated action still waiting for its decision was denied because Tenant was stopping.",
		"Tenant stopped while the prompt was open.",
		"None. The agent can ask again after the restart.")

var ApprovalExpired = Security.Event(4105, event.Warning, "Approval", "Approval expired unanswered: {action} ({category})", "category", "action", "detail", "origin", "nonce", "host", "host_name").
	Keywords("audit.failure").
	Doc("Nobody answered a gated action's prompt in time, so it did not run.",
		"The prompt timed out (relay.approval_timeout on Discord and iMessage), or the turn that asked ended first.",
		"None when intended. Otherwise answer sooner, or raise the timeout.")

var ModeChanged = Security.Event(4106, event.Info, "Approval", "{broker} approvals for {category} set to {mode}", "broker", "category", "mode", "was").
	Keywords("audit.success").
	Doc("The operator changed how one category of gated actions is approved on one surface (broker: local, discord or imessage) or for one fleet machine (broker: fleet: followed by the machine's id): ask, allow or deny.",
		"/permissions, /relay permissions or /imessage permissions in the terminal, tenant fleet permissions, or the dashboard's Access page or, for a fleet machine, its Fleet page. An Always grant sets allow too; its record is the grant (4102).",
		"None when intended.")

// Execution: every os_exec and cron shell command, on every backend. The
// surface is where it came from; the approval, the one that let it run.

var ExecRan = Security.Event(4201, event.Info, "Execution", "Command ran on {backend}: exit {exit}", "command", "backend", "exit", "timed_out", "err",
	"image", "image_id", "mounts", "network", "limits", "env", "output_bytes", "output_capped", "elapsed_ms").
	Keywords("audit.success").
	Doc("A shell command ran: the agent's os_exec, or a scheduled shell job. command is scrubbed of known secrets. On the container backend the record also holds the boundary the run had: the image and the id its tag resolved to, every mount with its mode, the network, the caps, the names (never the values) of the host's environment variables passed in, how many bytes the command wrote and whether that hit the output cap, and how long it took.",
		"The agent called os_exec (approved, or allowed by --allow-exec or its category's mode), or a cron shell job ran.",
		"None when expected. An unexpected command: read the turn (trace) and its approval, and tighten os_exec's approval mode or exec profile.")

var ExecRefused = Security.Event(4202, event.Warning, "Execution", "The sandbox refused a command on {backend}: {violations}", "command", "backend", "violations").
	Keywords("audit.failure").
	Doc("A confining exec backend refused what a command tried to reach, and the command was not re-run outside it.",
		"The command read or wrote outside its profile's roots, or used the network where the profile forbids it.",
		"None when the refusal is right. Otherwise widen the surface's exec profile (exec_profiles in settings), or approve the escalation when asked.")

var ExecEscalated = Security.Event(4203, event.Warning, "Execution", "A command the sandbox refused was re-run {where} ({why}): {violations}", "command", "backend", "violations", "why", "where").
	Keywords("audit.success").
	Doc("A command the sandbox refused was re-run: approved by the operator (why approved; the approval is the event's), or in a dry-run profile (why dry_run), which only reports. where says where it re-ran: on the host, outside the sandbox, or, on the container backend, in a fresh container with one more grant (the refused folder, or the network for that command). The record of that re-run is the 4201 beside it.",
		"The operator approved os_exec_unsandboxed, or the surface's exec profile has enforce off.",
		"None when intended. Turn enforce on to make refusals final.")

var RelayExecMode = Security.Event(4204, event.Info, "Execution", "The Discord relay's exec mode turned {mode}", "mode").
	Keywords("audit.success").
	Doc("The operator turned the Discord relay's exec mode on or off. Off refuses exec, write, destructive, sandbox and send actions offsite whatever the relay's category modes say. cron-exec's switch is security/4704.",
		"/relay exec in the terminal, or the dashboard's Access page.",
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
	Doc("A request was refused before it reached anything (reason: token, a wrong dashboard or peer token; sign_in, a wrong token on the sign-in form; csrf; host; origin; source, an address outside the local network on the dashboard's LAN listener). A request with no token at all is not recorded here: the dashboard log has it.",
		"A token that changed, a script with an old one, a page on another site, a name the dashboard doesn't answer to, or someone probing.",
		"Many from an address you don't know mean someone is guessing: keep the dashboard and the peer listener off public networks. See the dashboard or peering log's event for the same request.")

var DashboardLAN = Security.Event(4304, event.Info, "Access", "The dashboard's LAN listener turned {state}", "state", "address").
	Keywords("audit.success").
	Doc("The operator turned serving the dashboard to the local network on or off. On, other computers on the network can reach the sign-in form over HTTPS at address; nothing but loopback and the tailnet can when it is off.",
		"/dashboard serve lan or /dashboard serve off in the terminal, the --dashboard-lan flag, or dashboard.lan in config.json at launch.",
		"None when intended. Otherwise /dashboard serve off, and check who can edit config.json.")

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

// The fleet (TEN-531): a master's child machines. machine is the id the
// master gave the machine and name its label.

var FleetEnrollRefused = Security.Event(4321, event.Warning, "Fleet", "Fleet enrollment from {remote} refused: {reason}", "reason", "remote", "name", "machine", "certificate").
	Keywords("audit.failure").
	Doc("The fleet's enroll endpoint turned an attempt away. reason says why: rate_limited, bad_method, no_certificate, enrollment_off, store_unreadable, body_too_large, bad_request, unknown_code, bad_proof, expired_code, conflict (the name or the certificate is in use). name is the machine name on the code, when the code was recognised; certificate is the fingerprint the attempt presented.",
		"A code that was mistyped or has expired, a machine enrolled twice, or something on the network probing the fleet port.",
		"For a machine of yours, make a new code with tenant fleet enroll. Many from an address you don't know mean someone is guessing: keep the fleet port off public networks.")

var FleetCodeReplayed = Security.Event(4322, event.Error, "Fleet", "A spent fleet enrollment code for {name} was presented again from {remote}", "name", "machine", "remote", "certificate").
	Keywords("audit.failure").
	Doc("An enrollment code that had already enrolled a machine was presented a second time, and refused. Someone other than the machine it enrolled holds the code, or held it first. machine is the id of the machine the code enrolled.",
		"The install was run twice on one machine, or the code leaked and one of the two (the machine that is pending, or this attempt) is not yours.",
		"Check the pending machine before you confirm it: compare the six digits on its own screen. If in doubt, tenant fleet revoke it and enroll it again with a new code.")

var FleetMachineChanged = Security.Event(4323, event.Info, "Fleet", "Fleet machine {name}: {change}", "name", "machine", "change", "detail").
	Keywords("audit.success").
	Doc("The operator changed the fleet: change is initialised (this Tenant became a master), code issued, confirmed, renamed, revoked, removed (revoked, and its record and approval scheme deleted), scheme changed (the machine's approval scheme was replaced by a fresh copy of the master's, or the fleet's rules and its groups' rules were laid over it again; detail says which and names the groups) or rule changed (one of its own rules set or removed). A change made to several machines at once has one record per machine. Where laying rules replaced rules of the machine's own, detail names them with the mode each had. For a confirmed machine, detail is what the machine reported about itself (its own words), the categories the operator loosened, and the fleet's defaults (fleet_defaults) and the groups (groups_laid) laid over its copy. surface is where the operator was: cli, tui, dashboard, or dashboard-remote for a machine confirmed from a signed-in browser that is not on the computer running Tenant (detail then names the session and its address in asked_by). change is also remote confirmation turned on or off: whether such a browser may confirm a machine at all.",
		"tenant fleet init, enroll, confirm, rename, revoke or permissions --from; /fleet confirm or revoke in the terminal; or the dashboard's Fleet page.",
		"None when intended. Otherwise tenant fleet revoke the machine.")

var FleetSettingsChanged = Security.Event(4325, event.Info, "Fleet", "Fleet {what}: {change}", "what", "change", "detail").
	Keywords("audit.success").
	Doc("The operator changed what the fleet lays over its machines' schemes on purpose: what is a group (group <name>) or the defaults for new machines. change is created, deleted, machines added, machines removed, rule set, rule removed, default set or default unset; detail says which. No machine's scheme moves with it: a machine changes when it is confirmed, copied again or applied, and each of those has its own record (security/4323). surface is where the operator was: cli, tui or dashboard.",
		"tenant fleet group, tenant fleet permissions --defaults set|unset|add|rm, /fleet group and /permissions fleet --defaults in the terminal, or the dashboard's Fleet page.",
		"None when intended. A loosened default or an allow rule in a group reaches a machine only when it is applied: check the group's rules before tenant fleet group apply.")

var FleetConfirmRefused = Security.Event(4324, event.Warning, "Fleet", "Fleet machine {name} was not confirmed: {reason}", "name", "machine", "reason").
	Keywords("audit.failure").
	Doc("A confirmation of a pending fleet machine was refused, and the machine stays pending. On the dashboard the actor is the operator when a browser signed in on the computer running Tenant asked, and otherwise the address the request came from. With remote confirmation turned on, a refusal of a signed-in browser elsewhere has the surface dashboard-remote and names its session and address.",
		"The six digits typed are not the ones that machine shows (a typo, or the pending machine is not the one in front of you), or the machine is not waiting for confirmation. On the dashboard also: the request did not come from a browser signed in on the computer running Tenant (it came with the API key, by a tailnet or proxy name, through a proxy or from another address) while remote confirmation is off; or too many attempts for that machine failed and the next ones have to wait. With remote confirmation on: the sign-in key given again was wrong, was sent in the address, or too many wrong keys came from that address.",
		"Read the six digits from the machine's own screen and try again. If they never match, the pending machine is not yours: tenant fleet revoke it. A machine is confirmed in the terminal, or in a browser on the computer running Tenant at the dashboard's 127.0.0.1 address, or, after tenant fleet remote-confirm on, in a signed-in browser elsewhere with the sign-in key again; a refusal from an address you don't know means someone who can reach the dashboard tried. Wrong keys from a signed-in session mean someone has a session and not the key: sign out everywhere by restarting Tenant, and turn remote confirmation off (tenant fleet remote-confirm off).")

// system: the Security log's own health (4000s).

var AuditFailed = System.Event(4001, event.Critical, "Audit", "A Security record was not written: {err}", "event", "policy", "err").
	Doc("An audit record (a security event) could not be committed to logbook.db, so the Security log is missing it. The text mirror still has its line. policy is logs.security.on_failure: with continue the action went ahead; with deny, gated actions are refused until records are written again.",
		"The store is failing (a full disk, a locked or corrupt database), or it has no Security log key.",
		"Read the error and the logbook log; fix the disk or the key, then check that later records land (a system/4002 follows). tenant logs verify security checks the chain.")

var AuditRecovered = System.Event(4002, event.Info, "Audit", "Security records are written again, after {failed} were not", "failed").
	Doc("An audit record committed after one or more failed, so the Security log is being written again.",
		"Whatever stopped the store's commits was fixed or went away.",
		"None. The records that failed (system/4001) are only in the text mirror.")
