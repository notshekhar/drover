package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/notshekhar/drover/internal/config"
	"github.com/notshekhar/drover/internal/server"
)

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		dataDirFlag = fs.String("data-dir", "", "where objects and checkouts live (default ~/.drover)")
		configFlag  = fs.String("config", "", "config file (default <data-dir>/config.yaml)")
		listenFlag  = fs.String("listen", "", "address to bind (default from config, else "+config.DefaultListen+")")
		syncFlag    = fs.String("sync", "", "default refresh interval for repositories that do not set one (e.g. 30m, never)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	dataDir, err := config.DataDir(*dataDirFlag)
	if err != nil {
		return err
	}
	cfgPath, err := config.Path(*configFlag, dataDir)
	if err != nil {
		return err
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	// A bad --sync must fail now, not at the first tick an hour from now.
	if *syncFlag != "" {
		if _, err := config.ParseSync(*syncFlag); err != nil {
			return fmt.Errorf("--sync: %w", err)
		}
		cfg.Sync = *syncFlag
	}

	listen := cfg.ListenAddr()
	if *listenFlag != "" {
		listen = *listenFlag
	}

	srv, err := server.New(server.Options{
		DataDir: dataDir,
		Listen:  listen,
		Version: Version,
		Log:     os.Stderr,
		Sync:    cfg.SyncInterval(),
	})
	if err != nil {
		return err
	}

	// Everything is loaded and applied before the listener opens, so the
	// engine never serves a half-built view of the world.
	if err := srv.Bootstrap(cfg); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Workers come up after bootstrap and before serving. Reconciles run in
	// the background, so a slow first clone does not hold the listener shut.
	if err := srv.StartSync(ctx); err != nil {
		return err
	}
	defer srv.StopSync()

	// The health gate decides whether a sql tool is offered at all, so it runs
	// at startup rather than at the first query.
	srv.CheckSQLHealth(ctx)

	ln, err := srv.Listen()
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	fmt.Fprintf(os.Stderr, "drover %s listening on http://%s (data %s, sync %s)\n",
		Version, ln.Addr(), dataDir, cfg.SyncInterval())
	fmt.Fprintf(os.Stderr, "  MCP: http://%s%s\n", ln.Addr(), server.MCPPath)
	fmt.Fprintf(os.Stderr, "  add it with: claude mcp add --transport http drover http://%s%s\n", ln.Addr(), server.MCPPath)

	if err := srv.Serve(ln); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	fmt.Fprintln(os.Stderr, "drover stopped")
	return nil
}
