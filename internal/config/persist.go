package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"tenant/internal/fsperm"
)

// Path is config.json in cfgDir.
func Path(cfgDir string) string { return filepath.Join(cfgDir, "config.json") }

// Load reads + migrates config.json. Missing file → empty config (first run).
// Corrupt → error so the caller warns instead of wiping setup.
func Load(cfgDir string) (*Launch, error) {
	lc := &Launch{}
	b, err := os.ReadFile(Path(cfgDir))
	if os.IsNotExist(err) {
		return lc, nil
	}
	if err != nil {
		return lc, err
	}
	if err := json.Unmarshal(b, lc); err != nil {
		return &Launch{}, fmt.Errorf("config.json corrupt: %w", err)
	}
	lc.Migrate()
	lc.SanitizeEndpoints()
	if _, err := ResolveRoute(lc); err != nil {
		return &Launch{}, err
	}
	return lc, nil
}

// SanitizeEndpoints is the load-time guard against a pre-validation
// config containing a non-URL endpoint (most commonly: an API key
// pasted into the endpoint field via the bug `/model add zai <key>`
// that AddModel's URL gate now blocks).
//
// Policy: if a provider's saved endpoint isn't an http(s) URL, we
// REPLACE it with the catalog default for that kind (if known) and
// leave the auth alone. If no catalog default exists, the endpoint
// is BLANKED (so any later display shows "(unset)" via
// SafeDisplayEndpoint, never the secret). The provider stays in the
// map so the operator can fix it with `/model add-cloud <kind> <key>`
// or by editing config.json.
//
// Mutation is in-memory only — we don't auto-save. Persisting the
// sanitized state happens on the next intentional save (e.g. the
// next `/model use`).
func (lc *Launch) SanitizeEndpoints() {
	for _, p := range lc.Providers {
		if p == nil {
			continue
		}
		ep := strings.TrimSpace(p.Endpoint)
		if ep == "" || LooksLikeURL(ep) {
			continue
		}
		// Non-URL slipped in. Replace with catalog default if we have
		// one; else blank. NEVER preserve the bad value — that's how it
		// leaked into the model list / prompt context.
		if pk, ok := ProviderKinds[p.Kind]; ok && pk.DefaultEndpoint != "" {
			p.Endpoint = pk.DefaultEndpoint
		} else {
			p.Endpoint = ""
		}
	}
}

// Migrate folds v1 flat fields into the v2 providers model, in-memory. The
// next Save persists the upgraded shape and drops the legacy fields.
func (lc *Launch) Migrate() {
	if lc.Providers == nil && (lc.Backend != "" || lc.VLLMEndpoint != "") {
		kind := lc.Backend
		if kind == "" {
			kind = "vllm"
		}
		lc.Providers = map[string]*Provider{
			kind: {
				Kind:     kind,
				Endpoint: lc.VLLMEndpoint,
				Model:    lc.VLLMModel,
				ToolFmt:  lc.VLLMToolFmt,
			},
		}
		lc.Provider = kind
		if lc.EmbedEndpoint != "" || lc.EmbedModel != "" {
			lc.Embed = &Provider{
				Kind:     "ollama",
				Endpoint: lc.EmbedEndpoint,
				Model:    lc.EmbedModel,
				EmbedDim: lc.EmbedDim,
			}
		}
		if lc.SSEAddr != "" {
			lc.Gateway = Gateway{Mode: "sse", SSEAddr: lc.SSEAddr}
		}
	}
	// Drop legacy fields so they aren't re-serialized.
	lc.Backend, lc.VLLMEndpoint, lc.VLLMModel, lc.VLLMToolFmt = "", "", "", ""
	lc.EmbedEndpoint, lc.EmbedModel, lc.EmbedDim, lc.SSEAddr = "", "", 0, ""
}

// Save writes config.json atomically (temp + rename).
func (lc *Launch) Save(cfgDir string) error {
	if _, err := ResolveRoute(lc); err != nil {
		return err
	}
	lc.SchemaVersion = CurrentSchemaVersion
	b, err := json.MarshalIndent(lc, "", "  ")
	if err != nil {
		return err
	}
	if err := fsperm.MkdirAll(cfgDir); err != nil {
		return err
	}
	return AtomicWrite(Path(cfgDir), b)
}

// Active returns the selected generation provider's config (or nil).
func (lc *Launch) Active() *Provider {
	if lc == nil || lc.Provider == "" || lc.Providers == nil {
		return nil
	}
	return lc.Providers[lc.Provider]
}

// Persist is the ONLY safe write path for LONG-LIVED components
// (relay/dashboard/cron/tailscale/imessage/peer managers) that hold the
// boot-time *Launch. It applies fn to a FRESH copy loaded from disk and
// saves that, then applies fn to mem so live readers stay coherent. Saving
// the boot-time pointer directly is the lost-update bug that ate skill
// config: `/skill configure gsuite` saved fresh state to disk, then a
// stale-copy persist (a relay toggle, a dashboard flip) wrote the boot-time
// Skills map back over it — the skill silently vanished on the next launch.
// A corrupt/unreadable config falls back to saving mem (never save the empty
// struct Load returns alongside an error — that would wipe the operator's
// config wholesale).
func Persist(cfgDir string, mem *Launch, fn func(*Launch)) error {
	if mem != nil {
		fn(mem)
	}
	fresh, err := Load(cfgDir)
	if err != nil {
		if mem != nil {
			return mem.Save(cfgDir)
		}
		return err
	}
	fn(fresh)
	return fresh.Save(cfgDir)
}

// AtomicWrite writes via temp + rename so a crash can't truncate the file. The
// temp file is created with a UNIQUE name (os.CreateTemp) in the destination
// directory so concurrent writers — now possible since keys can be rotated live
// from the settings page, the credentials watcher, and /model at once — never
// share one ".tmp" path and clobber each other mid-write. The rename is atomic,
// so a concurrent reader always sees either the whole old or whole new file.
func AtomicWrite(path string, data []byte) error {
	dir, base := filepath.Dir(path), filepath.Base(path)
	f, err := os.CreateTemp(dir, base+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// Best-effort cleanup if we bail before the rename.
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := fsperm.RestrictFile(tmp); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
