package mcpremote

import (
	"context"
	"strings"

	"tenant/internal/logs/event"
)

// Events is the MCP provider (TEN-387). Each connector and local server
// writes its own log, mcp.<name> (ServerLog), which the viewer groups under
// Integrations › MCP; what belongs to no one server stays in mcp. Peers dial
// through this package too, but their events are peering's.
var Events = event.NewProvider("mcp", "mcp")

// ServerLog is the log of connector or server label: "mcp:github" writes
// mcp.github, "mcp:mcp.atlassian.com" mcp.mcp.atlassian.com. The name is
// lowercased, anything a folder name can't hold becomes '-', and it is cut to
// fit; a label with nothing usable left writes mcp.
func ServerLog(label string) string {
	name := strings.ToLower(strings.TrimPrefix(label, "mcp:"))
	b := []byte(name)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			b[i] = '-'
		}
	}
	const room = 40 - len("mcp.")
	name = strings.Trim(string(b), "-._")
	if len(name) > room {
		name = strings.Trim(name[:room], "-._")
	}
	if name == "" {
		return "mcp"
	}
	return "mcp." + name
}

// WithServerLog files the MCP events written under ctx in label's log.
func WithServerLog(ctx context.Context, label string) context.Context {
	return event.WithLog(ctx, Events, ServerLog(label))
}

// Lifecycle.

// Connection is the activity of one MCP session, remote or local.
var Connection = Events.Event(1, event.Info, "Connection", "Connection to {server}", "server", "url", "interactive").Sensitive("url").
	Doc("A session with an MCP server: start when Tenant connects, stop when it closes the session (a shutdown, a dropped duplicate), with its duration; fail when it could not connect. A remote connector reconnecting at launch from its saved sign-in has interactive=false.",
		"A connector was enabled or added, reconnected at launch, or a local server started.",
		"On a fail, read the error: the server is down or unreachable, refused the sign-in, or did not answer the MCP handshake. Its tools stay a stub until `/enable <label>` connects again.")

// Server is the activity of a local server's process (TEN-318). Its own
// lines, filed by internal/logs/capture, come in between: process started
// with the pid, its stderr, line by line, and process exited with the code.
var Server = Events.Event(2, event.Info, "Server", "Local server {server} under the {backend} sandbox", "server", "backend", "limits", "command").
	Doc("A local MCP server's process: start when Tenant starts it, with its sandbox backend and limits; stop when it exits after Tenant closed it, or at shutdown; fail when it could not start, or exited on its own.",
		"The server is configured in mcp_servers, and Tenant started or /enable started it.",
		"On a fail, read the error and the server's stderr lines before it: a missing command, a sandbox that refused what it needs, or a crash. Fix it in config.json (mcp_servers); /enable retries a server that never started, and a restart of Tenant one that exited.")

// Connections.

var Connected = Events.Event(1001, event.Info, "Connection", "Connected to {server}: {tools} tools", "server", "tools", "names").
	Doc("A session is up and the server's tools are listed (names). They come live gated by the connector's trust.",
		"A connection succeeded.",
		"None. A tool missing from names is missing on the server's side.")

var ToolsChanged = Events.Event(1002, event.Info, "Connection", "{server} changed its tool list; Tenant keeps the list it read at connect", "server").
	Doc("The server announced that its tools changed. Tenant lists a server's tools once, when it connects, so the agent still sees the old list.",
		"The server was updated, or it enables tools at run time.",
		"Restart Tenant, or remove and add the connector again, to pick up the new tools.")

var NoSavedSignIn = Events.Event(1003, event.Info, "Connection", "{server} has no saved sign-in; it stays off until /enable signs in", "server", "url").Sensitive("url").
	Doc("A connector could not reconnect at launch because it needs a browser sign-in, which launch never opens.",
		"It was never signed in from this config directory, or its saved sign-in was removed.",
		"`/enable <label>`, or the dashboard's Remote services page, signs in.")

// Sign-in.

var SignedIn = Events.Event(1101, event.Info, "Sign-in", "Signed in to {server}", "server").
	Doc("A browser sign-in to a remote connector finished, and its token is saved, so it reconnects after a restart without a browser.",
		"The server asked for a sign-in when it was added or enabled.",
		"None.")

