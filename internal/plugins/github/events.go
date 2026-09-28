package github

import "tenant/internal/pluginreg"

// Events is GitHub's event catalog (TEN-393), declared by its descriptor.
// Every API call goes through Events.Observe (Open).
var Events = pluginreg.NewAPIEvents("github", "GitHub")
