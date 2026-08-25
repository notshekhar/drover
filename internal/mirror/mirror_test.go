package mirror

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/notshekhar/drover/internal/object"
)

// fakeFetch answers from canned pages, and records what was asked for.
type fakeFetch struct {
	pages map[string][]string // path -> body per page
	calls []string
	fail  error
}

func (f *fakeFetch) Describe() string { return "fake" }

func (f *fakeFetch) Get(_ context.Context, _ Slug, path string, params url.Values) ([]byte, error) {
	f.calls = append(f.calls, path+"?"+params.Encode())
	if f.fail != nil {
		return nil, f.fail
	}
	page := 1
	if v := params.Get("page"); v != "" {
		fmt.Sscanf(v, "%d", &page)
	}
	bodies := f.pages[path]
	if page-1 >= len(bodies) {
		return []byte("[]"), nil
	}
	return []byte(bodies[page-1]), nil
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newMirror(t *testing.T, f Fetcher) (*Mirror, string) {
	t.Helper()
	dir := t.TempDir()
	return &Mirror{DataDir: dir, Fetch: f, Now: func() time.Time { return time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC) }}, dir
}

func sampleItems(t *testing.T) string {
	t.Helper()
	created := time.Date(2026, 3, 4, 9, 12, 0, 0, time.UTC)
	merged := time.Date(2026, 3, 6, 14, 2, 11, 0, time.UTC)
	return jsonOf(t, []map[string]any{
		{
			"number": 12, "title": "rate limit is wrong", "state": "open",
			"body":       "the webhook endpoint accepts everything\r\nsecond line",
			"html_url":   "https://github.com/acme/api/issues/12",
			"created_at": created, "updated_at": created, "comments": 1,
			"user":   map[string]any{"login": "someone"},
			"labels": []map[string]any{{"name": "backend"}, {"name": "bug"}},
		},
		{
			"number": 5678, "title": "rate limit the webhook endpoint", "state": "closed",
			"body": "closes #12", "html_url": "https://github.com/acme/api/pull/5678",
			"created_at": created, "updated_at": merged,
			"user":         map[string]any{"login": "another"},
			"pull_request": map[string]any{"merged_at": merged},
		},
	})
}