var TokenRefreshed = Events.Event(1102, event.Info, "Sign-in", "Token for {server} refreshed", "server", "expires").
	Doc("A remote connector's access token was renewed from its refresh token, ahead of its expiry, and saved.",
		"The token was within 5 minutes of expiring when a request needed it.",
		"None.")

var Reauthorized = Events.Event(1103, event.Info, "Sign-in", "{server} is authorized again", "server").
	Doc("A connector that had lost its sign-in has a working token again, and its tools run.",
		"The operator reconnected it (`tenant mcp connect`, the dashboard), or its token refreshed after all.",
		"None.")

var SignInURL = Events.Event(2001, event.Warning, "Sign-in", "Open this URL in a browser to sign in to {server}: {url}", "server", "url").
	Doc("A remote connector needs a browser sign-in, and no browser could be opened for it.",
		"Tenant runs headless (a server, SSH), or opening the browser failed.",
		"Open the URL in a browser on this machine: the sign-in returns to a callback on localhost. Elsewhere, run `tenant mcp connect <url> --config <config dir>` at the host, with the config dir of the Tenant that uses the connector.")

var RefreshFailed = Events.Event(2101, event.Warning, "Sign-in", "Token refresh for {server} failed; the current token is still valid: {err}", "server", "err").
	Doc("An early token refresh failed, so the connector keeps using its current token, and tries again on its next request.",
		"The authorization server was unreachable or refused the refresh.",
		"None if a later refresh succeeds (mcp 1102). If the token expires first, the connector needs re-authorization (mcp 3101).")

var StateNotSaved = Events.Event(2102, event.Warning, "Sign-in", "Could not save {what} for {server}: {err}", "server", "what", "err").
	Doc("Part of a connector's saved sign-in could not be written: its client registration, or whether it needs re-authorization. It applies now, but not after a restart.",
		"The config directory's mcp folder can't be written: permissions, a full disk, or the secret store is locked.",
		"Fix what the error names. A registration that was not saved is only redone at the next sign-in.")

var NotifyFailed = Events.Event(2103, event.Warning, "Sign-in", "The operator was not told about {server}'s sign-in: {err}", "server", "err").
	Doc("A connector's sign-in died or came back, and the direct message to the operator could not be sent. The status feed, the status pages and tenant doctor still show it.",
		"The Discord relay is down, or has no operator to message.",
		"Check the relay in the discord log.")

var RefreshRetry = Events.Event(2104, event.Warning, "Sign-in", "Token refresh for {server} failed and is tried again on the next call: {err}", "server", "err").
	Doc("A connector's access token expired and renewing it failed for a reason that can pass, so the call that needed it failed. The sign-in is kept and nobody is asked to re-authorize.",
		"The authorization server was unreachable, answered with a server error, or took longer than 30 seconds; or another Tenant process held the saved sign-in's lock for longer than a refresh can take.",
		"None if a later refresh succeeds (mcp 1102). If it keeps failing, check the network path to the authorization server.")

var CacheNotLocked = Events.Event(2105, event.Warning, "Sign-in", "Could not lock the saved sign-in for {server}; refreshing without the lock: {err}", "server", "err").
	Doc("Every Tenant process that uses a connector (serve, the TUI, tenant mcp connect) locks its saved sign-in before renewing the token, so no two spend the same single-use refresh token. This one could not, so it renews unlocked. Written once per connection.",
		"Another account created the lock file beside the sign-in and this one can't open it (serve running as SYSTEM beside an administrator's session).",
		"Run every Tenant that uses the connector as the same account and with the same --config. Two renewals at the same moment without the lock can lose the sign-in (mcp 3101).")

var NeedsReauth = Events.Event(3101, event.Error, "Sign-in", "{server} needs re-authorization: its token could not be refreshed: {err}", "server", "url", "err", "fix").Sensitive("url").
	Doc("A remote connector's sign-in died: its token expired and the server refused the refresh (invalid_grant, invalid_client, or another 4xx answer), so every call to its tools fails until it is reconnected. Written once, when it happens; the status pages, tenant doctor, the daily brief and a DM to the operator show it too.",
		"The server revoked the refresh token, it expired unused, or a rotated one was lost.",
		"Run the command in fix (`tenant mcp connect <url> --config <config dir>`) at the host, or use the dashboard's Remote services page.")

