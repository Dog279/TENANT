// Command tenant is the Tenant agent binary: one appliance, local or cloud
// models, with a memory that learns (docs/adr/0001-appliance-not-framework.md).
//
// Subcommands are declared once, in the command registry (cliregistry.go),
// which dispatches them and generates the help: `tenant help` for the jobs a
// new operator does first, `tenant help all` for every command, `tenant help
// <command>` for one, and `tenant completion <shell>` for tab completion.
//
// Global flags (per subcommand): --backend echo|vllm (default echo),
// --agent ID (default main), --data DIR, --config DIR.
//
// The "echo" backend is deterministic and dependency-free, so every
// subcommand runs offline. Switch --backend vllm (and point the
// shipped profiles at real endpoints) for production.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
)

func main() {
	if len(os.Args) < 2 {
		writeBeginnerHelp(os.Stderr)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	holdDataDir = lockDataDirForProcess

	c := cliRegistry().Lookup(os.Args[1])
	if c == nil {
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		writeBeginnerHelp(os.Stderr)
		os.Exit(2)
	}
	if err := c.Run(ctx, os.Args[2:]); err != nil {
		// `tenant <command> --help`: the flag package has already printed
		// the command's flags.
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
