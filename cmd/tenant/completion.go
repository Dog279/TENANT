package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// completionLoad says how to load each shell's script.
var completionLoad = map[string]string{
	"bash":       "source <(tenant completion bash)            # add to ~/.bashrc",
	"zsh":        "source <(tenant completion zsh)             # add to ~/.zshrc (after compinit)",
	"fish":       "tenant completion fish | source             # or save to ~/.config/fish/completions/tenant.fish",
	"powershell": "tenant completion powershell | Out-String | Invoke-Expression   # add to $PROFILE",
}

// cmdCompletion is `tenant completion <shell>` (TEN-320): a completion script
// generated from the command registry, so it always matches what dispatches.
func cmdCompletion(_ context.Context, args []string) error {
	if len(args) != 1 {
		var shells []string
		for s := range completionLoad {
			shells = append(shells, s)
		}
		sort.Strings(shells)
		return fmt.Errorf("usage: tenant completion %s", strings.Join(shells, "|"))
	}
	return writeCompletion(os.Stdout, strings.ToLower(args[0]))
}

// complEntry is what completion knows about one command word.
type complEntry struct {
	name, summary string
	sub, flags    []string
}

// completionEntries lists every command word (aliases included, flag-style
// names left out) with its subcommands and flags.
func completionEntries() []complEntry {
	var out []complEntry
	for _, c := range cliRegistry().Commands() {
		flags := append(append([]string{}, c.Flags...), commonFlagNames...)
		for _, n := range c.Names {
			if strings.HasPrefix(n, "-") {
				continue
			}
			out = append(out, complEntry{name: n, summary: c.Summary, sub: c.Sub, flags: flags})
		}
	}
	return out
}

