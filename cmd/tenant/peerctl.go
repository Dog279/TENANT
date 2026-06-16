package main

// peerctl.go is `tenant peer …` (TEN-183): Tenant-to-Tenant federation pairing.
// Mutual-consent invite codes — `tenant peer invite` on one side prints a
// one-time short-lived code, `tenant peer join <code>` on the other stores the
// dial record. peers.json (0600) is the single authoritative store; the
// TEN-184 listener reads it per-request to authenticate peers. This is the
// operator surface over internal/peering.

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"tenant/internal/mcp"
	"tenant/internal/model"
	"tenant/internal/peering"
	"tenant/internal/plugins/mcpremote"
)

// peerTLS loads/mints this instance's self-signed peer cert (TEN-185) unless
// overlay mode is declared (Tailscale/WireGuard → plain HTTP, the overlay is the
// security). Returns (nil, "") in overlay mode. LoadOrMintCert is idempotent, so
// the fingerprint an invite advertises always matches what the listener serves.
func peerTLS(cfgDir string, overlay bool) (*tls.Certificate, string, error) {
	if overlay {
		return nil, "", nil
	}
	cert, fp, err := peering.LoadOrMintCert(cfgDir)
	if err != nil {
		return nil, "", err
	}
	return &cert, fp, nil
}

// peerOverlay reports whether the operator declared overlay transport.
func peerOverlay(c *commonFlags) bool {
	return c.lc != nil && c.lc.Peer.Transport == "overlay"
}

func cmdPeer(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return peerUsage()
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "invite":
		return peerInvite(ctx, rest)
	case "join":
		return peerJoin(rest)
	case "list", "ls":
		return peerList(rest)
	case "show":
		return peerShow(rest)
	case "remove", "rm":
		return peerRemove(rest)
	case "revoke":
		return peerRevoke(rest)
	case "rotate":
		return peerRotate(rest)
	case "share":
		return peerShare(rest)
	case "serve":
		return peerServe(ctx, rest)
	case "query":
		return peerQuery(ctx, rest)
	default:
		return peerUsage()
	}
}

func peerUsage() error {
	return fmt.Errorf(`usage: tenant peer <command>

  invite <name> --url <addr> [--as <self>] [--ttl 1h]
                          mint a one-time invite code for a peer you'll let dial you
  join <code> [--as <local-name>]
                          accept an invite code and store the peer you'll dial
  list                    list paired peers + share policy
  show <name>             show one peer (token masked)
  remove <name>           delete a peer entirely
  revoke <name>           invalidate a peer's token (keep the record)
  rotate <name>           stage a new token (staged-pull; old stays valid until adopted)
  share <name> wiki=on|off memory=on|off [skills=…] [exec=…] [llm=…]
                          edit a peer's share policy (all-deny by default)
  serve [--listen addr] [--wiki-dir dir]
                          run the peer listener headless (the interactive TUI
                          starts it automatically when peer.listen is set;
                          set peer.transport: overlay in config for Tailscale)
  query <name> <wiki|memory> "<query>"
                          query a paired peer's shared knowledge with provenance`)
}

// peerServe runs the federation listener headless until interrupted — a focused
// subset of `tenant serve` (TEN-194) that exposes only the peer listener (no
// agent loop / dashboard). Useful for the two-machine trial and for testing the
// exact path the interactive TUI wires via startPeerListener.
func peerServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("peer serve", flag.ContinueOnError)
	listen := fs.String("listen", "", "address to bind (default: config peer.listen, else 127.0.0.1:9100)")
	wikiDir := fs.String("wiki-dir", "", "expose this markdown wiki to peers via peer_wiki_search")
	yesPair := fs.Bool("yes-pair", false, "AUTO-APPROVE every inbound /peer invite request — TESTING ONLY, no human Approve/Deny")
	c, store, _, err := peerStore(fs, args)
	if err != nil {
		return err
	}
	addr := strings.TrimSpace(*listen)
	if addr == "" && c.lc != nil {
		addr = c.lc.Peer.Listen
	}
	if addr == "" {
		addr = "127.0.0.1:9100"
	}
	// Overlay is single-sourced from config (peer.transport: overlay) so the
	// invite's advertised scheme always matches what the listener serves — a
	// transient serve-only flag could desync the two surfaces.
	ov := peerOverlay(c)
	id, err := ensureInstanceID(c)
	if err != nil {
		return err
	}
	cert, fp, err := peerTLS(c.cfgDir, ov)
	if err != nil {
		return err
	}
	deps, closeDeps, err := buildPeerToolDeps(ctx, c, strings.TrimSpace(*wikiDir))
	if err != nil {
		return err
	}
	defer closeDeps()
	hostName, _ := os.Hostname()
	approver := stdinPairApprover
	if *yesPair {
		fmt.Fprintln(os.Stderr, "⚠ --yes-pair: AUTO-APPROVING all pairing requests (testing only)")
		approver = func(context.Context, string) bool { return true }
	}
	ln, err := peering.NewListener(peering.ListenerConfig{
		Store:        store,
		SelfID:       id,
		SelfName:     hostName,
		SelfVersion:  mcp.LibraryVersion,
		SelfFinger:   fp,
		Overlay:      ov,
		TLSCert:      cert,
		Registrar:    peerKnowledgeRegistrar(deps),
		PairApprover: approver,
		Logger:       func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
	})
	if err != nil {
		return err
	}
	netLn, err := ln.Bind(addr)
	if err != nil {
		return err
	}
	scheme := "https"
	if cert == nil {
		scheme = "http"
	}
	fmt.Fprintf(os.Stderr, "peer listener on %s://%s (instance %s)", scheme, netLn.Addr(), id)
	if fp != "" {
		fmt.Fprintf(os.Stderr, " — cert fp %s…", fp[:16])
	}
	fmt.Fprintln(os.Stderr, " — Ctrl-C to stop")
	return ln.Serve(ctx, netLn)
}

