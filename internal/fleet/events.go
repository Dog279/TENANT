package fleet

import "tenant/internal/logs/event"

// Events is the fleet's event provider, writing the fleet log: what a master
// does with its machines, from the day it becomes one. IDs follow the blocks
// in internal/logs/event's package doc. The audit records of the same changes
// (who made them, from where) belong to the Security log (security/4321 to
// 4324); these are the operational side, where an operator looks to see what
// the fleet is doing.
//
// machine is the id the master gave a machine and name its label. An
// operator's change carries the surface it was made from (cli, tui,
// dashboard) as the event's own attribute. Whatever a
// machine says about itself is another computer's text: a call site hands it
// over already escaped and in quotes.
//
// A child machine's own process has no log book. It keeps its audit log and
// writes to its own log and the system's; these events are the master's.
var Events = event.NewProvider("fleet", "fleet")

// Lifecycle.

// Listening is the activity of the fleet port's lifetime in this process:
// start when it holds the fleet and listens, stop when it lets go.
var Listening = Events.Event(1, event.Info, "Listener", "Fleet port on {addr}", "addr", "role").
	Doc("This process holds the fleet and its port is open for the fleet's machines: start, then stop with its uptime when the process stops, the fleet's identity goes, or the port moves.",
		"This Tenant is a fleet master (tenant fleet init, /fleet init or the Fleet page's Set up the fleet) and no other Tenant process on the same config holds the fleet. role is serve or tui.",
		"None. A terminal app holds the fleet only while it is open: run tenant serve as a service for machines that should stay connected.")

var HeldElsewhere = Events.Event(2, event.Info, "Listener", "The fleet is held by another Tenant ({holder}); this one serves no machine", "holder").
	Doc("Another Tenant process on the same config directory holds the fleet's links, so this one opened no port. It can still list machines and change the fleet; calls to machines are made by the holder.",
		"tenant serve and a terminal app run on the same config at once: the first to start holds the fleet.",
		"None when intended. To move the fleet here, stop the other process.")

var MemoryReady = Events.Event(3, event.Info, "Memory", "The machines' memory is open", "machines").
	Doc("The process that holds the fleet opened the fleet key and the machines' memory, so each machine's memory is kept and compared with its own copy from now on.",
		"The fleet has a fleet key and a recovery key, and this process holds the fleet.",
		"None.")

var MemoryUnavailable = Events.Event(2003, event.Warning, "Memory", "The machines' memory is unavailable: {why}", "why").
	Doc("The fleet works, but no machine's memory is kept or read in this process. It is tried again when the fleet's keys change.",
		"The fleet has no recovery key yet, its fleet key is missing or does not open, or another process holds the memory.",
		"For a missing recovery key, run tenant fleet init (or /fleet init) at a terminal and save the identity it shows. Otherwise read why.")

var ListenerNotStarted = Events.Event(3001, event.Error, "Listener", "Fleet port on {addr} did not start: {err}", "addr", "err").
	Doc("The fleet port could not be opened, so no machine can connect. It is tried again every few seconds; this is logged once per cause.",
		"The address is in use, is not one of this computer's, or the fleet's identity file cannot be read.",
		"Fix what the error names, or move the port: tenant fleet init --listen HOST:PORT. Nothing needs a restart.")

var ServerError = Events.Event(2001, event.Warning, "Listener", "Fleet port: {err}", "err").
	Doc("Go's HTTP server reported a problem on the fleet port that it handled on its own, most often a TLS handshake that was refused.",
		"Something that is not one of the fleet's machines connected to the port, or a machine presented a certificate the master does not know.",
		"None for the odd one. Many from an address you don't know mean the port is being probed: keep it off public networks.")

var HolderNotRecorded = Events.Event(2002, event.Warning, "Listener", "Could not record who holds the fleet: {err}", "err").
	Doc("This process holds the fleet, but could not write down that it does. Other processes then cannot say which process holds it.",
		"The config directory is not writable, or its disk is full.",
		"Check the config directory's permissions and free space.")

// Setting up: the operator's changes. Each has its audit record in the
// Security log; these say what the fleet looks like after them.

