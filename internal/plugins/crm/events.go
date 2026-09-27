package crm

import (
	"tenant/internal/logs/event"
	"tenant/internal/pluginreg"
)

// Events is the CRM plugin's event catalog (TEN-393), declared by its
// descriptor.
var Events = pluginreg.NewEvents("crm", "CRM")

var ToolFailed = Events.Provider.Event(3101, event.Error, "Tool", "crm-tool {subcommand} could not run: {err}", "subcommand", "err").
	Doc("The external crm-tool did not run to completion, so the CRM tool failed. A run that exits with an error is the tool's answer, and is not logged.",
		"The binary is missing or not executable, or it ran past its timeout.",
		"Check --crm-tool-path (or $CRM_TOOL_PATH), and run the tool by hand to see where it hangs.")
