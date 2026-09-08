package files

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// The index is only allowed to make a search faster. This file exists to prove
// it never makes one different.
//
// Every test here runs the same query twice — once with an index attached and
// once without — and requires the two results to be identical, field for
// field. That is a stronger property than "the index is correct", and it is
// the one that matters: drover's answers must not depend on whether an index
// happens to exist, be stale, or be wrong.

// bothWays runs a query with and without narrowing and returns the two results.
func bothWays(t *testing.T, root string, index Narrower, pattern string, opts GrepOptions) (plain, narrowed *GrepResult) {
	t.Helper()

	bare := New(root)
	plain, err := bare.Grep(context.Background(), pattern, opts)
	if err != nil {
		t.Fatalf("unindexed search for %q: %v", pattern, err)
	}

	indexed := New(root)
	indexed.Index = index
	narrowed, err = indexed.Grep(context.Background(), pattern, opts)
	if err != nil {
		t.Fatalf("indexed search for %q: %v", pattern, err)
	}
	return plain, narrowed
}

// requireIdentical compares two results in full.
func requireIdentical(t *testing.T, pattern string, plain, narrowed *GrepResult) {
	t.Helper()
	if !reflect.DeepEqual(plain, narrowed) {
		t.Errorf("%q: the index changed the answer\n  without: %d matches, %d files, truncated=%v\n  with:    %d matches, %d files, truncated=%v",
			pattern, len(plain.Matches), plain.Files, plain.Truncated,
			len(narrowed.Matches), narrowed.Files, narrowed.Truncated)
		// Name the first divergence, which is what actually gets debugged.
		for i := range max(len(plain.Matches), len(narrowed.Matches)) {
			var a, b string
			if i < len(plain.Matches) {
				a = fmt.Sprintf("%s:%d:%s", plain.Matches[i].Path, plain.Matches[i].Line, plain.Matches[i].Text)
			}
			if i < len(narrowed.Matches) {
				b = fmt.Sprintf("%s:%d:%s", narrowed.Matches[i].Path, narrowed.Matches[i].Line, narrowed.Matches[i].Text)
			}
			if a != b {
				t.Errorf("  first difference at %d:\n    without: %s\n    with:    %s", i, a, b)
				break
			}
		}
	}
}

