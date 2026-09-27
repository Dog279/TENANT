package main

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"tenant/internal/cmdreg"
	"tenant/internal/model"
	"tenant/internal/plugins/web"
)

// webPlugin declares the web plugin: the agent browses the live web in a
// Chrome session. Every agent drives its own browser, so web is registered
// per agent (team.go addWebTool), never in the shared stub catalog.
func webPlugin() *pluginDescriptor {
	return &pluginDescriptor{
		ID:        "web",
		Events:    web.Events,
		Connector: true,
		// The search backends (optional; a key just upgrades the backend). env
		// wins at runtime, so EnvVars reflect resolution order.
		Keys: []keySpec{
			{CredID: "tavily", Name: "Tavily Search", Category: "Web search", EnvVars: []string{"TAVILY_API_KEY", "TAVILY_KEY"}, Kind: keyDirect},
			{CredID: "brave_search", Name: "Brave Search", Category: "Web search", EnvVars: []string{"BRAVE_SEARCH_API_KEY", "BRAVE_API_KEY"}, Kind: keyDirect},
			{CredID: "jina", Name: "Jina Reader", Category: "Web search", EnvVars: []string{"JINA_API_KEY", "JINA_KEY"}, Kind: keyDirect},
		},
		Flags:       bindWebFlags,
		On:          func(pf *pluginFlags) bool { return webFlagsOf(pf).on },
		ApplyConfig: applyWebConfig,
		Grants:      webGrants,
		Launch:      launchWeb,
		Eval:        evalWeb,
		Title:       "Web browsing",
		Version:     1,
		Tools:       func() []model.ToolSpec { return web.NewDispatcher(nil, web.Policy{}, "").Tools() },
		Configure:   webSkillKind,
		SetupOrder:  8,
		Actions: map[string]string{
			"web_interact": catWeb,         // click / fill / select
			"web_transact": catDestructive, // a purchase- or delete-shaped click
		},
		PerAgent: true,
		Doctor: []doctorCheck{
			// Chrome is detectable when the web plugin will be used.
			{"chrome (if web enabled)", checkChrome},
		},
		CLI: []*cliCommand{
			{Names: []string{"web"}, Section: "plugins", Run: cmdWeb,
				Summary: "the agent browses the live web (read/explore)",
				Rows:    []cmdreg.Row{{Syntax: `web "<task>"`, Desc: "the agent browses the live web (read/explore)"}}},
		},
	}
}

// webFlags are the web plugin's launch flags.
type webFlags struct {
	on             bool
	show           bool
	allowInteract  bool
	privateNetwork []string
}

func webFlagsOf(pf *pluginFlags) *webFlags { return flagsOf[webFlags](pf, "web") }

func bindWebFlags(fs *flag.FlagSet) any {
	f := &webFlags{}
	fs.BoolVar(&f.on, "web", false, "enable the web plugin (drives Chrome)")
	fs.BoolVar(&f.show, "web-show", false, "run a visible Chrome window")
	fs.BoolVar(&f.allowInteract, "web-allow-interact", false, "permit web click/fill/select")
	fs.Func("web-private-network", "allow an exact named private endpoint (repeatable; explicit SSRF-policy opt-in)", func(s string) error {
		s = strings.TrimSpace(s)
		if s == "" {
			return fmt.Errorf("web-private-network hostname cannot be empty")
		}
		f.privateNetwork = append(f.privateNetwork, s)
		return nil
	})
	return f
}

func applyWebConfig(pf *pluginFlags, s savedConfig) {
	if !s.set["web"] {
		webFlagsOf(pf).on = true
	}
}

func webGrants(pf *pluginFlags) map[string]string {
	if webFlagsOf(pf).allowInteract {
		return map[string]string{catWeb: "--web-allow-interact"}
	}
	return nil
}

// launchWeb starts a shared browser session for commands that give every
// agent the same one (the TUI, serve and research turn --web off and give
// each agent its own instead).
func launchWeb(_ context.Context, e *pluginEnv) ([]plugin, func(), error) {
	f := webFlagsOf(e.pf)
	if !f.on {
		return nil, nil, nil
	}
	cfg := webConfig(e.c.cfgDir, e.pf, e.c.lc)
	cfg.ReaderObserver = func(a web.ReaderActivity) {
		if e.log != nil {
			e.log.Info("web reader remote request", "provider", a.Provider, "target_host", a.TargetHost)
		}
	}
	sess, err := web.NewSession(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("web plugin: %w (is Chrome installed?)", err)
	}
	return []plugin{web.NewDispatcher(sess, web.Policy{AllowInteract: e.gateAllow(f.allowInteract), Confirm: e.confirm},
		filepath.Join(e.c.dataDir, "screenshots"))}, func() { _ = sess.Close() }, nil
}

// evalWeb: web is available wherever Chrome is installed; it needs no config.
func evalWeb(pf *pluginFlags, _ *commonFlags, explicit map[string]bool) bool {
	f := webFlagsOf(pf)
	if explicit["web"] || f.on {
		return false
	}
	f.on = true
	return true
}

// checkChrome — only meaningful when the web plugin is configured/enabled.
// detectChrome probes the well-known OS install paths; missing Chrome
// silently breaks web_navigate at first use, hence the upfront warning.
func checkChrome(_ context.Context, e *doctorEnv) checkResult {
	if e.lc == nil || e.lc.Skills == nil {
		return checkResult{Status: statusSkip, Detail: "web plugin not configured"}
	}
	wcfg := e.lc.Skills["web"]
	if wcfg == nil || !wcfg.Enabled {
		return checkResult{Status: statusSkip, Detail: "web plugin not enabled"}
	}
	// Optional override path from the web plugin config.
	overridePath := ""
	if wcfg.Settings != nil {
		overridePath = wcfg.Settings["chrome_path"]
	}
	path, err := web.DetectChrome(overridePath)
	if err != nil {
		return checkResult{Status: statusFail,
			Detail: "no Chrome/Chromium/Edge found on this system",
			Fix:    "install Chrome, OR set web.settings.chrome_path in config.json"}
	}
	return checkResult{Status: statusOK, Detail: "chrome at " + path}
}
