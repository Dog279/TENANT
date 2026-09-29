package approval

import "sync"

// explicitOnly holds the actions only a live decision or an exact tool rule may
// approve (RequireExplicit). Process-wide, like the tool rules: which actions
// are too consequential for a blanket grant is about the action, not the
// surface.
var explicitOnly sync.Map

// RequireExplicit marks action as explicit-only: a category allow and a
// session grant no longer approve it, so turning on "destructive: allow" for
// one reason can't approve it silently. The plugin that raises the action
// declares it at init (e.g. github_merge, TEN-432).
func RequireExplicit(action string) { explicitOnly.Store(action, true) }

func requiresExplicit(action string) bool {
	_, ok := explicitOnly.Load(action)
	return ok
}
