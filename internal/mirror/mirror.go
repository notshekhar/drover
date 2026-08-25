package mirror

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/notshekhar/drover/internal/atomicfile"
	"github.com/notshekhar/drover/internal/object"
)

// maxPagesPerRun bounds one sync.
//
// A repository with 40,000 issues is 400 pages, and doing that inside a
// reconcile tick would stall the worker for minutes and burn the hour's rate
// limit in one go. Instead a backfill is spread across ticks: the cursor only
// advances over pages that completed, so the next run picks up where this one
// stopped and the mirror is usable -- partially -- the whole way through.
const maxPagesPerRun = 20

// Mirror writes the discussion around a checkout beside it.
type Mirror struct {
	DataDir string
	Fetch   Fetcher
	Now     func() time.Time
}

// New builds a Mirror over a data directory.
func New(dataDir string, client *http.Client) *Mirror {
	return &Mirror{DataDir: dataDir, Fetch: NewFetcher(client), Now: time.Now}
}

// Root is the directory every mirror lives under. It is a top-level root of
// the file jail, reached by an agent as mirrors/<repository>/...
func (m *Mirror) Root() string { return filepath.Join(m.DataDir, "mirrors") }

// Path is one repository's mirror directory.
func (m *Mirror) Path(name string) string { return filepath.Join(m.Root(), name) }

// Result is what one sync did, for the status line.
type Result struct {
	Issues    int
	Pulls     int
	Comments  int
	Commits   int
	Complete  bool // false when a page cap or a rate limit stopped it early
	RateLimit *RateLimitError
	Via       string
}

// Summary is the one line `drover get repository` prints.
func (r *Result) Summary() string {
	parts := []string{fmt.Sprintf("%d issues", r.Issues), fmt.Sprintf("%d pulls", r.Pulls)}
	if r.Commits > 0 {
		parts = append(parts, fmt.Sprintf("%d indexed commits", r.Commits))
	}
	s := strings.Join(parts, ", ")
	switch {
	case r.RateLimit != nil:
		return s + " (paused: " + r.RateLimit.Error() + ")"
	case !r.Complete:
		return s + " (backfilling; more on the next sync)"
	}
	return s
}

// cursor is the high-water mark per stream, so a sync fetches what changed
// rather than everything.
type cursor struct {
	Items    string `yaml:"items,omitempty"`
	Comments string `yaml:"comments,omitempty"`
}

// overlap is subtracted from a stored cursor before it is used.
//
// GitHub's updated_at is not monotonic across pages: an item updated while
// the walk is in progress moves, and a strict since= boundary can step over
// it. Re-fetching a few minutes of already-seen items every run is cheap;
// missing one silently is not.
const overlap = 5 * time.Minute

