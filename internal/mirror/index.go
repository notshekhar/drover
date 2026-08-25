package mirror

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/notshekhar/drover/internal/atomicfile"
)

// maxIndexedCommits bounds the index on a repository with a long history.
// The newest commits are the ones anyone asks about.
const maxIndexedCommits = 20000

// refPattern finds the pull request or issue a commit names.
//
// Both of GitHub's merge styles put the number in the subject: a merge commit
// says "Merge pull request #5678 from ...", a squash says "title (#5678)".
// Anything else a person wrote by hand -- "fixes #1234" -- lands here too,
// which is a bonus rather than the point.
var refPattern = regexp.MustCompile(`#(\d+)`)

// writeCommitIndex builds index/commits.tsv from the checkout's own history.
//
// It costs no API call, which is why it is built this way. And it is the
// highest-value artefact this package produces, because it is the hop that
// turns blame into intent:
//
//	git blame -> a1b2c3d -> grep a1b2c3d index/commits.tsv -> pull 5678 -> read it
//
// What it cannot do is index a commit whose message names nothing. That is a
// real gap and there is no local way to close it: the association lives only
// in GitHub's own database.
func (m *Mirror) writeCommitIndex(ctx context.Context, dir, checkout string) (int, error) {
	if _, err := os.Stat(filepath.Join(checkout, ".git")); err != nil {
		return 0, err
	}
	cmd := exec.CommandContext(ctx, "git", "-C", checkout,
		// Merge commits are kept, deliberately: on a merge-commit workflow
		// they are the only commits that name the pull request at all.
		"log", "--no-color",
		"--max-count="+strconv.Itoa(maxIndexedCommits),
		"--format=%H%x1f%s")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("git log for the commit index: %w", err)
	}

	type row struct {
		sha     string
		number  int
		subject string
	}
	var rows []row
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		sha, subject, ok := strings.Cut(line, "\x1f")
		if !ok {
			continue
		}
		seen := map[int]bool{}
		for _, match := range refPattern.FindAllStringSubmatch(subject, -1) {
			n, err := strconv.Atoi(match[1])
			if err != nil || n <= 0 || seen[n] {
				continue
			}
			seen[n] = true
			rows = append(rows, row{sha: sha, number: n, subject: subject})
		}
	}

	// Sorted by sha, so the file is stable across runs and a re-index of an
	// unchanged history rewrites identical bytes.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].sha != rows[j].sha {
			return rows[i].sha < rows[j].sha
		}
		return rows[i].number < rows[j].number
	})

	var b bytes.Buffer
	b.WriteString("# commit\tnumber\tsubject -- grep a sha here to find the pull request that carried it\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "%s\t%d\t%s\n", r.sha, r.number, sanitiseTSV(r.subject))
	}
	if err := os.MkdirAll(filepath.Join(dir, "index"), 0o755); err != nil {
		return 0, err
	}
	if err := atomicfile.Write(filepath.Join(dir, "index", "commits.tsv"), b.Bytes(), 0o644); err != nil {
		return 0, err
	}
	return len(rows), nil
}

// sanitiseTSV keeps one row on one line with a fixed number of fields. A
// commit subject cannot contain a newline, but it can contain a tab.
func sanitiseTSV(s string) string {
	return strings.NewReplacer("\t", " ", "\r", "", "\n", " ").Replace(s)
}
