package simplefin

import "tenant/internal/pluginreg"

// Events is SimpleFIN's event catalog (TEN-393), declared by its descriptor.
// Every bridge call goes through Events.Observe (Open).
var Events = pluginreg.NewAPIEvents("simplefin", "SimpleFIN")