// warehouse writes a small warehouse and returns its data directory.
func warehouse(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		full := filepath.Join(dir, "repos", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// perfectIndex narrows using the same regexp the search will use, which is the
// tightest any index could ever be. If a search disagrees with itself under
// this, the fault is in the plumbing rather than in any index.
type perfectIndex struct{ root string }

func (p perfectIndex) Candidates(pattern string, caseSensitive bool, known []string) ([]string, error) {
	expr := pattern
	if !caseSensitive {
		expr = "(?i)" + expr
	}
	re, err := compileForTest(expr)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, rel := range known {
		body, err := os.ReadFile(filepath.Join(p.root, "repos", filepath.FromSlash(rel)))
		if err != nil {
			// Unreadable: keep it, since dropping it could lose a match.
			out = append(out, rel)
			continue
		}
		if re.Match(body) {
			out = append(out, rel)
		}
	}
	return out, nil
}

// dropEverything is the adversarial case: an index that nominates nothing.
//
// It exists to prove the plumbing is what it claims to be. A search under this
// index returns nothing, which is CORRECT behaviour for the contract as
// written — the index is trusted to be a superset — and the test asserts that
// explicitly so nobody later "fixes" the search into re-reading everything and
// silently loses the whole benefit.
type dropEverything struct{}

func (dropEverything) Candidates(string, bool, []string) ([]string, error) { return nil, nil }

// refuses always declines to narrow, which every search must survive.
type refuses struct{}

func (refuses) Candidates(string, bool, []string) ([]string, error) {
	return nil, errors.New("cannot narrow")
}

func TestIndexNeverChangesTheAnswer(t *testing.T) {
	dir := warehouse(t, map[string]string{
		"api/main.go":       "package main\n\nfunc handler() {\n\tlog.Print(\"start\")\n}\n",
		"api/util.go":       "func handler2(w Writer) { handler3(w) }\n",
		"api/README.md":     "# api\n\nThe handler is documented here.\n",
		"web/app.ts":        "export function handler() {}\nconst x = 1;\n",
		"web/style.css":     "body { color: red }\n",
		"web/crlf.txt":      "handler one\r\nhandler two\r\nplain\r\n",
		"web/unicode.txt":   "héllo wörld\nhandler ünicode\n",
		"docs-repo/spec.md": "no match in this one\n",
		"noeol/last.go":     "func handler4() {}",
	})

	patterns := []string{
		"handler", "handler2", "func ", "^func", "handler$",
		"h.ndler", "handler|Writer", "[hH]andler", "wörld",
		"zzznotpresent", "e", "(?:handler)+",
	}

	for _, pattern := range patterns {
		t.Run(pattern, func(t *testing.T) {
			for _, tc := range []struct {
				name  string
				index Narrower
			}{
				{"a perfectly tight index", perfectIndex{root: dir}},
				{"an index that refuses to narrow", refuses{}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					plain, narrowed := bothWays(t, dir, tc.index, pattern, GrepOptions{})
					requireIdentical(t, pattern, plain, narrowed)
				})
			}
		})
	}
}

// TestIndexPreservesEveryOption checks the narrowing against each way a search
// can be scoped, since the index sees the paths only after scoping is done.
func TestIndexPreservesEveryOption(t *testing.T) {
	dir := warehouse(t, map[string]string{
		"api/main.go":   "func handler() {}\n",
		"api/util.go":   "func handler2() {}\n",
		"api/notes.txt": "handler mentioned in prose\n",
		"web/app.ts":    "function handler() {}\n",
		"web/app.js":    "function handler() {}\n",
	})

	cases := []struct {
		name string
		opts GrepOptions
	}{
		{"whole warehouse", GrepOptions{}},
		{"one subtree", GrepOptions{Path: "api"}},
		{"a glob", GrepOptions{Include: "*.go"}},
		{"a selector", GrepOptions{Only: []string{"api"}}},
		{"case sensitive", GrepOptions{CaseSensitive: true}},
		{"a tight cap", GrepOptions{MaxResults: 2}},
		{"a subtree and a glob", GrepOptions{Path: "web", Include: "*.ts"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plain, narrowed := bothWays(t, dir, perfectIndex{root: dir}, "handler", tc.opts)
			requireIdentical(t, "handler", plain, narrowed)
		})
	}
}

// TestFilesCountReportsTheScope covers a reporting subtlety: narrowing must
// not make a search look smaller than it was.
//
// A caller reading "12 files searched" is asking how much of the warehouse the
// query covered. Reporting the post-narrowing count would make a well-working
// index indistinguishable from a query that barely looked anywhere.
func TestFilesCountReportsTheScope(t *testing.T) {
	dir := warehouse(t, map[string]string{
		"api/a.go": "func handler() {}\n",
		"api/b.go": "nothing here\n",
		"api/c.go": "nothing here either\n",
		"api/d.go": "still nothing\n",
	})

	plain, narrowed := bothWays(t, dir, perfectIndex{root: dir}, "handler", GrepOptions{})
	if plain.Files != 4 {
		t.Fatalf("unindexed search reports %d files, want 4", plain.Files)
	}
	if narrowed.Files != 4 {
		t.Errorf("indexed search reports %d files searched; it must report the scope (4), not the %d it opened",
			narrowed.Files, 1)
	}
}

// TestNarrowingActuallyNarrows is the control. Every test above would pass if
// the index were quietly ignored, so one of them has to prove it is not.
func TestNarrowingActuallyNarrows(t *testing.T) {
	dir := warehouse(t, map[string]string{
		"api/a.go": "func handler() {}\n",
		"api/b.go": "nothing here\n",
	})

	r := New(dir)
	r.Index = dropEverything{}
	res, err := r.Grep(context.Background(), "handler", GrepOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// An index that nominates nothing must yield nothing. If this returns a
	// match, the narrowing is not wired in at all and every conformance test
	// above is vacuous.
	if len(res.Matches) != 0 {
		t.Errorf("an index that nominated no files still produced %d matches — narrowing is not wired in",
			len(res.Matches))
	}
}

// TestRandomisedConformance runs generated patterns over a generated
// warehouse, which reaches combinations no hand-written list would.
func TestRandomisedConformance(t *testing.T) {
	rng := rand.New(rand.NewSource(20260908))
	words := []string{"alpha", "beta", "gamma", "handler", "socket", "buffer", "parse", "encode"}

	files := map[string]string{}
	for r := range 6 {
		for f := range 8 {
			var body strings.Builder
			for range 10 {
				fmt.Fprintf(&body, "func %s_%s() { return %d }\n",
					words[rng.Intn(len(words))], words[rng.Intn(len(words))], rng.Intn(100))
			}
			files[fmt.Sprintf("repo%d/file%d.go", r, f)] = body.String()
		}
	}
	dir := warehouse(t, files)
	index := perfectIndex{root: dir}

	for i := range 120 {
		w := words[rng.Intn(len(words))]
		var pattern string
		switch rng.Intn(5) {
		case 0:
			pattern = w
		case 1:
			pattern = w + "_" + words[rng.Intn(len(words))]
		case 2:
			pattern = "^func " + w
		case 3:
			pattern = w + "|" + words[rng.Intn(len(words))]
		default:
			pattern = w[:3] + "."
		}

		t.Run(fmt.Sprintf("%d/%s", i, pattern), func(t *testing.T) {
			opts := GrepOptions{CaseSensitive: rng.Intn(2) == 0}
			plain, narrowed := bothWays(t, dir, index, pattern, opts)
			requireIdentical(t, pattern, plain, narrowed)
		})
	}
}

// compileForTest compiles a pattern the way a whole-file probe must, so the
// test index sees the same language the line-by-line search will.
//
// The (?m) is not optional and getting it wrong is instructive: without it,
// "^func" anchors to the start of the whole FILE rather than each line, so the
// helper nominated only files whose first line matched and the conformance
// tests failed on every anchored pattern. Grep's own wholeFileProbe carries
// the same flag for the same reason — and its two carve-outs, for "$" and
// "\r", are why this helper is deliberately not used to model those.
//
// The real index does not have this hazard at all: a trigram plan ignores
// anchors, so "^func encode" plans as the literal "func encode" and nominates
// a superset of the lines that match it.
func compileForTest(expr string) (*regexp.Regexp, error) { return regexp.Compile("(?m)" + expr) }
