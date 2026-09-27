package web

import "tenant/internal/logs/event"

// Browser is the headless Chrome's own log: what the browser itself reports,
// apart from the plugin's API calls (Events).
var Browser = event.NewProvider("browser", "browser")

var NoSandbox = Browser.Event(2001, event.Warning, "Sandbox", "Chrome's process sandbox is disabled ({outer})", "outer").
	Keywords("security").
	Doc("Chrome runs without its own process sandbox, because browser_no_sandbox is set.",
		"The operator turned it off for an environment where Chrome's sandbox can't start; Tenant detected the named outer sandbox.",
		"Keep it only inside that outer sandbox; turn browser_no_sandbox off anywhere else.")

var DevToolsError = Browser.Event(3001, event.Error, "DevTools", "chromedp: {err}", "err").
	Doc("The browser automation library reported an error talking to Chrome.",
		"Chrome sent something chromedp could not handle, or the tab went away mid-command.",
		"Usually harmless once; if it repeats, check the browser plugin's recent tool results and restart the browser with /disable web then /enable web.")
