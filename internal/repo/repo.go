// Package repo reconciles one Repository object into a checkout on disk.
//
// The tree drover keeps is a mirror of a remote branch, not a place people
// commit: reconcile resets it to the remote every time. That is only safe
// because reconcile refuses to touch a directory drover did not create.
package repo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/notshekhar/drover/internal/object"
)

// markerFile lives inside .git, so it is invisible to an agent grepping the
// worktree but still travels with the clone. Its presence is what tells
// reconcile the directory is drover's to reset.
const markerFile = "drover-clone"

// DefaultTimeout caps a single git invocation. Without it a prompt or a dead
// host would hold a repository's goroutine forever.
const DefaultTimeout = 10 * time.Minute

// Reconciler turns Repository objects into checkouts under <data>/repos.
type Reconciler struct {
	DataDir string
	Timeout time.Duration
	Git     string // git binary, for tests; defaults to "git"
}

// New returns a reconciler rooted at the data directory.
func New(dataDir string) *Reconciler {
	return &Reconciler{DataDir: dataDir, Timeout: DefaultTimeout, Git: "git"}
}

// Root is the directory holding every checkout.
func (r *Reconciler) Root() string { return filepath.Join(r.DataDir, "repos") }

// Path is where one repository is checked out.
func (r *Reconciler) Path(name string) string { return filepath.Join(r.Root(), name) }

// Result describes what a reconcile did.
type Result struct {
	Cloned  bool
	Updated bool
	Commit  string
	Branch  string
}

// Reconcile brings the checkout in line with the spec.
//
//  1. missing        -> clone --single-branch --branch <branch>
//  2. present        -> fetch origin <branch>, then checkout -B <branch> origin/<branch>
//  3. url changed    -> point origin at the new url and carry on, rather than
//     making the user delete and re-apply
func (r *Reconciler) Reconcile(ctx context.Context, name string, spec *object.RepositorySpec) (*Result, error) {
	if err := object.ValidateName(name); err != nil {
		return nil, fmt.Errorf("metadata.name: %w", err)
	}
	path := r.Path(name)

	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := r.clone(ctx, path, spec); err != nil {
			return nil, err
		}
		res := &Result{Cloned: true, Branch: spec.Branch}
		res.Commit, _ = r.head(ctx, path)
		return res, nil

	case err != nil:
		return nil, err

	case !info.IsDir():
		return nil, fmt.Errorf("%s exists and is not a directory", path)
	}

	// The directory is there. Only touch it if drover made it -- step 2
	// discards local work, and doing that to someone's real checkout because
	// a name happened to collide would be unforgivable.
	if err := r.checkMarker(path, name); err != nil {
		return nil, err
	}
	if err := r.ensureRemote(ctx, path, spec.URL); err != nil {
		return nil, err
	}
	if err := r.fetch(ctx, path, spec.Branch); err != nil {
		return nil, err
	}
	if err := r.reset(ctx, path, spec.Branch); err != nil {
		return nil, err
	}

	res := &Result{Updated: true, Branch: spec.Branch}
	res.Commit, _ = r.head(ctx, path)
	return res, nil
}

// Remove deletes a checkout.
func (r *Reconciler) Remove(name string) error {
	if err := object.ValidateName(name); err != nil {
		return fmt.Errorf("metadata.name: %w", err)
	}
	return os.RemoveAll(r.Path(name))
}

func (r *Reconciler) clone(ctx context.Context, path string, spec *object.RepositorySpec) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// "--" keeps a url or path that starts with a dash from being read as a
	// flag, on top of the check the spec already does.
	_, err := r.git(ctx, "", "clone", "--single-branch", "--branch", spec.Branch, "--", spec.URL, path)
	if err != nil {
		// Leave nothing half-cloned behind, or the next reconcile finds a
		// directory with no marker and refuses to touch it forever.
		os.RemoveAll(path)
		return fmt.Errorf("clone %s: %w", spec.URL, err)
	}
	return r.writeMarker(path)
}

func (r *Reconciler) ensureRemote(ctx context.Context, path, want string) error {
	got, err := r.git(ctx, path, "remote", "get-url", "origin")
	if err != nil {
		return fmt.Errorf("read origin: %w", err)
	}
	if strings.TrimSpace(got) == want {
		return nil
	}
	if _, err := r.git(ctx, path, "remote", "set-url", "origin", "--", want); err != nil {
		return fmt.Errorf("point origin at %s: %w", want, err)
	}
	return nil
}

func (r *Reconciler) fetch(ctx context.Context, path, branch string) error {
	if _, err := r.git(ctx, path, "fetch", "origin", "--", branch); err != nil {
		return fmt.Errorf("fetch %s: %w", branch, err)
	}
	return nil
}

// reset moves the worktree onto the remote branch, discarding whatever was
// there. This tree is a mirror, so that is the intent, not a side effect.
func (r *Reconciler) reset(ctx context.Context, path, branch string) error {
	if _, err := r.git(ctx, path, "checkout", "-B", branch, "FETCH_HEAD"); err != nil {
		return fmt.Errorf("checkout %s: %w", branch, err)
	}
	if _, err := r.git(ctx, path, "reset", "--hard", "FETCH_HEAD"); err != nil {
		return fmt.Errorf("reset to %s: %w", branch, err)
	}
	return nil
}

func (r *Reconciler) head(ctx context.Context, path string) (string, error) {
	out, err := r.git(ctx, path, "rev-parse", "HEAD")
	return strings.TrimSpace(out), err
}

// --- marker ---

func (r *Reconciler) markerPath(path string) string {
	return filepath.Join(path, ".git", markerFile)
}

func (r *Reconciler) writeMarker(path string) error {
	return os.WriteFile(r.markerPath(path), []byte("this checkout is managed by drover; it is reset to its remote branch on every sync\n"), 0o644)
}

// checkMarker refuses a directory drover did not create.
func (r *Reconciler) checkMarker(path, name string) error {
	if _, err := os.Stat(r.markerPath(path)); err == nil {
		return nil
	}
	return fmt.Errorf("%s already exists but drover did not create it; reconcile resets a checkout to its remote and will not do that to a directory it does not own (move it aside, or give the Repository a different name than %q)", path, name)
}

// --- git ---

func (r *Reconciler) git(ctx context.Context, dir string, args ...string) (string, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	bin := r.Git
	if bin == "" {
		bin = "git"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir

	// Never let git stop and ask. A prompt in a background sync is a hang,
	// and a hang is much harder to diagnose than a failure.
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"GIT_SSH_COMMAND=ssh -oBatchMode=yes -oStrictHostKeyChecking=accept-new",
	)

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("git %s timed out after %s", args[0], timeout)
		}
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(firstLines(msg, 3))
	}
	return stdout.String(), nil
}

// firstLines trims git's chattier failures down to something readable in a
// status column.
func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(strings.Join(lines[:n], "\n"))
}

// Available reports whether git is usable, so callers can fail early with a
// clear message instead of at the first clone.
func Available(bin string) error {
	if bin == "" {
		bin = "git"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("git is not on PATH; drover shells out to git to clone and fetch")
	}
	return nil
}
