package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
)

// cmdSetup is the onboarding wizard — the one place an operator configures
// Tenant so everyday launch is just `tenant tui`. Modeled on OpenClaw's
// `onboard`: existing-config detection, provider + auth, gateway, embeddings,
// and a skills step. Interactive in a terminal; flag/config-driven otherwise.
//
//	tenant setup                       interactive wizard
//	tenant setup --show                print current config and exit
//	tenant setup --reset               start fresh, ignoring existing config
//	tenant setup --provider openai --api-key sk-... --gateway 127.0.0.1:8765
func cmdSetup(ctx context.Context, args []string) error {
	return runSetup(ctx, args, bufio.NewReader(os.Stdin))
}

// runSetup is cmdSetup with an injectable stdin reader, so the first-launch
// auto-offer can hand off the SAME reader (two bufio.Readers on os.Stdin would
// race over buffered input).
func runSetup(ctx context.Context, args []string, in *bufio.Reader) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	c := bindCommon(fs)
	provider := fs.String("provider", "", "preselect a model provider ("+strings.Join(providerOrder, "|")+")")
	gateway := fs.String("gateway", "", "serve mcp-memory over HTTP+SSE on this addr (e.g. 127.0.0.1:8765)")
	local := fs.Bool("local", false, "mcp-memory uses local stdio (clears any saved gateway addr)")
	both := fs.Bool("both", false, "mcp-memory serves BOTH stdio and SSE")
	show := fs.Bool("show", false, "print the current configuration and exit")
	reset := fs.Bool("reset", false, "ignore any existing config and start fresh")
	nonInteractive := fs.Bool("non-interactive", false, "don't prompt; persist flags + config + defaults as-is")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Resolve dirs only — we don't want resolve()'s auto-merge to mutate the
	// values we're about to (re)configure.
	c.dataDir = expandPath(c.dataDir)
	c.cfgDir = expandPath(c.cfgDir)
	if err := c.resolveDirs(); err != nil {
		return err
	}

	lc, err := loadLaunchConfig(c.cfgDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v (starting fresh)\n", err)
		lc = &launchConfig{}
	}
	if *reset {
		lc = &launchConfig{}
	}
	if *show {
		printSetupSummary(c.cfgDir, lc)
		return nil
	}
	creds, _ := loadCredentials(c.cfgDir)

	set := c.flagSet()
	interactive := !*nonInteractive && isatty.IsTerminal(os.Stdin.Fd())

	fmt.Fprintln(os.Stderr, "\n== tenant setup ==")
	fmt.Fprintf(os.Stderr, "config:      %s\ncredentials: %s\n\n", launchConfigPath(c.cfgDir), credentialsPath(c.cfgDir))

	// Existing-config: Keep / Modify / Reset (OpenClaw pattern).
	if lc.active() != nil && interactive {
		switch strings.ToLower(ask(in, "Existing config found. [k]eep, [m]odify, [r]eset", "modify")) {
		case "k", "keep":
			printSetupSummary(c.cfgDir, lc)
			return nil
		case "r", "reset":
			lc = &launchConfig{}
			creds = &credentials{Secrets: map[string]string{}}
		}
	}

	if lc.Providers == nil {
		lc.Providers = map[string]*providerConfig{}
	}

	// --- 1. provider + auth ---
	kindID := firstNonEmpty(valIf(set["provider"], *provider), lc.Provider, "vllm")
	if interactive {
		kindID = askProvider(in, kindID)
	}
	pk, ok := providerKinds[kindID]
	if !ok {
		return fmt.Errorf("unknown provider %q (choose one of %s)", kindID, strings.Join(providerOrder, ", "))
	}
	pc := lc.Providers[kindID]
	if pc == nil {
		pc = &providerConfig{Kind: kindID}
	}
	if !pk.Wired {
		fmt.Fprintf(os.Stderr, "  note: %s — config will be saved, but its backend is not implemented yet.\n", pk.Label)
	}

	if pk.Backend != "echo" { // an endpoint-based provider (vllm-compat or anthropic)
		ep := firstNonEmpty(valIf(set["vllm-endpoint"], c.vllmEndpoint), pc.Endpoint, pk.DefaultEndpoint)
		if interactive {
			ep = ask(in, "Endpoint (base URL)", ep)
		}
		ep = strings.TrimRight(strings.TrimSpace(ep), "/")
		if ep == "" {
			return fmt.Errorf("%s needs an endpoint", pk.Label)
		}
		pc.Endpoint = ep

		// auth
		apiKey := valIf(set["api-key"], c.genAPIKey)
		if pk.NeedsKey {
			pc.Auth = configureAuth(in, interactive, kindID, pk, pc.Auth, creds, apiKey)
		}

		// model — auto-detect for local single-model servers, else default.
		key := resolveSecret(c.cfgDir, kindID, pc.Auth)
		if apiKey != "" {
			key = apiKey
		}
		model := firstNonEmpty(valIf(set["vllm-model"], c.vllmModel), pc.Model, pk.DefaultModel)
		if pk.Local {
			if detected := probeModels(ctx, ep, key); len(detected) > 0 {
				fmt.Fprintf(os.Stderr, "  detected model(s): %s\n", strings.Join(detected, ", "))
				if model == "" {
					model = detected[0]
				}
			} else {
				fmt.Fprintf(os.Stderr, "  (could not reach %s/v1/models — model will auto-detect at launch)\n", ep)
			}
		}
		if interactive {
			model = ask(in, "Model (blank = auto-detect at launch)", model)
		}
		pc.Model = strings.TrimSpace(model)

		// Tool format is an OpenAI-compatible concern; Anthropic returns
		// structured tool_use blocks, so skip it for that backend.
		if pk.Backend == "vllm" {
			tf := firstNonEmpty(valIf(set["vllm-tool-format"], c.vllmToolFmt), pc.ToolFmt, pk.DefaultToolFmt, defaultVLLMToolFmt)
			if interactive {
				tf = ask(in, "Tool format (qwen|gemma|llama|mistral|openai)", tf)
			}
			pc.ToolFmt = strings.TrimSpace(tf)
		}
	}

	lc.Providers[kindID] = pc
	lc.Provider = kindID

	// --- 2. embeddings (skip for echo) ---
	if kindID != "echo" {
		emb := lc.Embed
		if emb == nil {
			emb = &providerConfig{Kind: "ollama"}
		}
		embEP := firstNonEmpty(valIf(set["embed-endpoint"], c.embedEndpoint), emb.Endpoint, defaultEmbedEndpoint)
		embModel := firstNonEmpty(valIf(set["embed-model"], c.embedModel), emb.Model, defaultEmbedModel)
		if interactive {
			embEP = ask(in, "Embedding endpoint", embEP)
			embModel = ask(in, "Embedding model", embModel)
		}
		emb.Endpoint = strings.TrimRight(strings.TrimSpace(embEP), "/")
		emb.Model = strings.TrimSpace(embModel)
		if emb.EmbedDim == 0 {
			emb.EmbedDim = defaultEmbedDim
		}
		lc.Embed = emb

		pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		okEmb := reachable(pctx, emb.Endpoint+"/v1/models")
		cancel()
		if okEmb {
			fmt.Fprintf(os.Stderr, "  embeddings OK: %s @ %s\n", emb.Model, emb.Endpoint)
		} else {
			fmt.Fprintf(os.Stderr, "\n"+embedSetupHint+"\n\n", emb.Endpoint, emb.Model)
		}
	}

	// --- 3. gateway (local | sse | both) ---
	lc.Gateway = configureGateway(in, interactive, lc.Gateway, set, *gateway, *local, *both)

	// --- 4. skills ---
	if interactive {
		lc.Skills = configureSkills(in, lc.Skills, creds, c.cfgDir)
	}

	// --- save ---
	if err := creds.save(c.cfgDir); err != nil {
		return fmt.Errorf("save credentials: %w", err)
	}
	if err := lc.save(c.cfgDir); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	printSetupSummary(c.cfgDir, lc)
	return nil
}

