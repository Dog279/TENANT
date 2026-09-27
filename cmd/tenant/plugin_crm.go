package main

import (
	"tenant/internal/model"
	"tenant/internal/plugins/crm"
)

// crmPlugin declares the CRM plugin, a wrapper over the operator's external
// crm-tool binary (TEN-269). Configured by path (--crm-tool-path or
// $CRM_TOOL_PATH).
func crmPlugin() *pluginDescriptor {
	return &pluginDescriptor{
		ID:      "crm",
		Title:   "CRM (external crm-tool)",
		Version: 1,
		Tools:   func() []model.ToolSpec { return crm.NewDispatcher(nil, crm.Policy{}).Tools() },
		// The gated subcommands raise their tool name as the action id.
		Actions: map[string]string{
			"crm_ask": catDestructive, "crm_align": catDestructive, "crm_commitments_list": catDestructive,
		},
		StubHint: "relaunch with --crm-tool-path <path> (or set $CRM_TOOL_PATH) to wrap the external crm-tool binary",
	}
}
