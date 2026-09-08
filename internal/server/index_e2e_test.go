package server_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/notshekhar/drover/internal/files"
	"github.com/notshekhar/drover/internal/tindex"
)

// TestEngineUsesTheIndexWhenTheWarehouseIsBigEnough drives the wiring the way
// the engine does: enumerate, build, attach, search.
func TestEngineUsesTheIndexWhenTheWarehouseIsBigEnough(t *testing.T) {
	dir := t.TempDir()
	repos := filepath.Join(dir, "repos")
	for r := range 3 {
		for f := range 40 {
			rel := fmt.Sprintf("repo%d/pkg%d/file%02d.go", r, f%5, f)
			full := filepath.Join(repos, rel)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			body := fmt.Sprintf("package pkg%d\n\nfunc Handler%d() {}\n", f%5, f)
			if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	root := files.New(dir)
	paths, err := root.Enumerate(context.Background())
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if len(paths) != 120 {
		t.Fatalf("enumerated %d paths, want 120", len(paths))
	}
	// Enumerate must give the search's own view: relative, forward-slashed,
	// sorted. An index built from anything else would not line up.
	for _, p := range paths[:3] {
		if strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
			t.Errorf("enumerated path is not root-relative and slashed: %q", p)
		}
	}

	ix, err := tindex.Build(context.Background(), dir, repos, paths)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer ix.Close()

	plainRoot := files.New(dir)
	indexedRoot := files.New(dir)
	indexedRoot.Index = ix

	for _, pattern := range []string{"Handler7", "func Handler", "package pkg3", "nothinghere"} {
		want, err := plainRoot.Grep(context.Background(), pattern, files.GrepOptions{})
		if err != nil {
			t.Fatal(err)
		}
		got, err := indexedRoot.Grep(context.Background(), pattern, files.GrepOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(want.Matches) != len(got.Matches) || want.Files != got.Files {
			t.Errorf("%q: plain %d matches/%d files, indexed %d matches/%d files",
				pattern, len(want.Matches), want.Files, len(got.Matches), got.Files)
		}
	}
}
