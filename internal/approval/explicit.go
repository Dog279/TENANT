package approval

import (
	"context"
	"sync"
)

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

// scopedAlways holds, per action, what "approve always" on its prompt saves
// instead of a blanket grant (OnAlways).
var scopedAlways sync.Map // action → func(context.Context)

// OnAlways makes "approve always" on action's prompt approve this request and
// call save, never turning its category (or a matching tool rule) to allow. For
// an action whose approval can be narrowed to what it asked for: a sandbox
// escalation saves the refused folder as an exception (TEN-347). save gets the
// asking call's ctx, which carries what to save; it records the outcome itself.
func OnAlways(action string, save func(ctx context.Context)) { scopedAlways.Store(action, save) }

// saveScoped runs action's OnAlways hook and reports whether it has one.
func saveScoped(ctx context.Context, action string) bool {
	f, ok := scopedAlways.Load(action)
	if ok {
		f.(func(context.Context))(ctx)
	}
	return ok
}
