package gsuite

// OAuth scopes, split by posture. The read-only set is the default and
// the least-privilege floor; the read/write set is selected only when the
// operator opts in (AllowSend). gmail.modify covers labels/drafts/trash
// (everything short of permanent delete + send); gmail.send is additive
// because modify deliberately excludes sending. We never request
// https://mail.google.com/ — that grants permanent delete, which this
// connector refuses to expose (trash is reversible; delete is not).
const (
	scopeGmailRead = "https://www.googleapis.com/auth/gmail.readonly"
	scopeGmailMod  = "https://www.googleapis.com/auth/gmail.modify"
	scopeGmailSend = "https://www.googleapis.com/auth/gmail.send"

	scopeCalRead = "https://www.googleapis.com/auth/calendar.readonly"
	scopeCalFull = "https://www.googleapis.com/auth/calendar"

	scopeDriveRead = "https://www.googleapis.com/auth/drive.readonly"
	scopeDriveFull = "https://www.googleapis.com/auth/drive"
)

// scopesFor returns the least-privilege scope set for the chosen posture.
// allowSend=false → read-only across Gmail/Calendar/Drive. allowSend=true
// → read/write: gmail.modify+gmail.send, full calendar, full drive. The
// SA/DWD admin (or the OAuth consent screen) must authorize exactly these.
func scopesFor(allowSend bool) []string {
	if allowSend {
		return []string{scopeGmailMod, scopeGmailSend, scopeCalFull, scopeDriveFull}
	}
	return []string{scopeGmailRead, scopeCalRead, scopeDriveRead}
}

// ScopesFor is the exported view of the posture→scopes mapping, for callers
// that need to DISPLAY or validate the exact scope set an SA/DWD admin (or
// OAuth consent screen) must authorize — e.g. the /configure probe's
// remediation hint when Google returns unauthorized_client. Mirrors
// scopesFor exactly so there's a single source of truth.
func ScopesFor(allowSend bool) []string { return scopesFor(allowSend) }