// peerStore resolves cfgDir and opens peers.json. It also separates positional
// arguments from flags so the CLI is order-insensitive: Go's flag package stops
// parsing at the first non-flag token, so `peer invite laptop --url …` would
// otherwise never see --url. We split at the first token beginning with "-",
// parse the flags, and recombine the leading positionals with any trailing
// non-flag args flag.Parse left over — so both `invite NAME --url X` and
// `invite --config D NAME` work. Returns (commonFlags, store, positionals, err).
func peerStore(fs *flag.FlagSet, args []string) (*commonFlags, *peering.Store, []string, error) {
	leading, flags := splitPositional(args)
	c := bindCommon(fs)
	if err := fs.Parse(flags); err != nil {
		return nil, nil, nil, err
	}
	positional := append(leading, fs.Args()...) // tolerate flags-before-name order
	if err := c.resolve(); err != nil {
		return nil, nil, nil, err
	}
	store, err := peering.LoadStore(c.cfgDir)
	if err != nil {
		return nil, nil, nil, err
	}
	return c, store, positional, nil
}

// splitPositional returns the args before the first flag (token starting with
// "-") and the rest. A flag's value (e.g. "http://…" after "--url") never
// starts with "-", so it stays grouped with the flags.
func splitPositional(args []string) (positional, flags []string) {
	for i, a := range args {
		if strings.HasPrefix(a, "-") {
			return args[:i], args[i:]
		}
	}
	return args, nil
}

func peerInvite(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("peer invite", flag.ContinueOnError)
	url := fs.String("url", "", "the address a joiner will dial to reach THIS instance (e.g. http://my-host:9100/ or your Tailscale name)")
	to := fs.String("to", "", "PUSH-invite: pair with the peer at this URL (they Approve/Deny + match the PIN) instead of minting a code")
	as := fs.String("as", "", "how this instance identifies itself to the peer (default: hostname)")
	ttl := fs.Duration("ttl", time.Hour, "how long the invite code stays valid")
	c, store, pos, err := peerStore(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: tenant peer invite <name> (--url <addr> | --to <peer-url>) [--as <self>]")
	}
	peerName := pos[0]
	// TEN-239 push-invite: POST a pairing request to the peer; they Approve/Deny.
	if strings.TrimSpace(*to) != "" {
		return peerPushInvite(ctx, c, store, peerName, strings.TrimSpace(*to))
	}
	if strings.TrimSpace(*url) == "" {
		return fmt.Errorf("--url is required (or use --to <peer-url> to push-invite)")
	}
	selfName := *as
	if selfName == "" {
		if h, err := os.Hostname(); err == nil {
			selfName = h
		} else {
			selfName = "tenant"
		}
	}
	id, err := ensureInstanceID(c)
	if err != nil {
		return err
	}
	// TEN-185: carry the self-signed cert fingerprint so the joiner pins it
	// (TOFU-by-invite). Empty under overlay transport (plain HTTP over the
	// tailnet). LoadOrMintCert is idempotent with what the listener serves.
	_, fp, err := peerTLS(c.cfgDir, peerOverlay(c))
	if err != nil {
		return err
	}
	code, err := store.CreateInvite(selfName, id, *url, fp, *ttl, peerName)
	if err != nil {
		return err
	}
	fmt.Printf("%s\n", code)
	scheme := "TLS (cert pinned via this code)"
	if fp == "" {
		scheme = "plain HTTP over your overlay network"
	}
	fmt.Fprintf(os.Stderr, "\n↑ Give this code to %q. It expires in %s, is single-use, and uses %s.\n"+
		"They run:  tenant peer join <code>\n"+
		"Then set what they may read:  tenant peer share %s wiki=on\n", peerName, ttl.String(), scheme, peerName)
	return nil
}