func writeCompletion(w io.Writer, shell string) error {
	entries := completionEntries()
	var names []string
	for _, e := range entries {
		names = append(names, e.name)
	}
	load, ok := completionLoad[shell]
	if !ok {
		return fmt.Errorf("no completion for %q; choose bash, zsh, fish or powershell", shell)
	}
	switch shell {
	case "bash":
		fmt.Fprintf(w, "# tenant completion for bash. Load it with:\n#   %s\n", load)
		fmt.Fprint(w, "_tenant() {\n\tlocal cur=\"${COMP_WORDS[COMP_CWORD]}\" subs=\"\" flags=\"\"\n")
		fmt.Fprintf(w, "\tif [[ $COMP_CWORD -eq 1 ]]; then\n\t\tCOMPREPLY=( $(compgen -W %q -- \"$cur\") )\n\t\treturn\n\tfi\n", strings.Join(names, " "))
		fmt.Fprint(w, "\tcase \"${COMP_WORDS[1]}\" in\n")
		for _, e := range entries {
			fmt.Fprintf(w, "\t\t%s) subs=%q; flags=%q ;;\n", e.name, strings.Join(e.sub, " "), strings.Join(e.flags, " "))
		}
		fmt.Fprint(w, "\tesac\n")
		fmt.Fprint(w, "\tif [[ \"$cur\" == -* ]]; then\n\t\tCOMPREPLY=( $(compgen -W \"$flags\" -- \"$cur\") )\n")
		fmt.Fprint(w, "\telif [[ $COMP_CWORD -eq 2 && -n \"$subs\" ]]; then\n\t\tCOMPREPLY=( $(compgen -W \"$subs\" -- \"$cur\") )\n")
		fmt.Fprint(w, "\telse\n\t\tCOMPREPLY=( $(compgen -f -- \"$cur\") )\n\tfi\n}\ncomplete -F _tenant tenant\n")
	case "zsh":
		fmt.Fprintf(w, "#compdef tenant\n# tenant completion for zsh. Load it with:\n#   %s\n", load)
		fmt.Fprint(w, "_tenant() {\n\tlocal -a cmds\n\tcmds=(\n")
		for _, e := range entries {
			fmt.Fprintf(w, "\t\t'%s:%s'\n", e.name, shellSafe(asciiDesc(e.summary), "':"))
		}
		fmt.Fprint(w, "\t)\n\tif (( CURRENT == 2 )); then\n\t\t_describe 'command' cmds\n\t\treturn\n\tfi\n")
		fmt.Fprint(w, "\tlocal subs=\"\" flags=\"\"\n\tcase $words[2] in\n")
		for _, e := range entries {
			fmt.Fprintf(w, "\t\t%s) subs=%q; flags=%q ;;\n", e.name, strings.Join(e.sub, " "), strings.Join(e.flags, " "))
		}
		fmt.Fprint(w, "\tesac\n")
		fmt.Fprint(w, "\tif [[ $PREFIX == -* ]]; then\n\t\tcompadd -- ${=flags}\n")
		fmt.Fprint(w, "\telif (( CURRENT == 3 )) && [[ -n $subs ]]; then\n\t\tcompadd -- ${=subs}\n")
		fmt.Fprint(w, "\telse\n\t\t_files\n\tfi\n}\ncompdef _tenant tenant\n")
	case "fish":
		fmt.Fprintf(w, "# tenant completion for fish. Load it with:\n#   %s\n", load)
		fmt.Fprint(w, "complete -c tenant -f\n")
		for _, e := range entries {
			fmt.Fprintf(w, "complete -c tenant -n __fish_use_subcommand -a %s -d '%s'\n", e.name, shellSafe(asciiDesc(e.summary), "'"))
		}
		for _, e := range entries {
			cond := "'__fish_seen_subcommand_from " + e.name + "'"
			if len(e.sub) > 0 {
				fmt.Fprintf(w, "complete -c tenant -n %s -a '%s'\n", cond, strings.Join(e.sub, " "))
			}
			for _, f := range e.flags {
				fmt.Fprintf(w, "complete -c tenant -n %s -l %s\n", cond, strings.TrimPrefix(f, "--"))
			}
		}
	case "powershell":
		fmt.Fprintf(w, "# tenant completion for PowerShell. Load it with:\n#   %s\n", load)
		fmt.Fprint(w, "Register-ArgumentCompleter -Native -CommandName tenant, tenant.exe -ScriptBlock {\n")
		fmt.Fprint(w, "\tparam($wordToComplete, $commandAst, $cursorPosition)\n\t$table = @{\n")
		for _, e := range entries {
			fmt.Fprintf(w, "\t\t'%s' = @{ Sub = @(%s); Flags = @(%s); Help = '%s' }\n", e.name, psList(e.sub), psList(e.flags), strings.ReplaceAll(asciiDesc(e.summary), "'", "''"))
		}
		fmt.Fprint(w, "\t}\n")
		fmt.Fprint(w, "\t$words = @($commandAst.CommandElements | ForEach-Object { $_.ToString() })\n")
		fmt.Fprint(w, "\t$typed = $words.Count\n\tif ($wordToComplete -ne '') { $typed-- }\n")
		fmt.Fprint(w, "\tif ($typed -le 1) {\n\t\t$candidates = $table.Keys | Sort-Object\n")
		fmt.Fprint(w, "\t} else {\n\t\t$entry = $table[$words[1]]\n\t\tif ($null -eq $entry) { return }\n")
		fmt.Fprint(w, "\t\tif ($wordToComplete -like '-*') { $candidates = $entry.Flags }\n")
		fmt.Fprint(w, "\t\telseif ($typed -eq 2) { $candidates = $entry.Sub }\n\t\telse { return }\n\t}\n")
		fmt.Fprint(w, "\t$candidates | Where-Object { $_ -like \"$wordToComplete*\" } | ForEach-Object {\n")
		fmt.Fprint(w, "\t\t$tip = $_\n\t\tif ($table.ContainsKey($_)) { $tip = $table[$_].Help }\n")
		fmt.Fprint(w, "\t\t[System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $tip)\n\t}\n}\n")
	}
	return nil
}

// asciiDesc makes a description plain ASCII. A completion script is saved to
// disk and read back by shells that do not all assume UTF-8: Windows
// PowerShell 5.1 reads a file without a BOM as ANSI, and treats the mangled
// bytes of an arrow as a quote.
func asciiDesc(s string) string {
	r := strings.NewReplacer("→", "->", "—", "-", "–", "-", "…", "...", "‘", "'", "’", "'", "“", `"`, "”", `"`, "·", "-")
	return strings.Map(func(c rune) rune {
		if c < 32 || c > 126 {
			return -1
		}
		return c
	}, r.Replace(s))
}

// shellSafe drops characters that would end a quoted description.
func shellSafe(s, drop string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(drop, r) {
			return -1
		}
		return r
	}, s)
}

func psList(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = "'" + s + "'"
	}
	return strings.Join(quoted, ", ")
}
