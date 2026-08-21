package repo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/notshekhar/drover/internal/object"
)

// requireGit skips when git is missing, so the suite still runs offline or on
// a machine without it.
func requireGit(t *testing.T) {
	t.Helper()
	if err := Available(""); err != nil {
		t.Skip("git is not available")
	}
}

// originRepo builds a real local repository to clone from. A file:// remote
// exercises the same git paths as a network one without needing a network.
func originRepo(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()

	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=drover", "GIT_AUTHOR_EMAIL=drover@example.com",
			"GIT_COMMITTER_NAME=drover", "GIT_COMMITTER_EMAIL=drover@example.com",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	run("init", "-q", "-b", branch)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "first")
	return dir
}

func commitTo(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-qm", "another"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=drover", "GIT_AUTHOR_EMAIL=drover@example.com",
			"GIT_COMMITTER_NAME=drover", "GIT_COMMITTER_EMAIL=drover@example.com",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
}

func newReconciler(t *testing.T) *Reconciler {
	t.Helper()
	r := New(t.TempDir())
	r.Timeout = 60 * time.Second
	return r
}

func TestCloneThenFetch(t *testing.T) {
	requireGit(t)
	origin := originRepo(t, "main")
	r := newReconciler(t)
	spec := &object.RepositorySpec{URL: origin, Branch: "main"}

	res, err := r.Reconcile(context.Background(), "api", spec)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Cloned {
		t.Error("first reconcile did not report a clone")
	}
	if res.Commit == "" {
		t.Error("no commit reported")
	}
	if _, err := os.Stat(filepath.Join(r.Path("api"), "README.md")); err != nil {
		t.Fatalf("the checkout has no content: %v", err)
	}

	// A new commit upstream must arrive on the next reconcile.
	commitTo(t, origin, "NEW.md", "new\n")
	res2, err := r.Reconcile(context.Background(), "api", spec)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Cloned {
		t.Error("second reconcile cloned again instead of fetching")
	}
	if res2.Commit == res.Commit {
		t.Error("the new upstream commit was not picked up")
	}
	if _, err := os.Stat(filepath.Join(r.Path("api"), "NEW.md")); err != nil {
		t.Errorf("the new file did not arrive: %v", err)
	}
}

