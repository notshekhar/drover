package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/notshekhar/drover/internal/client"
	"github.com/notshekhar/drover/internal/tui"
)

// remoteSource draws the dashboard for an engine reached over HTTP.
//
// It is the same screen `drover serve` shows, against an engine that may be
// on another machine. Snapshot is called on every repaint, so the fetched
// state is cached and refreshed in the background rather than blocking the
// paint on a round trip.
type remoteSource struct {
	ctx     context.Context
	c       *client.Client
	started time.Time

	latest tui.Model
	errMsg string
}

func (r *remoteSource) refresh() {
	state, err := r.c.Dashboard(r.ctx)
	if err != nil {
		r.errMsg = err.Error()
		return
	}
	r.errMsg = ""
	r.latest = tui.FromState(*state, r.started)
}

func (r *remoteSource) Snapshot() tui.Model {
	r.refresh()
	m := r.latest
	if r.errMsg != "" {
		// Keep drawing the last good picture, but say it is stale rather than
		// letting the numbers quietly stop meaning anything.
		m.Notice = "engine unreachable: " + r.errMsg
		m.NoticeKind = "err"
	}
	return m
}

func (r *remoteSource) Reload() (string, error) { return r.c.Reload(r.ctx) }
func (r *remoteSource) SyncAll() error          { return r.c.SyncAll(r.ctx) }

func cmdDash(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("dash", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		dataDirFlag = fs.String("data-dir", "", "")
		configFlag  = fs.String("config", "", "")
		serverFlag  = fs.String("server", "", "engine to watch")
	)
	flags, _ := splitArgs(args, clientFlags())
	if err := fs.Parse(flags); err != nil {
		return err
	}

	c, err := clientFor(dataDirFlag, configFlag, serverFlag)
	if err != nil {
		return err
	}
	// Fail here rather than painting an empty screen that never fills in.
	if _, err := c.Status(ctx); err != nil {
		return fmt.Errorf("%w\nstart it with `drover serve`", err)
	}

	if !tui.Supported(os.Stdin, os.Stdout) {
		return fmt.Errorf("drover dash needs a terminal; try `drover get repository` instead")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	src := &remoteSource{ctx: ctx, c: c, started: time.Now()}
	runner := &tui.Runner{
		Source: src,
		// dash opens on the detail view; that is the point of opening it.
		Mode: tui.Detail,
		// The engine watches its own files, so a reload key here would be
		// offering to do something that already happened.
		AutoReload: true,
		In:         os.Stdin,
		Out:        os.Stdout,
	}
	return runner.Run(ctx)
}
