package gsuite

import (
	"tenant/internal/logs/event"
	"tenant/internal/pluginreg"
)

// Events is Google Workspace's event catalog (TEN-393), declared by its
// descriptor. Every API call goes through Events.Observe (Open).
var Events = pluginreg.NewAPIEvents("gsuite", "Google Workspace")

var TokenRefreshed = Events.Provider.Event(1101, event.Info, "Auth", "Google access token refreshed; valid until {expires}", "expires").
	Doc("A new access token was minted for the Gmail, Calendar and Drive calls.",
		"The previous one was about to expire, or this is the first call since the plugin started.",
		"None.")

var TokenNotSaved = Events.Provider.Event(2101, event.Warning, "Auth", "Rotated Google OAuth token not saved; a restart may need a new sign-in: {err}", "err").
	Doc("Google rotated the refresh token, and the new one could not be written to the credentials. The plugin keeps working until Tenant restarts.",
		"The credentials file or its directory can't be written: permissions, a full disk, or a lock another process holds.",
		"Fix what the error names; the save is retried on the next refresh. After a restart that finds the old token refused, run /configure gsuite again.")

var TokenRefreshFailed = Events.Provider.Event(3101, event.Error, "Auth", "Google access token refresh failed: {err}", "err").
	Doc("No access token could be minted, so every Gmail, Calendar and Drive call fails.",
		"The OAuth refresh token was revoked or expired, the service account's key or delegation changed, or gcloud is signed out.",
		"Sign in again with /configure gsuite (for gcloud mode, `gcloud auth login`); `tenant doctor` checks the setup.")