// Sync brings one repository's mirror up to date.
//
// checkout is the path to the clone, used for the commit index; it may be
// empty, in which case the index is skipped.
func (m *Mirror) Sync(ctx context.Context, name, repoURL, checkout string, spec *object.MirrorSpec) (*Result, error) {
	if !spec.Enabled() {
		return nil, nil
	}
	slug, err := ParseSlug(repoURL)
	if err != nil {
		return nil, err
	}
	if !slug.IsGitHub() {
		return nil, fmt.Errorf("mirroring is implemented for GitHub, and %s is not a GitHub host", slug.Host)
	}

	dir := m.Path(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	cur := m.readCursor(dir)
	res := &Result{Complete: true, Via: m.Fetch.Describe()}

	since, bounded := spec.Since.Since(m.now())
	if cur.Items != "" {
		if t, err := time.Parse(time.RFC3339, cur.Items); err == nil {
			since, bounded = t.Add(-overlap), true
		}
	}

	items, high, complete, err := m.fetchItems(ctx, slug, spec, since, bounded)
	if err != nil {
		if rl := rateLimit(err); rl != nil {
			res.RateLimit, res.Complete = rl, false
			m.writeCursor(dir, cur) // keep what we had; resume from it
			return res, nil
		}
		return nil, err
	}
	res.Complete = complete

	byNumber := map[int][]comment{}
	if spec.WantComments() && len(items) > 0 {
		csince, cbounded := since, bounded
		if cur.Comments != "" {
			if t, err := time.Parse(time.RFC3339, cur.Comments); err == nil {
				csince, cbounded = t.Add(-overlap), true
			}
		}
		comments, chigh, ccomplete, err := m.fetchComments(ctx, slug, csince, cbounded)
		if err != nil {
			if rl := rateLimit(err); rl != nil {
				res.RateLimit, res.Complete = rl, false
			} else {
				return nil, err
			}
		} else {
			res.Complete = res.Complete && ccomplete
			if chigh != "" {
				cur.Comments = chigh
			}
		}
		for _, c := range comments {
			if n, ok := issueNumber(c.IssueURL); ok {
				byNumber[n] = append(byNumber[n], c)
				res.Comments++
			}
		}
	}

	for _, it := range items {
		if it.isPull() && !spec.PullRequests {
			continue
		}
		if !it.isPull() && !spec.Issues {
			continue
		}
		// A file is only rewritten when its thread was fetched too, or the
		// comments already on disk would be dropped on an item-only update.
		body, err := m.merge(dir, it, byNumber[it.Number], spec.WantComments())
		if err != nil {
			return nil, err
		}
		if err := atomicfile.Write(m.itemPath(dir, it), body, 0o644); err != nil {
			return nil, err
		}
		if it.isPull() {
			res.Pulls++
		} else {
			res.Issues++
		}
	}

	if high != "" {
		cur.Items = high
	}
	if err := m.writeCursor(dir, cur); err != nil {
		return nil, err
	}

	if checkout != "" {
		n, err := m.writeCommitIndex(ctx, dir, checkout)
		if err == nil {
			res.Commits = n
		}
	}
	return res, nil
}

// merge decides what a mirrored file should contain now.
//
// When comments were not fetched this run, the thread already on disk is kept
// rather than dropped: an item whose body changed should not lose its
// discussion because only the item stream moved.
func (m *Mirror) merge(dir string, it item, fetched []comment, wantComments bool) ([]byte, error) {
	if !wantComments || len(fetched) > 0 || it.Comments == 0 {
		return render(it, fetched)
	}
	existing, err := os.ReadFile(m.itemPath(dir, it))
	if err != nil {
		return render(it, fetched)
	}
	fresh, err := render(it, nil)
	if err != nil {
		return nil, err
	}
	if thread := threadOf(string(existing)); thread != "" {
		return append(fresh, []byte(thread)...), nil
	}
	return fresh, nil
}

// threadOf returns the comment section of an already-mirrored file.
func threadOf(s string) string {
	if i := strings.Index(s, "\n## comment: "); i >= 0 {
		return s[i:]
	}
	return ""
}

func (m *Mirror) itemPath(dir string, it item) string {
	sub := "issues"
	if it.isPull() {
		sub = "pulls"
	}
	return filepath.Join(dir, sub, strconv.Itoa(it.Number)+".md")
}

// fetchItems walks the issues stream, which carries pull requests too.
func (m *Mirror) fetchItems(ctx context.Context, slug Slug, spec *object.MirrorSpec, since time.Time, bounded bool) ([]item, string, bool, error) {
	state := spec.State
	if state == "" {
		state = "all"
	}
	var all []item
	high := ""
	for page := 1; ; page++ {
		if page > maxPagesPerRun {
			return all, high, false, nil
		}
		params := url.Values{}
		params.Set("state", state)
		params.Set("sort", "updated")
		params.Set("direction", "asc")
		params.Set("per_page", strconv.Itoa(PerPage))
		params.Set("page", strconv.Itoa(page))
		if bounded {
			params.Set("since", since.UTC().Format(time.RFC3339))
		}

		body, err := m.Fetch.Get(ctx, slug, fmt.Sprintf("repos/%s/%s/issues", slug.Owner, slug.Name), params)
		if err != nil {
			return all, high, false, err
		}
		got, err := decodePage[item](body)
		if err != nil {
			return all, high, false, err
		}
		for _, it := range got {
			if s := stamp(&it.UpdatedAt); s > high {
				high = s
			}
		}
		all = append(all, got...)
		if len(got) < PerPage {
			return all, high, true, nil
		}
	}
}

// fetchComments pulls every thread in one stream.
//
// The repository-wide comments endpoint is what makes mirroring discussion
// affordable: one paginated walk instead of one request per issue. On a
// repository with 2,000 issues that is 20 requests rather than 2,000.
func (m *Mirror) fetchComments(ctx context.Context, slug Slug, since time.Time, bounded bool) ([]comment, string, bool, error) {
	var all []comment
	high := ""
	for page := 1; ; page++ {
		if page > maxPagesPerRun {
			return all, high, false, nil
		}
		params := url.Values{}
		params.Set("sort", "updated")
		params.Set("direction", "asc")
		params.Set("per_page", strconv.Itoa(PerPage))
		params.Set("page", strconv.Itoa(page))
		if bounded {
			params.Set("since", since.UTC().Format(time.RFC3339))
		}

		body, err := m.Fetch.Get(ctx, slug, fmt.Sprintf("repos/%s/%s/issues/comments", slug.Owner, slug.Name), params)
		if err != nil {
			return all, high, false, err
		}
		got, err := decodePage[comment](body)
		if err != nil {
			return all, high, false, err
		}
		for _, c := range got {
			if s := stamp(&c.CreatedAt); s > high {
				high = s
			}
		}
		all = append(all, got...)
		if len(got) < PerPage {
			return all, high, true, nil
		}
	}
}

// issueNumber pulls the number out of an api issue url, which is how a bulk
// comment says which thread it belongs to.
func issueNumber(raw string) (int, bool) {
	i := strings.LastIndex(raw, "/")
	if i < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(raw[i+1:])
	if err != nil {
		return 0, false
	}
	return n, true
}

func (m *Mirror) readCursor(dir string) cursor {
	var c cursor
	data, err := os.ReadFile(filepath.Join(dir, "cursor.yaml"))
	if err != nil {
		return c
	}
	_ = yaml.Unmarshal(data, &c)
	return c
}

func (m *Mirror) writeCursor(dir string, c cursor) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	head := "# drover: how far this mirror has read. Delete to re-mirror from scratch.\n"
	return atomicfile.Write(filepath.Join(dir, "cursor.yaml"), append([]byte(head), data...), 0o644)
}

// Remove drops a repository's mirror, for `drover delete`.
func (m *Mirror) Remove(name string) error {
	if err := object.ValidateName(name); err != nil {
		return err
	}
	return os.RemoveAll(m.Path(name))
}

func (m *Mirror) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func rateLimit(err error) *RateLimitError {
	var rl *RateLimitError
	if errors.As(err, &rl) {
		return rl
	}
	return nil
}