// maybeOfferSetup runs on the first interactive launch of tui/chat when no
// config exists: it offers to run the wizard right then. Non-interactive
// launches just print a one-line hint and proceed with defaults. Returns true
// if setup ran (so the caller should reload config).
func maybeOfferSetup(ctx context.Context, c *commonFlags) (bool, error) {
	if err := c.resolveDirs(); err != nil {
		return false, err
	}
	if _, err := os.Stat(launchConfigPath(c.cfgDir)); err == nil {
		return false, nil // already configured
	}
	if !isatty.IsTerminal(os.Stdin.Fd()) {
		fmt.Fprintln(os.Stderr, "tip: run `tenant setup` to configure providers, gateway, and skills.")
		return false, nil
	}
	in := bufio.NewReader(os.Stdin)
	if !yes(ask(in, "\nNo configuration found. Run setup now? [Y/n]", "y")) {
		return false, nil
	}
	if err := runSetup(ctx, []string{"--config", c.cfgDir, "--data", c.dataDir}, in); err != nil {
		return false, err
	}
	return true, nil
}

// configureAuth captures a provider's API key as either an env-var reference
// or a stored secret (OpenClaw's keyRef vs stored distinction).
func configureAuth(in *bufio.Reader, interactive bool, providerID string, pk providerKind, cur authCfg, creds *credentials, flagKey string) authCfg {
	// A key passed via --api-key is stored immediately.
	if flagKey != "" {
		creds.set(providerID, flagKey)
		return authCfg{Mode: "apikey", Stored: true}
	}
	if !interactive {
		// Keep whatever was configured; default to an env reference if none.
		if cur.KeyEnv == "" && !cur.Stored {
			return authCfg{Mode: "apikey", KeyEnv: pk.KeyEnv}
		}
		return cur
	}
	def := "env"
	if cur.Stored {
		def = "paste"
	}
	switch strings.ToLower(ask(in, fmt.Sprintf("%s auth: [env] reference an env var, or [paste] store the key", pk.Label), def)) {
	case "paste", "p", "key":
		k := ask(in, "Paste API key (stored in credentials.json, 0600)", "")
		if strings.TrimSpace(k) != "" {
			creds.set(providerID, strings.TrimSpace(k))
			return authCfg{Mode: "apikey", Stored: true}
		}
		fallthrough
	default:
		env := ask(in, "Env var name to read the key from", firstNonEmpty(cur.KeyEnv, pk.KeyEnv))
		return authCfg{Mode: "apikey", KeyEnv: strings.TrimSpace(env)}
	}
}

