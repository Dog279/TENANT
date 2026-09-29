package main

// modelctl_server.go adds a model from a server on the network: the Model
// page's "Add a model from a server on your network". The operator gives an
// address and, if they like, a model, a name and an API key; the server is
// checked first (modeldiscover.go), so what's saved is its real kind, a
// model it serves and the format its tool calls come back in.

import (
	"context"
	"errors"
	"fmt"
	neturl "net/url"
	"regexp"
	"strings"

	"tenant/internal/model"
	"tenant/internal/model/toolfmt"
)

// serverRequest is the form: only Address is required.
type serverRequest struct{ Address, Model, Name, APIKey string }

// serverAdded is what was saved, and what the check found.
type serverAdded struct {
	Name  string
	Saved string // the tool_format saved: toolfmt.Auto, or a format
	*serverFound
}

// providerNameRe is a name the operator may give a model: what config.json,
// the credential store and every picker key it by.
var providerNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,39}$`)

// addServer checks the server and saves it as a provider. The check runs
// without mc.mu (it can take seconds); the save takes it and looks at the
// names again. A model left blank is saved blank (found at each start, so
// the provider follows a server that swaps models), and then so is its tool
// format, unless the test call showed a text format the model's name
// wouldn't have picked.
func (mc *modelControl) addServer(ctx context.Context, req serverRequest, surface string) (*serverAdded, error) {
	added, err := mc.checkAndSaveServer(ctx, req, surface)
	if err != nil {
		var pe *plainError
		if errors.As(err, &pe) {
			endpoint, _ := normalizeServerAddress(req.Address) // "" for an address that isn't one
			model.ServerNotAdded.Log(ctx, "endpoint", endpoint, "problem", pe.plain, "err", err)
		}
		return nil, err
	}
	model.ServerAdded.Log(ctx, "provider", added.Name, "kind", added.Kind, "endpoint", added.Endpoint,
		"model", added.Model, "auto", added.Auto, "tools", string(added.Tools), "format", added.ToolFmt)
	if added.Tools == toolsNone || added.Tools == toolsUnchecked {
		model.ServerToolsUnconfirmed.Log(ctx, "provider", added.Name, "model", added.Model,
			"tools", string(added.Tools), "format", added.ToolFmt, "detail", added.Why)
	}
	return added, nil
}

func (mc *modelControl) checkAndSaveServer(ctx context.Context, req serverRequest, surface string) (*serverAdded, error) {
	endpoint, err := normalizeServerAddress(req.Address)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(req.Name)
	if name != "" {
		if err := checkServerName(name); err != nil {
			return nil, err
		}
		// Before the slow check: a taken name fails fast.
		if lc, err := loadLaunchConfig(mc.cfgDir); err == nil && lc.Providers[name] != nil {
			return nil, nameTaken(name)
		}
	}
	key := strings.TrimSpace(req.APIKey)
	found, err := discoverServer(ctx, endpoint, key, req.Model)
	if err != nil {
		return nil, err
	}
	saved := found.ToolFmt
	if found.Auto && saved == firstNonEmpty(toolfmt.ForModel(found.Model), "openai") {
		saved = toolfmt.Auto
	}

	mc.mu.Lock()
	defer mc.mu.Unlock()
	lc, err := loadLaunchConfig(mc.cfgDir)
	if err != nil {
		return nil, err
	}
	if lc.Providers == nil {
		lc.Providers = map[string]*providerConfig{}
	}
	switch {
	case name == "":
		name = freeServerName(lc, suggestServerName(found.Model, endpoint))
	case lc.Providers[name] != nil:
		return nil, nameTaken(name)
	}
	pc := &providerConfig{Kind: found.Kind, Endpoint: endpoint, ToolFmt: saved}
	if !found.Auto {
		pc.Model = found.Model
	}
	// The key first: a provider saved as holding a key it doesn't hold
	// would send nothing and fail. A key once stored under this name that
	// nothing uses any more is dropped instead, so a keyless server never
	// inherits one.
	if key != "" {
		creds, err := loadCredentials(mc.cfgDir)
		if err != nil {
			return nil, fmt.Errorf("read the credential store: %w", err)
		}
		creds.Set(name, key)
		if err := saveCredentials(mc.cfgDir, creds, surface); err != nil {
			return nil, fmt.Errorf("store the key: %w", err)
		}
		pc.Auth = authCfg{Mode: "apikey", Stored: true}
	} else if err := forgetProviderSecret(mc.cfgDir, name, surface); err != nil {
		return nil, err
	}
	lc.Providers[name] = pc
	if lc.Embed == nil {
		lc.Embed = &providerConfig{Kind: "ollama", Endpoint: defaultEmbedEndpoint, Model: defaultEmbedModel, EmbedDim: defaultEmbedDim}
	}
	if err := lc.Save(mc.cfgDir); err != nil {
		if key != "" {
			_ = forgetProviderSecret(mc.cfgDir, name, surface)
		}
		return nil, err
	}
	return &serverAdded{Name: name, Saved: saved, serverFound: found}, nil
}

// checkServerName refuses a name config.json can't key a model by, and the
// catalog's own kind names: those belong to the models Add a cloud model
// and the terminal's commands file under the kind's name, which would
// overwrite this one.
func checkServerName(name string) error {
	if !providerNameRe.MatchString(name) {
		return plainf(nil, "A name can have letters, numbers, dots, dashes and underscores, up to 40 of them.")
	}
	for id := range providerKinds {
		if strings.EqualFold(id, name) {
			return plainf(nil, "%s is the name of a kind of model Tenant knows. Choose another name.", name)
		}
	}
	return nil
}

func nameTaken(name string) error {
	return plainf(nil, "You already have a model named %s. Choose another name, or remove that one first.", name)
}

// suggestServerName names a server after its model ("qwen3.8-27b" for
// "Qwen/Qwen3.8-27B", "gemma-3-27b-it-gguf-q4_k_m" for
// "hf.co/unsloth/gemma-3-27b-it-GGUF:Q4_K_M"), else after its host and port.
func suggestServerName(modelID, endpoint string) string {
	id := modelID
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	if n := cleanServerName(id); n != "" {
		return n
	}
	if u, err := neturl.Parse(endpoint); err == nil {
		if n := cleanServerName(u.Hostname() + "-" + u.Port()); n != "" {
			return n
		}
	}
	return "server"
}

// cleanServerName keeps a name to lower-case letters, digits, dots, dashes
// and underscores, 32 characters at most.
func cleanServerName(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	n := b.String()
	if len(n) > 32 {
		n = n[:32]
	}
	return strings.Trim(n, "-._")
}

// freeServerName is base, or base-2, base-3… when a model already has it or
// it is a catalog kind's name.
func freeServerName(lc *launchConfig, base string) string {
	taken := func(n string) bool {
		if lc.Providers[n] != nil {
			return true
		}
		_, kind := providerKinds[n]
		return kind
	}
	if !taken(base) {
		return base
	}
	for i := 2; ; i++ {
		if n := fmt.Sprintf("%s-%d", base, i); !taken(n) {
			return n
		}
	}
}