var TokenNotSaved = Events.Event(3102, event.Error, "Sign-in", "The refreshed token for {server} was not saved: {err}", "server", "err").
	Doc("A connector's token was refreshed, but the new one was not written. The server may have retired the old refresh token, so after a restart the connector can need a new sign-in.",
		"The config directory's mcp folder can't be written: permissions, a full disk, or the secret store is locked.",
		"Fix what the error names; the next refresh saves again. If the connector needs re-authorization after a restart, sign in again.")

// Tool calls.

var CallFailed = Events.Event(3001, event.Error, "Tool call", "Tool {tool} on {server} failed: {err}", "server", "tool", "err").
	Doc("A call to one of the server's tools did not complete: the model got the error. A tool that ran and reported an error is not this event; it is the tool's result.",
		"The server is down or unreachable, it dropped the session, or a local server exited.",
		"Check the server (its log here, or its status page). A local server that exited starts again when Tenant restarts.")

// Local servers.

var ServerKilled = Events.Event(2201, event.Warning, "Server", "Local server {server} did not exit when asked, and was killed after {grace}", "server", "pid", "grace").
	Doc("Tenant closed a local server's stdin to stop it, and it was still running after the grace period, so it was killed.",
		"The server ignores its stdin closing, or was busy with a call.",
		"Nothing is lost for Tenant. If it happens every time, report it to the server's authors: MCP servers should exit when their input closes.")

var ServerSkipped = Events.Event(2202, event.Warning, "Server", "Local server {server} skipped: {err}", "server", "err").
	Doc("A local server in mcp_servers was not registered, so it has no tools.",
		"Its entry is invalid (a bad name, no command, an unknown sandbox or network, a relative path), or its name is taken by another connector or plugin.",
		"Fix the entry in config.json (mcp_servers) as the error says, then restart Tenant.")

// Configuration: the operational side of connector changes. Their audit
// records are the Security log's.

var ConnectorAdded = Events.Event(5001, event.Info, "Connector", "Connector {server} added: {url}", "server", "url", "tools", "trust").Keywords("config").
	Doc("A remote connector was added and saved, so it reconnects after a restart.",
		"/mcp add, the dashboard's Remote services page, or the agent's mcp_add, which the operator approved.",
		"None.")

var ConnectorRemoved = Events.Event(5002, event.Info, "Connector", "Connector {server} removed", "server", "url").Keywords("config").
	Doc("A remote connector was removed: its tools are gone and it no longer reconnects.",
		"/mcp remove, the dashboard, or the agent's mcp_remove, which the operator approved.",
		"None.")

var TrustChanged = Events.Event(5003, event.Info, "Connector", "Connector {server} trust set to {trust}", "server", "url", "trust").Keywords("config").
	Doc("A connector's trust changed, live: ask prompts for each gated call, deny refuses its calls and hides its tools.",
		"/mcp trust, the dashboard, or the agent's mcp_trust, which the operator approved.",
		"None.")

var TrustNotSet = Events.Event(2301, event.Warning, "Connector", "Connector {server} stays at trust ask; setting {trust} failed: {err}", "server", "trust", "err").
	Doc("The agent added a connector and the operator approved trusting it, but the trust change failed, so its tools still ask for each gated call.",
		"The trust could not be saved to config.json, or the connector went away in between.",
		"Set it again with /mcp trust <label> allow, or on the dashboard's Remote services page, once the error is fixed.")

var TrustAllowed = Events.Event(5004, event.Warning, "Connector", "Connector {server} trusted: its tools run without per-call approval", "server", "url").Keywords("config").
	Doc("A connector's trust was set to allow: from now on its tools run without asking anyone.",
		"/mcp trust <label> allow, the dashboard, or the agent's mcp_add or mcp_trust, which the operator approved.",
		"Make sure the server deserves it; /mcp trust <label> ask takes it back.")
