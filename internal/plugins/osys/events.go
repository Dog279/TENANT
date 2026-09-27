package osys

import "tenant/internal/pluginreg"

// Events is the OS plugin's event catalog (TEN-393), declared by its
// descriptor: the host writes its lifecycle.
var Events = pluginreg.NewEvents("os", "OS")