// peerPushInvite is the TEN-239 inviter side: mint a PIN, display it, POST a
// pairing request to the peer at toURL, and on approval store the peer (dial
// side, cert TOFU-pinned). The PIN must be read to the peer's operator out of
// band so they can match it before approving.
func peerPushInvite(ctx context.Context, c *commonFlags, store *peering.Store, label, toURL string) error {
	pin, err := peering.GeneratePIN()
	if err != nil {
		return err
	}
	id, err := ensureInstanceID(c)
	if err != nil {
		return err
	}
	selfName, _ := os.Hostname()
	if selfName == "" {
		selfName = "tenant"
	}
	fmt.Fprintf(os.Stderr, "Pairing with %q at %s.\n→ Tell their operator this PIN to approve:  %s\n(waiting for them to Approve — up to 3 min)\n",
		label, toURL, peering.FormatPIN(pin))

	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	pr, err := peering.RequestPair(reqCtx, toURL, peering.PairRequest{Name: selfName, InstanceID: id, PIN: pin}, peerOverlay(c))
	if err != nil {
		return err
	}
	if err := store.Put(&peering.Peer{
		Name:        label,
		InstanceID:  pr.InstanceID,
		URL:         toURL,
		Dial:        true,
		Token:       pr.Token,
		Fingerprint: pr.Fingerprint,
		CreatedAt:   nowStamp(),
	}); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "✓ paired with %q (%s). Set what they may read:  /configure peer %s\n", label, pr.Name, label)
	return nil
}

// stdinPairApprover prompts on stderr and reads a y/n from stdin — the headless
// `tenant peer serve` approver. Non-interactive stdin (EOF) denies (fail-closed).
func stdinPairApprover(_ context.Context, prompt string) bool {
	fmt.Fprintf(os.Stderr, "\n=== PAIRING REQUEST ===\n%s\nApprove? [y/N]: ", prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false // fail closed on ANY read error (EOF/partial) — a security approver never approves on a faulty read
	}
	return yes(line)
}

func nowStamp() string { return time.Now().UTC().Format(time.RFC3339) }

func peerJoin(args []string) error {
	fs := flag.NewFlagSet("peer join", flag.ContinueOnError)
	as := fs.String("as", "", "local name to file this peer under (default: the inviter's self-name)")
	_, store, pos, err := peerStore(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: tenant peer join <code> [--as <local-name>]")
	}
	p, err := store.AcceptInvite(pos[0], *as)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "✓ paired with %q (%s) at %s\n", p.Name, p.InstanceID, p.URL)
	fmt.Fprintf(os.Stderr, "  You can now reach their shared knowledge once they enable a share policy on their side.\n")
	return nil
}

func peerList(args []string) error {
	fs := flag.NewFlagSet("peer list", flag.ContinueOnError)
	_, store, _, err := peerStore(fs, args)
	if err != nil {
		return err
	}
	peers := store.List()
	if len(peers) == 0 {
		fmt.Fprintln(os.Stderr, "no peers paired yet — `tenant peer invite <name> --url <addr>` to start")
		return nil
	}
	fmt.Printf("%-16s %-6s %-26s %-22s %s\n", "NAME", "ROLE", "URL", "TOKEN", "SHARE")
	for _, p := range peers {
		role := "accept"
		if p.Dial {
			role = "dial"
		}
		fmt.Printf("%-16s %-6s %-26s %-22s %s\n", p.Name, role, dash(p.URL), tokenState(p), shareSummary(p.Share))
	}
	return nil
}

