package files

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/notshekhar/drover/internal/object"
)

// newRoots builds a data directory holding one checkout and one mirror.
func newRoots(t *testing.T) (*Root, string) {
	t.Helper()
	data := t.TempDir()
	mustWrite(t, filepath.Join(data, "repos", "api", "main.go"), "package main // rate limit\n")
	mustWrite(t, filepath.Join(data, "mirrors", "api", "issues", "12.md"), "---\nstate: open\n---\nthe rate limit is wrong\n")
	return New(data), data
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The paths that worked before the jail grew roots still work, unchanged.
// Those paths are baked into the git tool's prefix trimming, the lsp result
// format and the tool descriptions the model has already learned.
func TestCheckoutPathsAreUnchanged(t *testing.T) {
	r, _ := newRoots(t)
	res, err := r.Read("api/main.go", 0, 0)
	if err != nil {
		t.Fatalf("read a checkout path: %v", err)
	}
	if res.Path != "api/main.go" {
		t.Errorf("path came back as %q, want api/main.go", res.Path)
	}
}

func TestExtraRootResolves(t *testing.T) {
	r, _ := newRoots(t)
	res, err := r.Read("mirrors/api/issues/12.md", 0, 0)
	if err != nil {
		t.Fatalf("read through an extra root: %v", err)
	}
	if res.Path != "mirrors/api/issues/12.md" {
		t.Errorf("path came back as %q", res.Path)
	}
}

// An extra root with no directory behind it is not a root yet, so the name is
// an ordinary miss inside the checkouts rather than an error about emptiness.
func TestUnusedRootIsNotARoot(t *testing.T) {
	r, _ := newRoots(t)
	if _, err := r.Read("docs/whatever.md", 0, 0); err == nil {
		t.Fatal("expected a miss for an extra root that does not exist yet")
	}
	if _, err := r.List(""); err != nil {
		t.Fatalf("listing the top level: %v", err)
	}
}

func TestListShowsTheRoots(t *testing.T) {
	r, _ := newRoots(t)
	res, err := r.List("")
	if err != nil {
		t.Fatal(err)
	}
	var sawRepo, sawMirrors bool
	for _, e := range res.Entries {
		switch e.Name {
		case "api":
			sawRepo = true
			if e.Root {
				t.Error("a checkout was marked as a root")
			}
		case "mirrors":
			sawMirrors = true
			if !e.Root {
				t.Error("mirrors was listed but not marked as a root")
			}
		case "docs", "logs":
			t.Errorf("%q was listed even though nothing has written there", e.Name)
		}
	}
	if !sawRepo || !sawMirrors {
		t.Errorf("top level listed %+v, want the checkout and the mirrors root", res.Entries)
	}
}

// The default scope is the checkouts: a search for a symbol should not come
// back half pull-request comments. But silence about that would make "no
// matches" mean two different things, so the roots that were skipped are named.
func TestBareSearchStaysInTheCheckouts(t *testing.T) {
	r, _ := newRoots(t)
	res, err := r.Grep(context.Background(), "rate limit", GrepOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range res.Matches {
		if strings.HasPrefix(m.Path, "mirrors/") {
			t.Errorf("a bare grep reached %s", m.Path)
		}
	}
	if len(res.Matches) != 1 {
		t.Errorf("got %d matches, want the one in the checkout", len(res.Matches))
	}
	if len(res.Unsearched) != 1 || res.Unsearched[0] != "mirrors" {
		t.Errorf("unsearched roots came back as %v, want [mirrors]", res.Unsearched)
	}
}

func TestNamingARootSearchesIt(t *testing.T) {
	r, _ := newRoots(t)
	res, err := r.Grep(context.Background(), "rate limit", GrepOptions{Path: "mirrors/api"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 1 || res.Matches[0].Path != "mirrors/api/issues/12.md" {
		t.Fatalf("grep of a named root returned %+v", res.Matches)
	}
	if res.Unsearched != nil {
		t.Errorf("a search that named a path disclosed unsearched roots: %v", res.Unsearched)
	}
}

// The jail is one jail with several roots. An escape attempt out of an extra
// root has to fail exactly as it does out of the checkouts.
func TestExtraRootIsJailedToo(t *testing.T) {
	r, data := newRoots(t)
	mustWrite(t, filepath.Join(data, "secret.txt"), "nope\n")
	if err := os.Symlink(filepath.Join(data, "secret.txt"), filepath.Join(data, "mirrors", "api", "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for _, path := range []string{"mirrors/../secret.txt", "mirrors/api/escape"} {
		if _, err := r.Read(path, 0, 0); err == nil {
			t.Errorf("%s was readable through an extra root", path)
		}
	}
}

// The two lists are declared apart so the file tools stay independent of the
// object model. This is what keeps them honest.
func TestReservedNamesMatchTheRoots(t *testing.T) {
	if len(object.ReservedNames) != len(ExtraRootNames) {
		t.Fatalf("object.ReservedNames %v and files.ExtraRootNames %v have drifted", object.ReservedNames, ExtraRootNames)
	}
	for i, name := range ExtraRootNames {
		if object.ReservedNames[i] != name {
			t.Errorf("position %d: root %q, reserved %q", i, name, object.ReservedNames[i])
		}
	}
}
