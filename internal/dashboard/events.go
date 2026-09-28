package dashboard

import "tenant/internal/logs/event"

// Events is the dashboard's event provider, writing the dashboard log. IDs
// follow the blocks in internal/logs/event's package doc. Rejections here are
// the operational side; the Security log keeps the audit records.
var Events = event.NewProvider("dashboard", "dashboard")

// Lifecycle.

var Serving = Events.Event(1, event.Info, "Server", "Dashboard on {addr}", "addr", "tls").
	Doc("The dashboard's web server: start, then stop with its uptime, or fail with the error that stopped it.",
		"dashboard.enabled is on, or /dashboard on started it; it stops at shutdown or on /dashboard off.",
		"On a failure, read the error: usually the address is taken or the TLS certificate can't be read. /dashboard on starts it again.")

var ServerError = Events.Event(2001, event.Warning, "Server", "HTTP server: {err}", "err", "stack").
	Doc("Go's HTTP server reported a problem it handled on its own, such as a failed TLS handshake or a handler that panicked.",
		"A client that doesn't speak TLS (or doesn't trust the certificate), a scanner, a dropped connection, or a bug in a page's handler.",
		"A handshake error from an unknown address is usually noise. A panic is a bug: report it with its stack.")

// Live streams: an SSE feed or the WebSocket, one per open page.

var Stream = Events.Event(1001, event.Info, "Stream", "Live {stream} stream to {remote}", "stream", "remote").
	Doc("A browser page subscribed to a live feed (chat and activity events, or the log book): start when it connects, stop when it leaves.",
		"A dashboard page with a live feed was opened, reloaded or closed.",
		"None.")

var StreamLagging = Events.Event(2002, event.Warning, "Stream", "Live {stream} stream to {remote} fell behind", "stream", "remote", "missed").
	Doc("A page read its live feed more slowly than events arrived, and its buffer filled, so events were dropped for it (missed counts them where known): the page no longer shows all of them.",
		"A slow or suspended browser tab, a slow network, or a burst of events (a long tool run streams many).",
		"Reload the page to redraw it. The events themselves are still in the log book.")

var StreamRefused = Events.Event(2003, event.Warning, "Stream", "WebSocket handshake from {remote} failed: {err}", "remote", "err").
	Doc("A client asked for the chat WebSocket, but its handshake was malformed; it got an HTTP error instead.",
		"A client or proxy that doesn't speak WebSocket, or a proxy that strips the Upgrade headers.",
		"Use the dashboard in a browser directly, or make the proxy pass WebSocket upgrades through.")

// Rejected requests. The response tells the client; these tell the operator.

var HostRejected = Events.Event(2101, event.Warning, "Access", "Request for unrecognized host {host} rejected: {method} {route}", "host", "method", "route", "remote").
	Doc("A request named a Host the dashboard doesn't answer to, and got 403.",
		"The dashboard was reached under a name or address it doesn't know (a reverse proxy, a LAN name, a new tailnet name), or a DNS-rebinding attempt.",
		"If the name is yours, add it to dashboard.allowed_hosts. Otherwise nothing: the request was refused.")

var OriginRejected = Events.Event(2102, event.Warning, "Access", "Cross-origin request from {origin} rejected: {method} {route}", "origin", "method", "route", "remote").
	Doc("A browser request came from a page on another site, and got 403.",
		"A web page on another origin tried to call the dashboard, or the dashboard is open under two different addresses.",
		"Open the dashboard at one address. If you don't recognize the origin, a site you visited tried to reach it; it was refused.")

var AuthRejected = Events.Event(2103, event.Warning, "Access", "Request without a valid token rejected: {method} {route}", "method", "route", "remote", "credential").
	Doc("A request carried no dashboard token, or a wrong one (credential says which), and got 401.",
		"A browser that hasn't signed in, a token that changed since sign-in, a script with an old token, or someone guessing.",
		"Sign in at /settings with the token in <config>/dashboard-auth-token. Many wrong tokens from an address you don't know mean someone is guessing: keep the dashboard off public networks.")