func peerShow(args []string) error {
	fs := flag.NewFlagSet("peer show", flag.ContinueOnError)
	_, store, pos, err := peerStore(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: tenant peer show <name>")
	}
	p, ok := store.Get(pos[0])
	if !ok {
		return fmt.Errorf("no peer named %q", pos[0])
	}
	role := "accept (they dial us)"
	if p.Dial {
		role = "dial (we dial them)"
	}
	fmt.Printf("name:        %s\n", p.Name)
	fmt.Printf("instance_id: %s\n", p.InstanceID)
	fmt.Printf("role:        %s\n", role)
	fmt.Printf("url:         %s\n", dash(p.URL))
	fmt.Printf("token:       %s\n", tokenState(p))
	fmt.Printf("fingerprint: %s\n", dash(p.Fingerprint))
	fmt.Printf("share:       %s\n", shareSummary(p.Share))
	if p.InviteExpiry != 0 {
		fmt.Printf("invite:      unused, expires %s\n", time.Unix(p.InviteExpiry, 0).Format(time.RFC3339))
	}
	return nil
}

func peerRemove(args []string) error {
	fs := flag.NewFlagSet("peer remove", flag.ContinueOnError)
	_, store, pos, err := peerStore(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: tenant peer remove <name>")
	}
	if _, ok := store.Get(pos[0]); !ok {
		return fmt.Errorf("no peer named %q", pos[0])
	}
	if err := store.Remove(pos[0]); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "✓ removed peer %q\n", pos[0])
	return nil
}

func peerRevoke(args []string) error {
	fs := flag.NewFlagSet("peer revoke", flag.ContinueOnError)
	_, store, pos, err := peerStore(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: tenant peer revoke <name>")
	}
	ok, err := store.Revoke(pos[0])
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no peer named %q", pos[0])
	}
	fmt.Fprintf(os.Stderr, "✓ revoked %q — its next request will be rejected. (record kept; `tenant peer remove %s` to delete)\n", pos[0], pos[0])
	return nil
}

func peerRotate(args []string) error {
	fs := flag.NewFlagSet("peer rotate", flag.ContinueOnError)
	_, store, pos, err := peerStore(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: tenant peer rotate <name>")
	}
	secret, err := store.Rotate(pos[0])
	if err != nil {
		return err
	}
	fmt.Printf("%s\n", secret)
	fmt.Fprintf(os.Stderr, "\n↑ New token for %q (staged). The OLD token stays valid until %q presents this one,\n"+
		"so there's no lockout window. Hand it over the authenticated channel.\n", pos[0], pos[0])
	return nil
}

func peerShare(args []string) error {
	fs := flag.NewFlagSet("peer share", flag.ContinueOnError)
	_, store, pos, err := peerStore(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return fmt.Errorf("usage: tenant peer share <name> wiki=on|off memory=on|off [skills=…] [exec=…] [llm=…]")
	}
	name := pos[0]
	if _, ok := store.Get(name); !ok {
		return fmt.Errorf("no peer named %q", name)
	}
	for _, kv := range pos[1:] {
		key, val, found := strings.Cut(kv, "=")
		if !found {
			return fmt.Errorf("expected key=value, got %q", kv)
		}
		on, err := parseOnOff(val)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		if err := store.SetShare(name, key, on); err != nil {
			return err
		}
	}
	p, _ := store.Get(name)
	fmt.Fprintf(os.Stderr, "✓ %s share policy: %s\n", name, shareSummary(p.Share))
	return nil
}

// --- helpers --------------------------------------------------------------

func parseOnOff(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "true", "yes", "1":
		return true, nil
	case "off", "false", "no", "0":
		return false, nil
	}
	return false, fmt.Errorf("want on|off, got %q", s)
}

func shareSummary(p peering.SharePolicy) string {
	on := []string{}
	for _, f := range []struct {
		k string
		v bool
	}{{"wiki", p.Wiki}, {"memory", p.Memory}, {"skills", p.Skills}, {"exec", p.Exec}, {"llm", p.LLM}} {
		if f.v {
			on = append(on, f.k)
		}
	}
	if len(on) == 0 {
		return "(all-deny)"
	}
	return strings.Join(on, ",")
}

