package sandbox

import "tenant/internal/logs/event"

// Events is the sandbox's event provider, writing the sandbox log (TEN-388):
// every command run through the seam, on every backend, and what the boundary
// did to it. The composition root writes them (cmd/tenant/execprofiles.go);
// the exec audit record is the Security log's (TEN-397).
var Events = event.NewProvider("sandbox", "sandbox")

// Run is the activity of one command run through the seam: an os_exec call or
// a cron shell job. Its start is related to the turn or job that ran it, and
// the command's environment carries its ID (TENANT_ACTIVITY_ID).
var Run = Events.Event(1, event.Info, "Run", "Command on {backend} for {surface}", "surface", "backend", "command", "limits").
	Doc("A command ran through the sandbox: start, then stop with its duration; fail when the backend could not start it. Its exit is sandbox 1001, and anything the boundary refused is a 2000s event in between. command is scrubbed and cut at 300 characters.",
		"The agent called os_exec, or a cron shell job came due.",
		"On a fail, read the error: the backend's launcher (sandbox-exec, bubblewrap, the WSL distro) is missing or broken. `tenant sandbox probe` checks it.")

var Exited = Events.Event(1001, event.Info, "Run", "Command exited: {status}", "surface", "backend", "pid", "exit", "status", "timed_out", "violations", "tail").
	Doc("A command run through the sandbox exited. tail is the last 512 bytes of its output (stdout and stderr together, as the model got it), scrubbed; violations lists what the boundary refused or cut short. For an escalated or dry-run command it describes the host re-run.",
		"The command finished, failed, or hit its wall-clock limit (timed_out).",
		"None. A non-zero exit is the command's own result, which the model sees.")

// Refusals: one event per outcome.

var Refused = Events.Event(2001, event.Warning, "Refusal", "The sandbox refused a command: {violations}", "surface", "backend", "command", "exit", "violations").
	Doc("The boundary refused a file or network access, or a Windows program (in WSL), and the command was not run again: its failure went back to the model or the cron job.",
		"The command reached outside the profile's roots or network, or started a Windows program. Enforce is on, and the escalation was denied or nobody could be asked (an unattended surface such as cron).",
		"None if the refusal was right. If the command needs that access, widen the surface's exec_profiles entry (read_roots, write_roots, network), or approve the escalation next time.")

var WouldRefuse = Events.Event(2002, event.Warning, "Refusal", "Dry run: the sandbox would have refused a command, which ran outside it: {violations}", "surface", "backend", "command", "exit", "violations").
	Doc("The profile is in dry run (\"enforce\": false): the boundary refused an access, and the command ran again outside the sandbox, unconfined (on the host, or on Windows as sh in the same WSL distro), so the refusal is only reported.",
		"The command reached outside the profile's roots or network while the surface's exec_profiles entry has enforce off.",
		"Widen the profile for the accesses that are legitimate, then turn enforce on. Until then the sandbox confines nothing on this surface.")

var Escalated = Events.Event(2003, event.Warning, "Refusal", "A command the sandbox refused was approved to run outside it: {violations}", "surface", "backend", "command", "exit", "violations").
	Doc("The boundary refused an access, a person approved os_exec_unsandboxed, and the command ran again outside the sandbox, unconfined (on the host, or on Windows as sh in the same WSL distro). The approval itself is in the Security log.",
		"The command needed an access outside the profile, and the operator allowed it once.",
		"If the same escalation keeps being approved, approve it \"always\": that saves the refused folder as an exception for the surface (exec_exceptions), so the access stays confined to what it needs.")

var ExceptionSaved = Events.Event(2004, event.Warning, "Exception", "Sandbox exception saved: {surface} commands may write under {path}", "surface", "path").
	Doc("The operator approved a sandbox escalation \"always\", and the folder the sandbox refused was saved in exec_exceptions in the agent's settings file: the surface's sandboxed commands may write there from now on, without asking. The approval itself is in the Security log.",
		"Always, on an os_exec_unsandboxed prompt whose refusal named a folder outside Tenant's own dirs, the credential stores, the system folders and roots, and the home folder itself.",
		"None if it was meant. To take it back, delete the path from exec_exceptions in settings.<agent>.json; `tenant permissions effective` lists them.")

var ExceptionSkipped = Events.Event(2006, event.Warning, "Exception", "Sandbox exception {path} for {surface} is not used: {why}", "surface", "path", "why").
	Doc("An exec_exceptions folder is not what it was saved as, so it is not made writable: it is gone, is no longer a folder, or now resolves somewhere else (a symlink swapped in, perhaps to the home folder). Said once per process for each such entry.",
		"The folder was deleted or replaced after it was saved, or the entry names a path through a symlink (write its real path).",
		"If the folder moved on purpose, save its real path in exec_exceptions; otherwise delete the entry, and look at what replaced the folder.")

var DryRunOnHost = Events.Event(2005, event.Warning, "Backend", "The {surface} exec profile's backend {backend} is unavailable; it is in dry run, so the command ran on the host: {err}", "surface", "backend", "err").
	Doc("A dry-run profile (\"enforce\": false) names a sandbox backend that does not work here. A dry run never blocks a command, so it ran on the host, unconfined, and nothing about it was observed.",
		"The profile names a backend this OS or build lacks (restricted without bubblewrap or WSL), or its launcher is missing.",
		"Install what the error names (`tenant sandbox probe` says what is unavailable), or set another backend for the surface in exec_profiles. With enforce on, the same profile runs nothing.")

// Errors.

var BackendUnavailable = Events.Event(3001, event.Error, "Backend", "The {surface} exec profile's backend {backend} is unavailable, so nothing ran: {err}", "surface", "backend", "err").
	Doc("A command was not run because the sandbox backend its profile names does not work here. An enforcing profile fails closed: it never falls back to the host.",
		"The profile names a backend this OS or build lacks (jobobject off Windows, restricted without bubblewrap or WSL), or its launcher is missing.",
		"Install what the error names (`tenant sandbox probe` says what is unavailable), or set another backend for the surface in exec_profiles.")