var Initialised = Events.Event(1001, event.Info, "Setup", "This Tenant became a fleet master", "certificate", "listen", "addresses").
	Doc("This Tenant now has a fleet: its certificate and fleet key were made. Its port opens within a few seconds in the running Tenant.",
		"tenant fleet init or enroll, /fleet init in the terminal app, or Set up the fleet on the dashboard's Fleet page (surface says which).",
		"Add a machine: tenant fleet enroll <name>, /fleet enroll, or Add machine on the Fleet page. Make the recovery key at a terminal if init did not.")

var MachineChanged = Events.Event(1002, event.Info, "Setup", "Fleet machine {name}: {change}", "name", "machine", "change", "detail").
	Doc("The operator changed the fleet: change is code issued, confirmed, renamed, revoked, settings copied again, and the like. The same change is in the Security log (security/4323) with who made it.",
		"tenant fleet enroll, confirm, rename, revoke or permissions --from; the same in the terminal app; or the dashboard's Fleet page.",
		"None when intended.")

var Enrolled = Events.Event(1003, event.Info, "Setup", "Machine {name} enrolled from {remote} with its one-time code", "name", "machine", "remote", "certificate").
	Doc("A machine spent its one-time code and is now pending: it shows six digits on its own screen and the master will not call it until they are typed here.",
		"The line from tenant fleet enroll (or Add machine) was pasted on the machine.",
		"Compare the six digits on the machine's own screen, then tenant fleet confirm <name>, /fleet confirm, or the Fleet page in a browser on this computer.")

var AddressesChanged = Events.Event(5001, event.Info, "Setup", "The addresses machines dial are now {addresses}", "addresses").
	Doc("The list of host:port addresses a new code tells a machine to dial was replaced. Connected machines are told the new list over their links.",
		"tenant fleet init --address, tenant fleet address add or remove, or Set up the fleet on the dashboard.",
		"None when intended. A machine at another site needs an address it can reach.")

var RelayChanged = Events.Event(5002, event.Info, "Setup", "Turns from the {relay} relay may {state} the fleet", "relay", "state").
	Doc("Whether a turn that came from a relay (Discord or iMessage) may use the fleet's tools at all was switched. state is reach or not reach.",
		"tenant fleet relay or /fleet relay.",
		"None when intended. It is off by default.")

var SettingsChanged = Events.Event(5003, event.Info, "Setup", "Fleet {what}: {change}", "what", "change", "detail").
	Doc("A group of machines, or the defaults new machines start with, changed: what names which. No machine moved: a group's rules reach its members when it is applied, and the defaults when a machine is confirmed or copied again. The same change is in the Security log (security/4325) with who made it.",
		"tenant fleet group, tenant fleet permissions --defaults set|unset|add|rm, /fleet group in the terminal app, or the dashboard's Fleet page.",
		"None when intended. To lay a group's rules over its members: tenant fleet group apply <group>.")

// Enrollment and confirmation turned away (the operational side of
// security/4321, 4322 and 4324).

var EnrollRefused = Events.Event(2101, event.Warning, "Setup", "Enrollment from {remote} refused: {reason}", "reason", "remote", "name", "machine").
	Doc("The fleet's enroll endpoint turned an attempt away, and nothing was added. The audit record is security/4321.",
		"A code that was mistyped or has expired, a machine enrolled twice, or something on the network probing the fleet port.",
		"For a machine of yours, make a new code with tenant fleet enroll. Many from an address you don't know mean someone is guessing.")

var ConfirmRefused = Events.Event(2102, event.Warning, "Setup", "Machine {name} was not confirmed: {reason}", "name", "machine", "reason").
	Doc("A confirmation of a pending machine was refused, and it stays pending. The audit record is security/4324.",
		"The six digits typed were not the ones the machine shows, or the machine is not waiting to be confirmed.",
		"Read the six digits from the machine's own screen and try again. If they never match, the pending machine is not yours: revoke it.")

