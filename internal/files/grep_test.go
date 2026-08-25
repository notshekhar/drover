package files

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// vendorTree is a checkout shaped like a real one: a little source, buried
// under a lot of dependency and build output.
func vendorTree(t *testing.T) *Root {
	t.Helper()
	data := t.TempDir()
	r := New(data)

	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(r.Dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("web/src/app.ts", "export function start() {}\n")
	write("web/node_modules/left-pad/index.js", "export function start() {}\n")
	write("web/node_modules/nested/deep/x.js", "export function start() {}\n")
	write("web/dist/bundle.js", "export function start() {}\n")
	write("web/vendor/thing.go", "export function start() {}\n")
	return r
}

func TestGrepSkipsVendorAndBuildDirs(t *testing.T) {
	r := vendorTree(t)

	res, err := r.Grep(context.Background(), "export function", GrepOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 1 {
		for _, m := range res.Matches {
			t.Logf("match %s:%d", m.Path, m.Line)
		}
		t.Fatalf("got %d matches, want only the one in src", len(res.Matches))
	}
	if res.Matches[0].Path != "web/src/app.ts" {
		t.Errorf("matched %s, want web/src/app.ts", res.Matches[0].Path)
	}
}

// Pointing a search at a skipped directory still searches it. The list is
// about what a walk wanders into, not what it is aimed at.
func TestGrepSearchesVendorWhenNamed(t *testing.T) {
	r := vendorTree(t)

	res, err := r.Grep(context.Background(), "export function", GrepOptions{Path: "web/node_modules"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 2 {
		t.Fatalf("got %d matches, want 2 from inside node_modules", len(res.Matches))
	}
}

func TestGrepResultsAreSorted(t *testing.T) {
	data := t.TempDir()
	r := New(data)
	for _, name := range []string{"c", "a", "b"} {
		p := filepath.Join(r.Dir, "repo", name+".txt")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("one hit\nnothing\nanother hit\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res, err := r.Grep(context.Background(), "hit", GrepOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range res.Matches {
		got = append(got, m.Path+":"+itoa(m.Line))
	}
	want := []string{
		"repo/a.txt:1", "repo/a.txt:3",
		"repo/b.txt:1", "repo/b.txt:3",
		"repo/c.txt:1", "repo/c.txt:3",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

// The whole-file probe reads raw bytes, where a CRLF line still has its "\r".
// Line matching does not, because the line is delivered with it stripped. A
// pattern anchored at end-of-line therefore gets no probe, or the probe would
// reject a file that does in fact match.
func TestGrepEndAnchorOnCRLF(t *testing.T) {
	data := t.TempDir()
	r := New(data)
	p := filepath.Join(r.Dir, "repo", "win.txt")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("alpha\r\nbeta\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := r.Grep(context.Background(), "alpha$", GrepOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 1 {
		t.Fatalf("got %d matches, want 1", len(res.Matches))
	}
	if res.Matches[0].Text != "alpha" {
		t.Errorf("text = %q, want %q with the carriage return stripped", res.Matches[0].Text, "alpha")
	}
}

// A file too large to read in one piece is scanned rather than skipped.
func TestGrepLargeFileFallsBackToStreaming(t *testing.T) {
	old := maxGrepFileBytes
	maxGrepFileBytes = 8
	defer func() { maxGrepFileBytes = old }()

	data := t.TempDir()
	r := New(data)
	p := filepath.Join(r.Dir, "repo", "big.txt")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("padding line\nthe needle is here\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := r.Grep(context.Background(), "needle", GrepOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 1 || res.Matches[0].Line != 2 {
		t.Fatalf("got %+v, want one match on line 2", res.Matches)
	}
}

func TestGrepTruncatesAfterSorting(t *testing.T) {
	data := t.TempDir()
	r := New(data)
	p := filepath.Join(r.Dir, "repo", "many.txt")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString("hit\n")
	}
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := r.Grep(context.Background(), "hit", GrepOptions{MaxResults: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 5 || !res.Truncated {
		t.Fatalf("got %d matches truncated=%v, want 5 and truncated", len(res.Matches), res.Truncated)
	}
	// Truncation takes the first five in order, not five arbitrary ones.
	for i, m := range res.Matches {
		if m.Line != i+1 {
			t.Errorf("match %d is line %d, want %d", i, m.Line, i+1)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// A search across every checkout is the longest-running thing drover does on
// its own CPU, and until the context reached it there was no way to stop one:
// a client that hung up left the walk running to completion against a caller
// nobody was waiting on.
func TestGrepStopsOnCancelledContext(t *testing.T) {
	r := vendorTree(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := r.Grep(ctx, "export function", GrepOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("grep with a cancelled context: got %v, want context.Canceled", err)
	}
	if _, err := r.Find(ctx, "*.ts", FindOptions{Path: "", MaxResults: 0}); !errors.Is(err, context.Canceled) {
		t.Fatalf("find with a cancelled context: got %v, want context.Canceled", err)
	}
}

// Cancelling while the workers are running must not deadlock or double-close
// the queue that feeds them: the cancel path closes it and waits, and the
// normal path closes it again straight after. Whether this particular run
// gets far enough to be cancelled is a race, which is the point -- both
// outcomes have to be safe.
func TestGrepCancelDuringWorkIsSafe(t *testing.T) {
	r := vendorTree(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A pattern that matches everything, cancelled after the walk has started
	// collecting but before the workers can drain a full tree.
	go cancel()
	_, err := r.Grep(ctx, ".", GrepOptions{})
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected error: %v", err)
	}
}
