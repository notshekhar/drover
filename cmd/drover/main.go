// Command drover is a context engine: it holds git checkouts in one place and
// hands coding agents real filesystem tools over MCP.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// Version is the build version, overridden at release with -ldflags.
var Version = "dev"

func main() {
	// One context for the process, cancelled by Ctrl-C or SIGTERM. Every
	// command takes it, so an interrupt reaches the work itself -- a long
	// clone, a slow query, a grep across every checkout -- instead of only
	// killing the process once the call it is inside finally returns.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "drover: "+err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		usage(os.Stdout)
		return nil
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "serve":
		return cmdServe(ctx, rest)
	case "apply":
		return cmdApply(ctx, rest)
	case "get":
		return cmdGet(ctx, rest)
	case "delete":
		return cmdDelete(ctx, rest)
	case "sync":
		return cmdSync(ctx, rest)
	case "dash", "dashboard":
		return cmdDash(ctx, rest)
	case "mcp":
		return cmdMCP(ctx, rest)
	case "call":
		return cmdCall(ctx, rest)
	case "query":
		return cmdQuery(ctx, rest)
	case "health":
		return cmdHealth(ctx, rest)
	case "review":
		return cmdReview(ctx, rest)
	case "import":
		return cmdImport(ctx, rest)
	case "forget":
		return cmdForget(ctx, rest)
	case "upgrade", "update", "self-update":
		return cmdUpgrade(ctx, rest)
	case "version", "--version", "-v":
		fmt.Println("drover " + Version)
		return nil
	case "help", "--help", "-h":
		usage(os.Stdout)
		return nil
	default:
		usage(os.Stderr)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `drover -- kubectl, but for context

Usage:
  drover serve                      run the engine (draws a dashboard on a tty)
  drover dash                       open the dashboard for a running engine
  drover apply -f <file|dir>        apply objects (-f - reads stdin)
  drover get <kind> [name]          list or show objects
  drover delete <kind> <name>       remove an object and its checkout
  drover sync [name]                refresh now (all repositories, or one)
  drover call <name> [-p k=v]       execute an HTTPRequest
  drover query <name> "SELECT ..."  query a SQLConnection
  drover health <name>              re-run a SQLConnection's health gate
  drover mcp                        bridge an agent's stdio to the engine (MCP)
  drover review <repository>        show what a repository declares about itself
  drover import openapi -f <spec>   turn an OpenAPI spec into documents
  drover import bruno -f <dir>      turn a Bruno collection into documents
  drover forget <path>              drop a path from the config apply list
  drover upgrade                    install the latest release
  drover version
  drover help

Kinds are spelled in full: repository, environment, httprequest, sqlconnection.

Common flags:
  --data-dir <path>   where objects and checkouts live (default ~/.drover)
  --config <path>     config file (default <data-dir>/config.yaml)
  --server <url>      engine to talk to (client commands)

Examples:
  drover serve
  drover apply -f repo.yaml
  drover get repository
  drover get repository api -o yaml
  drover call get-user -p userId=usr_1a2b3c --environment prod
  drover query analytics "SELECT count(*) FROM events"
  drover import openapi -f openapi.yaml --tag billing --out billing.yaml

Point an agent at the engine over MCP. drover serve hosts it at /mcp:
  claude mcp add --transport http drover http://127.0.0.1:7432/mcp
or, for a client that only speaks stdio:
  claude mcp add drover -- drover mcp
`)
}