var CodeReplayed = Events.Event(3101, event.Error, "Setup", "A spent enrollment code for {name} was presented again from {remote}", "name", "machine", "remote").
	Doc("A one-time code that already enrolled a machine was used a second time, and refused. Someone other than that machine has the code, or had it first. The audit record is security/4322.",
		"The paste line was run on two machines, or the code leaked.",
		"Check the pending machine before you confirm it: compare the six digits on its own screen. If in doubt, revoke it and enroll it again.")

// Links.

var Connected = Events.Event(1101, event.Info, "Link", "Machine {name} connected from {remote}", "name", "machine", "remote", "link").
	Doc("A machine's link to the master is up. A confirmed machine can be called from now on; a pending one only waits. link is the link version it speaks.",
		"The machine's Tenant child started, or its network came back.",
		"None.")

var Disconnected = Events.Event(1102, event.Info, "Link", "Machine {name} disconnected: {why}", "name", "machine", "remote", "why").
	Doc("A machine's link ended. Calls in flight on it end with their outcome unknown, and it cannot be called until it connects again.",
		"The machine stopped or restarted, its network dropped, it connected again (the older link is given up), or the master closed the link: a revoke, a pause, or its own shutdown.",
		"None when expected. A machine that keeps dropping: read why, and tenant child status on the machine.")

var LinkNotUpgraded = Events.Event(1103, event.Verbose, "Link", "A connection from {remote} for {machine} did not become a link: {err}", "machine", "remote", "err").
	Doc("A machine reached the fleet port with its certificate, and the connection ended before the link was set up.",
		"The machine went away mid-handshake, or something between the two closed the connection.",
		"None: the machine tries again by itself.")

var NotAllowed = Events.Event(2111, event.Warning, "Link", "Machine {name} sent what the link does not allow: {what}", "name", "machine", "what").
	Doc("A machine sent a message a child never sends to its master, or more of them than the link takes, and it was dropped. A machine that keeps on is disconnected.",
		"A machine running a different or modified Tenant, or one that has been taken over.",
		"If it is not a version mismatch, treat the machine as compromised: tenant fleet revoke it.")

var ReportNotRecorded = Events.Event(2112, event.Warning, "Link", "What machine {name} reports about itself was not recorded: {err}", "name", "machine", "err").
	Doc("A machine said what system, account, version and sandbox it has, and the master could not save it. The Fleet page and tenant fleet show keep showing what was recorded before.",
		"fleet.json is not writable, or is locked by another process.",
		"Check the config directory's permissions and free space.")

var Rekeying = Events.Event(1104, event.Info, "Link", "Machine {name} is moving to a new certificate", "name", "machine", "certificate").
	Doc("A machine enrolled before Tenant moved to ML-DSA-87 certificates sent the new certificate it has made for itself, and the master pinned it beside the old one. The machine connects again with it by itself.",
		"The machine's Tenant was updated to one that knows the new certificates, and it connected with its old one.",
		"None. Until it has moved, the machine can be updated and nothing else.")

var Rekeyed = Events.Event(1105, event.Info, "Link", "Machine {name} moved to its new certificate", "name", "machine", "certificate").
	Doc("A machine connected with the new certificate it had sent, and the master dropped the old one. The machine is on ML-DSA-87 from now on and can be called again.",
		"The step after the machine sent its new certificate.",
		"None.")

var RekeyRefused = Events.Event(2114, event.Warning, "Link", "Machine {name} was not moved to a new certificate: {err}", "name", "machine", "err").
	Doc("A machine on its old certificate sent a new one and the master did not pin it. The machine stays on the old one, where it can be updated and nothing else, and tries again when it next connects.",
		"What it sent is not an ML-DSA-87 certificate, is another machine's, or fleet.json could not be saved.",
		"Read err. For a file that cannot be saved, check the config directory. If it keeps on, revoke the machine and enroll it again.")

var AddressesNotTaken = Events.Event(2113, event.Warning, "Link", "Machine {name} did not take the master's addresses: {err}", "name", "machine", "err").
	Doc("The master sent a connected machine its current list of addresses and the machine did not acknowledge it. The machine keeps dialling the addresses it has.",
		"The link dropped as the list was sent, or the machine runs a Tenant too old to take it.",
		"None for a dropped link: the list is sent again when the machine connects. For an old Tenant, update the machine.")

