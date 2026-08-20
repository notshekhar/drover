package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func requireGit(t *testing.T) {
	t.Helper()
	if err := Available(""); err != nil {
		t.Skip("git is not available")
	}
}

// fixture builds a checkout with the shape the operations have to survive:
// several authors, a rename, a deletion, a tag, and a string that appears in
// one commit and disappears in another.
func fixture(t *testing.T) *Repos {
	t.Helper()
	requireGit(t)

	root := t.TempDir()
	dir := filepath.Join(root, "repos", "api")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	run := func(env []string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		), env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	as := func(name, email string) []string {
		return []string{
			"GIT_AUTHOR_NAME=" + name, "GIT_AUTHOR_EMAIL=" + email,
			"GIT_COMMITTER_NAME=" + name, "GIT_COMMITTER_EMAIL=" + email,
		}
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ann := as("Ann", "ann@example.com")
	bob := as("Bob", "bob@example.com")

	run(nil, "init", "-q", "-b", "main")
	write("README.md", "hello\n")
	write("internal/db.go", "package internal\n\nconst secretToken = \"abc\"\n")
	run(ann, "add", ".")
	run(ann, "commit", "-qm", "first commit")

	write("internal/db.go", "package internal\n\nfunc Connect() error { return nil }\n")
	run(bob, "add", ".")
	run(bob, "commit", "-qm", "drop the token\n\nIt should never have been checked in.")
	run(bob, "tag", "-a", "v1.0.0", "-m", "release one")

	run(ann, "mv", "internal/db.go", "internal/database.go")
	write("internal/database.go", "package internal\n\nfunc Connect() error { return nil }\n\nfunc Close() {}\n")
	write("notes.txt", "scratch\n")
	run(ann, "add", "-A")
	run(ann, "commit", "-qm", "rename db to database")

	run(ann, "rm", "-q", "notes.txt")
	run(ann, "commit", "-qm", "remove notes")

	r := New(root)
	r.Timeout = 60 * time.Second
	return r
}

func run(t *testing.T, r *Repos, opts Options) *Result {
	t.Helper()
	res, err := r.Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("%s: %v", opts.Operation, err)
	}
	return res
}

func TestLog(t *testing.T) {
	r := fixture(t)
	res := run(t, r, Options{Operation: "log"})
	if len(res.Commits) != 4 {
		t.Fatalf("want 4 commits, got %d", len(res.Commits))
	}
	if res.Commits[0].Subject != "remove notes" {
		t.Errorf("newest first: got %q", res.Commits[0].Subject)
	}
	if res.Repository != "api" {
		t.Errorf("a single repository should be inferred, got %q", res.Repository)
	}
	if res.Branch != "main" {
		t.Errorf("branch = %q", res.Branch)
	}
	// --shortstat has to land in a field of its own, not glued to the body.
	if res.Commits[0].Files != 1 || res.Commits[0].Deletions != 1 {
		t.Errorf("shortstat not parsed: %+v", res.Commits[0])
	}
	if body := res.Commits[2].Body; body != "It should never have been checked in." {
		t.Errorf("body = %q", body)
	}
}

func TestLogFiltersAndFollowsRenames(t *testing.T) {
	r := fixture(t)

	byAuthor := run(t, r, Options{Operation: "log", Author: "bob@example.com"})
	if len(byAuthor.Commits) != 1 || byAuthor.Commits[0].Subject != "drop the token" {
		t.Errorf("author filter: %+v", byAuthor.Commits)
	}

	byMessage := run(t, r, Options{Operation: "log", Grep: "RENAME"})
	if len(byMessage.Commits) != 1 {
		t.Errorf("grep should be case-insensitive, got %d", len(byMessage.Commits))
	}

	// The file was internal/db.go for its first two commits; --follow is what
	// makes its history survive the rename.
	followed := run(t, r, Options{Operation: "log", Path: "internal/database.go"})
	if len(followed.Commits) != 3 {
		t.Errorf("want the file's whole history through the rename, got %d", len(followed.Commits))
	}

	// The repository-prefixed form the file tools hand out has to work too.
	prefixed := run(t, r, Options{Operation: "log", Path: "api/internal/database.go"})
	if len(prefixed.Commits) != len(followed.Commits) {
		t.Errorf("prefixed path: %d vs %d", len(prefixed.Commits), len(followed.Commits))
	}

	limited := run(t, r, Options{Operation: "log", Limit: 2})
	if len(limited.Commits) != 2 || !limited.Truncated {
		t.Errorf("limit: %d commits, truncated=%v", len(limited.Commits), limited.Truncated)
	}
}

