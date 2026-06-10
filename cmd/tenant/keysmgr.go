package main

// keysmgr.go adapts the credentials store to dashboard.SecretsControl for the
// write-only API-key settings page (TEN-145). It reads only PRESENCE into the
// view (never a value), and routes writes per catalog kind:
//   - keyProvider → modelControl.AddCloudModel (registers provider + stores key
//     + sets Auth.Stored); remove → forgetProviderSecret + clearProviderStored.
//   - keyDirect   → credentials.json set/delete under the catalog CredID.

import (
	"context"
	"fmt"
	"os"
	"time"

	"tenant/internal/dashboard"
	"tenant/internal/tui"
)

// credsWatchInterval is how often watchCredentials polls credentials.json for
// an external key rotation. Cheap (one os.Stat); 20s is a fine latency for the
// rare key-rotation event.
const credsWatchInterval = 20 * time.Second

type dashKeys struct {
	cfgDir string
	mc     *modelControl
}

var _ dashboard.SecretsControl = dashKeys{}

// tuiKeys adapts dashKeys to tui.SecretsControl for the no-arg `/configure`
// picker (TEN-149) — same keyCatalog + live-apply path as the dashboard Keys
// page, just the TUI's view type.
type tuiKeys struct{ dk dashKeys }

var _ tui.SecretsControl = tuiKeys{}

func (k tuiKeys) List() []tui.SecretItem {
	views := k.dk.List()
	out := make([]tui.SecretItem, 0, len(views))
	for _, v := range views {
		out = append(out, tui.SecretItem{CredID: v.CredID, Name: v.Name, Category: v.Category, Set: v.Set})
	}
	return out
}

func (k tuiKeys) Set(credID, value string) error { return k.dk.SetSecret(credID, value) }
func (k tuiKeys) Remove(credID string) error     { return k.dk.RemoveSecret(credID) }

// List cross-references the fixed catalog against the credentials store + the
// environment. It reads ONLY presence (get(id) != "") into the view — never the
// value.
func (k dashKeys) List() []dashboard.ServiceKeyView {
	creds, _ := loadCredentials(k.cfgDir) // missing/corrupt → empty store, no panic
	out := make([]dashboard.ServiceKeyView, 0, len(keyCatalog))
	for _, spec := range keyCatalog {
		var envVar string
		for _, ev := range spec.EnvVars {
			if os.Getenv(ev) != "" {
				envVar = ev
				break
			}
		}
		set := creds != nil && creds.get(spec.CredID) != ""
		out = append(out, dashboard.ServiceKeyView{
			CredID:      spec.CredID,
			Name:        spec.Name,
			Category:    spec.Category,
			Set:         set,
			EnvDetected: envVar != "",
			EnvVar:      envVar,
			Required:    spec.Required,
		})
	}
	return out
}

func (k dashKeys) SetSecret(credID, value string) error {
	spec, ok := lookupKeySpec(credID) // guard: only catalog ids are mutable
	if !ok {
		return fmt.Errorf("unknown key")
	}
	switch spec.Kind {
	case keyProvider:
		// AddCloudModel registers the provider (Auth.Stored=true) + stores the
		// key in credentials.json (0600). Idempotent: re-set overwrites the key.
		if _, err := k.mc.AddCloudModel(spec.CredID, value); err != nil {
			return err
		}
		k.maybeReloadProvider(spec.CredID) // hot-swap it live if it's the active provider
		return nil
	default: // keyDirect
		creds, err := loadCredentials(k.cfgDir)
		if err != nil {
			return err
		}
		creds.set(spec.CredID, value)
		return creds.save(k.cfgDir) // 0600 atomicWrite
	}
}

func (k dashKeys) RemoveSecret(credID string) error {
	spec, ok := lookupKeySpec(credID)
	if !ok {
		return fmt.Errorf("unknown key")
	}
	switch spec.Kind {
	case keyProvider:
		forgetProviderSecret(k.cfgDir, spec.CredID) // delete the cred entry
		// forgetProviderSecret does NOT clear Auth.Stored, which would leave
		// resolveSecret returning "" for a Stored:true provider with no secret —
		// finish the job so the provider goes cleanly keyless.
		if err := clearProviderStored(k.cfgDir, spec.CredID); err != nil {
			return err
		}
		k.maybeReloadProvider(spec.CredID) // apply the keyless state live if active
		return nil
	default: // keyDirect
		creds, err := loadCredentials(k.cfgDir)
		if err != nil {
			return err
		}
		delete(creds.Secrets, spec.CredID)
		return creds.save(k.cfgDir)
	}
}

// maybeReloadProvider hot-swaps the active provider's key into the live router
// when the changed key belongs to the ACTIVE provider — so a key set/removed via
// the settings page takes effect without a restart (and, when the agent was
// degraded because that provider's key was missing, recovers it). Runs async so
// the HTTP handler doesn't block on the reachability probe. No-op for non-active
// providers (use /model use to switch to a freshly-keyed one).
func (k dashKeys) maybeReloadProvider(credID string) {
	if k.mc == nil {
		return
	}
	lc, err := loadLaunchConfig(k.cfgDir)
	if err != nil || lc.Provider != credID {
		return
	}
	go func() { _, _ = k.mc.ReloadKeys() }()
}

// watchCredentials polls credentials.json and, when it changes, hot-reloads the
// ACTIVE provider's key into the live router — so an EXTERNAL key rotation (a
// secrets manager or a hand edit, not via the settings page) is picked up
// without a restart. Web-search keys are resolved lazily and need no trigger.
// While degraded to echo it SKIPS: an unrelated creds change must not swap echo
// for a possibly-down real provider (degraded recovery is driven by the settings
// page / reconnect monitor / `/model use`). Runs until ctx is cancelled.
func watchCredentials(ctx context.Context, cfgDir string, mc *modelControl, degraded *degradedState, notify func(string)) {
	if mc == nil {
		return
	}
	path := credentialsPath(cfgDir)
	last := credsModTime(path)
	t := time.NewTicker(credsWatchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cur := credsModTime(path)
			if cur == last {
				continue
			}
			last = cur
			if degraded.Degraded() {
				continue // don't auto-swap echo → a possibly-down real provider
			}
			status, err := mc.ReloadKeys()
			if err != nil {
				if notify != nil {
					notify("keys: reload after credentials change failed: " + err.Error())
				}
				continue
			}
			if status != "" && notify != nil {
				notify("keys: credentials.json changed — " + status)
			}
		}
	}
}

// credsModTime returns credentials.json's mod time in unix-nanos, or 0 if absent.
func credsModTime(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.ModTime().UnixNano()
}

// clearProviderStored marks a provider as no-longer-having a stored key so
// resolveSecret stops returning "" for a Stored:true provider whose secret was
// just deleted. Leaves the (now keyless) provider entry so it can be re-keyed.
// No-op when the provider isn't configured or already keyless.
func clearProviderStored(cfgDir, kindID string) error {
	lc, err := loadLaunchConfig(cfgDir)
	if err != nil {
		return err
	}
	pc := lc.Providers[kindID]
	if pc == nil || !pc.Auth.Stored {
		return nil
	}
	pc.Auth.Stored = false
	return lc.save(cfgDir)
}