// configureGateway resolves local vs SSE vs both.
func configureGateway(in *bufio.Reader, interactive bool, cur gatewayConfig, set map[string]bool, gwFlag string, localFlag, bothFlag bool) gatewayConfig {
	switch {
	case bothFlag:
		return gatewayConfig{Mode: "both", SSEAddr: firstNonEmpty(gwFlag, cur.SSEAddr, "127.0.0.1:8765")}
	case localFlag:
		return gatewayConfig{Mode: "local"}
	case set["gateway"] || gwFlag != "":
		return gatewayConfig{Mode: "sse", SSEAddr: gwFlag}
	case !interactive:
		if cur.Mode == "" {
			return gatewayConfig{Mode: "local"}
		}
		return cur
	}
	def := cur.Mode
	if def == "" {
		def = "local"
	}
	mode := strings.ToLower(ask(in, "mcp-memory transport: [local] stdio, [sse] HTTP, or [both]", def))
	switch mode {
	case "sse", "both":
		addr := ask(in, "SSE bind address", firstNonEmpty(cur.SSEAddr, "127.0.0.1:8765"))
		return gatewayConfig{Mode: mode, SSEAddr: strings.TrimSpace(addr)}
	default:
		return gatewayConfig{Mode: "local"}
	}
}

// probeModels best-effort lists served models within a short timeout.
func probeModels(ctx context.Context, endpoint, apiKey string) []string {
	pctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	ids, err := fetchModelIDs(pctx, endpoint, apiKey)
	if err != nil {
		return nil
	}
	return ids
}

