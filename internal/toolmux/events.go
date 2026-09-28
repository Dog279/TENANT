package toolmux

import "tenant/internal/logs/event"

// The tool mux's own events (TEN-393): bringing paired peers live, and
// saving the usage counts. What a plugin logs is its own provider's,
// plugin.<id>; the host writes a plugin's enable and disable from
// SetOnToggle. An MCP connector's or local server's events are in its own
// log, mcp.<name> (TEN-387), so the four Connectors events are retired.
var Events = event.NewProvider("toolmux", "toolmux")

var MCPReauthorized = Events.Event(1001, event.Info, "Connectors", "MCP connector {server} re-authorized", "server").Retired().
	Doc("A connector that had lost its sign-in has a working one again, and its tools run.",
		"The operator reconnected it, or its token refreshed after all.",
		"None.")

var MCPServerNotStarted = Events.Event(2001, event.Warning, "Connectors", "Local MCP server {connector} did not start: {err}", "connector", "err").Retired().
	Doc("A local MCP server registered for this machine failed to start at launch, so its tools stay a stub.",
		"Its command is missing or fails, or the sandbox refused its profile.",
		"Fix what the error names, then /enable the connector to start it again.")

var MCPReconnectFailed = Events.Event(2002, event.Warning, "Connectors", "Remote MCP connector {server} did not reconnect: {err}", "server", "err").Retired().
	Doc("A remote connector could not come back at launch from its cached sign-in, so its tools stay a stub.",
		"The server is down or unreachable, or refused the cached token.",
		"/enable the connector to sign in again, or check the server's status.")

var MCPNeedsReauth = Events.Event(2003, event.Warning, "Connectors", "MCP connector {server} needs re-authorization: {err}", "server", "url", "err", "fix").Retired().
	Doc("A connector's sign-in expired and could not be refreshed, so its tools fail until it is reconnected.",
		"The server revoked the refresh token, or it expired unused.",
		"Run the command in fix (`tenant mcp connect <url>`) at the host, or use the dashboard's Remote services page.")

var PeersUnreadable = Events.Event(2004, event.Warning, "Peers", "peers.json unreadable; no peer is reconnected: {err}", "err").
	Doc("The paired peers could not be read at launch, so federated search runs on this machine alone.",
		"peers.json is corrupt, or can't be read.",
		"Fix or restore peers.json in the config directory (`tenant peer list` reads it), then restart.")

var PeerReconnectFailed = Events.Event(2005, event.Warning, "Peers", "Peer {peer} did not reconnect: {err}", "peer", "err").
	Doc("A paired peer could not be reached at launch, so searches leave it out.",
		"The peer is offline, its address changed, or it revoked this machine's token.",
		"Check the peer is up (`tenant peer show <name>`); pair again if its token was revoked.")

var StatsNotSaved = Events.Event(2006, event.Warning, "Stats", "Tool stats not saved to {path}; retrying every flush: {err}", "path", "err").
	Doc("The per-tool usage counts could not be written. They are kept in memory and retried, and logged again only after a success.",
		"The data directory can't be written: permissions or a full disk.",
		"Fix what the error names; nothing else is affected.")
