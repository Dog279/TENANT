package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"tenant/internal/plugins/onepassword"
)

// onepasswordSkillKind is the plugin's config schema for /configure, the
// Integrations page and `tenant setup`: how it signs in, that mode's own
// fields, the vaults the agent may touch, where op is, and whether writes ask.
func onepasswordSkillKind() skillKind {
	mode := func(want string) func(map[string]string) bool {
		return func(v map[string]string) bool { return onepasswordAuth(v) == want }
	}
	return skillKind{
		ID:    "onepassword",
		Label: "1Password - vaults, items and secrets (a service account, a Connect server or the desktop app)",
		Wired: true,
		SetupHint: "Pick how the agent signs in to 1Password:\n" +
			"  service account - a token scoped to the vaults you grant it; works unattended. Create it in\n" +
			"    1Password (Developer, then Directory). It can never reach a Private, Personal or Employee vault.\n" +
			"  Connect server  - your own server in front of 1Password, with no rate limits; works unattended.\n" +
			"    It cannot archive an item, so op_delete is refused in this mode.\n" +
			"  desktop app     - the 1Password app on THIS machine asks you to approve (Touch ID, Windows Hello).\n" +
			"    For a person at the machine only: scheduled and remote turns are refused. The approval covers\n" +
			"    your whole account, so the vaults the agent may use must be named.\n" +
			"Reading a secret value is gated under the egress category and every read is logged by its\n" +
			"reference, never its value.",
		Fields: []skillKindField{
			{
				// Not Required: a config saved before this setting has none,
				// and blank means a service account.
				Key: "auth", Default: opAuthService,
				Prompt:  "How does the agent sign in to 1Password?",
				Options: []string{opAuthService, opAuthConnect, opAuthDesktop},
				OptionLabels: []string{
					"service account token - unattended, scoped to the vaults you grant (recommended)",
					"Connect server - your own self-hosted server, unattended, no rate limits",
					"desktop app - the 1Password app on this machine asks you each time",
				},
				Validate: validateOneOf(opAuthService, opAuthConnect, opAuthDesktop),
			},
			{
				Key: "token", Prompt: "Service account token (ops_...)", Secret: true, Required: true,
				Validate: validateOnePasswordToken, ShowIf: mode(opAuthService),
			},
			{
				Key: "connect_host", Prompt: "Connect server address (for example https://connect.example.internal:8443)", Required: true,
				Validate: validateConnectHost, ShowIf: mode(opAuthConnect),
			},
			{
				Key: "connect_token", Prompt: "Connect server access token", Secret: true, Required: true,
				Validate: validateConnectToken, ShowIf: mode(opAuthConnect),
			},
			{
				Key:    "account",
				Prompt: "Account to use - its sign-in address, like my.1password.com (blank: the app's only account)",
				ShowIf: mode(opAuthDesktop),
			},
			{
				Key:    "vaults",
				Prompt: "Vaults the agent may use - comma-separated names or ids (blank: every vault the sign-in can see; REQUIRED with the desktop app)",
			},
			{
				Key:    "op_path",
				Prompt: "Path to the op binary (blank: found on PATH; set it when Tenant runs as a service account without your PATH)",
				ShowIf: func(v map[string]string) bool { return onepasswordAuth(v) != opAuthConnect },
			},
			{
				Key: "allow_write", Default: "false",
				Prompt:       "Create and edit items WITHOUT a per-action approval prompt? (archiving always asks)",
				Options:      []string{"false", "true"},
				OptionLabels: []string{"no - ask before each write (recommended)", "yes - auto-approve op_create and op_edit"},
				Validate:     validateOneOf("false", "true"),
			},
		},
		Probe: probeOnePassword,
		// Long enough for a person to answer the desktop app's prompt.
		ProbeTimeout: onepassword.DesktopTimeout + 10*time.Second,
	}
}

// validateOnePasswordToken catches the two common pastes that are not a
// service account token: a Connect token (a JWT) and a Secret Key.
func validateOnePasswordToken(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return fmt.Errorf("the token is required")
	}
	if strings.HasPrefix(v, "eyJ") {
		return fmt.Errorf("that looks like a 1Password Connect token (a JWT); pick the Connect server sign-in for it, or paste a service account token, which starts with ops_")
	}
	if strings.HasPrefix(v, "A3-") {
		return fmt.Errorf("that looks like a Secret Key; this plugin needs a service account token, which starts with ops_")
	}
	if !strings.HasPrefix(v, "ops_") {
		return fmt.Errorf("a service account token starts with ops_")
	}
	return nil
}

// validateConnectToken catches a service account token or a Secret Key
// pasted where a Connect token belongs. Blank passes: ShowIf hides the field
// in the other modes.
func validateConnectToken(v string) error {
	v = strings.TrimSpace(v)
	switch {
	case v == "":
		return nil
	case strings.HasPrefix(v, "ops_"):
		return fmt.Errorf("that is a service account token; pick the service account sign-in for it, or paste the Connect server's access token")
	case strings.HasPrefix(v, "A3-"):
		return fmt.Errorf("that looks like a Secret Key; this field needs the Connect server's access token")
	}
	return nil
}

// validateConnectHost wants an http or https address with a host. Blank
// passes: ShowIf hides the field in the other modes.
func validateConnectHost(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("the address must start with http:// or https:// and name the server, like https://connect.example.internal:8443")
	}
	return nil
}

// probeOnePassword signs in the way the settings say and reports who that
// is. With the desktop app this is the one place a prompt is wanted: the
// person configuring the plugin is at the machine and approves it once.
func probeOnePassword(ctx context.Context, creds *credentials, settings map[string]string, _ func() error) (string, error) {
	be, err := onepasswordBackendFrom(creds, settings)
	if err != nil {
		return "", err
	}
	id, err := be.WhoAmI(ctx)
	if err != nil {
		return "", err
	}
	return onepasswordIdentity(be, id), nil
}
