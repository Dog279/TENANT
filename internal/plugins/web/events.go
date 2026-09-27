package web

import "tenant/internal/pluginreg"

// Events is the web plugin's event catalog (TEN-393), declared by its
// descriptor. The search and reader APIs' calls go through Events.Observe;
// the browser's own output is the browser log's.
var Events = pluginreg.NewAPIEvents("web", "Web")