var CSRFRejected = Events.Event(2104, event.Warning, "Access", "Browser change without a matching Origin rejected: {method} {route}", "method", "route", "remote").
	Doc("A change (a POST) authenticated by the sign-in cookie didn't prove it came from a dashboard page, and got 403.",
		"A page on another site submitted a form to the dashboard, or an old browser or extension stripped the Origin header.",
		"Make the change from the dashboard page itself, or call the API with an Authorization: Bearer token.")

var SignInFailed = Events.Event(2105, event.Warning, "Access", "Sign-in with a wrong token from {remote}", "remote").
	Doc("The sign-in form was sent a token that doesn't match the dashboard's, and got 403.",
		"A typo, a token from before it changed, or someone guessing.",
		"Use the token in <config>/dashboard-auth-token (or dashboard.auth). Many failures from an address you don't know mean someone is guessing.")

// Operator actions on the pages.

var ActionFailed = Events.Event(2201, event.Warning, "Action", "Dashboard action {action} failed: {err}", "action", "target", "err").
	Doc("A change or command made on a dashboard page didn't apply. The page showed the operator a plain message; this event has the error.",
		"The value was refused (an unknown category or mode, a bad schedule, a job or fact that no longer exists), or the change couldn't be saved: config.json or a store is unwritable or locked, or the subsystem isn't running.",
		"Read the error. Fix the value and try again; if saving failed, check the data directory's permissions and free space.")

var KeyNotSaved = Events.Event(2202, event.Warning, "Action", "API key {cred} was not {op}", "cred", "op", "err").
	Sensitive("err").
	Doc("Storing or removing an API key from the Keys page failed. The page said so without the reason, which can quote the key.",
		"The credential store is unwritable or locked, or the key's ID is unknown.",
		"Check the config directory's permissions, then try again. The reason is kept, masked, in this event.")

var KeyInURL = Events.Event(2203, event.Warning, "Action", "API key {cred} was sent in the URL and refused", "cred").
	Doc("A request sent an API key, an integration's setting or the dashboard's sign-in key in the URL's query string, where browser history and proxy logs keep it. It was refused before anything used it.",
		"A script or bookmark that puts the key in the URL instead of the form's body.",
		"Rotate that key: it may now sit in a history or a log. For the sign-in key, delete the file dashboard-auth-token and restart Tenant. Submit keys through the page's form.")

var ProbeFailed = Events.Event(2204, event.Warning, "Action", "Integration {integration} test failed: {err}", "integration", "err").
	Doc("The Test button's check of an integration ran in the background and failed; the page shows the result on refresh.",
		"The integration's credentials are missing or expired, its sign-in on the computer running Tenant wasn't finished, or its service is unreachable.",
		"Finish any sign-in on the computer running Tenant, fix the integration's settings, and test again.")

var ConnectorNotAdded = Events.Event(2205, event.Warning, "Action", "A remote service was not connected: {err}", "url", "err").
	Sensitive("url").
	Doc("Connecting a remote MCP service from the MCP page ran in the background and failed.",
		"The URL isn't an MCP endpoint, the service is unreachable, or its sign-in on the computer running Tenant wasn't approved.",
		"Check the URL, approve the sign-in window on the computer running Tenant, and connect again. The mcp log has the details.")

var ChatTurnFailed = Events.Event(2206, event.Warning, "Chat", "Chat turn failed: {err}", "err").
	Doc("A message sent from the Chat page didn't get an answer; the chat shows the error.",
		"The turn gate refused it, the model is unreachable, or the agent failed before its loop started.",
		"Read the error. For the model, see the model log; then send the message again.")

// Errors.

var RenderFailed = Events.Event(3001, event.Error, "Page", "Page {page} could not be rendered: {err}", "page", "err").
	Doc("A dashboard page's template failed, and the browser got a 500.",
		"A bug: the page's data doesn't fit its template.",
		"Report it with the error. Other pages keep working.")

// Diagnostics.

// Request is the activity of one HTTP request: its method and route pattern
// at the start, its status at the end. Never the query, the body or a token.
var Request = Events.Event(9001, event.Info, "Request", "{method} {route}", "method", "route", "status").
	Diagnostic().
	Doc("One request to the dashboard: start, then stop with its status and duration, or fail on a 5xx.",
		"A browser page, the dashboard API or a live stream.",
		"None. For slow or failing pages, compare durations and statuses by route.")
