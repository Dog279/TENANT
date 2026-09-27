package main

import (
	"context"
	"flag"
	"fmt"

	"tenant/internal/cmdreg"
	"tenant/internal/model"
	xp "tenant/internal/plugins/x"
)

// xPlugin declares X/Twitter.
func xPlugin() *pluginDescriptor {
	return &pluginDescriptor{
		ID:          "x",
		Flags:       bindXFlags,
		On:          func(pf *pluginFlags) bool { return xFlagsOf(pf).on },
		ApplyConfig: applyXConfig,
		Grants:      xGrants,
		Launch:      launchX,
		Title:       "X / Twitter",
		Version:     1,
		Tools:       func() []model.ToolSpec { return xp.NewDispatcher(nil, xp.Policy{}).Tools() },
		Configure:   xSkillKind,
		SetupOrder:  4,
		// x_post and x_delete both raise x_post.
		Actions:  map[string]string{"x_post": catSend},
		StubHint: "run `/configure x <bearer>`, then `/enable x` (or relaunch with --x)",
		Activate: activateX,
		Doctor: []doctorCheck{
			// TEN-67: X bearer resolvable + api.x.com reachable when x enabled.
			{"x (if enabled)", checkX},
		},
		CLI: []*cliCommand{
			{Names: []string{"x"}, Section: "plugins", Run: cmdX,
				Summary: "the agent uses X/Twitter",
				Rows:    []cmdreg.Row{{Syntax: `x "<task>" | x --login`, Desc: "the agent uses X/Twitter (read; --allow-post to act)"}}},
		},
	}
}

// activateX builds the X plugin from the /configure-saved bearer (or the
// launch flag / env), TEN-67. Same resolution as the launch path via
// resolveXBearer, so both pick the same token.
func activateX(_ context.Context, e *pluginEnv) (plugin, func(), error) {
	bearer := resolveXBearer(e.c.cfgDir, xFlagsOf(e.pf).bearer)
	if bearer == "" {
		return nil, nil, fmt.Errorf("x plugin: %w yet — run `/configure x <bearer>`", errNotConfigured)
	}
	svc, err := xp.Open(xp.Config{Bearer: bearer, TokenPath: xp.TokenPath(e.c.dataDir), AllowPost: xFlagsOf(e.pf).allowPost})
	if err != nil {
		return nil, nil, fmt.Errorf("x plugin: %w", err)
	}
	return xp.NewDispatcher(svc, xp.Policy{AllowPost: e.gateAllow(xFlagsOf(e.pf).allowPost), Confirm: e.confirm}), func() {}, nil
}

// xFlags are X's launch flags.
type xFlags struct {
	on        bool
	bearer    string
	allowPost bool
}

func xFlagsOf(pf *pluginFlags) *xFlags { return flagsOf[xFlags](pf, "x") }

func bindXFlags(fs *flag.FlagSet) any {
	f := &xFlags{}
	fs.BoolVar(&f.on, "x", false, "enable X/Twitter")
	fs.StringVar(&f.bearer, "x-bearer", "", "X app bearer token (or $X_BEARER_TOKEN)")
	fs.BoolVar(&f.allowPost, "x-allow-post", false, "permit x_post/x_delete")
	return f
}

func applyXConfig(pf *pluginFlags, s savedConfig) {
	f := xFlagsOf(pf)
	if !s.set["x"] {
		f.on = true
	}
	if !s.set["x-bearer"] {
		f.bearer = firstNonEmpty(f.bearer, s.secret("bearer"))
	}
}

func xGrants(pf *pluginFlags) map[string]string {
	if xFlagsOf(pf).allowPost {
		return map[string]string{catSend: "--x-allow-post"}
	}
	return nil
}

// launchX builds the X plugin from the launch flags and saved bearer.
func launchX(_ context.Context, e *pluginEnv) ([]plugin, func(), error) {
	f := xFlagsOf(e.pf)
	if !f.on {
		return nil, nil, nil
	}
	bearer := resolveXBearer(e.c.cfgDir, f.bearer)
	svc, err := xp.Open(xp.Config{Bearer: bearer, TokenPath: xp.TokenPath(e.c.dataDir), AllowPost: f.allowPost})
	if err != nil {
		return nil, nil, fmt.Errorf("x plugin: %w", err)
	}
	return []plugin{xp.NewDispatcher(svc, xp.Policy{AllowPost: e.gateAllow(f.allowPost), Confirm: e.confirm})}, nil, nil
}
