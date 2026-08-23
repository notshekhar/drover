package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/notshekhar/drover/internal/mcp"
)

func cmdMCP(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		dataDirFlag = fs.String("data-dir", "", "")
		configFlag  = fs.String("config", "", "")
		serverFlag  = fs.String("server", "", "engine to talk to")
	)
	flags, _ := splitArgs(args, clientFlags())
	if err := fs.Parse(flags); err != nil {
		return err
	}

	c, err := clientFor(dataDirFlag, configFlag, serverFlag)
	if err != nil {
		return err
	}

	// Fail here rather than at the first tool call. An agent that spawns this
	// and gets a working stdio session, only for every tool to error, is much
	// harder to diagnose than one that never starts.
	if _, err := c.Status(ctx); err != nil {
		return fmt.Errorf("%w\nstart it with `drover serve`", err)
	}

	// stdout is the protocol channel and must carry nothing but JSON-RPC, so
	// every human-readable word goes to stderr.
	fmt.Fprintf(os.Stderr, "drover mcp: bridging %s\n", c.BaseURL)

	srv := &mcp.Server{Backend: c, Version: Version}
	return srv.Serve(ctx, os.Stdin, os.Stdout)
}
