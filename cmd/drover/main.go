// Command drover is a context engine: it holds git checkouts in one place and
// hands coding agents real filesystem tools over MCP.
package main

import (
	"fmt"
	"os"
)

// Version is the build version, overridden at release with -ldflags.
var Version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "drover: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage(os.Stdout)
		return nil
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "serve":
		return cmdServe(rest)
	case "apply":
		return cmdApply(rest)
	case "get":
		return cmdGet(rest)
	case "delete":
		return cmdDelete(rest)
	case "sync":
		return cmdSync(rest)
	case "dash", "dashboard":
		return cmdDash(rest)
	case "mcp":
		return cmdMCP(rest)
	case "call":
		return cmdCall(rest)
	case "query":
		return cmdQuery(rest)
	case "health":
		return cmdHealth(rest)
	case "forget":
		return cmdForget(rest)
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
  drover forget <path>              drop a path from the config apply list
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

Point an agent at the engine over MCP. drover serve hosts it at /mcp:
  claude mcp add --transport http drover http://127.0.0.1:7432/mcp
or, for a client that only speaks stdio:
  claude mcp add drover -- drover mcp
`)
}
