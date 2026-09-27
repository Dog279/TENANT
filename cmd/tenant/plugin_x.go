package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"tenant/internal/cmdreg"
	"tenant/internal/model"
	xp "tenant/internal/plugins/x"
)

// xPlugin declares X/Twitter.
func xPlugin() *pluginDescriptor {
	return &pluginDescriptor{
		ID:          "x",
		Events:      xp.Events,
		Connector:   true,
		Forget:      func(_, data string) []string { return []string{xp.TokenPath(data)} },
		DataSecrets: func(data string) []string { return []string{xp.TokenPath(data)} },
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

// checkX verifies the X / Twitter plugin can talk to api.x.com when it's
// enabled (TEN-67). Tiers mirror checkDiscord:
//
//	skip — plugin not configured / not enabled
//	FAIL — bearer unresolvable (env empty, secret missing) or 401 (revoked)
//	WARN — bearer present but network unreachable, or a non-401 4xx/5xx
//	OK   — /2/users/me returns the account identity
//
// Best-effort and offline-tolerant: a network error degrades to WARN (the
// bearer resolved; we just can't confirm x.com is reachable).
func checkX(ctx context.Context, e *doctorEnv) checkResult {
	if e.lc == nil || e.lc.Skills == nil {
		return checkResult{Status: statusSkip, Detail: "x plugin not configured"}
	}
	xcfg := e.lc.Skills["x"]
	if xcfg == nil || !xcfg.Enabled {
		return checkResult{Status: statusSkip, Detail: "x plugin not enabled"}
	}
	// Resolve bearer. Env wins over stored secret (matches toolmux.go).
	bearer := os.Getenv("X_BEARER_TOKEN")
	if bearer == "" {
		bearer = e.resolveSecret(skillSecretID("x", "bearer"), authCfg{Stored: true})
	}
	if bearer == "" {
		return checkResult{Status: statusFail,
			Detail: "x enabled but bearer token is missing",
			Fix:    "set $X_BEARER_TOKEN, or run `/configure x <bearer>`"}
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(probeCtx, "GET", "https://api.x.com/2/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return checkResult{Status: statusWarn,
			Detail: "bearer present but could not reach api.x.com: " + err.Error(),
			Fix:    "if intentional offline, ignore; else check network / firewall"}
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 {
		return checkResult{Status: statusFail,
			Detail: "x returned 401 Unauthorized — bearer token is invalid or revoked",
			Fix:    "regenerate the bearer at https://developer.x.com and run `/configure x <bearer>`"}
	}
	if resp.StatusCode >= 400 {
		return checkResult{Status: statusWarn,
			Detail: fmt.Sprintf("x /2/users/me returned HTTP %d", resp.StatusCode),
			Fix:    "check the app's status / permissions at https://developer.x.com"}
	}
	return checkResult{Status: statusOK, Detail: "bearer valid; api.x.com reachable"}
}