func TestShow(t *testing.T) {
	r := fixture(t)
	head := run(t, r, Options{Operation: "log"}).Commits[1] // the rename

	res := run(t, r, Options{Operation: "show", Rev: head.Hash, Patch: true})
	if res.Commit == nil || res.Commit.Subject != "rename db to database" {
		t.Fatalf("commit = %+v", res.Commit)
	}
	var renamed, added bool
	for _, f := range res.Files {
		if strings.HasPrefix(f.Status, "R") && f.OldPath == "internal/db.go" && f.Path == "internal/database.go" {
			renamed = true
		}
		if f.Status == "A" && f.Path == "notes.txt" {
			added = true
		}
	}
	if !renamed {
		t.Errorf("the rename should carry both paths: %+v", res.Files)
	}
	if !added {
		t.Errorf("the added file is missing: %+v", res.Files)
	}
	if !strings.Contains(res.Patch, "func Close()") {
		t.Errorf("patch missing the change:\n%s", res.Patch)
	}
}

func TestDiff(t *testing.T) {
	r := fixture(t)
	res := run(t, r, Options{Operation: "diff", From: "HEAD~3", To: "HEAD"})
	if len(res.Files) == 0 {
		t.Fatal("no files changed")
	}
	for _, f := range res.Files {
		if f.Path == "internal/database.go" && f.Insertions == 0 {
			t.Errorf("numstat did not line up with name-status: %+v", f)
		}
	}
	if res.Patch != "" {
		t.Error("the patch should be opt-in")
	}

	if _, err := r.Run(context.Background(), Options{Operation: "diff"}); err == nil {
		t.Error("diff without from should say so")
	}
}

func TestBlame(t *testing.T) {
	r := fixture(t)
	res := run(t, r, Options{Operation: "blame", Path: "internal/database.go"})
	if len(res.Blame) != 5 {
		t.Fatalf("want a line per line of the file, got %d", len(res.Blame))
	}
	if res.Blame[0].Line != 1 || res.Blame[0].Author == "" || res.Blame[0].Date == "" {
		t.Errorf("blame line = %+v", res.Blame[0])
	}
	if !strings.Contains(res.Blame[4].Text, "func Close()") {
		t.Errorf("line 5 = %q", res.Blame[4].Text)
	}

	ranged := run(t, r, Options{Operation: "blame", Path: "internal/database.go", Lines: "3-5"})
	if len(ranged.Blame) != 3 || ranged.Blame[0].Line != 3 {
		t.Errorf("range: %d lines starting at %d", len(ranged.Blame), ranged.Blame[0].Line)
	}
}

func TestSearchFindsWhenAStringLeft(t *testing.T) {
	r := fixture(t)
	res := run(t, r, Options{Operation: "search", Query: "secretToken"})
	if len(res.Commits) != 2 {
		t.Fatalf("the commit that added it and the one that removed it: got %d", len(res.Commits))
	}
	if res.Commits[0].Subject != "drop the token" {
		t.Errorf("newest first: %q", res.Commits[0].Subject)
	}

	none := run(t, r, Options{Operation: "search", Query: "nothingLikeThis"})
	if len(none.Commits) != 0 || none.Note == "" {
		t.Errorf("an empty search should explain itself: %+v", none)
	}
}

