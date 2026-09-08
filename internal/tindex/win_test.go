package tindex_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/notshekhar/drover/internal/files"
	"github.com/notshekhar/drover/internal/tindex"
)

// TestIndexedVsUnindexed measures what the index actually buys, at the sizes a
// warehouse might reach. It is the number the whole integration stands on.
func TestIndexedVsUnindexed(t *testing.T) {
	if os.Getenv("DROVER_BENCH") == "" {
		t.Skip("set DROVER_BENCH=1 to run timing measurements (takes minutes)")
	}

	sizes := []struct {
		name  string
		repos int
		files int
	}{
		{"today", 2, 280},
		{"5k", 10, 500},
		{"20k", 40, 500},
		{"100k", 100, 1000},
	}
	patterns := []string{"handleRequestPayload", "Handler42", "^import"}

	for _, size := range sizes {
		dir, paths := writeWarehouse(t, size.repos, size.files)
		repos := filepath.Join(dir, "repos")

		buildStart := time.Now()
		ix, err := tindex.Build(context.Background(), dir, repos, paths)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		buildTook := time.Since(buildStart)
		defer ix.Close()

		plain := files.New(dir)
		indexed := files.New(dir)
		indexed.Index = ix

		t.Logf("--- %s: %d files, index built in %.1fs ---", size.name, len(paths), buildTook.Seconds())
		for _, pat := range patterns {
			timeIt := func(r *files.Root) (time.Duration, int) {
				r.Grep(context.Background(), pat, files.GrepOptions{})
				start := time.Now()
				var n int
				for range 3 {
					res, err := r.Grep(context.Background(), pat, files.GrepOptions{})
					if err != nil {
						t.Fatal(err)
					}
					n = len(res.Matches)
				}
				return time.Since(start) / 3, n
			}
			a, na := timeIt(plain)
			b, nb := timeIt(indexed)
			if na != nb {
				t.Errorf("%q: unindexed found %d, indexed found %d", pat, na, nb)
			}
			speedup := float64(a) / float64(b)
			t.Logf("  %-22q  plain %7dms   indexed %7dms   %.1fx   (%d matches)",
				pat, a.Milliseconds(), b.Milliseconds(), speedup, na)
		}
	}
}

func writeWarehouse(t *testing.T, repos, perRepo int) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	var paths []string
	for r := range repos {
		for f := range perRepo {
			rel := fmt.Sprintf("repo%03d/pkg%02d/file%04d.go", r, f%20, f)
			full := filepath.Join(dir, "repos", rel)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			body := fmt.Sprintf("package pkg%02d\n\nimport (\n\t\"context\"\n\t\"fmt\"\n)\n\nfunc Handler%d(ctx context.Context) error {\n\tfmt.Println(\"handling %d in repo %d\")\n\treturn nil\n}\n",
				f%20, f, f, r)
			if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			paths = append(paths, rel)
		}
	}
	slices.Sort(paths)
	return dir, paths
}
