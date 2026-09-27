package main

import (
	"context"
	"fmt"
	"path/filepath"

	"tenant/internal/model"
	"tenant/internal/plugins/simplefin"
)

// simplefinPlugin declares SimpleFIN: read-only bank balances and
// transactions. It raises no approval: nothing it does writes anywhere.
func simplefinPlugin() *pluginDescriptor {
	return &pluginDescriptor{
		ID:        "simplefin",
		Title:     "SimpleFIN (bank balances and transactions, read-only)",
		Version:   1,
		Tools:     func() []model.ToolSpec { return simplefin.NewDispatcher(nil, nil).Tools() },
		Configure: simplefinSkillKind,
		StubHint:  "run `/configure simplefin <setup-token>` (token from bridge.simplefin.org), then `/enable simplefin`",
		Activate:  activateSimpleFIN,
		CLI: []*cliCommand{
			{Names: []string{"simplefin"}, Section: "plugins", Run: cmdSimpleFIN, Flags: []string{"--days"},
				Summary: "print linked bank balances and recent transactions (read-only)"},
		},
	}
}

// activateSimpleFIN builds the read-only finance connector. The only
// credential is the access URL (claim-on-first-use handles a raw setup token
// pasted via the dashboard keys page). The ledger lives under
// <dataDir>/simplefin as an append-only JSONL.
func activateSimpleFIN(ctx context.Context, e *pluginEnv) (plugin, func(), error) {
	access, err := resolveSimpleFINAccessURL(ctx, e.c.cfgDir)
	if err != nil {
		return nil, nil, err
	}
	svc, err := simplefin.Open(simplefin.Config{AccessURL: access})
	if err != nil {
		return nil, nil, fmt.Errorf("simplefin plugin: %w", err)
	}
	led, err := simplefin.OpenLedger(filepath.Join(e.c.dataDir, "simplefin"))
	if err != nil {
		return nil, nil, fmt.Errorf("simplefin plugin: %w", err)
	}
	return simplefin.NewDispatcher(svc, led), nil, nil
}
