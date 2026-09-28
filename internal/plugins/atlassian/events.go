package atlassian

import (
	"net/http"

	"tenant/internal/logs/event"
	"tenant/internal/pluginreg"
)

// Events is Jira's event catalog (TEN-393), declared by its descriptor.
var Events = pluginreg.NewAPIEvents("atlassian", "Jira")

// apiClient carries every Jira call, so its failures reach the plugin's log.
var apiClient = &http.Client{Transport: Events.Observe(nil)}

var TokenRefreshed = Events.Provider.Event(1101, event.Info, "Auth", "Jira OAuth token refreshed").
	Doc("The OAuth access token expired and was renewed; the rotated refresh token was saved.",
		"Atlassian access tokens last an hour.",
		"None.")

var TokenNotSaved = Events.Provider.Event(2101, event.Warning, "Auth", "Jira's rotated refresh token not saved: {err}", "err").
	Doc("Atlassian rotated the refresh token, and the new one could not be written, so the call that refreshed it failed and the saved token may no longer work.",
		"The token cache or its directory can't be written: permissions or a full disk.",
		"Fix what the error names. If Jira calls then fail to refresh, sign in again with `tenant atlassian login`.")

var TokenRefreshFailed = Events.Provider.Event(3101, event.Error, "Auth", "Jira OAuth token refresh failed: {err}", "err").
	Doc("The access token could not be renewed, so Jira calls fail.",
		"The refresh token was revoked, expired after 90 days unused, or the OAuth app's secret changed.",
		"Sign in again with `tenant atlassian login`, then /enable atlassian.")

var SignInURL = Events.Provider.Event(2102, event.Warning, "Auth", "Open the sign-in URL to authorize Tenant's Jira access", "url").
	Doc("Signing in to Jira needs the operator's browser, and it could not be opened for them.",
		"No browser opener in this process (a service, a remote shell), or opening it failed.",
		"Open the logged url in a browser on this machine and approve access; the sign-in completes by itself.")
