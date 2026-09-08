package files

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestGrepTiming measures what a search actually costs, at the size the
// warehouse is now and at sizes it might grow to.
//
// This exists to answer one question before any index is bolted on: is the
// current walk-and-scan slow enough to be worth replacing? An index is not
// free — it has to be built, kept fresh, and reasoned about when stale — so it
// should be adopted on evidence rather than on the assumption that indexing is
// always better.
//
// Run with: go test ./internal/files -run TestGrepTiming -v
func TestGrepTiming(t *testing.T) {
	if os.Getenv("DROVER_BENCH") == "" {
		t.Skip("set DROVER_BENCH=1 to run timing measurements (takes minutes)")
	}

	sizes := []struct {
		name  string
		repos int
		files int // per repo
	}{
		{"tiny (today's warehouse)", 2, 280},
		{"small", 10, 500},
		{"medium", 40, 500},
		{"large", 100, 1000},
	}

	patterns := []string{
		"handleRequestPayload", // rare: no match anywhere
		"func ",                // common
		"^import",              // anchored, common
	}

	for _, size := range sizes {
		root := buildWarehouse(t, size.repos, size.files)
		r := New(root)
		total := size.repos * size.files

		t.Run(size.name, func(t *testing.T) {
			for _, pattern := range patterns {
				// Warm the page cache so the first pattern does not pay for
				// every later one.
				r.Grep(context.Background(), pattern, GrepOptions{})

				start := time.Now()
				const runs = 5
				var res *GrepResult
				for range runs {
					var err error
					res, err = r.Grep(context.Background(), pattern, GrepOptions{})
					if err != nil {
						t.Fatalf("%q: %v", pattern, err)
					}
				}
				per := time.Since(start) / runs
				t.Logf("%-22s %6d files  %-22q %6dms  (%d matches, %d files searched)",
					size.name, total, pattern, per.Milliseconds(), len(res.Matches), res.Files)
			}
		})
	}
}

// buildWarehouse writes a synthetic warehouse of Go-ish source.
//
// Content is varied per file so trigram selectivity is realistic: files share
// some tokens and not others, which is what decides whether an index could
// narrow anything.
func buildWarehouse(t *testing.T, repos, perRepo int) string {
	t.Helper()
	root := t.TempDir()
	reposDir := filepath.Join(root, "repos")

	for r := range repos {
		repo := filepath.Join(reposDir, fmt.Sprintf("repo%03d", r))
		for f := range perRepo {
			dir := filepath.Join(repo, fmt.Sprintf("pkg%02d", f%20))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			body := fmt.Sprintf(`package pkg%02d

import (
	"context"
	"fmt"
)

func Handler%d(ctx context.Context) error {
	fmt.Println("handling %d in repo %d")
	return nil
}

func helper%d(v int) int { return v * %d }
`, f%20, f, f, r, f, f+1)
			name := filepath.Join(dir, fmt.Sprintf("file%04d.go", f))
			if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}
