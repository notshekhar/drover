// Package git answers questions about the history of the checkouts drover
// holds.
//
// It is read-only by construction. Every operation maps to a git command that
// only inspects -- log, show, diff, blame, for-each-ref, rev-list, shortlog --
// and never one that writes or reaches the network. Fetching is the
// reconciler's job, and keeping the two apart is what makes it safe to hand
// this to an agent: nothing here can change a checkout, and nothing here can
// hang on a credential prompt for a remote it cannot reach.
//
// Everything is scoped to $DROVER_DATA/repos/<name>. A caller names a
// repository; it cannot name a directory.
package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultTimeout caps one git invocation. History questions are local and
// fast, so this is short enough to fail rather than hang a tool call.
const DefaultTimeout = 2 * time.Minute

// Limits keep one careless call from filling a context window. A repository
// with a hundred thousand commits will answer `log` just as happily as one
// with ten.
const (
	DefaultCommits    = 30
	MaxCommits        = 200
	DefaultBlameLines = 300
	MaxBlameLines     = 2000
	MaxPatchBytes     = 128 << 10
	MaxFileBytes      = 256 << 10
	MaxRefs           = 300
	MaxAuthors        = 200
	MaxLineLen        = 2000

	// maxOutput caps what we will buffer from one git process. A patch
	// against a vendored tree can be hundreds of megabytes, and the answer to
	// that is not to hold it in memory before throwing most of it away.
	maxOutput = 16 << 20
)

// Operations are every operation the tool understands, in the order they are
// advertised. The list is exported because the MCP schema, the CLI help and
// the docs all have to agree on it, and three hand-maintained copies would
// not stay in agreement.
var Operations = []string{
	"log", "show", "diff", "blame", "search",
	"file", "branches", "tags", "contributors", "status",
}

func known(op string) bool {
	for _, o := range Operations {
		if o == op {
			return true
		}
	}
	return false
}

// Repos runs git operations against the checkouts under Dir.
type Repos struct {
	Dir     string
	Git     string // git binary, for tests; defaults to "git"
	Timeout time.Duration
}

// New returns a Repos over the data directory's repos folder.
func New(dataDir string) *Repos {
	return &Repos{Dir: filepath.Join(dataDir, "repos"), Git: "git", Timeout: DefaultTimeout}
}

// Options is one request. Which fields matter depends on Operation; Run says
// so plainly when a required one is missing, because "invalid argument" sends
// a model guessing.
type Options struct {
	Operation  string
	Repository string
	Path       string
	Rev        string
	From       string
	To         string
	Author     string
	Since      string
	Until      string
	Grep       string
	Query      string
	Regex      bool
	Merges     string // "" | "exclude" | "only"
	Patch      bool
	Lines      string // blame range, "120-180"
	Limit      int
}

// Commit is one commit's metadata.
type Commit struct {
	Hash       string
	Short      string
	Author     string
	Email      string
	Date       string
	Committer  string
	CommitDate string
	Parents    []string
	Subject    string
	Body       string
	Files      int
	Insertions int
	Deletions  int
}

// Merge reports whether the commit has more than one parent.
func (c Commit) Merge() bool { return len(c.Parents) > 1 }

// FileChange is one path touched by a commit or a diff.
type FileChange struct {
	Status     string // A, M, D, R, C, T
	Path       string
	OldPath    string // set for renames and copies
	Insertions int
	Deletions  int
	Binary     bool
}

// BlameLine is one line of a file with the commit that last touched it.
type BlameLine struct {
	Line    int
	Hash    string
	Short   string
	Author  string
	Date    string
	Summary string
	Text    string
}

// Ref is a branch or a tag.
type Ref struct {
	Name    string
	Type    string // branch, remote, tag
	Hash    string
	Short   string
	Date    string
	Subject string
	Head    bool
}

// Author is one contributor's tally.
type Author struct {
	Name    string
	Email   string
	Commits int
	First   string
	Last    string
}

// Status is the state of one checkout.
type Status struct {
	Repository string
	URL        string
	Branch     string
	Head       Commit
	Commits    int
	FirstDate  string
	Clean      bool
	Dirty      []string
	LastFetch  string
}

