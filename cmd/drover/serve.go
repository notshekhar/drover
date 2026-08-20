package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/notshekhar/drover/internal/config"
	"github.com/notshekhar/drover/internal/server"
	"github.com/notshekhar/drover/internal/tui"
)

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		dataDirFlag = fs.String("data-dir", "", "where objects and checkouts live (default ~/.drover)")
		configFlag  = fs.String("config", "", "config file (default <data-dir>/config.yaml)")
		listenFlag  = fs.String("listen", "", "address to bind (default from config, else "+config.DefaultListen+")")
		syncFlag    = fs.String("sync", "", "default refresh interval for repositories that do not set one (e.g. 30m, never)")
		noTUIFlag   = fs.Bool("no-tui", false, "log plainly instead of drawing the dashboard")
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

	// The dashboard needs a terminal on both ends. Without one -- under
	// systemd, in a pipe, in CI -- the log lines are the only interface there
	// is, so they stay.
	dashboard := !*noTUIFlag && tui.Supported(os.Stdin, os.Stderr)

	// With the dashboard up, a stray log line would scribble over the drawing.
	var engineLog io.Writer = os.Stderr
	if dashboard {
		engineLog = io.Discard
	}

	srv, err := server.New(server.Options{
		DataDir:    dataDir,
		Listen:     listen,
		Version:    Version,
		Log:        engineLog,
		Sync:       cfg.SyncInterval(),
		ConfigPath: cfgPath,
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
	// at startup rather than at the first query. In the background, because a
	// database that is down should not hold the listener shut.
	go srv.CheckSQLHealth(ctx)

	ln, err := srv.Listen()
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	serveErr := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if err != nil && errors.Is(err, net.ErrClosed) {
			err = nil
		}
		serveErr <- err
	}()

	if !dashboard {
		fmt.Fprintf(os.Stderr, "drover %s listening on http://%s (data %s, sync %s)\n",
			Version, ln.Addr(), dataDir, cfg.SyncInterval())
		fmt.Fprintf(os.Stderr, "  MCP: http://%s%s\n", ln.Addr(), server.MCPPath)
		fmt.Fprintf(os.Stderr, "  add it with: claude mcp add --transport http drover http://%s%s\n",
			ln.Addr(), server.MCPPath)
		err := <-serveErr
		fmt.Fprintln(os.Stderr, "drover stopped")
		return err
	}

	// Quitting the dashboard stops the engine: it is the foreground process
	// the user started, so closing it should not leave a daemon behind.
	uiCtx, cancelUI := context.WithCancel(ctx)
	defer cancelUI()
	go func() {
		<-serveErr
		cancelUI()
	}()

	runner := &tui.Runner{
		Source: srv.NewDashboard(cfg, ln.Addr().String()),
		In:     os.Stdin,
		Out:    os.Stderr,
	}
	if err := runner.Run(uiCtx); err != nil {
		// The screen could not be set up (no raw mode, say). That is cosmetic,
		// so fall back to serving rather than refusing to run at all.
		fmt.Fprintf(os.Stderr, "dashboard unavailable (%v); serving on http://%s\n", err, ln.Addr())
		return <-serveErr
	}

	ln.Close()
	<-serveErr
	return nil
}
