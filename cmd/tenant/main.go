// Command tenant is the Tenant agent binary: one appliance, local or cloud
// models, with a memory that learns (docs/adr/0001-appliance-not-framework.md).
//
// Subcommands are declared once, in the command registry (cliregistry.go),
// which dispatches them and generates the help: `tenant help` for the jobs a
// new operator does first, `tenant help all` for every command, `tenant help
// <command>` for one, and `tenant completion <shell>` for tab completion.
//
// Global flags (per subcommand, after its name: `tenant tui --backend echo`):
// --backend echo|vllm|anthropic|claudecode (default: config.json's provider,
// else echo), --agent ID (default main), --data DIR, --config DIR.
//
// The "echo" backend is deterministic and dependency-free, so every
// subcommand runs offline. `tenant setup` connects a real model.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"slices"
)

func main() {
	if runWindowsService() { // started by the service control manager (svc_windows.go, TEN-498)
		return
	}
	if len(os.Args) < 2 {
		writeBeginnerHelp(os.Stderr)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	holdDataDir = lockDataDirForProcess
	killChildrenOnExit() // Windows: a hard kill takes the browser down too (TEN-401)

	c := cliRegistry().Lookup(os.Args[1])
	if c == nil {
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		writeBeginnerHelp(os.Stderr)
		os.Exit(2)
	}
	if n := c.Names[0]; n != "serve" && n != "tui" { // those open the full log book themselves
		openCommandLog = openOneShotLog
	}
	err := c.Run(ctx, os.Args[2:])
	closeCommandLog()
	if err != nil {
		// `tenant <command> --help`: the flag package has already printed
		// the command's flags.
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		if helpInstead(err, os.Args[2:]) {
			writeCommandHelp(os.Stdout, c)
			return
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// helpInstead is `tenant <command> --help` answered with a usage error: a
// command that reads a subcommand or a task first took the flag for one. Its
// help page answers instead. A command whose flag set came first has already
// printed its flags (flag.ErrHelp).
func helpInstead(err error, args []string) bool {
	return err != nil && !errors.Is(err, flag.ErrHelp) && len(args) > 0 &&
		slices.Contains([]string{"-h", "-help", "--help"}, args[0])
}