// Result is what an operation returned. It is a union: which fields are set
// depends on Operation, and the transport renders whichever it finds.
type Result struct {
	Operation  string
	Repository string
	Branch     string
	Range      string
	Rev        string
	Path       string

	Commits []Commit
	Commit  *Commit
	Files   []FileChange
	Patch   string
	Blame   []BlameLine
	Refs    []Ref
	Authors []Author
	Status  *Status
	Content string

	Truncated bool
	Note      string
}

// Run dispatches one operation.
func (r *Repos) Run(ctx context.Context, opts Options) (*Result, error) {
	op := strings.TrimSpace(strings.ToLower(opts.Operation))
	if op == "" {
		return nil, fmt.Errorf("operation is required; one of %s", strings.Join(Operations, ", "))
	}
	if !known(op) {
		return nil, fmt.Errorf("unknown operation %q; one of %s", opts.Operation, strings.Join(Operations, ", "))
	}
	opts.Operation = op

	name, dir, err := r.resolve(opts.Repository)
	if err != nil {
		return nil, err
	}
	opts.Repository = name

	if err := opts.check(); err != nil {
		return nil, err
	}
	opts.Path, err = cleanPath(name, opts.Path)
	if err != nil {
		return nil, err
	}

	res := &Result{Operation: op, Repository: name, Path: opts.Path}
	switch op {
	case "log":
		err = r.log(ctx, dir, opts, res)
	case "show":
		err = r.show(ctx, dir, opts, res)
	case "diff":
		err = r.diff(ctx, dir, opts, res)
	case "blame":
		err = r.blame(ctx, dir, opts, res)
	case "search":
		err = r.search(ctx, dir, opts, res)
	case "file":
		err = r.file(ctx, dir, opts, res)
	case "branches":
		err = r.refs(ctx, dir, opts, res, false)
	case "tags":
		err = r.refs(ctx, dir, opts, res, true)
	case "contributors":
		err = r.contributors(ctx, dir, opts, res)
	case "status":
		err = r.status(ctx, dir, opts, res)
	}
	if err != nil {
		return nil, err
	}
	if res.Branch == "" {
		res.Branch, _ = r.branch(ctx, dir)
	}
	return res, nil
}

