package files

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tree builds a fake repos root: <data>/repos/api/...
func tree(t *testing.T) *Root {
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

	write("api/README.md", "# api\nthe api service\n")
	write("api/main.go", "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n")
	write("api/internal/db.go", "package internal\n\n// TODO: connection pooling\nfunc Connect() {}\n")
	write("api/.git/config", "[core]\n")
	write("api/.gitignore", "*.tmp\n")
	write("web/index.js", "console.log('hi')\n")

	// A binary file, to prove it is skipped rather than dumped.
	if err := os.WriteFile(filepath.Join(r.Dir, "api", "logo.png"), []byte{0x89, 'P', 'N', 'G', 0x00, 0x01}, 0o644); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestListRootShowsRepositories(t *testing.T) {
	r := tree(t)
	res, err := r.List("")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range res.Entries {
		names = append(names, e.Name)
	}
	if len(names) != 2 || names[0] != "api" || names[1] != "web" {
		t.Errorf("entries = %v, want [api web]", names)
	}
}

func TestListDirectory(t *testing.T) {
	r := tree(t)
	res, err := r.List("api")
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, e := range res.Entries {
		names = append(names, e.Name)
	}
	// Directories first, then files, and .git is hidden.
	if names[0] != "internal" {
		t.Errorf("entries = %v, want the directory first", names)
	}
	for _, n := range names {
		if n == ".git" {
			t.Error(".git was listed; it is drover bookkeeping and pure noise")
		}
	}
	// .gitignore is real content and must survive.
	found := false
	for _, n := range names {
		if n == ".gitignore" {
			found = true
		}
	}
	if !found {
		t.Errorf("entries = %v, want .gitignore kept", names)
	}
	if res.Path != "api" {
		t.Errorf("path = %q", res.Path)
	}
}

func TestListOnAFileIsAnError(t *testing.T) {
	r := tree(t)
	_, err := r.List("api/main.go")
	if err == nil {
		t.Fatal("listing a file succeeded")
	}
	if !strings.Contains(err.Error(), "use read") {
		t.Errorf("error = %q, want it to point at the right tool", err)
	}
}

func TestRead(t *testing.T) {
	r := tree(t)
	res, err := r.Read("api/main.go", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "func main()") {
		t.Errorf("content = %q", res.Content)
	}
	if res.TotalLines != 5 {
		t.Errorf("total lines = %d, want 5", res.TotalLines)
	}
	if res.StartLine != 1 || res.EndLine != 5 {
		t.Errorf("range = %d..%d", res.StartLine, res.EndLine)
	}
}

func TestReadOffsetAndLimit(t *testing.T) {
	r := tree(t)
	res, err := r.Read("api/main.go", 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.StartLine != 3 || res.EndLine != 4 {
		t.Errorf("range = %d..%d, want 3..4", res.StartLine, res.EndLine)
	}
	if strings.Contains(res.Content, "package main") {
		t.Errorf("content = %q, want it to start at line 3", res.Content)
	}
	if !res.Truncated {
		t.Error("a limited read should report itself truncated")
	}
	// The caller still learns how long the file really is.
	if res.TotalLines != 5 {
		t.Errorf("total lines = %d, want 5", res.TotalLines)
	}
}

// Dumping a binary into a context window helps nobody.
func TestReadRefusesBinary(t *testing.T) {
	r := tree(t)
	_, err := r.Read("api/logo.png", 0, 0)
	if err == nil {
		t.Fatal("a binary file was read")
	}
	if !strings.Contains(err.Error(), "binary") {
		t.Errorf("error = %q", err)
	}
}

func TestReadMissingFile(t *testing.T) {
	r := tree(t)
	if _, err := r.Read("api/nope.go", 0, 0); err == nil {
		t.Fatal("reading a missing file succeeded")
	}
}

func TestGrep(t *testing.T) {
	r := tree(t)
	res, err := r.Grep(context.Background(), "TODO", GrepOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 1 {
		t.Fatalf("matches = %+v, want 1", res.Matches)
	}
	m := res.Matches[0]
	if m.Path != "api/internal/db.go" || m.Line != 3 {
		t.Errorf("match = %+v", m)
	}
	if !strings.Contains(m.Text, "connection pooling") {
		t.Errorf("text = %q", m.Text)
	}
}

func TestGrepIsCaseInsensitiveByDefault(t *testing.T) {
	r := tree(t)
	res, err := r.Grep(context.Background(), "todo", GrepOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 1 {
		t.Errorf("case-insensitive search found %d matches, want 1", len(res.Matches))
	}

	res, err = r.Grep(context.Background(), "todo", GrepOptions{CaseSensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 0 {
		t.Errorf("case-sensitive search found %d matches, want 0", len(res.Matches))
	}
}

func TestGrepScopeAndInclude(t *testing.T) {
	r := tree(t)

	res, err := r.Grep(context.Background(), "package", GrepOptions{Path: "api"})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range res.Matches {
		if !strings.HasPrefix(m.Path, "api/") {
			t.Errorf("match outside the scope: %s", m.Path)
		}
	}

	res, err = r.Grep(context.Background(), "o", GrepOptions{Include: "*.js"})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range res.Matches {
		if !strings.HasSuffix(m.Path, ".js") {
			t.Errorf("include was ignored: %s", m.Path)
		}
	}
}

func TestGrepSkipsBinaryAndGit(t *testing.T) {
	r := tree(t)
	res, err := r.Grep(context.Background(), "PNG|core", GrepOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range res.Matches {
		if strings.Contains(m.Path, ".git/") {
			t.Errorf(".git was searched: %s", m.Path)
		}
		if strings.HasSuffix(m.Path, ".png") {
			t.Errorf("a binary file was searched: %s", m.Path)
		}
	}
}

func TestGrepBadPattern(t *testing.T) {
	r := tree(t)
	if _, err := r.Grep(context.Background(), "([", GrepOptions{}); err == nil {
		t.Fatal("an invalid regular expression was accepted")
	}
}

func TestGrepTruncates(t *testing.T) {
	r := tree(t)
	res, err := r.Grep(context.Background(), ".", GrepOptions{MaxResults: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 2 || !res.Truncated {
		t.Errorf("got %d matches truncated=%v, want 2 and true", len(res.Matches), res.Truncated)
	}
}

func TestFindByName(t *testing.T) {
	r := tree(t)
	res, err := r.Find(context.Background(), "*.go", FindOptions{Path: "", MaxResults: 0})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"api/internal/db.go", "api/main.go"}
	if len(res.Paths) != len(want) {
		t.Fatalf("paths = %v, want %v", res.Paths, want)
	}
	for i := range want {
		if res.Paths[i] != want[i] {
			t.Errorf("paths = %v, want %v", res.Paths, want)
		}
	}
}

// A pattern with a slash matches the whole repo-relative path, so both forms
// do what they look like.
func TestFindByPath(t *testing.T) {
	r := tree(t)
	res, err := r.Find(context.Background(), "api/internal/*.go", FindOptions{Path: "", MaxResults: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Paths) != 1 || res.Paths[0] != "api/internal/db.go" {
		t.Errorf("paths = %v", res.Paths)
	}
}

func TestFindSkipsGit(t *testing.T) {
	r := tree(t)
	res, err := r.Find(context.Background(), "config", FindOptions{Path: "", MaxResults: 0})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range res.Paths {
		if strings.Contains(p, ".git/") {
			t.Errorf(".git was walked: %s", p)
		}
	}
}

// --- the jail ---

func TestPathTraversalIsRefused(t *testing.T) {
	r := tree(t)
	for _, bad := range []string{
		"../secrets.txt",
		"../../etc/passwd",
		"api/../../outside",
		"/etc/passwd",
		"api/../../../",
	} {
		if _, err := r.List(bad); err == nil {
			t.Errorf("List(%q) was allowed", bad)
		}
		if _, err := r.Read(bad, 0, 0); err == nil {
			t.Errorf("Read(%q) was allowed", bad)
		}
		if _, err := r.Grep(context.Background(), "x", GrepOptions{Path: bad}); err == nil {
			t.Errorf("Grep(path=%q) was allowed", bad)
		}
		if _, err := r.Find(context.Background(), "*", FindOptions{Path: bad, MaxResults: 0}); err == nil {
			t.Errorf("Find(path=%q) was allowed", bad)
		}
	}
}

// The case that actually matters: repository contents are written by whoever
// wrote the repository, and a symlink out of the tree contains no dots at all.
func TestSymlinkOutOfTheRootIsRefused(t *testing.T) {
	r := tree(t)

	outside := filepath.Join(filepath.Dir(r.Dir), "secret.txt")
	if err := os.WriteFile(outside, []byte("do not read me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(r.Dir, "api", "escape.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, err := r.Read("api/escape.txt", 0, 0)
	if err == nil {
		t.Fatal("a symlink pointing out of the checkouts was read")
	}
	if !errors.Is(err, ErrOutsideRoot) {
		t.Errorf("error = %v, want ErrOutsideRoot", err)
	}
}

// A symlinked directory pointing outside must not be walked either.
func TestGrepDoesNotFollowSymlinksOut(t *testing.T) {
	r := tree(t)

	outsideDir := filepath.Join(filepath.Dir(r.Dir), "elsewhere")
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outsideDir, "leak.txt"), []byte("NEEDLE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(r.Dir, "api", "linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	res, err := r.Grep(context.Background(), "NEEDLE", GrepOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 0 {
		t.Errorf("grep followed a symlink out of the root: %+v", res.Matches)
	}
}

// A link that stays inside the root is fine; the check is about the target,
// not about links as such.
func TestSymlinkInsideTheRootIsAllowed(t *testing.T) {
	r := tree(t)
	if err := os.Symlink(filepath.Join(r.Dir, "api", "main.go"), filepath.Join(r.Dir, "api", "alias.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	res, err := r.Read("api/alias.go", 0, 0)
	if err != nil {
		t.Fatalf("a link inside the root was refused: %v", err)
	}
	if !strings.Contains(res.Content, "func main()") {
		t.Errorf("content = %q", res.Content)
	}
}
