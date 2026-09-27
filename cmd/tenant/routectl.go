package main

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"

	"tenant/internal/route"
)

// cmdRoute is the headless counterpart to the TUI's /route control.
// Configuration changes are persistent and take effect on the next process
// launch; status and stats inspect the same classifier and durable decision log
// used by the live runtime.
//
//	tenant route status
//	tenant route on|off|shadow|assisted|everywhere
//	tenant route stats
//	tenant route force <profileID>
//	tenant route export-dataset [--out FILE] [--since DUR] [--include-relay]
func cmdRoute(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"status"}
	}
	sub := strings.ToLower(strings.TrimSpace(args[0]))
	rest := args[1:]

	// Pull force's positional profile before flag parsing. Go's flag package
	// stops at the first positional, while the documented shape puts flags after
	// the profile just as `tenant model use` does.
	profileID := ""
	if sub == "force" && len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		profileID, rest = rest[0], rest[1:]
	}

	fs := flag.NewFlagSet("route "+sub, flag.ContinueOnError)
	c := bindCommon(fs)
	exportOut := fs.String("out", "", "export-dataset: output JSONL path (default <data>/route-dataset.jsonl)")
	exportSince := fs.Duration("since", 0, "export-dataset: only decisions newer than this age (0 = all)")
	exportRelay := fs.Bool("include-relay", false, "export-dataset: include relay-surface decisions (TEN-256 excludes them by default)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if sub == "force" && profileID == "" && fs.NArg() > 0 {
		profileID = fs.Arg(0)
	}
	consumedArgs := 0
	if sub == "force" && len(fs.Args()) > 0 && fs.Arg(0) == profileID {
		consumedArgs = 1
	}
	if fs.NArg() != consumedArgs {
		return routeCLIUsageError(sub)
	}
	switch sub {
	case "status", "on", "off", "shadow", "assisted", "everywhere", "stats", "export-dataset":
	case "force":
		profileID = strings.TrimSpace(profileID)
		if profileID == "" {
			return routeCLIUsageError(sub)
		}
	default:
		return routeCLIUsageError(sub)
	}
	if err := c.resolveDirs(); err != nil {
		return err
	}

	lc, err := loadLaunchConfig(c.cfgDir)
	if err != nil {
		return err
	}

	switch sub {
	case "status":
		rt, err := newRouteRuntime(lc, c.cfgDir, c.dataDir, nil, nil)
		if err != nil {
			return err
		}
		fmt.Println(rt.Status(ctx))
		return nil
	case "stats":
		rt, err := newRouteRuntime(lc, c.cfgDir, c.dataDir, nil, nil)
		if err != nil {
			return err
		}
		fmt.Println(rt.Stats())
		return nil
	case "on", "off", "shadow", "assisted", "everywhere":
		rt, err := newRouteRuntime(lc, c.cfgDir, c.dataDir, nil, nil)
		if err != nil {
			return err
		}
		status, err := rt.SetMode(sub)
		if err != nil {
			return err
		}
		fmt.Println(status)
		return nil
	case "export-dataset":
		summary, err := exportRouteDataset(ctx, c.dataDir, exportOptions{
			Out: *exportOut, Since: *exportSince, IncludeRelay: *exportRelay,
		})
		if err != nil {
			return err
		}
		fmt.Println(summary)
		return nil
	case "force":
		canonical, err := canonicalConfiguredRouteProfileID(lc, profileID)
		if err != nil {
			return err
		}
		if lc.Route == nil {
			lc.Route = &routeConfig{}
		}
		lc.Route.Enabled = true
		lc.Route.Mode = route.ModeActive
		lc.Route.ForceProfile = canonical
		// save re-runs resolveRouteConfig, deliberately retaining the actionable
		// active-without-big_role rejection rather than writing an inert force.
		if err := lc.Save(c.cfgDir); err != nil {
			return err
		}
		fmt.Printf("WARNING: route forced to profile %q; this explicitly pierces never_cloud and will be audit-logged\n", canonical)
		return nil
	}
	return nil
}