func TestSyncWritesIssuesAndPulls(t *testing.T) {
	f := &fakeFetch{pages: map[string][]string{
		"repos/acme/api/issues": {sampleItems(t)},
		"repos/acme/api/issues/comments": {jsonOf(t, []map[string]any{{
			"issue_url":  "https://api.github.com/repos/acme/api/issues/12",
			"body":       "agreed, it is unbounded",
			"created_at": time.Date(2026, 3, 5, 11, 0, 0, 0, time.UTC),
			"user":       map[string]any{"login": "another"},
		}})},
	}}
	m, dir := newMirror(t, f)

	res, err := m.Sync(context.Background(), "api", "https://github.com/acme/api", "",
		&object.MirrorSpec{Issues: true, PullRequests: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Issues != 1 || res.Pulls != 1 || res.Comments != 1 {
		t.Fatalf("result was %+v", res)
	}
	if !res.Complete {
		t.Error("a single short page was reported as incomplete")
	}

	issue := readFile(t, filepath.Join(dir, "mirrors", "api", "issues", "12.md"))
	for _, want := range []string{
		"number: 12", "kind: issue", "state: open",
		"labels:", "- backend", "# rate limit is wrong",
		"## comment: another", "agreed, it is unbounded",
	} {
		if !strings.Contains(issue, want) {
			t.Errorf("the mirrored issue is missing %q:\n%s", want, issue)
		}
	}
	// GitHub stores bodies with CRLF. Left in, every grep result from a
	// mirrored file ends in a stray carriage return.
	if strings.Contains(issue, "\r") {
		t.Error("a carriage return survived into the mirrored file")
	}

	pull := readFile(t, filepath.Join(dir, "mirrors", "api", "pulls", "5678.md"))
	if !strings.Contains(pull, "state: merged") {
		t.Errorf("a merged pull request was not reported as merged:\n%s", pull)
	}
	if _, err := os.Stat(filepath.Join(dir, "mirrors", "api", "issues", "5678.md")); err == nil {
		t.Error("a pull request was written into issues/ as well")
	}
}

// A file is only rewritten with a thread when one was fetched. An item-only
// update must not drop the discussion already on disk.
func TestItemUpdateKeepsTheThread(t *testing.T) {
	f := &fakeFetch{pages: map[string][]string{
		"repos/acme/api/issues": {sampleItems(t)},
		"repos/acme/api/issues/comments": {jsonOf(t, []map[string]any{{
			"issue_url":  "https://api.github.com/repos/acme/api/issues/12",
			"body":       "agreed, it is unbounded",
			"created_at": time.Date(2026, 3, 5, 11, 0, 0, 0, time.UTC),
			"user":       map[string]any{"login": "another"},
		}})},
	}}
	m, dir := newMirror(t, f)
	spec := &object.MirrorSpec{Issues: true, PullRequests: true}
	if _, err := m.Sync(context.Background(), "api", "https://github.com/acme/api", "", spec); err != nil {
		t.Fatal(err)
	}

	// Second run: the item stream still has the issue, the comment stream has
	// moved past it.
	f.pages["repos/acme/api/issues/comments"] = []string{"[]"}
	if _, err := m.Sync(context.Background(), "api", "https://github.com/acme/api", "", spec); err != nil {
		t.Fatal(err)
	}
	issue := readFile(t, filepath.Join(dir, "mirrors", "api", "issues", "12.md"))
	if !strings.Contains(issue, "agreed, it is unbounded") {
		t.Errorf("the thread was dropped on an item-only update:\n%s", issue)
	}
}

// The cursor is what makes a sync incremental. It must be written, and the
// next run must ask for what changed since it, with an overlap.
func TestCursorMakesTheNextSyncIncremental(t *testing.T) {
	f := &fakeFetch{pages: map[string][]string{"repos/acme/api/issues": {sampleItems(t)}}}
	m, dir := newMirror(t, f)
	spec := &object.MirrorSpec{Issues: true, PullRequests: true, Comments: boolPtr(false)}

	if _, err := m.Sync(context.Background(), "api", "https://github.com/acme/api", "", spec); err != nil {
		t.Fatal(err)
	}
	cursor := readFile(t, filepath.Join(dir, "mirrors", "api", "cursor.yaml"))
	if !strings.Contains(cursor, "2026-03-06T14:02:11Z") {
		t.Fatalf("the cursor did not record the newest updated_at:\n%s", cursor)
	}

	f.calls = nil
	if _, err := m.Sync(context.Background(), "api", "https://github.com/acme/api", "", spec); err != nil {
		t.Fatal(err)
	}
	// 14:02:11 minus the five-minute overlap.
	if !strings.Contains(strings.Join(f.calls, " "), "since=2026-03-06T13%3A57%3A11Z") {
		t.Errorf("the second sync did not resume from the cursor with an overlap: %v", f.calls)
	}
}

// A rate limit is a state, not a failure: the mirror keeps what it has, says
// when to come back, and does not advance the cursor over pages it never read.
func TestRateLimitIsAState(t *testing.T) {
	reset := time.Date(2026, 8, 25, 1, 0, 0, 0, time.UTC)
	f := &fakeFetch{fail: &RateLimitError{Reset: reset}}
	m, _ := newMirror(t, f)

	res, err := m.Sync(context.Background(), "api", "https://github.com/acme/api", "",
		&object.MirrorSpec{Issues: true})
	if err != nil {
		t.Fatalf("a rate limit was returned as an error: %v", err)
	}
	if res.RateLimit == nil || res.Complete {
		t.Fatalf("result was %+v", res)
	}
	if !strings.Contains(res.Summary(), "2026-08-25T01:00:00Z") {
		t.Errorf("the summary does not say when to come back: %s", res.Summary())
	}
}

func TestNonGitHubIsRefusedClearly(t *testing.T) {
	m, _ := newMirror(t, &fakeFetch{})
	_, err := m.Sync(context.Background(), "api", "https://gitlab.com/acme/api", "",
		&object.MirrorSpec{Issues: true})
	if err == nil || !strings.Contains(err.Error(), "GitHub") {
		t.Fatalf("a GitLab url produced %v", err)
	}
}

// The commit index is the hop that turns blame into intent, and it costs no
// API call: both of GitHub's merge styles put the number in the subject.
func TestCommitIndexFromLocalHistory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	m, dir := newMirror(t, &fakeFetch{pages: map[string][]string{"repos/acme/api/issues": {"[]"}}})
	checkout := filepath.Join(dir, "checkout")
	initRepo(t, checkout,
		"rate limit the webhook endpoint (#5678)",
		"Merge pull request #91 from acme/fix",
		"a commit that names nothing",
	)

	if _, err := m.Sync(context.Background(), "api", "https://github.com/acme/api", checkout,
		&object.MirrorSpec{Issues: true}); err != nil {
		t.Fatal(err)
	}
	index := readFile(t, filepath.Join(dir, "mirrors", "api", "index", "commits.tsv"))
	for _, want := range []string{"\t5678\t", "\t91\t"} {
		if !strings.Contains(index, want) {
			t.Errorf("the index is missing %q:\n%s", want, index)
		}
	}
	if strings.Count(index, "\n") != 3 { // header plus two rows
		t.Errorf("a commit naming nothing was indexed anyway:\n%s", index)
	}
}

func boolPtr(b bool) *bool { return &b }

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func initRepo(t *testing.T, dir string, subjects ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	for i, subject := range subjects {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		run("add", ".")
		run("commit", "-q", "-m", subject)
	}
}
