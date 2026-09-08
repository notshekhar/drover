package server

import (
	"sync"
	"testing"
	"time"
)

// TestRebuildRequestsCoalesce is the guard on a defect that would only appear
// on a large warehouse: `drover sync` kicks every repository, each one that
// advances asks for a rebuild, and every rebuild walks and reads the whole
// warehouse. Fifty repositories meant fifty full rebuilds for one useful
// result — and, before the staging directories were made unique, several of
// them failing and one of them briefly leaving no index at all.
func TestRebuildRequestsCoalesce(t *testing.T) {
	var mu sync.Mutex
	var builds int

	s := &Server{indexStop: make(chan struct{})}
	// Stand in for the real rebuild, which needs a warehouse.
	s.refreshForTest = func() {
		mu.Lock()
		builds++
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}

	// Fifty repositories all reporting a new commit at once.
	for range 50 {
		s.requestIndexRefresh()
	}

	// One debounce window plus the build, plus slack.
	time.Sleep(indexDebounce + 300*time.Millisecond)
	close(s.indexStop)

	mu.Lock()
	got := builds
	mu.Unlock()
	if got != 1 {
		t.Errorf("50 requests produced %d rebuilds, want 1", got)
	}
}

// TestRequestDuringABuildIsNotLost checks the other half: a repository that
// lands while a build is running must still get picked up, or the index would
// silently miss whatever changed during it.
func TestRequestDuringABuildIsNotLost(t *testing.T) {
	var mu sync.Mutex
	var builds int
	started := make(chan struct{}, 4)

	s := &Server{indexStop: make(chan struct{})}
	s.refreshForTest = func() {
		mu.Lock()
		builds++
		mu.Unlock()
		select {
		case started <- struct{}{}:
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}

	s.requestIndexRefresh()
	<-started // the first build is running

	s.requestIndexRefresh() // arrives mid-build
	time.Sleep(indexDebounce + 400*time.Millisecond)
	close(s.indexStop)

	mu.Lock()
	got := builds
	mu.Unlock()
	if got < 2 {
		t.Errorf("a request that arrived during a build produced %d rebuilds, want at least 2", got)
	}
}

// TestStopEndsTheLoop makes sure shutdown does not leave a goroutine waiting
// out a debounce window.
func TestStopEndsTheLoop(t *testing.T) {
	s := &Server{indexStop: make(chan struct{})}
	s.refreshForTest = func() {}
	s.requestIndexRefresh()
	close(s.indexStop)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.indexJobMu.Lock()
		running := s.indexRunning
		s.indexJobMu.Unlock()
		if !running {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("the rebuild loop was still running well after stop")
}
