package main

// atlassianctl.go is `tenant atlassian login` — the one-time OAuth 3LO browser
// flow (TEN-59). It prints the consent URL, captures the localhost callback, and
// caches the refresh token (0600) so the agent's jira_* tools authenticate via
// Path B on subsequent runs without re-login. Path A (API token) needs no login.

import (
	"context"
	"flag"
	"fmt"
	"os"

	"tenant/internal/plugins/atlassian"
)

func cmdAtlassian(ctx context.Context, args []string) error {
	if len(args) < 1 || args[0] != "login" {
		return fmt.Errorf("usage: tenant atlassian login --site <url> --client-id <id>   ($ATLASSIAN_CLIENT_SECRET for the secret)")
	}
	fs := flag.NewFlagSet("atlassian login", flag.ContinueOnError)
	c := bindCommon(fs)
	site := fs.String("site", "", "Atlassian site URL, e.g. https://you.atlassian.net")
	clientID := fs.String("client-id", "", "Atlassian OAuth app client id")
	callback := fs.String("oauth-callback", "", "callback bind addr (default 127.0.0.1:8765)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *clientID == "" {
		return fmt.Errorf("atlassian login: --client-id is required (create an OAuth 2.0 app in the Atlassian Developer Console; secret via $ATLASSIAN_CLIENT_SECRET)")
	}
	if err := c.resolveDirs(); err != nil {
		return err
	}
	cfg := atlassian.Config{
		SiteURL:          *site,
		ClientID:         *clientID,
		OAuthCallback:    *callback,
		TokenPath:        atlassianTokenPath(c.cfgDir),
		OAuthOpenBrowser: openBrowser,
	}
	resolved, err := atlassian.Authorize(ctx, cfg)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "\nAtlassian connected (%s). Token cached at %s.\nLaunch with --atlassian --atlassian-client-id %s to use it.\n",
		resolved, cfg.TokenPath, *clientID)
	return nil
}