// Calls: the agent's work on a machine.

var Called = Events.Event(1201, event.Info, "Call", "Call {tool} on {name}: {outcome}", "host", "name", "tool", "call", "origin", "outcome", "took", "audit").
	Doc("The agent made one tool call on a fleet machine. host is the machine's id, origin where the turn came from, and audit the hash of the record the machine wrote of the same call in its own audit log, so the two logs can be laid side by side.",
		"A turn used one of the fleet tools on this machine, and the machine's approval settings let it.",
		"None. For a call that was not done, outcome says why.")

var CallResult = Events.Event(1202, event.Info, "Call", "The rest of call {call}'s result on {name} was read", "host", "name", "call", "origin", "audit").
	Doc("The agent read a further page of a call's result that the machine kept for it.",
		"A call returned more than one answer holds, and the turn asked for the rest.",
		"None.")

var AsksFirst = Events.Event(1203, event.Info, "Call", "A call of {tool} on {name} asks first: this turn has read from {read_from}", "host", "name", "tool", "read_from").
	Doc("A call that would have run without asking is asked about instead, because earlier in the same turn the agent read from another machine: what one machine said must not steer work on the next without the operator seeing it.",
		"One turn touched more than one machine, and this call is not a read.",
		"Answer the approval card. To avoid it, keep a turn to one machine.")

var NoteKept = Events.Event(1204, event.Info, "Call", "A note of this turn's work on {name} was kept for its memory", "host", "name", "origin").
	Doc("What the agent did on a machine in this turn was written down, to go into that machine's own memory when the turn ends.",
		"A turn made calls on the machine.",
		"None.")

var WorkRemembered = Events.Event(1205, event.Info, "Call", "This turn's work goes into the memory of {hosts}", "hosts").
	Doc("After a turn that worked on fleet machines, its notes are being added to each machine's memory.",
		"A turn made calls on one or more machines.",
		"None.")

var CallHeld = Events.Event(1206, event.Info, "Call", "A call to {name} waited {waited} for the machine's update to {to}", "name", "machine", "to", "waited").
	Doc("A call for a machine that was applying an update was kept back, unsent, until the machine said how the update went or the wait's limit passed. It was then made as any call is.",
		"A turn called a machine between the moment it said it was applying an update and its first link after it: the machine restarts into the new version in between, and a call sent then would have ended with the process that had it.",
		"None. A wait of several minutes means the machine did not say how its update went: tenant fleet show NAME says where it stands.")

var WorkNotRemembered = Events.Event(2201, event.Warning, "Call", "This turn's work on machine {host} did not go into its memory: {err}", "host", "err").
	Doc("The turn's notes about a machine could not be added to that machine's memory. The work was done; only the memory of it is missing.",
		"The machine's memory is unavailable (no recovery key, or held by another process), or its store could not be written.",
		"Check the Fleet page's memory column for the machine, and tenant doctor.")

// Updates sent over a machine's link.

var UpdateStep = Events.Event(1301, event.Info, "Update", "Update of {name}: {line}", "name", "machine", "stage", "line", "from", "to").
	Doc("A step of sending a machine a newer Tenant over its link: fetching the release, sending it, the machine applying it, done. stage is fetching, sending, applying or done.",
		"tenant fleet update, or Update this machine on the Fleet page.",
		"None.")

var UpdateNotDone = Events.Event(2301, event.Warning, "Update", "Update of {name} {stage}: {line}", "name", "machine", "stage", "line", "from", "to").
	Doc("An update of a machine did not go through. stage is refused (it never started), failed, or rolled_back (the machine put its old version back because the new one did not reach the master within a minute).",
		"The release could not be fetched or verified, the machine is offline or refuses the update under its own rule, the link dropped, or the new version did not start.",
		"Read line. The machine runs what it ran before; tenant fleet update <name> tries again.")

// The machines' memory: the master's copy of each machine's memory, and the
// encrypted copies the machine holds of it.

