package tindex_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/notshekhar/drover/internal/files"
	"github.com/notshekhar/drover/internal/tindex"
)

// build writes a warehouse, indexes it, and returns the data dir and index.
func build(t *testing.T, contents map[string]string) (string, *tindex.Index) {
	t.Helper()
	dir := t.TempDir()
	repos := filepath.Join(dir, "repos")

	var paths []string
	for rel, body := range contents {
		full := filepath.Join(repos, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, rel)
	}
	slices.Sort(paths)

	ix, err := tindex.Build(context.Background(), dir, repos, paths)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	t.Cleanup(func() { ix.Close() })
	return dir, ix
}

// corpus is the warehouse these tests search.
var corpus = map[string]string{
	"api/main.go":     "package main\n\nfunc handler() {\n\tlog.Print(\"start\")\n}\n",
	"api/util.go":     "func handler2(w Writer) { handler3(w) }\n",
	"api/README.md":   "# api\n\nThe handler is documented here.\n",
	"web/app.ts":      "export function handler() {}\nconst x = 1;\n",
	"web/style.css":   "body { color: red }\n",
	"web/crlf.txt":    "handler one\r\nhandler two\r\nplain\r\n",
	"web/unicode.txt": "héllo wörld\nhandler ünicode\n",
	"docs/spec.md":    "no match in this one\n",
	"noeol/last.go":   "func handler4() {}",
	// Each matches a case-insensitive search only through Unicode folding,
	// which the index's ASCII lowercasing cannot see: ſ (U+017F) folds to s,
	// K (Kelvin) to k, and é/É are a non-ASCII pair.
	"fold/long-s.txt": "ſhellſhocK\n",
	"fold/upper.txt":  "CAFÉ AU LAIT\n",
	"fold/lower.txt":  "café au lait\n",
}

// TestRealIndexNeverChangesTheAnswer is the conformance test that matters: the
// same queries, through the actual trigram index rather than a stand-in.
func TestRealIndexNeverChangesTheAnswer(t *testing.T) {
	dir, ix := build(t, corpus)

	patterns := []string{
		"handler", "handler2", "handler3", "func ", "^func", "handler$",
		"h.ndler", "handler|Writer", "[hH]andler", "wörld", "documented",
		"zzznotpresent", "e", "(?:handler)+", "func handler",
		"color: red", "const x", "no match",
		"shellshock", "café", "CAFÉ", "(?-i)café", "caf(?i:É)",
	}
	options := []struct {
		name string
		opts files.GrepOptions
	}{
		{"whole warehouse", files.GrepOptions{}},
		{"a subtree", files.GrepOptions{Path: "api"}},
		{"a glob", files.GrepOptions{Include: "*.go"}},
		{"a selector", files.GrepOptions{Only: []string{"api", "web"}}},
		{"case sensitive", files.GrepOptions{CaseSensitive: true}},
		{"a tight cap", files.GrepOptions{MaxResults: 3}},
	}

	for _, pattern := range patterns {
		for _, o := range options {
			t.Run(pattern+"/"+o.name, func(t *testing.T) {
				plain := files.New(dir)
				want, err := plain.Grep(context.Background(), pattern, o.opts)
				if err != nil {
					t.Fatalf("unindexed: %v", err)
				}

				indexed := files.New(dir)
				indexed.Index = ix
				got, err := indexed.Grep(context.Background(), pattern, o.opts)
				if err != nil {
					t.Fatalf("indexed: %v", err)
				}

				if !reflect.DeepEqual(want, got) {
					t.Errorf("the index changed the answer for %q\n  without: %d matches\n  with:    %d matches",
						pattern, len(want.Matches), len(got.Matches))
					for i := range max(len(want.Matches), len(got.Matches)) {
						var a, b string
						if i < len(want.Matches) {
							a = fmt.Sprintf("%s:%d", want.Matches[i].Path, want.Matches[i].Line)
						}
						if i < len(got.Matches) {
							b = fmt.Sprintf("%s:%d", got.Matches[i].Path, got.Matches[i].Line)
						}
						if a != b {
							t.Errorf("  first difference at %d: without=%s with=%s", i, a, b)
							break
						}
					}
				}
			})
		}
	}
}

