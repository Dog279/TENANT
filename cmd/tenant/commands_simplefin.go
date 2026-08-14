package main

// `tenant simplefin` — one-shot verification print for the SimpleFIN
// connector: sync from the bridge, then balances + recent transactions +
// spend summary. Deliberately NOT agent-driven (no LLM in the loop) so
// it verifies the credential, the bridge, and the ledger in isolation —
// the same role `tenant gsuite` played for Workspace bring-up.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"path/filepath"

	"tenant/internal/model"
	"tenant/internal/plugins/simplefin"
)

func cmdSimpleFIN(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("simplefin", flag.ContinueOnError)
	c := bindCommon(fs)
	days := fs.Int("days", 30, "transaction look-back window in days")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := c.resolve(); err != nil {
		return err
	}
	access, err := resolveSimpleFINAccessURL(ctx, c.cfgDir)
	if err != nil {
		return err
	}
	svc, err := simplefin.Open(simplefin.Config{AccessURL: access})
	if err != nil {
		return err
	}
	led, err := simplefin.OpenLedger(filepath.Join(c.dataDir, "simplefin"))
	if err != nil {
		return err
	}
	d := simplefin.NewDispatcher(svc, led)

	calls := []struct{ name, args string }{
		{"money_sync", `{}`},
		{"money_balances", `{}`},
		{"money_transactions", fmt.Sprintf(`{"days": %d}`, *days)},
		{"money_spend", fmt.Sprintf(`{"days": %d}`, *days)},
	}
	for _, call := range calls {
		out, isErr, err := d.Dispatch(ctx, model.ToolCall{Name: call.name, Arguments: json.RawMessage(call.args)})
		if err != nil {
			return fmt.Errorf("%s: %w", call.name, err)
		}
		fmt.Printf("--- %s ---\n%s\n\n", call.name, out)
		if isErr {
			return fmt.Errorf("%s reported an error (see above)", call.name)
		}
	}
	return nil
}
