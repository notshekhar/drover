package server

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/notshekhar/drover/internal/config"
)

// WatchInterval is how often the data directory is checked for edits.
//
// Polling rather than an OS watcher: the set of files is small and flat, a
// second of latency on a config edit is imperceptible, and it avoids a
// dependency plus the per-platform failure modes that come with one (editors
// that write-then-rename, watches that silently stop on network mounts).
const WatchInterval = time.Second

// settleDelay is how long a file must stop changing before it is applied.
//
// An editor writing a large file can be observed halfway through, and an agent
// writing several files wants them applied together rather than one at a time
// with errors in between -- an HTTPRequest saved before the Environment it
// names would fail on its own.
const settleDelay = 750 * time.Millisecond

// Watch re-applies the config and the data directory's yaml whenever they
// change on disk, so editing a file is enough and there is nothing to press.
//
// onReload is called with the outcome so a dashboard can show it.
func (s *Server) Watch(ctx context.Context, cfgPath string, onReload func(msg string, err error)) {
	last := s.fingerprint(cfgPath)
	var pendingSince time.Time

	ticker := time.NewTicker(WatchInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		current := s.fingerprint(cfgPath)
		if current != last {
			// Something moved; restart the settle timer rather than applying
			// a file that may still be being written.
			last = current
			pendingSince = time.Now()
			continue
		}
		if pendingSince.IsZero() || time.Since(pendingSince) < settleDelay {
			continue
		}
		pendingSince = time.Time{}

		msg, _, err := s.Reload(cfgPath)
		if onReload != nil {
			onReload(msg, err)
		}
		if err != nil {
			s.logf("reload failed: %v", err)
			continue
		}
		s.logf("%s", msg)
	}
}

// fingerprint is a cheap summary of every file that feeds the engine: the
// config plus the yaml sitting in the data directory.
//
// Name, size and modification time together catch every edit that matters.
// Hashing contents would be more precise and is not worth reading every file
// once a second to get.
func (s *Server) fingerprint(cfgPath string) string {
	paths := []string{cfgPath}
	if dropIns, err := config.DropInFiles(s.opts.DataDir); err == nil {
		paths = append(paths, dropIns...)
	}
	// The config may point at files outside the data directory.
	if cfg, err := config.Load(cfgPath); err == nil && len(cfg.Apply) > 0 {
		if files, err := config.CollectAll(cfg.Apply); err == nil {
			paths = append(paths, files...)
		}
	}
	sort.Strings(paths)

	var b strings.Builder
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			// A file that has just been deleted is a change too.
			fmt.Fprintf(&b, "%s:gone\n", p)
			continue
		}
		fmt.Fprintf(&b, "%s:%d:%d\n", p, info.Size(), info.ModTime().UnixNano())
	}
	return b.String()
}