// The tree is a mirror, so local edits are discarded on the next sync. This
// is the behaviour that makes the ownership marker necessary.
func TestReconcileDiscardsLocalChanges(t *testing.T) {
	requireGit(t)
	origin := originRepo(t, "main")
	r := newReconciler(t)
	spec := &object.RepositorySpec{URL: origin, Branch: "main"}

	if _, err := r.Reconcile(context.Background(), "api", spec); err != nil {
		t.Fatal(err)
	}
	readme := filepath.Join(r.Path("api"), "README.md")
	if err := os.WriteFile(readme, []byte("locally edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(context.Background(), "api", spec); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(readme)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello\n" {
		t.Errorf("README = %q, want the remote's content", got)
	}
}

// Reconcile resets a tree to its remote. Doing that to a directory drover did
// not create would destroy someone's work, so it refuses.
func TestRefusesDirectoryItDoesNotOwn(t *testing.T) {
	requireGit(t)
	origin := originRepo(t, "main")
	r := newReconciler(t)

	// Someone's own checkout, sitting where drover wants to put one.
	mine := r.Path("api")
	if err := os.MkdirAll(filepath.Join(mine, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	precious := filepath.Join(mine, "my-work.txt")
	if err := os.WriteFile(precious, []byte("do not delete\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := r.Reconcile(context.Background(), "api", &object.RepositorySpec{URL: origin, Branch: "main"})
	if err == nil {
		t.Fatal("reconcile touched a directory it did not create")
	}
	if !strings.Contains(err.Error(), "did not create") {
		t.Errorf("error = %q, want it to explain the refusal", err)
	}
	if _, err := os.Stat(precious); err != nil {
		t.Fatal("the existing directory was damaged")
	}
}

// Changing spec.url must move the checkout, not require delete and re-apply.
func TestURLChangeRepointsOrigin(t *testing.T) {
	requireGit(t)
	first := originRepo(t, "main")
	second := originRepo(t, "main")
	commitTo(t, second, "SECOND.md", "second\n")

	r := newReconciler(t)
	if _, err := r.Reconcile(context.Background(), "api", &object.RepositorySpec{URL: first, Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), "api", &object.RepositorySpec{URL: second, Branch: "main"}); err != nil {
		t.Fatalf("changing the url failed instead of repointing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.Path("api"), "SECOND.md")); err != nil {
		t.Errorf("the checkout did not move to the new remote: %v", err)
	}
}

// Changing spec.branch must move HEAD to the new branch.
func TestBranchChangeMovesHead(t *testing.T) {
	requireGit(t)
	origin := originRepo(t, "main")

	cmd := exec.Command("git", "checkout", "-qb", "develop")
	cmd.Dir = origin
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout: %v\n%s", err, out)
	}
	commitTo(t, origin, "DEV.md", "dev\n")

	r := newReconciler(t)
	if _, err := r.Reconcile(context.Background(), "api", &object.RepositorySpec{URL: origin, Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), "api", &object.RepositorySpec{URL: origin, Branch: "develop"}); err != nil {
		t.Fatalf("changing the branch failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.Path("api"), "DEV.md")); err != nil {
		t.Errorf("the checkout is not on the new branch: %v", err)
	}
}

// A failed clone must leave nothing behind, or the next attempt finds a
// directory with no marker and refuses it forever.
func TestFailedCloneLeavesNothingBehind(t *testing.T) {
	requireGit(t)
	r := newReconciler(t)

	_, err := r.Reconcile(context.Background(), "api", &object.RepositorySpec{
		URL:    filepath.Join(t.TempDir(), "does-not-exist"),
		Branch: "main",
	})
	if err == nil {
		t.Fatal("cloning a missing remote succeeded")
	}
	if _, statErr := os.Stat(r.Path("api")); statErr == nil {
		t.Error("a failed clone left a directory behind, which would wedge every retry")
	}
}

func TestMissingBranchFails(t *testing.T) {
	requireGit(t)
	origin := originRepo(t, "main")
	r := newReconciler(t)

	_, err := r.Reconcile(context.Background(), "api", &object.RepositorySpec{URL: origin, Branch: "nope"})
	if err == nil {
		t.Fatal("cloning a branch that does not exist succeeded")
	}
}

func TestRemove(t *testing.T) {
	requireGit(t)
	origin := originRepo(t, "main")
	r := newReconciler(t)
	if _, err := r.Reconcile(context.Background(), "api", &object.RepositorySpec{URL: origin, Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Remove("api"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.Path("api")); err == nil {
		t.Error("the checkout survived Remove")
	}
}

// A name is a path segment here too, so the reconciler re-checks it.
func TestReconcileRefusesUnsafeNames(t *testing.T) {
	r := newReconciler(t)
	for _, name := range []string{"../escape", "a/b", "API", ""} {
		if _, err := r.Reconcile(context.Background(), name, &object.RepositorySpec{URL: "https://x/y", Branch: "main"}); err == nil {
			t.Errorf("Reconcile accepted name %q", name)
		}
		if err := r.Remove(name); err == nil {
			t.Errorf("Remove accepted name %q", name)
		}
	}
}

// The marker lives in .git so an agent grepping the worktree never sees it.
func TestMarkerIsHiddenFromTheWorktree(t *testing.T) {
	requireGit(t)
	origin := originRepo(t, "main")
	r := newReconciler(t)
	if _, err := r.Reconcile(context.Background(), "api", &object.RepositorySpec{URL: origin, Branch: "main"}); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(r.Path("api"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == markerFile {
			t.Errorf("the marker is in the worktree, where an agent would grep it")
		}
	}
	if _, err := os.Stat(filepath.Join(r.Path("api"), ".git", markerFile)); err != nil {
		t.Errorf("the marker was not written: %v", err)
	}
}

// git must never stop and wait for input in a background sync -- a hang is
// far harder to diagnose than a failure.
func TestGitDoesNotPrompt(t *testing.T) {
	requireGit(t)
	r := newReconciler(t)
	r.Timeout = 20 * time.Second

	done := make(chan error, 1)
	go func() {
		_, err := r.Reconcile(context.Background(), "api", &object.RepositorySpec{
			URL:    "https://github.com/notshekhar/definitely-not-a-real-private-repo-xyz.git",
			Branch: "main",
		})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Skip("that repository unexpectedly exists")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("git hung, most likely on a credential prompt")
	}
}

// A tick that finds nothing new must not rewrite the worktree.
//
// Asserted on the git commands actually run, because that is the contract:
// `checkout -B` plus `reset --hard` rewrites the index and walks the tree,
// and doing it on every refresh interval of every repository, forever, to
// arrive at the tree already on disk is the cost this avoids.
func TestReconcileSkipsResetWhenUnchanged(t *testing.T) {
	requireGit(t)
	origin := originRepo(t, "main")
	r := newReconciler(t)
	spec := &object.RepositorySpec{URL: origin, Branch: "main"}

	first, err := r.Reconcile(context.Background(), "api", spec)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Cloned {
		t.Fatal("first reconcile did not clone")
	}

	log := recordGit(t, r)
	second, err := r.Reconcile(context.Background(), "api", spec)
	if err != nil {
		t.Fatal(err)
	}
	if second.Updated {
		t.Error("a reconcile with nothing to fetch reported an update")
	}
	if second.Commit != first.Commit {
		t.Errorf("commit moved from %s to %s with no new upstream work", first.Commit, second.Commit)
	}
	for _, line := range readLines(t, log) {
		if strings.HasPrefix(line, "checkout") || strings.HasPrefix(line, "reset") {
			t.Errorf("ran %q on a tick with nothing to fetch", line)
		}
	}
}

// recordGit points the reconciler at a wrapper that appends each invocation's
// arguments to a file, and returns that file's path.
func recordGit(t *testing.T, r *Reconciler) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the recorder is a shell script")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	wrapper := filepath.Join(dir, "git")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\nexec " + r.Git + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r.Git = wrapper
	return log
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no git calls were recorded: %v", err)
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// The skip must not swallow a local edit: this tree is a mirror, so an
// unchanged remote is not on its own a reason to leave the worktree alone.
func TestReconcileResetsDespiteUnchangedRemote(t *testing.T) {
	requireGit(t)
	origin := originRepo(t, "main")
	r := newReconciler(t)
	spec := &object.RepositorySpec{URL: origin, Branch: "main"}

	if _, err := r.Reconcile(context.Background(), "api", spec); err != nil {
		t.Fatal(err)
	}
	readme := filepath.Join(r.Path("api"), "README.md")
	want, err := os.ReadFile(readme)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(readme, []byte("locally edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(context.Background(), "api", spec); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(readme)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("README = %q, want the remote's content back", got)
	}
}
