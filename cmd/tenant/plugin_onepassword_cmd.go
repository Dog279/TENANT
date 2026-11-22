package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"tenant/internal/plugins/onepassword"
)

// cmdOnePassword is the read-only one-shot: who the sign-in is, the vaults it
// sees, the items in one. It never prints a secret value; that is what
// op_read under the agent's approvals is for. With desktop sign-in the
// 1Password app asks the person running it to approve.
func cmdOnePassword(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("onepassword", flag.ContinueOnError)
	c := bindCommon(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := c.resolve(); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return fmt.Errorf("usage: tenant onepassword whoami | vaults | items <vault>")
	}
	lc, err := loadLaunchConfig(c.cfgDir)
	if err != nil {
		return err
	}
	settings := onepasswordSettings(lc)
	cli, err := onepasswordBackend(c.cfgDir, settings)
	if err != nil {
		return fmt.Errorf("onepassword: %w", err)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
	defer w.Flush()
	switch rest[0] {
	case "whoami":
		id, err := cli.WhoAmI(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "sign-in\t%s\ntype\t%s\nurl\t%s\n", onepasswordAuth(settings), strings.ToLower(firstNonEmpty(id.Type, "account")), id.URL)
		if id.Email != "" {
			fmt.Fprintf(w, "email\t%s\n", id.Email)
		}
		if id.ID != "" {
			fmt.Fprintf(w, "id\t%s\n", id.ID)
		}
		return nil
	case "vaults":
		vaults, err := cli.Vaults(ctx)
		if err != nil {
			return err
		}
		allow := splitVaults(settings["vaults"])
		for _, v := range vaults {
			mark := ""
			if len(allow) > 0 && !vaultAllowed(allow, v) {
				mark = "\t(not allowed to the agent)"
			}
			fmt.Fprintf(w, "%s\t%s%s\n", v.ID, v.Name, mark)
		}
		return nil
	case "items":
		if len(rest) < 2 {
			return fmt.Errorf("usage: tenant onepassword items <vault>")
		}
		// A vault name may hold spaces: the words after `items` are the name,
		// up to anything that looks like a flag (flags go before the
		// subcommand; one left after it is a mistake, not part of the name).
		var words []string
		for _, w := range rest[1:] {
			if strings.HasPrefix(w, "-") {
				break
			}
			words = append(words, w)
		}
		items, err := cli.Items(ctx, strings.Join(words, " "), nil, nil)
		if err != nil {
			return err
		}
		for _, it := range items {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", it.ID, it.Title, strings.ToLower(it.Category), strings.Join(it.Tags, ","))
		}
		return nil
	}
	return fmt.Errorf("unknown subcommand %q: whoami | vaults | items <vault>", rest[0])
}

func vaultAllowed(allow []string, v onepassword.Vault) bool {
	for _, a := range allow {
		if strings.EqualFold(a, v.ID) || strings.EqualFold(a, v.Name) {
			return true
		}
	}
	return false
}