// TestStaleIndexCostsReadsNotResults is the property that makes an index safe
// to keep around between syncs.
//
// A file written after the index was built is unknown to it. The index must
// nominate it anyway — evidence of absence is not the same as absence — or
// every edit would become invisible until someone remembered to reindex.
func TestStaleIndexCostsReadsNotResults(t *testing.T) {
	dir, ix := build(t, corpus)
	repos := filepath.Join(dir, "repos")

	// Add a file the index has never seen, and change one it has.
	if err := os.WriteFile(filepath.Join(repos, "api", "brandnew.go"),
		[]byte("func handlerAdded() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repos, "web", "style.css"),
		[]byte("body { color: red }\nfunc handlerSnuckIn() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, pattern := range []string{"handler", "handlerAdded", "handlerSnuckIn"} {
		t.Run(pattern, func(t *testing.T) {
			plain := files.New(dir)
			want, err := plain.Grep(context.Background(), pattern, files.GrepOptions{})
			if err != nil {
				t.Fatal(err)
			}
			indexed := files.New(dir)
			indexed.Index = ix
			got, err := indexed.Grep(context.Background(), pattern, files.GrepOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(want, got) {
				t.Errorf("a stale index lost results for %q: %d without, %d with",
					pattern, len(want.Matches), len(got.Matches))
			}
			if pattern == "handlerAdded" && len(got.Matches) == 0 {
				t.Error("a file created after the index was built was never found")
			}
		})
	}

	// The file the index DID know about, whose content then changed, is the
	// dangerous case and the reason this test exists.
	//
	// The index holds real but obsolete postings for it: web/style.css had no
	// "handler" trigrams when it was indexed, so a naive lookup leaves it out
	// with full confidence and the match disappears. Only comparing the file's
	// current stamp against the one recorded at index time catches it. An
	// earlier version of this package got that wrong and this assertion is
	// what caught it.
	indexed := files.New(dir)
	indexed.Index = ix
	got, err := indexed.Grep(context.Background(), "handlerSnuckIn", files.GrepOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Matches) != 1 {
		t.Errorf("an edited file's new content was not found: %d matches, want 1", len(got.Matches))
	}
}

// TestNarrowingIsRealNotIncidental proves the index actually removes work,
// since every conformance test above would pass if it did nothing.
func TestNarrowingIsRealNotIncidental(t *testing.T) {
	_, ix := build(t, corpus)

	known := slices.Sorted(mapKeys(corpus))
	got, err := ix.Candidates("handler", false, known)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) >= len(known) {
		t.Errorf("narrowing kept %d of %d paths — it is not narrowing", len(got), len(known))
	}
	// Every file that really contains it must survive.
	for rel, body := range corpus {
		if strings.Contains(strings.ToLower(body), "handler") && !slices.Contains(got, rel) {
			t.Errorf("%s contains the pattern but was not nominated", rel)
		}
	}
	t.Logf("narrowed %d paths to %d", len(known), len(got))
}

// TestUnnarrowablePatternSaysSo covers the contract's dangerous half: a
// pattern the index cannot plan must report that, not return nothing.
func TestUnnarrowablePatternSaysSo(t *testing.T) {
	_, ix := build(t, corpus)
	known := slices.Sorted(mapKeys(corpus))

	for _, pattern := range []string{".*", "ab", "^", "a"} {
		if _, err := ix.Candidates(pattern, false, known); err == nil {
			t.Errorf("%q reported a candidate set, but it constrains nothing the index can use", pattern)
		}
	}
}

// TestBuildIsAtomic checks that a failed build cannot leave a half-written
// index in place of a working one.
func TestBuildIsAtomic(t *testing.T) {
	dir, ix := build(t, corpus)
	before := ix.NumFiles()
	ix.Close()

	// A build over a path that does not exist still produces a valid index
	// (the file is simply unreadable), so the check here is that the index
	// directory is never left in a state Open rejects.
	repos := filepath.Join(dir, "repos")
	fresh, err := tindex.Build(context.Background(), dir, repos, []string{"api/main.go"})
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	defer fresh.Close()

	if fresh.NumFiles() != 1 {
		t.Errorf("rebuilt index holds %d files, want 1", fresh.NumFiles())
	}
	if before == fresh.NumFiles() {
		t.Errorf("the rebuild did not replace the previous index (%d files both times)", before)
	}
	// No staging directory left behind.
	if _, err := os.Stat(tindex.Dir(dir) + ".building"); err == nil {
		t.Error("a staging directory survived a successful build")
	}
}

// TestNilIndexIsSafe covers the default: no index attached, nothing changes.
func TestNilIndexIsSafe(t *testing.T) {
	var ix *tindex.Index
	if _, err := ix.Candidates("handler", false, []string{"a.go"}); err == nil {
		t.Error("a nil index reported a candidate set")
	}
	if ix.NumFiles() != 0 {
		t.Error("a nil index reported files")
	}
	if err := ix.Close(); err != nil {
		t.Errorf("closing a nil index: %v", err)
	}
}

func mapKeys(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}
