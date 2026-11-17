package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
		return &Launch{}, fmt.Errorf("config.json corrupt%s: %w", at(b, err), err)
	}
	lc.Migrate()
	if lc.Gateway.Mode == "both" { // it never served stdio as well: one process serves one transport
		lc.Gateway.Mode = "sse"
	}
	lc.SanitizeEndpoints()
	if _, err := ResolveRoute(lc); err != nil {
		return &Launch{}, err
	}
	return lc, nil
}

// at is where in b a JSON error happened, " at line L, column C", so a hand
// edit gone wrong can be found; "" when err carries no offset.
func at(b []byte, err error) string {
	var off int64
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syn):
		off = syn.Offset
	case errors.As(err, &typ):
		off = typ.Offset
	default:
		return ""
	}
	before := b[:min(int(off), len(b))] // the offset counts the byte at fault
	line := bytes.Count(before, []byte("\n")) + 1
	col := max(len(before)-bytes.LastIndexByte(before, '\n')-1, 1)
	return fmt.Sprintf(" at line %d, column %d", line, col)
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
	lc.migrateJudge()
}

// migrateJudge moves the pre-TEN-516 spellings into their places: judge_loop
// becomes judge (kind clef), improve.judge* become improve.quality_model*.
// When a file carries both the old and the new key, the new one wins. The
// legacy fields are cleared, so the next save writes only the new keys. Load
// migrates on every read and Persist re-loads before every save, so a
// migrated key cannot come back.
func (lc *Launch) migrateJudge() {
	if l := lc.LegacyJudgeLoop; l != nil {
		if lc.Judge == nil {
			lc.Judge = &Judge{Enabled: l.Enabled, Kind: "clef", Endpoint: l.Endpoint, Model: l.Model, Rounds: l.MaxRounds}
		}
		lc.LegacyJudgeLoop = nil
	}
	ic := &lc.Improve
	if ic.LegacyJudge != "" && ic.QualityModel == "" {
		ic.QualityModel, ic.QualityModelKind, ic.QualityModelEndpoint, ic.QualityModelKeyEnv =
			ic.LegacyJudge, ic.LegacyJudgeKind, ic.LegacyJudgeEndpoint, ic.LegacyJudgeKeyEnv
	}
	ic.LegacyJudge, ic.LegacyJudgeKind, ic.LegacyJudgeEndpoint, ic.LegacyJudgeKeyEnv = "", "", "", ""
}

// Save writes config.json atomically (temp + rename), and files the change
// in the Security log.
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
	return saveAudited(Path(cfgDir), b)
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
//
// The bytes are flushed to the disk before the rename. Without that a power
// cut soon after a save can leave the new name on a file whose contents never
// left the write cache: found on Windows Server 2025 (2026-11-17), where
// config.json came back as a run of zero bytes and serve refused to start.
// A flush that fails leaves the old file in place.
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
	if err := syncFile(f); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := fsperm.RestrictFile(tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}

// syncFile flushes a file's contents to the disk. Tests swap it to see when
// it runs and to make it fail.
var syncFile = func(f *os.File) error { return f.Sync() }

// syncDir flushes a directory, which is what makes a rename in it survive a
// power cut on Linux and macOS. Best effort: the file is whole either way,
// and Windows, whose file system journals the rename itself, does not allow
// a directory to be flushed.
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}