// List returns the repositories that have a checkout on disk, which is not
// the same set as the Repository objects that have been applied: one that has
// never synced, or whose clone failed, has an object but no history to ask
// about.
func (r *Repos) List() []string {
	entries, err := os.ReadDir(r.Dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(r.Dir, e.Name(), ".git")); err != nil {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// ErrNoRepository is returned when there is nothing to ask about.
var ErrNoRepository = errors.New("drover holds no repository checkouts yet")

// resolve turns a repository name into a directory.
//
// An omitted name is filled in when exactly one repository exists, because
// that is the common case and making the model name it adds a round trip.
// With several, guessing would be worse than asking.
func (r *Repos) resolve(name string) (string, string, error) {
	have := r.List()
	name = strings.TrimSpace(name)
	name = strings.Trim(name, "/")

	if name == "" {
		switch len(have) {
		case 0:
			return "", "", ErrNoRepository
		case 1:
			name = have[0]
		default:
			return "", "", fmt.Errorf("repository is required; drover holds %s", strings.Join(have, ", "))
		}
	}
	// A repository is one path segment. Rejecting the rest here means no
	// later join can walk out of the checkouts.
	if strings.ContainsAny(name, `/\`) || name == "." || name == ".." || strings.HasPrefix(name, "-") {
		return "", "", fmt.Errorf("%q is not a repository name; name one of %s", name, strings.Join(have, ", "))
	}

	dir := filepath.Join(r.Dir, name)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if len(have) == 0 {
			return "", "", ErrNoRepository
		}
		return "", "", fmt.Errorf("no checkout named %q; drover holds %s", name, strings.Join(have, ", "))
	}
	return name, dir, nil
}

// check validates the arguments that get handed to git.
//
// None of them can become a flag -- every one is passed as its own argv entry
// behind an "=" or a "--" -- but a value with a newline in it still produces
// output that cannot be parsed back, and a leading dash is far more likely to
// be a mistake than a revision.
func (o Options) check() error {
	for _, f := range []struct {
		name, value string
		rev         bool
	}{
		{"rev", o.Rev, true},
		{"from", o.From, true},
		{"to", o.To, true},
		{"author", o.Author, false},
		{"since", o.Since, false},
		{"until", o.Until, false},
		{"grep", o.Grep, false},
		{"query", o.Query, false},
	} {
		if f.value == "" {
			continue
		}
		if strings.ContainsAny(f.value, "\x00\n\r") {
			return fmt.Errorf("%s may not contain newlines", f.name)
		}
		if len(f.value) > 512 {
			return fmt.Errorf("%s is too long", f.name)
		}
		if f.rev && strings.HasPrefix(f.value, "-") {
			return fmt.Errorf("%s %q is not a revision; give a commit, branch or tag like HEAD, HEAD~3 or a sha", f.name, f.value)
		}
	}
	switch o.Merges {
	case "", "exclude", "only":
	default:
		return fmt.Errorf("merges must be %q or %q", "exclude", "only")
	}
	return nil
}

// cleanPath normalises a path argument.
//
// The file tools hand out repository-prefixed paths (api/internal/db.go), so
// a model that just grepped will pass one here. Accepting both that and the
// repository-relative form costs one TrimPrefix and saves a failed call.
func cleanPath(repo, path string) (string, error) {
	path = strings.TrimSpace(filepath.ToSlash(path))
	if path == "" {
		return "", nil
	}
	if strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("path %q must be relative to the repository root", path)
	}
	if path == repo {
		return "", nil
	}
	path = strings.TrimPrefix(path, repo+"/")
	for _, seg := range strings.Split(path, "/") {
		if seg == ".." {
			return "", fmt.Errorf("path %q walks upwards", path)
		}
	}
	return strings.TrimSuffix(path, "/"), nil
}

func (o Options) commitLimit() int {
	n := o.Limit
	if n <= 0 {
		return DefaultCommits
	}
	if n > MaxCommits {
		return MaxCommits
	}
	return n
}

func or(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// --- running git ---

// run executes git in dir and returns stdout.
//
// The environment is the same lockdown the reconciler uses: nothing here
// should ever reach the network, but a repository whose origin is an
// unreachable ssh host would otherwise be able to make a read-only tool sit
// at a password prompt until the timeout.
func (r *Repos) run(ctx context.Context, dir string, args ...string) (string, bool, error) {
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
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"GIT_PAGER=cat",
		"GIT_OPTIONAL_LOCKS=0",
		"LC_ALL=C",
	)

	out := &capped{max: maxOutput}
	var stderr strings.Builder
	cmd.Stdout = out
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if out.truncated {
			// We stopped reading, so git died of a broken pipe. That is our
			// doing, not a failure worth reporting as one.
			return out.b.String(), true, nil
		}
		if ctx.Err() == context.DeadlineExceeded {
			return "", false, fmt.Errorf("git %s timed out after %s", args[0], timeout)
		}
		if errors.Is(err, exec.ErrNotFound) {
			return "", false, errors.New("git is not on PATH; drover shells out to git to read history")
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", false, errors.New(firstLines(msg, 3))
	}
	return out.b.String(), out.truncated, nil
}

// capped is a writer that stops at max bytes instead of growing without
// bound.
type capped struct {
	b         strings.Builder
	max       int
	truncated bool
}

func (c *capped) Write(p []byte) (int, error) {
	if c.truncated {
		return len(p), nil
	}
	if room := c.max - c.b.Len(); room < len(p) {
		if room > 0 {
			c.b.Write(p[:room])
		}
		c.truncated = true
		return len(p), nil
	}
	c.b.Write(p)
	return len(p), nil
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(strings.Join(lines[:n], "\n"))
}

func (r *Repos) branch(ctx context.Context, dir string) (string, error) {
	out, _, err := r.run(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	return strings.TrimSpace(out), err
}

// Available reports whether git is usable.
func Available(bin string) error {
	if bin == "" {
		bin = "git"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return errors.New("git is not on PATH; drover shells out to git to read history")
	}
	return nil
}