func routeCLIUsageError(sub string) error {
	if sub == "force" {
		return fmt.Errorf("usage: tenant route force <profileID> [--config DIR] [--data DIR]")
	}
	if sub == "export-dataset" {
		return fmt.Errorf("usage: tenant route export-dataset [--out FILE] [--since DUR] [--include-relay] [--config DIR] [--data DIR]")
	}
	return fmt.Errorf("usage: tenant route <status|on|off|shadow|assisted|everywhere|stats|force|export-dataset> [--config DIR] [--data DIR]")
}

// validateConfiguredRouteProfileID performs the headless force check without
// constructing a model backend or touching the network. The accepted set is
// exactly the generation-profile IDs wiring can register from the configured
// active, planner-big, and fallback providers.
func validateConfiguredRouteProfileID(lc *launchConfig, profileID string) error {
	_, err := canonicalConfiguredRouteProfileID(lc, profileID)
	return err
}

func canonicalConfiguredRouteProfileID(lc *launchConfig, profileID string) (string, error) {
	profileID = strings.TrimSpace(profileID)
	aliases := configuredRouteProfileAliases(lc)
	if canonical, ok := aliases[profileID]; ok {
		return canonical, nil
	}
	knownSet := configuredRouteProfileIDs(lc)
	known := make([]string, 0, len(knownSet))
	for id := range knownSet {
		known = append(known, id)
	}
	sort.Strings(known)
	if len(known) == 0 {
		return "", fmt.Errorf("route: no forceable model profiles are configured; configure an active provider and route.big_role first")
	}
	return "", fmt.Errorf("route: unknown profile %q; configured forceable profiles: %s", profileID, strings.Join(known, ", "))
}

func configuredRouteProfileIDs(lc *launchConfig) map[string]struct{} {
	ids := make(map[string]struct{})
	for _, canonical := range configuredRouteProfileAliases(lc) {
		ids[canonical] = struct{}{}
	}
	return ids
}

func configuredRouteProfileAliases(lc *launchConfig) map[string]string {
	ids := make(map[string]string)
	if lc == nil {
		return ids
	}
	add := func(providerName, namespace string, plannerOnly, acceptBare bool) {
		pc := lc.Providers[providerName]
		base := generationProfileIDs(pc)
		if plannerOnly && len(base) > 0 {
			base = base[:1]
		}
		for _, id := range base {
			canonical := providerName + "/" + id
			if namespace != "" {
				canonical = namespace + "/" + id
			}
			ids[canonical] = canonical
			if acceptBare {
				ids[id] = canonical
			}
		}
	}

	active := strings.TrimSpace(lc.Provider)
	if active != "" {
		add(active, active, false, true)
	}
	if lc.Route != nil {
		big := strings.TrimSpace(lc.Route.BigRole)
		if big != "" {
			// installRouteBigRole registers only the provider's planner profile.
			add(big, big, true, false)
		}
	}
	seen := map[string]bool{active: true}
	for _, raw := range lc.Fallbacks {
		name := strings.TrimSpace(raw)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		add(name, name, false, false)
	}
	// A canonical provider/profile binding remains forceable even after
	// /model use makes that provider neither active nor a fallback. This is
	// what makes an explicit force stable across swaps and restarts.
	for name := range lc.Providers {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		add(name, name, false, false)
	}
	return ids
}

// generationProfileIDs mirrors genProfiles' stable IDs without requiring a
// model, endpoint, credential, backend construction, or live model catalog.
func generationProfileIDs(pc *providerConfig) []string {
	if pc == nil {
		return nil
	}
	kind, ok := providerKinds[pc.Kind]
	if !ok || !kind.Wired {
		return nil
	}
	prefix := ""
	switch kind.Backend {
	case "vllm":
		prefix = "vllm"
	case "anthropic":
		prefix = "anthropic"
	case "echo":
		prefix = "echo"
	default:
		return nil
	}
	return []string{prefix + "-planner", prefix + "-executor", prefix + "-summarizer"}
}
