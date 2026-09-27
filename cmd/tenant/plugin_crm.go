package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"tenant/internal/model"
	"tenant/internal/plugins/crm"
)

// crmPlugin declares the CRM plugin, a wrapper over the operator's external
// crm-tool binary (TEN-269). Configured by path (--crm-tool-path or
// $CRM_TOOL_PATH).
func crmPlugin() *pluginDescriptor {
	return &pluginDescriptor{
		ID:      "crm",
		Events:  crm.Events,
		Flags:   bindCRMFlags,
		On:      func(pf *pluginFlags) bool { return crmToolPath(pf) != "" },
		Launch:  launchCRM,
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

// crmFlags are the CRM plugin's launch flags.
type crmFlags struct {
	path        string
	allowMutate bool
}

func crmFlagsOf(pf *pluginFlags) *crmFlags { return flagsOf[crmFlags](pf, "crm") }

func bindCRMFlags(fs *flag.FlagSet) any {
	f := &crmFlags{}
	fs.StringVar(&f.path, "crm-tool-path", "", "path to the external crm-tool binary (or $CRM_TOOL_PATH) — enables the gated crm_* tools")
	fs.BoolVar(&f.allowMutate, "crm-allow-mutate", false, "permit the gated crm subcommands (ask/align/commitments-list) without per-action confirm")
	return f
}

// crmToolPath is the crm-tool binary: the flag, else $CRM_TOOL_PATH.
func crmToolPath(pf *pluginFlags) string {
	return strings.TrimSpace(firstNonEmpty(crmFlagsOf(pf).path, os.Getenv("CRM_TOOL_PATH")))
}

// launchCRM wraps the external crm-tool when its path is configured; without
// one the stub advertises the tools and how to enable them. Read
// subcommands are ungated; the heavier set (ask/align/commitments-list) is
// gated through the confirm broker.
func launchCRM(_ context.Context, e *pluginEnv) ([]plugin, func(), error) {
	path := crmToolPath(e.pf)
	if path == "" {
		return nil, nil, nil
	}
	svc, err := crm.Open(crm.Config{Path: path})
	if err != nil {
		return nil, nil, fmt.Errorf("crm plugin: %w", err)
	}
	return []plugin{crm.NewDispatcher(svc, crm.Policy{AllowMutate: e.gateAllow(crmFlagsOf(e.pf).allowMutate), Confirm: e.confirm})}, nil, nil
}
