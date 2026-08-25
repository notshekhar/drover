package mirror

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// item is one issue or pull request as GitHub reports it.
//
// GitHub's /issues endpoint returns both, with pull requests carrying an
// extra pull_request key. That is why one fetch covers both streams instead
// of two, and it is the single largest saving in this package.
type item struct {
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	State     string     `json:"state"`
	Body      string     `json:"body"`
	HTMLURL   string     `json:"html_url"`
	Comments  int        `json:"comments"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	ClosedAt  *time.Time `json:"closed_at"`

	User   struct{ Login string } `json:"user"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	PullRequest *struct {
		MergedAt *time.Time `json:"merged_at"`
	} `json:"pull_request"`
}

func (i item) isPull() bool { return i.PullRequest != nil }

func (i item) kind() string {
	if i.isPull() {
		return "pull"
	}
	return "issue"
}

// state folds GitHub's two-value state plus the merge flag into the three
// words a person would actually use.
func (i item) resolvedState() string {
	if i.isPull() && i.PullRequest.MergedAt != nil {
		return "merged"
	}
	return i.State
}

func (i item) labelNames() []string {
	out := make([]string, 0, len(i.Labels))
	for _, l := range i.Labels {
		out = append(out, l.Name)
	}
	sort.Strings(out)
	return out
}

// comment is one message in a thread.
type comment struct {
	IssueURL  string                 `json:"issue_url"`
	HTMLURL   string                 `json:"html_url"`
	Body      string                 `json:"body"`
	CreatedAt time.Time              `json:"created_at"`
	User      struct{ Login string } `json:"user"`
}

// frontmatter is the structured half of a mirrored file.
//
// A struct rather than a map, so the field order is stable and a re-mirror of
// an unchanged issue produces byte-identical output. It is marshalled by the
// yaml package rather than printed, because a title can contain a colon, a
// quote or a newline, and hand-rolling the escaping is how a mirror starts
// writing files that no longer parse.
type frontmatter struct {
	Number  int      `yaml:"number"`
	Kind    string   `yaml:"kind"`
	State   string   `yaml:"state"`
	Title   string   `yaml:"title"`
	Author  string   `yaml:"author"`
	Created string   `yaml:"created"`
	Updated string   `yaml:"updated"`
	Closed  string   `yaml:"closed,omitempty"`
	Merged  string   `yaml:"merged,omitempty"`
	Labels  []string `yaml:"labels,omitempty"`
	URL     string   `yaml:"url"`
}

// render writes one issue or pull request as markdown with a frontmatter
// header.
//
// The header is what makes structure greppable without making the prose
// unreadable: `grep '^state: open' mirrors/api/issues` works, and so does
// grepping the discussion itself.
func render(it item, comments []comment) ([]byte, error) {
	fm := frontmatter{
		Number:  it.Number,
		Kind:    it.kind(),
		State:   it.resolvedState(),
		Title:   it.Title,
		Author:  it.User.Login,
		Created: stamp(&it.CreatedAt),
		Updated: stamp(&it.UpdatedAt),
		Closed:  stamp(it.ClosedAt),
		Labels:  it.labelNames(),
		URL:     it.HTMLURL,
	}
	if it.isPull() {
		fm.Merged = stamp(it.PullRequest.MergedAt)
	}

	head, err := yaml.Marshal(fm)
	if err != nil {
		return nil, err
	}

	var b strings.Builder
	b.WriteString("---\n")
	b.Write(head)
	b.WriteString("---\n\n")
	fmt.Fprintf(&b, "# %s\n\n", strings.TrimSpace(it.Title))

	body := strings.TrimSpace(it.Body)
	if body == "" {
		body = "_(no description)_"
	}
	b.WriteString(normaliseNewlines(body))
	b.WriteString("\n")

	sort.SliceStable(comments, func(i, j int) bool { return comments[i].CreatedAt.Before(comments[j].CreatedAt) })
	for _, c := range comments {
		fmt.Fprintf(&b, "\n## comment: %s — %s\n\n", orUnknown(c.User.Login), stamp(&c.CreatedAt))
		b.WriteString(normaliseNewlines(strings.TrimSpace(c.Body)))
		b.WriteString("\n")
	}
	return []byte(b.String()), nil
}

func stamp(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(deleted user)"
	}
	return s
}

// normaliseNewlines strips carriage returns.
//
// GitHub stores bodies with CRLF line endings. Left in, every grep result
// from a mirrored file ends in a stray \r, and a pattern anchored with $
// silently stops matching -- the same trap drover already hit once in the
// whole-file grep probe.
func normaliseNewlines(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}
