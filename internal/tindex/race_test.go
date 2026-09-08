package tindex_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/notshekhar/drover/internal/tindex"
)

// TestConcurrentBuildsCorrupt demonstrates that two builds racing on the same
// staging directory do not both succeed. It is the failure a sync of many
// repositories would cause if each triggered its own rebuild.
func TestConcurrentBuildsCorrupt(t *testing.T) {
	dir := t.TempDir()
	repos := filepath.Join(dir, "repos")
	var paths []string
	for i := range 400 {
		rel := fmt.Sprintf("pkg%d/file%03d.go", i%10, i)
		full := filepath.Join(repos, rel)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(fmt.Sprintf("package pkg%d\nfunc Handler%d() {}\n", i%10, i)), 0o644)
		paths = append(paths, rel)
	}
	slices.Sort(paths)

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ix, err := tindex.Build(context.Background(), dir, repos, paths)
			if err != nil {
				errs <- err
				return
			}
			if n := ix.NumFiles(); n != len(paths) {
				errs <- fmt.Errorf("index holds %d files, want %d", n, len(paths))
			}
			ix.Close()
		}()
	}
	wg.Wait()
	close(errs)

	var failures int
	for err := range errs {
		failures++
		t.Logf("concurrent build failed: %v", err)
	}
	if failures > 0 {
		t.Errorf("%d of 8 concurrent builds failed or produced a bad index", failures)
	}
}
