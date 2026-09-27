package x

import (
	"tenant/internal/logs/event"
	"tenant/internal/pluginreg"
)

// Events is X's event catalog (TEN-393), declared by its descriptor. Every
// API call goes through Events.Observe (Open).
var Events = pluginreg.NewAPIEvents("x", "X")

var TokenRefreshed = Events.Provider.Event(1101, event.Info, "Auth", "X user token refreshed").
	Doc("The user-context access token expired and was renewed with the refresh token, which X rotates; both were saved.",
		"X access tokens last about two hours.",
		"None.")

var TokenRefreshFailed = Events.Provider.Event(3101, event.Error, "Auth", "X user token refresh failed: {err}", "err").
	Doc("The user-context token could not be renewed or saved, so posting and user reads fail.",
		"The refresh token was revoked or already used, the app's client id changed, X is unreachable, or the token file can't be written.",
		"Sign in again with `tenant x --login`.")
