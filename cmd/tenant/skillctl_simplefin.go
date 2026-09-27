package main

// SimpleFIN catalog entry for the `/configure` skill-config surface —
// the read-only personal-finance connector (internal/plugins/simplefin).
//
// ONE secret, two accepted shapes: the operator pastes either the
// one-time SETUP TOKEN from bridge.simplefin.org (base64 — the normal
// path) or an already-claimed ACCESS URL. A setup token is claimed at
// probe time and the stored secret is REPLACED with the returned access
// URL via the mid-probe save callback (the gsuite-oauth persistence
// pattern) — setup tokens are one-shot at the bridge, so the claim
// result must land on disk immediately.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"tenant/internal/plugins/simplefin"
)

func simplefinSkillKind() skillKind {
	return skillKind{
		ID:    "simplefin",
		Label: "SimpleFIN — read-only bank/card balances + transactions",
		Wired: true,
		SetupHint: "Sign up at bridge.simplefin.org, link your banks THERE (credentials never touch tenant), " +
			"then create a setup token and paste it here. The token is claimed once and swapped for the " +
			"long-lived access URL automatically. Read-only by protocol — tenant cannot move money.",
		Fields: []skillKindField{{
			Key:      "access_url",
			Prompt:   "SimpleFIN setup token (from bridge.simplefin.org) — or an already-claimed access URL",
			Secret:   true,
			Required: true,
			Validate: validateNonEmpty("setup token / access URL"),
		}},
		Probe: probeSimpleFIN,
		// The bridge aggregates from banks on demand; first fetch can be slow.
		ProbeTimeout: 90 * time.Second,
	}
}

func probeSimpleFIN(ctx context.Context, creds *credentials, _ map[string]string, save func() error) (string, error) {
	return probeSimpleFINWith(ctx, creds, save, nil)
}

// probeSimpleFINWith is the seamed implementation (hc nil ⇒ real
// network). Claims a stored setup token in place, then verifies with a
// 7-day accounts fetch.
func probeSimpleFINWith(ctx context.Context, creds *credentials, save func() error, hc *http.Client) (string, error) {
	raw := strings.TrimSpace(creds.Get(skillSecretID("simplefin", "access_url")))
	if raw == "" {
		return "", errors.New("no setup token stored — run `/configure simplefin <setup-token>`")
	}
	if simplefin.LooksLikeSetupToken(raw) {
		var access string
		var err error
		if hc != nil {
			access, err = simplefin.Claim(ctx, raw, hc)
		} else {
			access, err = simplefin.Claim(ctx, raw, nil)
		}
		if err != nil {
			return "", err
		}
		creds.Set(skillSecretID("simplefin", "access_url"), access)
		if save != nil {
			if err := save(); err != nil {
				return "", fmt.Errorf("claimed OK but could not persist the access URL (the setup token is now consumed — mint a fresh one if this state is lost): %w", err)
			}
		}
		raw = access
	}
	cfg := simplefin.Config{AccessURL: raw}
	if hc != nil {
		cfg.HTTP = hc
	}
	svc, err := simplefin.Open(cfg)
	if err != nil {
		return "", err
	}
	now := time.Now()
	set, err := svc.Accounts(ctx, now.Add(-7*24*time.Hour), now)
	if err != nil {
		return "", err
	}
	orgs := map[string]bool{}
	for _, a := range set.Accounts {
		orgs[a.Org.Name] = true
	}
	identity := fmt.Sprintf("%d account(s) across %d institution(s)", len(set.Accounts), len(orgs))
	if len(set.Errors) > 0 {
		identity += " — bridge notices: " + strings.Join(set.Errors, "; ")
	}
	return identity, nil
}

// resolveSimpleFINAccessURL is the shared credential read used by the
// toolmux activator and the one-shot CLI. If the stored secret is still
// a setup token (e.g. pasted via the dashboard keys page, which has no
// probe), it is claimed here and the credential rewritten — claim-on-
// first-use keeps every entry path equivalent.
func resolveSimpleFINAccessURL(ctx context.Context, cfgDir string) (string, error) {
	creds, err := loadCredentials(cfgDir)
	if err != nil {
		return "", fmt.Errorf("simplefin plugin: load credentials: %w", err)
	}
	raw := strings.TrimSpace(creds.Get(skillSecretID("simplefin", "access_url")))
	if raw == "" {
		return "", fmt.Errorf("simplefin plugin: %w yet — run `/configure simplefin <setup-token>`", errNotConfigured)
	}
	if simplefin.LooksLikeSetupToken(raw) {
		access, err := simplefin.Claim(ctx, raw, nil)
		if err != nil {
			return "", fmt.Errorf("simplefin plugin: claim setup token: %w", err)
		}
		creds.Set(skillSecretID("simplefin", "access_url"), access)
		if err := creds.Save(cfgDir); err != nil {
			return "", fmt.Errorf("simplefin plugin: claimed OK but could not persist the access URL (the one-time token is consumed — mint a fresh one if this state is lost): %w", err)
		}
		raw = access
	}
	return raw, nil
}
