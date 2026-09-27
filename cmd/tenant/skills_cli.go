package main

// skills_cli.go is `tenant skills list|show|import|export|trust|untrust`
// (TEN-298): the skill library from the shell, for a hub run as `tenant serve`
// with no terminal UI. The same skillControl the TUI's /skills uses does the
// work.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"tenant/internal/memory/skills"
	"tenant/internal/model"
	"tenant/internal/tui"
)

// cmdSkillsLibrary runs one library subcommand. Positional arguments come
// before or after the flags (--agent, --data, --trust).
func cmdSkillsLibrary(ctx context.Context, sub string, args []string) error {
	var pos []string
	rest := args
	for len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		pos, rest = append(pos, rest[0]), rest[1:]
	}
	fs := flag.NewFlagSet("skills "+sub, flag.ContinueOnError)
	c := bindCommon(fs)
	trust := fs.Bool("trust", false, "import: trust the skill now (default: untrusted until you run `tenant skills trust`)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	pos = append(pos, fs.Args()...)
	if err := c.resolve(); err != nil {
		return err
	}
	st, err := skills.Open(filepath.Join(c.dataDir, "skills.db"))
	if err != nil {
		return fmt.Errorf("open skills db: %w", err)
	}
	defer st.Close()
	sc := skillControl{st: st, agentID: c.agent}
	out := os.Stdout
	need := func(n int, usage string) error {
		if len(pos) < n {
			return fmt.Errorf("usage: tenant skills %s", usage)
		}
		return nil
	}

	switch sub {
	case "list":
		list := sc.SkillList()
		if len(list) == 0 {
			fmt.Fprintln(out, "no skills")
			return nil
		}
		for _, s := range list {
			state := "on"
			if !s.Enabled {
				state = "off"
			}
			var marks []string
			if s.Status != skills.StatusLive {
				marks = append(marks, s.Status)
			}
			if !s.Trusted {
				marks = append(marks, "untrusted")
			}
			if s.Origin != "" {
				marks = append(marks, s.Origin)
			}
			fmt.Fprintf(out, "%-3s %s [%s] — %s\n", state, s.Name, strings.Join(marks, ", "), s.Description)
		}
		return nil
	case "show":
		if err := need(1, "show <name>"); err != nil {
			return err
		}
		d, err := sc.SkillDetail(pos[0])
		if err != nil {
			return err
		}
		if d == nil {
			return fmt.Errorf("no skill named %q", pos[0])
		}
		fmt.Fprintln(out, strings.ReplaceAll(tui.RenderSkillDetail(d), "/skills ", "tenant skills "))
		return nil
	case "import":
		if err := need(1, "import <package dir or SKILL.md> [--trust]"); err != nil {
			return err
		}
		// Embed the description with the configured embedder, like skills the
		// TUI saves; without one the skill is stored unembedded.
		if router, rerr := buildRouter(c, newLogger()); rerr == nil {
			sc.emb, _, _ = router.EmbedderForRole(ctx, model.RoleEmbedder)
		}
		msg, err := sc.ImportSkill(pos[0], *trust)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, strings.ReplaceAll(msg, "/skills ", "tenant skills "))
		return nil
	case "export":
		if err := need(2, "export <name> <dir>"); err != nil {
			return err
		}
		dir, err := sc.ExportSkill(pos[0], pos[1])
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "exported skill %s to %s\n", pos[0], dir)
		return nil
	case "trust", "untrust":
		if err := need(1, sub+" <name>"); err != nil {
			return err
		}
		ok, err := sc.SetSkillTrust(pos[0], sub == "trust")
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no skill named %q", pos[0])
		}
		fmt.Fprintf(out, "skill %s is now %sed\n", pos[0], sub)
		return nil
	}
	return fmt.Errorf("unknown skills subcommand %q", sub)
}
