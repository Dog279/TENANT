package peering

import "tenant/internal/logs/event"

// Peering is federation's event provider, writing the peering log. IDs
// follow the blocks in internal/logs/event's package doc. The audit records
// of pairing and authentication belong to the Security log.
var Peering = event.NewProvider("peering", "peering")

// Lifecycle.

// Listening is the activity of the listener's lifetime: start when it
// serves, stop when it shuts down, fail when it stops on its own.
var Listening = Peering.Event(1, event.Info, "Listener", "Peer listener on {addr}", "addr", "tls").
	Doc("The federation listener that paired peers dial: start, then stop with its uptime, or fail with the error that stopped it.",
		"peer.listen is set, or /peer serve started it; it stops at shutdown or when /peer serve moves it.",
		"On a failure, read the error: usually the address was taken or the network went away. /peer serve restarts it.")

var ListenerNotStarted = Peering.Event(3001, event.Error, "Listener", "Peer listener on {addr} did not start: {err}", "addr", "err").
	Doc("The federation listener could not start, so no peer can reach this instance.",
		"The address is in use, a non-loopback address has neither TLS nor peer.transport: overlay, or the peer cert or peers.json could not be read.",
		"Fix the cause the error names, then /peer serve <addr> or restart.")

// Pairing (the operational side).

var Paired = Peering.Event(1101, event.Info, "Pairing", "Paired with {name} at {remote}, filed as {peer}", "name", "instance", "remote", "peer").
	Doc("The operator approved a pairing request, and the inviter is now a peer that may dial this instance (sharing nothing yet).",
		"Another instance ran tenant peer invite --to (or /peer invite) at this one.",
		"Choose what it may read: /configure peer, or tenant peer share.")

var PairDenied = Peering.Event(1102, event.Info, "Pairing", "Pairing request from {name} at {remote} was denied", "name", "instance", "remote").
	Doc("A pairing request was refused, so nothing was stored.",
		"The operator denied it, or nobody answered within 90 seconds.",
		"None when intended. If it was expected, ask the inviter to try again and approve it when the PINs match.")

// PairInvite is the activity of this instance asking another to pair.
var PairInvite = Peering.Event(1103, event.Info, "Pairing", "Pairing request to {url}", "url").
	Doc("This instance asked a peer to pair: start, then stop when the peer approved, or fail with why not.",
		"The operator ran tenant peer invite --to or /peer invite.",
		"On a failure, read the error: the peer was unreachable, its operator denied it, or its certificate did not match.")

var PairThrottled = Peering.Event(2101, event.Warning, "Pairing", "Pairing request from {remote} refused: too many are pending", "remote").
	Doc("A pairing request was turned away before reaching the operator: each host may have one pending request, and three may be pending in all.",
		"The same host asked again before its first request was answered, or many hosts are asking at once.",
		"Answer the pending request. If the requests are unexpected, block the host: the pairing endpoint is open to anyone who can reach the listener.")

var PairFailed = Peering.Event(3101, event.Error, "Pairing", "Pairing with {name} failed after approval: {err}", "name", "remote", "err").
	Doc("The operator approved a pairing, but it could not be completed.",
		"A token could not be minted, or peers.json could not be written.",
		"Check the config directory's disk space and permissions, then ask the inviter to try again.")

// Peers.

var PeerUp = Peering.Event(1201, event.Info, "Heartbeat", "Peer {peer} is reachable ({latency})", "peer", "url", "latency").
	Doc("A peer this instance dials answered its heartbeat, for the first time or after being unreachable.",
		"The peer started, or its network came back.",
		"None.")

// Connection and RPC are verbose activities: a heartbeat opens a session and
// calls peer_hello every 30 seconds, so only failures show by default.

var Connection = Peering.Event(1301, event.Verbose, "Connection", "Session from peer {peer}", "peer").
	Doc("A paired peer opened a session with the listener: start, then stop when it closes.",
		"The peer's heartbeat, a federated search or tenant peer query.",
		"None.")

var RPC = Peering.Event(1302, event.Verbose, "RPC", "Peer {peer} called {tool}", "peer", "tool").
	Doc("A paired peer called a tool on this instance: start, then stop with its duration, or fail with its error.",
		"The peer's heartbeat (peer_hello), or its agent searching this instance's shared wiki or memory.",
		"On a denial, the peer asked for something this instance doesn't share: share it (/configure peer) or ask its operator. Otherwise, read the error.")

// Warnings.

var TokenRejected = Peering.Event(2001, event.Warning, "Authentication", "A request from {remote} was refused: its token matches no paired peer", "remote").
	Doc("A request reached the listener with a bearer token that is not a current peer's.",
		"A removed or revoked peer is still dialing, a peer kept its old token after a rotation, or someone is probing the port.",
		"If the address is a peer you trust, re-pair it or hand it its new token; otherwise block the address.")

var ReloadFailed = Peering.Event(2002, event.Warning, "Authentication", "peers.json could not be reloaded; peers are checked against the copy in memory: {err}", "err").
	Doc("The listener could not re-read peers.json, so a revoke, rotation or new pairing made elsewhere hasn't landed.",
		"The file is unreadable or malformed, or the disk failed.",
		"Check peers.json in the config directory. Requests authenticate against the last good copy until it reads again.")

var StoreNotUpdated = Peering.Event(2003, event.Warning, "Authentication", "Peer {peer} authenticated, but peers.json was not updated ({change}): {err}", "peer", "change", "err").
	Doc("A peer's first use of its invite, or of its rotated token, could not be saved.",
		"peers.json could not be written.",
		"None at once: the request still authenticates, and the next one retries. If it repeats, check the config directory's permissions.")

var SearchFailed = Peering.Event(2301, event.Warning, "RPC", "Peer {peer}'s {tool} could not search {store}: {err}", "peer", "tool", "store", "err").
	Doc("A peer's search of this instance's shared knowledge failed here, so it got partial results or none.",
		"The wiki index or a memory store failed: a locked or damaged database, or a disk error.",
		"Check the memory log around this time. The peer's next search tries again.")

var PeerDown = Peering.Event(2201, event.Warning, "Heartbeat", "Peer {peer} is unreachable: {err}", "peer", "url", "err").
	Doc("A peer this instance dials failed its heartbeat, so its shared knowledge is out of reach.",
		"The peer is off or asleep, its network or address changed, or it revoked this instance's token.",
		"Check the peer is running and reachable at its URL. It is reported again when it answers.")

var HeartbeatStopped = Peering.Event(2202, event.Warning, "Heartbeat", "Heartbeats stopped: peers.json can't be read: {err}", "err").
	Doc("The heartbeat could not read the paired peers, so no peer's liveness is checked.",
		"peers.json is unreadable or malformed.",
		"Check peers.json in the config directory; heartbeats resume on the next round after it reads.")
