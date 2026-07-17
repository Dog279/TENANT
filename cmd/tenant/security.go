package main

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"tenant/internal/memory/soul"
	"tenant/internal/plugins/web"
)

func cmdSecurity(_ context.Context, args []string) error {
	if len(args) == 0 || args[0] != "audit" {
		return fmt.Errorf("usage: tenant security audit [--config DIR]")
	}
	fs := flag.NewFlagSet("security audit", flag.ContinueOnError)
	cfgDir := fs.String("config", "", "config dir (default OS config dir)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: tenant security audit [--config DIR]")
	}
	dir := expandPath(*cfgDir)
	if dir == "" {
		var err error
		dir, err = soul.DefaultDir()
		if err != nil {
			return fmt.Errorf("resolve config dir: %w", err)
		}
	}
	lc, err := loadLaunchConfig(dir)
	if err != nil {
		return err
	}
	outerKind, outerDetected := web.OuterSandbox()
	fmt.Print(formatBrowserSandboxAudit(lc.BrowserNoSandbox, outerKind, outerDetected))
	return nil
}

func formatBrowserSandboxAudit(noSandbox bool, outerKind string, outerDetected bool) string {
	var b strings.Builder
	b.WriteString("Browser process sandbox: ")
	switch {
	case !noSandbox:
		b.WriteString("ENABLED\n  --no-sandbox is excluded from Chrome launch flags (secure default).\n")
	case outerDetected:
		fmt.Fprintf(&b, "DISABLED (WARNING)\n  browser_no_sandbox is active inside detected %s.\n", outerKind)
	default:
		b.WriteString("ENFORCED\n  browser_no_sandbox was requested, but no supported outer sandbox was detected; browser launch will be refused.\n")
	}
	return b.String()
}