// askProvider prints the provider menu and returns the chosen kind id.
func askProvider(in *bufio.Reader, cur string) string {
	fmt.Fprintln(os.Stderr, "\nModel providers:")
	for i, id := range providerOrder {
		marker := "  "
		if id == cur {
			marker = "* "
		}
		fmt.Fprintf(os.Stderr, "  %s%d) %-26s %s\n", marker, i+1, id, providerKinds[id].Label)
	}
	ans := strings.TrimSpace(ask(in, "Choose a provider (number or id)", cur))
	// numeric?
	for i, id := range providerOrder {
		if ans == fmt.Sprint(i+1) || strings.EqualFold(ans, id) {
			return id
		}
	}
	return ans
}

// ask prints a prompt with the current value in brackets and returns the
// trimmed reply, or def if the operator hits Enter (or stdin is EOF).
func ask(in *bufio.Reader, prompt, def string) string {
	if def != "" {
		fmt.Fprintf(os.Stderr, "%s [%s]: ", prompt, def)
	} else {
		fmt.Fprintf(os.Stderr, "%s: ", prompt)
	}
	line, err := in.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" || err != nil && line == "" {
		return def
	}
	return line
}

// valIf returns v when cond is true, else "". Gives an explicitly-set flag
// priority over the persisted config value.
func valIf(cond bool, v string) string {
	if cond {
		return v
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func printSetupSummary(cfgDir string, lc *launchConfig) {
	var b strings.Builder
	fmt.Fprintf(&b, "\nSaved %s\n", launchConfigPath(cfgDir))
	p := lc.active()
	if p == nil {
		fmt.Fprintf(&b, "  (no provider configured)\n")
		fmt.Fprint(os.Stderr, b.String())
		return
	}
	pk := providerKinds[p.Kind]
	fmt.Fprintf(&b, "  provider      %s (%s)\n", p.Kind, pk.Label)
	if pk.Backend != "echo" {
		fmt.Fprintf(&b, "  endpoint      %s\n", orDash(p.Endpoint))
		model := p.Model
		if model == "" {
			model = "(auto-detect at launch)"
		}
		fmt.Fprintf(&b, "  model         %s\n", model)
		if pk.Backend == "vllm" {
			fmt.Fprintf(&b, "  tool format   %s\n", orDash(p.ToolFmt))
		}
		fmt.Fprintf(&b, "  auth          %s\n", authSummary(p.Auth))
	}
	if lc.Embed != nil {
		fmt.Fprintf(&b, "  embeddings    %s @ %s\n", orDash(lc.Embed.Model), orDash(lc.Embed.Endpoint))
	}
	switch lc.Gateway.Mode {
	case "sse":
		fmt.Fprintf(&b, "  mcp transport HTTP+SSE @ %s\n", lc.Gateway.SSEAddr)
	case "both":
		fmt.Fprintf(&b, "  mcp transport stdio + HTTP+SSE @ %s\n", lc.Gateway.SSEAddr)
	default:
		fmt.Fprintf(&b, "  mcp transport local (stdio)\n")
	}
	if n := enabledSkills(lc); len(n) > 0 {
		fmt.Fprintf(&b, "  skills        %s\n", strings.Join(n, ", "))
	}
	fmt.Fprintf(&b, "\nLaunch:\n  tenant tui\n  tenant mcp-memory --tools\n")
	if !pk.Wired {
		fmt.Fprintf(&b, "\nNOTE: provider %q is not wired yet — launch will error until you switch providers.\n", p.Kind)
	}
	fmt.Fprint(os.Stderr, b.String())
}

func authSummary(a authCfg) string {
	switch {
	case a.KeyEnv != "":
		return "env $" + a.KeyEnv
	case a.Stored:
		return "stored (credentials.json)"
	default:
		return "none"
	}
}

func enabledSkills(lc *launchConfig) []string {
	var out []string
	for id, s := range lc.Skills {
		if s != nil && s.Enabled {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}
