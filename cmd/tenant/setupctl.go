package main

// setupctl.go adapts the launch config + modelControl to tui.SetupControl, so
// the in-TUI `/setup` menu (TEN-150) can read + edit the same settings the
// external `tenant setup` wizard writes — applied live where possible (provider/
// model/endpoint/tool-format/key via the TEN-147 reload; embeddings/gateway need
// a restart, surfaced in the status).

import (
	"fmt"
	"strings"

	"tenant/internal/tui"
)

type setupControl struct {
	cfgDir string
	mc     *modelControl
}

var _ tui.SetupControl = setupControl{}

func (s setupControl) Snapshot() tui.SetupView {
	v := tui.SetupView{}
	lc, err := loadLaunchConfig(s.cfgDir)
	if err != nil {
		return v
	}
	active := lc.Provider
	v.Provider = active
	pk := providerKinds[active]
	v.ProviderLabel = pk.Label
	v.NeedsKey = pk.NeedsKey
	v.NeedsEndpoint = pk.Backend != "echo" && pk.Backend != ""
	v.IsVLLM = pk.Backend == "vllm"
	if pc := lc.Providers[active]; pc != nil {
		v.Model = pc.Model
		v.Endpoint = pc.Endpoint
		v.ToolFormat = pc.ToolFmt
		v.KeySource = authSummary(pc.Auth)
		v.KeySet = resolveSecret(s.cfgDir, active, pc.Auth) != ""
	}
	if lc.Embed != nil {
		v.EmbedEndpoint = lc.Embed.Endpoint
		v.EmbedModel = lc.Embed.Model
	}
	v.Gateway = lc.Gateway.Mode
	if v.Gateway == "" {
		v.Gateway = "local"
	}
	return v
}

func (s setupControl) ProviderOptions() []tui.SetupOption {
	out := make([]tui.SetupOption, 0, len(providerOrder))
	for _, id := range providerOrder {
		out = append(out, tui.SetupOption{Label: id + " — " + providerKinds[id].Label, Value: id})
	}
	return out
}

func (s setupControl) ToolFormatOptions() []string {
	return []string{"qwen", "gemma", "llama", "mistral", "openai"}
}

// SetProvider switches the active provider (persist + probe + live swap).
func (s setupControl) SetProvider(kind string) (string, error) {
	if s.mc == nil {
		return "", fmt.Errorf("model control unavailable")
	}
	status, _, err := s.mc.UseModel(kind, "")
	return status, err
}

// SetModel sets the active provider's model (blank = auto-detect at launch) and
// hot-swaps the router.
func (s setupControl) SetModel(model string) (string, error) {
	lc, pc, err := s.activeProvider()
	if err != nil {
		return "", err
	}
	pc.Model = strings.TrimSpace(model)
	if err := lc.save(s.cfgDir); err != nil {
		return "", err
	}
	return "model updated — " + s.reload(), nil
}

func (s setupControl) SetEndpoint(url string) (string, error) {
	lc, pc, err := s.activeProvider()
	if err != nil {
		return "", err
	}
	pc.Endpoint = strings.TrimRight(strings.TrimSpace(url), "/")
	if err := lc.save(s.cfgDir); err != nil {
		return "", err
	}
	return "endpoint set — " + s.reload(), nil
}

func (s setupControl) SetToolFormat(format string) (string, error) {
	lc, pc, err := s.activeProvider()
	if err != nil {
		return "", err
	}
	pc.ToolFmt = strings.TrimSpace(format)
	if err := lc.save(s.cfgDir); err != nil {
		return "", err
	}
	return "tool format set to " + pc.ToolFmt + " — " + s.reload(), nil
}

// SetKey stores the active provider's API key (0600) and makes it the active
// auth source (clears any env reference so the pasted key wins), then reloads.
func (s setupControl) SetKey(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("empty key")
	}
	lc, pc, err := s.activeProvider()
	if err != nil {
		return "", err
	}
	creds, err := loadCredentials(s.cfgDir)
	if err != nil {
		return "", err
	}
	creds.set(lc.Provider, value)
	if err := creds.save(s.cfgDir); err != nil {
		return "", err
	}
	pc.Auth = authCfg{Mode: "apikey", Stored: true}
	if err := lc.save(s.cfgDir); err != nil {
		return "", err
	}
	return "API key stored (0600) — " + s.reload(), nil
}

func (s setupControl) SetEmbeddings(endpoint, model string) (string, error) {
	lc, err := loadLaunchConfig(s.cfgDir)
	if err != nil {
		return "", err
	}
	emb := lc.Embed
	if emb == nil {
		emb = &providerConfig{Kind: "ollama"}
	}
	if e := strings.TrimRight(strings.TrimSpace(endpoint), "/"); e != "" {
		emb.Endpoint = e
	}
	if m := strings.TrimSpace(model); m != "" {
		emb.Model = m
	}
	if emb.EmbedDim == 0 {
		emb.EmbedDim = defaultEmbedDim
	}
	lc.Embed = emb
	if err := lc.save(s.cfgDir); err != nil {
		return "", err
	}
	return "embeddings set (applies on restart)", nil
}

func (s setupControl) SetGateway(mode, addr string) (string, error) {
	lc, err := loadLaunchConfig(s.cfgDir)
	if err != nil {
		return "", err
	}
	gw := gatewayConfig{Mode: mode}
	if mode == "sse" || mode == "both" {
		gw.SSEAddr = firstNonEmpty(strings.TrimSpace(addr), lc.Gateway.SSEAddr, "127.0.0.1:8765")
	}
	lc.Gateway = gw
	if err := lc.save(s.cfgDir); err != nil {
		return "", err
	}
	return "mcp gateway set to " + mode + " (applies on mcp-memory restart)", nil
}

// activeProvider loads the config + the active provider's entry.
func (s setupControl) activeProvider() (*launchConfig, *providerConfig, error) {
	lc, err := loadLaunchConfig(s.cfgDir)
	if err != nil {
		return nil, nil, err
	}
	if lc.Provider == "" {
		return nil, nil, fmt.Errorf("no active provider — pick one first")
	}
	pc := lc.Providers[lc.Provider]
	if pc == nil {
		return nil, nil, fmt.Errorf("active provider %q has no config", lc.Provider)
	}
	return lc, pc, nil
}

// reload re-resolves the active provider into the live router and returns a short
// status (empty when no model control is wired).
func (s setupControl) reload() string {
	if s.mc == nil {
		return "saved"
	}
	if st, err := s.mc.ReloadKeys(); err == nil && st != "" {
		return st
	}
	return "saved (applied live)"
}