// tokenState shows whether a usable token exists, never the token itself.
func tokenState(p *peering.Peer) string {
	switch {
	case p.Token == "" && p.PendingToken == "":
		return "(revoked)"
	case p.PendingToken != "":
		return "set (rotating)"
	default:
		return "set"
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// startPeerListener stands up the federation peer listener (TEN-184) in the
// interactive run path (the host process holding the live stores/broker/bus).
// Best-effort: a bind-policy refusal or port conflict is a feed note, never
// fatal. The knowledge-tool registrar is injected in TEN-186; today it serves
// the peer_hello handshake. Binds synchronously so the bound address (and any
// refusal) is reported before serving in a goroutine.
func startPeerListener(ctx context.Context, c *commonFlags, deps peerToolDeps, pairApprove func(context.Context, string) bool, pushSys func(string), log *slog.Logger) {
	store, err := peering.LoadStore(c.cfgDir)
	if err != nil {
		pushSys("peer: listener not started — " + err.Error())
		return
	}
	id, err := ensureInstanceID(c)
	if err != nil {
		pushSys("peer: listener not started — " + err.Error())
		return
	}
	overlay := c.lc.Peer.Transport == "overlay"
	cert, fp, err := peerTLS(c.cfgDir, overlay)
	if err != nil {
		pushSys("peer: listener not started — " + err.Error())
		return
	}
	hostName, _ := os.Hostname()
	ln, err := peering.NewListener(peering.ListenerConfig{
		Store:        store,
		SelfID:       id,
		SelfName:     hostName,
		SelfVersion:  mcp.LibraryVersion,
		SelfFinger:   fp,
		Overlay:      overlay,
		TLSCert:      cert,
		Registrar:    peerKnowledgeRegistrar(deps), // TEN-186 share-gated knowledge tools
		PairApprover: pairApprove,                  // TEN-239 push-invite Approve/Deny
		Logger: func(f string, a ...any) {
			if log != nil {
				log.Info(fmt.Sprintf(f, a...))
			}
		},
	})
	if err != nil {
		pushSys("peer: listener not started — " + err.Error())
		return
	}
	netLn, err := ln.Bind(c.lc.Peer.Listen)
	if err != nil {
		pushSys("peer: listener not started — " + err.Error())
		return
	}
	transport := "TLS"
	if overlay {
		transport = "overlay (plain HTTP)"
	}
	pushSys(fmt.Sprintf("peer: federation listener on %s (%s)", netLn.Addr().String(), transport))
	go func() {
		if serr := ln.Serve(ctx, netLn); serr != nil {
			pushSys("peer: listener stopped — " + serr.Error())
		}
	}()
}

// peerQuery dials a paired peer and runs one knowledge query against it
// (TEN-186 client path): `tenant peer query <name> <wiki|memory> "<query>"`.
// Uses the static pairing token + the pinned cert fingerprint (TOFU) the peer
// advertised at invite. This is the scriptable cross-query — the agent gets the
// same tools live via the launch-time peer reconnect.
func peerQuery(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("peer query", flag.ContinueOnError)
	k := fs.Int("k", 8, "max results")
	_, store, pos, err := peerStore(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 3 {
		return fmt.Errorf(`usage: tenant peer query <name> <wiki|memory> "<query>"`)
	}
	name, kind, query := pos[0], strings.ToLower(pos[1]), strings.Join(pos[2:], " ")
	var tool string
	switch kind {
	case "wiki":
		tool = "peer_wiki_search"
	case "memory", "mem":
		tool = "peer_memory_search"
	default:
		return fmt.Errorf("kind must be wiki or memory, got %q", kind)
	}
	p, ok := store.Get(name)
	if !ok {
		return fmt.Errorf("no peer named %q (tenant peer list)", name)
	}
	if !p.Dial || p.URL == "" || p.Token == "" {
		return fmt.Errorf("peer %q is not a dialable peer (it dials us, or has no token) — join its invite first", name)
	}

	d, cleanup, err := mcpremote.OpenStatic(ctx, mcpremote.StaticConfig{
		ServerURL: p.URL,
		Token:     p.Token,
		Label:     "peer:" + name,
		TLS:       peering.PinnedTLSClientConfig(p.Fingerprint), // nil ⇒ plain HTTP (overlay)
	}, mcpremote.Policy{})
	if err != nil {
		return fmt.Errorf("connect peer %q: %w", name, err)
	}
	defer cleanup()

	argsJSON, _ := json.Marshal(map[string]any{"query": query, "k": *k})
	out, _, err := d.Dispatch(ctx, model.ToolCall{Name: tool, Arguments: argsJSON})
	if err != nil {
		return fmt.Errorf("%s on %q: %w", tool, name, err)
	}
	fmt.Println(out)
	return nil
}

// ensureInstanceID returns this installation's stable instance_id, minting and
// persisting one in config.json on first use.
func ensureInstanceID(c *commonFlags) (string, error) {
	lc := c.lc
	if lc == nil {
		var err error
		if lc, err = loadLaunchConfig(c.cfgDir); err != nil {
			return "", err
		}
	}
	if strings.TrimSpace(lc.InstanceID) != "" {
		return lc.InstanceID, nil
	}
	id, err := peering.NewInstanceID()
	if err != nil {
		return "", err
	}
	lc.InstanceID = id
	if err := lc.save(c.cfgDir); err != nil {
		return "", fmt.Errorf("persist instance_id: %w", err)
	}
	c.lc = lc
	return id, nil
}
