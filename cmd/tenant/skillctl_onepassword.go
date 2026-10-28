package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"tenant/internal/plugins/onepassword"
)

// onepasswordSkillKind is the plugin's config schema for /configure, the
// Integrations page and `tenant setup`: the service account token (secret),
// the vaults the agent may touch, where op is, and whether writes ask.
func onepasswordSkillKind() skillKind {
	return skillKind{
		ID:    "onepassword",
		Label: "1Password - vaults, items and secrets through the op CLI (service account)",
		Wired: true,
		SetupHint: "Create a SERVICE ACCOUNT in 1Password (Developer, then Directory) and grant it ONLY the\n" +
			"vault(s) this agent should use: read items, and write items if it keeps credentials.\n" +
			"A service account can never reach your Private, Personal or Employee vault.\n" +
			"The token is shown once; paste it here. Reading a secret value is gated under the egress\n" +
			"category and every read is logged by its reference, never its value.",
		Fields: []skillKindField{
			{
				Key: "token", Prompt: "Service account token (ops_...)", Secret: true, Required: true,
				Validate: validateOnePasswordToken,
			},
			{
				Key:    "vaults",
				Prompt: "Vaults the agent may use - comma-separated names or ids (blank: every vault the token can see)",
			},
			{
				Key:    "op_path",
				Prompt: "Path to the op binary (blank: found on PATH; set it when Tenant runs as a service account without your PATH)",
			},
			{
				Key: "allow_write", Default: "false",
				Prompt:       "Create and edit items WITHOUT a per-action approval prompt? (archiving always asks)",
				Options:      []string{"false", "true"},
				OptionLabels: []string{"no - ask before each write (recommended)", "yes - auto-approve op_create and op_edit"},
				Validate:     validateOneOf("false", "true"),
			},
		},
		Probe:        probeOnePassword,
		ProbeTimeout: 20 * time.Second,
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
		return fmt.Errorf("that looks like a 1Password Connect token (a JWT); this plugin needs a service account token, which starts with ops_")
	}
	if strings.HasPrefix(v, "A3-") {
		return fmt.Errorf("that looks like a Secret Key; this plugin needs a service account token, which starts with ops_")
	}
	if !strings.HasPrefix(v, "ops_") {
		return fmt.Errorf("a service account token starts with ops_")
	}
	return nil
}

// probeOnePassword runs `op whoami` with the token: the CLI is found and
// 1Password accepts the token. The identity shown is the account URL and
// the token's kind.
func probeOnePassword(ctx context.Context, creds *credentials, settings map[string]string, _ func() error) (string, error) {
	token := strings.TrimSpace(os.Getenv("OP_SERVICE_ACCOUNT_TOKEN"))
	if token == "" {
		token = strings.TrimSpace(creds.Get(skillSecretID("onepassword", "token")))
	}
	if token == "" {
		return "", fmt.Errorf("no token stored - run `/configure onepassword` again")
	}
	cli, err := onepassword.NewCLI(settings["op_path"], token)
	if err != nil {
		return "", err
	}
	id, err := cli.WhoAmI(ctx)
	if err != nil {
		return "", err
	}
	kind := strings.ToLower(firstNonEmpty(id.Type, "account"))
	if id.Email != "" {
		return fmt.Sprintf("%s %s at %s", kind, id.Email, id.URL), nil
	}
	return fmt.Sprintf("%s at %s", kind, id.URL), nil
}
