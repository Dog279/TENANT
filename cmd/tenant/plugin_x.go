package main

import (
	"context"
	"fmt"

	"tenant/internal/cmdreg"
	"tenant/internal/model"
	xp "tenant/internal/plugins/x"
)

// xPlugin declares X/Twitter.
func xPlugin() *pluginDescriptor {
	return &pluginDescriptor{
		ID:        "x",
		Title:     "X / Twitter",
		Version:   1,
		Tools:     func() []model.ToolSpec { return xp.NewDispatcher(nil, xp.Policy{}).Tools() },
		Configure: xSkillKind,
		Setup:     4,
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
	bearer := resolveXBearer(e.c.cfgDir, e.pf.xBearer)
	if bearer == "" {
		return nil, nil, fmt.Errorf("x plugin: %w yet — run `/configure x <bearer>`", errNotConfigured)
	}
	svc, err := xp.Open(xp.Config{Bearer: bearer, TokenPath: xp.TokenPath(e.c.dataDir), AllowPost: e.pf.xAllowPost})
	if err != nil {
		return nil, nil, fmt.Errorf("x plugin: %w", err)
	}
	return xp.NewDispatcher(svc, xp.Policy{AllowPost: e.gateAllow(e.pf.xAllowPost), Confirm: e.confirm}), func() {}, nil
}
