package main

// peerctl.go is `tenant peer …` (TEN-183): Tenant-to-Tenant federation pairing.
// Mutual-consent invite codes — `tenant peer invite` on one side prints a
// one-time short-lived code, `tenant peer join <code>` on the other stores the
// dial record. peers.json (0600) is the single authoritative store; the
// TEN-184 listener reads it per-request to authenticate peers. This is the
// operator surface over internal/peering.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"tenant/internal/peering"
)

func cmdPeer(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return peerUsage()
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "invite":
		return peerInvite(rest)
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
                          edit a peer's share policy (all-deny by default)`)
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

func peerInvite(args []string) error {
	fs := flag.NewFlagSet("peer invite", flag.ContinueOnError)
	url := fs.String("url", "", "the address a joiner will dial to reach THIS instance (e.g. http://my-host:9100/ or your Tailscale name)")
	as := fs.String("as", "", "how this instance identifies itself to the peer (default: hostname)")
	ttl := fs.Duration("ttl", time.Hour, "how long the invite code stays valid")
	c, store, pos, err := peerStore(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: tenant peer invite <name> --url <addr> [--as <self>] [--ttl 1h]")
	}
	peerName := pos[0]
	if strings.TrimSpace(*url) == "" {
		return fmt.Errorf("--url is required: the address the peer will dial to reach you")
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
	// Fingerprint is empty at TEN-183 (TLS pinning lands in TEN-185); overlay
	// (Tailscale) needs none anyway.
	code, err := store.CreateInvite(selfName, id, *url, "", *ttl, peerName)
	if err != nil {
		return err
	}
	fmt.Printf("%s\n", code)
	fmt.Fprintf(os.Stderr, "\n↑ Give this code to %q. It expires in %s and is single-use.\n"+
		"They run:  tenant peer join <code>\n"+
		"Then set what they may read:  tenant peer share %s wiki=on\n", peerName, ttl.String(), peerName)
	return nil
}

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