var MemoryCopy = Events.Event(1401, event.Info, "Memory", "Memory of machine {machine} was {what} a copy (generation {generation})", "machine", "what", "generation", "aside").
	Doc("The master compared its copy of a machine's memory with the copy the machine holds and brought the two in step: what says which way (sent to the machine, or rebuilt from the machine's).",
		"The machine connected, or the regular comparison ran, and the two differed.",
		"None.")

var ForgetApplied = Events.Event(1402, event.Info, "Memory", "A forget was applied to the memory of machine {machine}", "machine", "removed", "generation").
	Doc("Something the operator asked to be forgotten was removed from a machine's memory, and a new generation of it was made.",
		"A forget on the master that also names what a machine remembers.",
		"None.")

var ForgetReapplied = Events.Event(1403, event.Info, "Memory", "A forget was applied again to the memory of machine {machine}, rebuilt from an older copy", "machine", "from").
	Doc("A machine's memory was rebuilt from a copy made before a forget, so the forget was applied to it again: what was forgotten stays forgotten.",
		"The master's copy was lost or replaced, and the machine's older copy took its place.",
		"None.")

var CopiesDropped = Events.Event(1404, event.Info, "Memory", "Machine {machine} dropped the copies of its memory older than a forget", "machine", "kept").
	Doc("After a forget, the machine deleted the older encrypted copies of its memory it still held, which contained what was forgotten.",
		"A forget was applied and the machine was told the oldest generation it may keep.",
		"None.")

var ForgetsDropped = Events.Event(1405, event.Info, "Memory", "Forgets with nothing left to do were dropped from the record", "dropped", "left").
	Doc("Entries in the master's record of forgets that every machine has applied, and no older copy can undo, were removed.",
		"Routine upkeep of the record.",
		"None.")

var InventoryRecorded = Events.Event(1406, event.Info, "Memory", "The inventory of machine {machine} was recorded in its memory", "machine").
	Doc("What a machine reports about itself was written into its memory, so the agent knows the machine it is working on.",
		"The machine connected, or what it reports changed.",
		"None.")

var RevokedMemoryDeleted = Events.Event(1407, event.Info, "Memory", "The memory of revoked machine {machine} was deleted", "machine").
	Doc("A machine was revoked with its record deleted, and the master's copy of its memory was removed.",
		"tenant fleet revoke --delete-scheme, /fleet revoke delete, or Delete its record on the Fleet page.",
		"None when intended. It cannot be brought back.")

var KeyNotLocked = Events.Event(2401, event.Warning, "Memory", "The fleet key is held in memory that is not locked against the swap file: {why}", "why").
	Doc("While Tenant runs, the fleet key is in memory the system could write to its swap file. Tenant runs all the same.",
		"This system or this account does not allow locking memory.",
		"See tenant doctor. On Linux, raise the locked-memory limit for the account that runs Tenant.")

var MemoryNotWritten = Events.Event(2402, event.Warning, "Memory", "The memory of machine {machine} could not be written out: {err}", "machine", "err").
	Doc("The master could not save its copy of a machine's memory. What is in memory is still used, and it is tried again.",
		"The data directory is not writable, or its disk is full.",
		"Check the data directory's permissions and free space.")

var MemoryNotClosed = Events.Event(2403, event.Warning, "Memory", "The machines' memory did not close cleanly: {err}", "err").
	Doc("As this process let go of the fleet, a machine's memory could not be closed properly. The last changes to it may not be on disk.",
		"A store could not be written as the process stopped.",
		"Check the data directory's free space. The next comparison with the machine's own copy puts it right.")

var NoNewGeneration = Events.Event(2404, event.Warning, "Memory", "No new generation of the memory of machine {machine}: {err}", "machine", "err").
	Doc("The master could not make a new encrypted copy of a machine's memory to send it. The machine keeps the copies it has.",
		"The memory could not be read, or the fleet key could not seal it.",
		"Read err, and see tenant doctor's fleet checks.")