func TestFileAtRevision(t *testing.T) {
	r := fixture(t)
	res := run(t, r, Options{Operation: "file", Path: "internal/db.go", Rev: "HEAD~3"})
	if !strings.Contains(res.Content, "secretToken") {
		t.Errorf("want the old contents, got %q", res.Content)
	}
	// The path does not exist at HEAD, which is the whole point.
	if _, err := r.Run(context.Background(), Options{Operation: "file", Path: "internal/db.go"}); err == nil {
		t.Error("a path missing at HEAD should fail")
	}
}

func TestBranchesAndTags(t *testing.T) {
	r := fixture(t)
	branches := run(t, r, Options{Operation: "branches"})
	if len(branches.Refs) != 1 || branches.Refs[0].Name != "main" || !branches.Refs[0].Head {
		t.Errorf("branches = %+v", branches.Refs)
	}
	tags := run(t, r, Options{Operation: "tags"})
	if len(tags.Refs) != 1 || tags.Refs[0].Name != "v1.0.0" {
		t.Fatalf("tags = %+v", tags.Refs)
	}
	if tags.Refs[0].Subject != "release one" {
		t.Errorf("annotated tag subject = %q", tags.Refs[0].Subject)
	}
}

func TestContributors(t *testing.T) {
	r := fixture(t)
	res := run(t, r, Options{Operation: "contributors"})
	if len(res.Authors) != 2 {
		t.Fatalf("want two authors, got %+v", res.Authors)
	}
	if res.Authors[0].Name != "Ann" || res.Authors[0].Commits != 3 {
		t.Errorf("busiest first: %+v", res.Authors[0])
	}
	if res.Authors[0].First == "" || res.Authors[0].Last == "" {
		t.Errorf("first and last commit dates: %+v", res.Authors[0])
	}
}

func TestStatus(t *testing.T) {
	r := fixture(t)
	res := run(t, r, Options{Operation: "status"})
	st := res.Status
	if st == nil {
		t.Fatal("no status")
	}
	if st.Branch != "main" || st.Commits != 4 {
		t.Errorf("status = %+v", st)
	}
	if st.Head.Subject != "remove notes" {
		t.Errorf("head = %+v", st.Head)
	}
	if !st.Clean {
		t.Errorf("a fresh checkout is clean, got %v", st.Dirty)
	}
	if st.FirstDate == "" {
		t.Error("the root commit dates the repository")
	}

	// A mirror that has been written into is about to lose that work on the
	// next sync, so status has to notice.
	if err := os.WriteFile(filepath.Join(r.Dir, "api", "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	again := run(t, r, Options{Operation: "status"})
	if again.Status.Clean || len(again.Status.Dirty) != 1 {
		t.Errorf("dirty tree not reported: %+v", again.Status)
	}
}

func TestRefusals(t *testing.T) {
	r := fixture(t)
	for _, tc := range []struct {
		name string
		opts Options
		want string
	}{
		{"unknown operation", Options{Operation: "push"}, "unknown operation"},
		{"missing operation", Options{}, "operation is required"},
		{"unknown repository", Options{Operation: "log", Repository: "nope"}, "no checkout named"},
		{"repository with a slash", Options{Operation: "log", Repository: "../../etc"}, "not a repository name"},
		{"path walking up", Options{Operation: "log", Path: "../../etc/passwd"}, "walks upwards"},
		{"rev that is a flag", Options{Operation: "show", Rev: "--upload-pack=touch"}, "is not a revision"},
		{"blame with no path", Options{Operation: "blame"}, "needs a path"},
		{"search with no query", Options{Operation: "search"}, "needs a query"},
		{"bad line range", Options{Operation: "blame", Path: "README.md", Lines: "nonsense"}, "should be a range"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.Run(context.Background(), tc.opts)
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestRepositoryIsRequiredWhenAmbiguous(t *testing.T) {
	r := fixture(t)
	second := filepath.Join(r.Dir, "web")
	if err := os.MkdirAll(filepath.Join(second, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := r.Run(context.Background(), Options{Operation: "log"})
	if err == nil || !strings.Contains(err.Error(), "repository is required") {
		t.Fatalf("want a request to name one, got %v", err)
	}
	if !strings.Contains(err.Error(), "api, web") {
		t.Errorf("the message should list what there is: %v", err)
	}
}
