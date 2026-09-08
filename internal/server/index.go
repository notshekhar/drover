package server

import (
	"context"
	"fmt"
	"time"

	"github.com/notshekhar/drover/internal/api"
	"github.com/notshekhar/drover/internal/tindex"
)

// The trigram index, and when the engine bothers with one.
//
// Search is fast enough without it on any warehouse anyone has today — a few
// hundred files answer in single-digit milliseconds — so an index is built
// only once a warehouse is large enough for the difference to be felt. Below
// that it would be work done, disk used, and staleness reasoned about, in
// exchange for nothing.
//
// Nothing here can change an answer. The index narrows which files a search
// opens; internal/files still does the matching, against the same bytes, with
// the same line semantics. See internal/tindex for why that holds even when
// the index is stale.

// attachIndex opens an existing index and gives it to the file root.
//
// A missing or unreadable index is not an error and is not logged loudly: it
// means searches run the way they always have. That is the whole point of the
// index being optional — nothing should break, or even look different, when it
// is absent.
func (s *Server) attachIndex() {
	ix, err := tindex.Open(s.opts.DataDir, s.files.Dir)
	if err != nil {
		return
	}
	s.indexMu.Lock()
	old := s.index
	s.index = ix
	s.files.Index = ix
	s.indexMu.Unlock()

	if old != nil {
		old.Close()
	}
	fmt.Fprintf(s.opts.Log, "search index attached (%d files)\n", ix.NumFiles())
}

// detachIndex drops the index, leaving searches on the plain path.
func (s *Server) detachIndex() {
	s.indexMu.Lock()
	old := s.index
	s.index = nil
	s.files.Index = nil
	s.indexMu.Unlock()
	if old != nil {
		old.Close()
	}
}

// RefreshIndex rebuilds the index if the warehouse is large enough to want
// one, and drops it if it is not.
//
// Called after a sync, because that is when the checkouts change. It walks the
// warehouse to enumerate what to index — the same enumeration a search would
// have done — so the index and the search agree about what exists.
//
// Errors are reported to the log and swallowed: a failed index build must
// leave a working engine, since every search still works without one.
func (s *Server) RefreshIndex(ctx context.Context) {
	paths, err := s.files.Enumerate(ctx)
	if err != nil {
		fmt.Fprintf(s.opts.Log, "search index: enumerating the warehouse failed: %v\n", err)
		return
	}

	if len(paths) < tindex.Threshold {
		// Small enough that a plain search is already instant. Drop any index
		// left over from when it was bigger, rather than keep one that now
		// only costs freshness checks.
		if s.currentIndex() != nil {
			fmt.Fprintf(s.opts.Log, "search index dropped: %d files is below the %d-file threshold\n",
				len(paths), tindex.Threshold)
			s.detachIndex()
		}
		return
	}

	start := time.Now()
	ix, err := tindex.Build(ctx, s.opts.DataDir, s.files.Dir, paths)
	if err != nil {
		fmt.Fprintf(s.opts.Log, "search index: build failed, searches continue without one: %v\n", err)
		return
	}

	s.indexMu.Lock()
	old := s.index
	s.index = ix
	s.files.Index = ix
	s.indexMu.Unlock()
	if old != nil {
		old.Close()
	}
	fmt.Fprintf(s.opts.Log, "search index built: %d files in %.1fs\n", len(paths), time.Since(start).Seconds())
}

// currentIndex returns the attached index, or nil.
func (s *Server) currentIndex() *tindex.Index {
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	return s.index
}

// closeIndex releases the index at shutdown.
func (s *Server) closeIndex() {
	s.detachIndex()
}

// indexStatus describes the index for the status endpoint, so an operator can
// tell a search that legitimately found nothing from one running unnarrowed
// over a warehouse large enough to need an index.
func (s *Server) indexStatus() *api.SearchIndexStatus {
	ix := s.currentIndex()
	if ix == nil {
		return &api.SearchIndexStatus{Threshold: tindex.Threshold}
	}
	return &api.SearchIndexStatus{Attached: true, Files: ix.NumFiles(), Threshold: tindex.Threshold}
}