var NeedsRecoveryKey = Events.Event(2405, event.Warning, "Memory", "The memory of machine {machine} needs the recovery key", "machine").
	Doc("The machine holds copies of its memory made under another fleet key, which this master cannot read. Nothing is overwritten until the operator brings the recovery identity.",
		"This master was rebuilt from nothing or restored from a plain backup, so its fleet key is new.",
		"tenant fleet recover <name>, with the recovery identity the first init showed.")

var CopyNotWhole = Events.Event(2406, event.Warning, "Memory", "Machine {machine}'s copy of its memory did not come whole (generation {generation}): {err}", "machine", "generation", "err").
	Doc("The master asked a machine for its copy of its memory and did not get all of it. Nothing was replaced.",
		"The link dropped during the transfer, or the machine's file is damaged.",
		"None for a dropped link: it is asked again. If it repeats, check the machine's disk.")

var CopyNotVerified = Events.Event(2407, event.Warning, "Memory", "Machine {machine}'s copy of its memory did not verify (generation {generation}): {err}", "machine", "generation", "err").
	Doc("A copy a machine sent back is not one this master made, or has been changed. It was not used.",
		"The file on the machine was damaged or tampered with, or was made under another fleet key.",
		"If the master was rebuilt, tenant fleet recover. Otherwise treat the machine's disk as suspect.")

var Diverged = Events.Event(2408, event.Warning, "Memory", "The memory of machine {machine} and the master's copy diverged; the machine's stands", "machine", "generation", "aside").
	Doc("Both the master's copy and the machine's copy of its memory moved on from the same generation. The machine's is kept, and the master's is set aside (aside says where) rather than deleted.",
		"The master was restored from an older backup, or two masters wrote to one machine.",
		"None if the machine's is right. To look at what was set aside, see the path in aside.")

var NotReembedded = Events.Event(2409, event.Warning, "Memory", "The rebuilt memory of machine {machine} was not re-embedded: {err}", "machine", "err").
	Doc("A machine's memory was rebuilt from its copy but its search vectors were not made again, so similarity recall on it is incomplete. It is tried again at the next comparison.",
		"The embedding model is unreachable.",
		"Check the embedding model (tenant doctor).")

var ForgetsUnreadable = Events.Event(2410, event.Warning, "Memory", "The record of forgets cannot be read; it was moved aside to {file}", "file", "err").
	Doc("The master's record of what was forgotten on its machines is damaged. It was set aside and a new one started, so forgets made before are no longer applied again to a memory rebuilt from an older copy.",
		"The file was cut short by a crash or a full disk, or edited by hand.",
		"Keep the file that was set aside. Forget again anything that must stay forgotten.")

var ForgetNotRecorded = Events.Event(2411, event.Warning, "Memory", "A forget could not be recorded: {err}", "err").
	Doc("A forget was not written to the master's record, or the record was not saved. It is tried again.",
		"The data directory is not writable, or its disk is full.",
		"Check the data directory's permissions and free space.")

var ForgetUnreadable = Events.Event(2414, event.Warning, "Memory", "A forget left for the machines' memory cannot be read; it was moved aside to {file}", "file", "err").
	Doc("tenant privacy forget left a request for the running Tenant to apply to its machines' memory, and the request cannot be read. It was set aside and nothing was forgotten on the machines.",
		"The file was cut short, or was sealed under another fleet key.",
		"Run the forget again: tenant privacy forget.")

var ForgetNotReapplied = Events.Event(2412, event.Warning, "Memory", "A forget could not be applied again to the rebuilt memory of machine {machine}: {err}", "machine", "err").
	Doc("A machine's memory was rebuilt from a copy made before a forget, and the forget could not be applied to it again yet. It is tried again.",
		"The machine's memory could not be written.",
		"Check the data directory's free space. Until it succeeds, what was forgotten may be in that machine's memory again.")

var NotToldToDrop = Events.Event(2413, event.Warning, "Memory", "Machine {machine} was not told to drop its older copies after a forget: {err}", "machine", "err").
	Doc("After a forget, the machine still holds older encrypted copies of its memory that contain what was forgotten. It is told at the next comparison.",
		"The machine is offline, or the link dropped.",
		"None: it is told when it connects. The copies are encrypted, and only the master can read them.")
