// Command tenant is the Tenant agent binary. Subcommands:
//
//	tenant version                 print version
//	tenant chat                    interactive agent loop (stdin → stdout)
//	tenant distill                 run one distillation pass
//	tenant memory search <query>   search episodes + facts
//	tenant mcp-memory              run the MCP memory server over stdio
//	tenant mcp-selftest            spawn mcp-memory as a subprocess and
//	                               drive the MCP protocol against it
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
	"fmt"
	"os"
	"os/signal"

	"tenant/internal/mcp"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	switch os.Args[1] {
	case "version", "-v", "--version":
		fmt.Printf("tenant %s — speaks MCP %s (Go)\n", mcp.LibraryVersion, mcp.ProtocolVersion)
		return
	case "setup":
		err = cmdSetup(ctx, os.Args[2:])
	case "oauth-setup":
		err = cmdOAuthSetup(ctx, os.Args[2:])
	case "model", "models":
		err = cmdModel(ctx, os.Args[2:])
	case "agents", "agent":
		err = cmdAgents(ctx, os.Args[2:])
	case "chat":
		err = cmdChat(ctx, os.Args[2:])
	case "distill":
		err = cmdDistill(ctx, os.Args[2:])
	case "consolidate":
		err = cmdConsolidate(ctx, os.Args[2:])
	case "memory":
		err = cmdMemory(ctx, os.Args[2:])
	case "mcp-memory":
		err = cmdMCPMemory(ctx, os.Args[2:])
	case "mcp-selftest":
		err = cmdMCPSelftest(ctx, os.Args[2:])
	case "tool-test":
		err = cmdToolTest(ctx, os.Args[2:])
	case "web":
		err = cmdWeb(ctx, os.Args[2:])
	case "sql":
		err = cmdSQL(ctx, os.Args[2:])
	case "wiki":
		err = cmdWiki(ctx, os.Args[2:])
	case "gsuite":
		err = cmdGSuite(ctx, os.Args[2:])
	case "x":
		err = cmdX(ctx, os.Args[2:])
	case "imessage":
		err = cmdIMessage(ctx, os.Args[2:])
	case "os":
		err = cmdOS(ctx, os.Args[2:])
	case "serve":
		err = cmdServe(ctx, os.Args[2:])
	case "tui":
		err = cmdTUI(ctx, os.Args[2:])
	case "orchestrate", "team":
		err = cmdOrchestrate(ctx, os.Args[2:])
	case "research":
		err = cmdResearch(ctx, os.Args[2:])
	case "goal":
		err = cmdGoal(ctx, os.Args[2:])
	case "review":
		err = cmdReview(ctx, os.Args[2:])
	case "skills":
		err = cmdSkills(ctx, os.Args[2:])
	case "atlassian":
		err = cmdAtlassian(ctx, os.Args[2:])
	case "mcp":
		err = cmdMCP(ctx, os.Args[2:])
	case "peer":
		err = cmdPeer(ctx, os.Args[2:])
	case "ack":
		err = cmdFeedback(ctx, os.Args[2:], "ack")
	case "undo":
		err = cmdFeedback(ctx, os.Args[2:], "undo")
	case "doctor":
		err = cmdDoctor(ctx, os.Args[2:])
	case "eval":
		err = cmdEval(ctx, os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `tenant — Go-native MCP agent framework

USAGE
  tenant <command> [flags]

COMMANDS
  version                  print version
  setup                    one-time config wizard (endpoints, model, gateway) → config.json
  model <list|use|add|...> manage model backends + set the primary (also /model in the TUI)
  chat                     interactive agent loop (one stdin line = one turn)
  distill                  run one T2->T3 distillation pass
  memory search <query>    search episodes + facts from the CLI
  mcp-memory               run the MCP memory server over stdio
  mcp-selftest             spawn mcp-memory as a subprocess and exercise it
  tool-test                run a tool-calling agent turn (hardening harness)
  web "<task>"             agent browses the live web (read/explore)
  sql "<q>" --db FILE      agent queries a SQLite DB (read; --allow-write)
  wiki "<q>" --dir DIR     agent answers from a markdown knowledge base (read-only)
  gsuite "<task>"          agent uses Gmail + Calendar (read; --allow-send to act)
  x "<task>" | x --login   agent uses X/Twitter (read; --allow-post to act)
  imessage "<task>"        agent uses iMessage via BlueBubbles (read; --allow-send to act)
  os "<task>"              agent inspects the machine + runs shell cmds (read; --allow-exec to run)
  mcp connect <url>        connect to a remote MCP server (OAuth2.1+DCR browser sign-in) + list its tools
  peer <invite|join|...>   federate Tenant instances: mutual-consent invite pairing + share policy
  serve                    run background self-improvement (distillation) on a cadence
  tui                      full-screen terminal UI: streaming chat + live activity feed
  orchestrate "<task>"     multi-agent team: orchestrator spawns concurrent sub-agents that
                           talk over a live bus and self-resolve; streams all progress
  research "<question>"    deep research: plan → parallel web-researchers → cited report (--out FILE)
  review <plan.md>         cascading CEO/Eng/Designer review appended to a plan file
  doctor                   diagnose (and --fix) the setup when things break
  eval                     run the eval harness (Phase 1: --subset smoke only)

COMMON FLAGS
  --backend echo|vllm   inference backend (default echo: deterministic, offline)
  --agent ID            agent id (default "main")
  --data DIR            data dir (default OS data dir)
  --config DIR          config dir (default OS config dir)

EXAMPLES
  tenant setup --backend vllm --vllm-endpoint http://localhost:8000
  tenant tui
  printf 'remember I prefer Go\nwhat do I prefer?\n' | tenant chat
  tenant distill
  tenant memory search "what do I prefer"
  tenant mcp-selftest
`)
}
