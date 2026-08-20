package git

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The record and field separators used in every --format string.
//
// Commit bodies contain newlines and subjects contain almost anything, so a
// line-oriented format cannot be parsed back reliably. \x1e and \x1f are the
// ASCII record and unit separators and cannot appear in git output we did not
// ask for.
const (
	recordSep = "\x1e"
	fieldSep  = "\x1f"
)

// commitFormat ends with a field separator so that --shortstat's trailing
// line lands in a field of its own instead of being glued to the body.
var commitFormat = "--format=" + recordSep + strings.Join([]string{
	"%H", "%h", "%an", "%ae", "%ad", "%cn", "%cd", "%P", "%s", "%b",
}, fieldSep) + fieldSep

var shortstatRe = regexp.MustCompile(`(\d+) files? changed(?:, (\d+) insertions?\(\+\))?(?:, (\d+) deletions?\(-\))?`)

func parseCommits(out string) []Commit {
	var commits []Commit
	for _, record := range strings.Split(out, recordSep) {
		if strings.TrimSpace(record) == "" {
			continue
		}
		f := strings.Split(record, fieldSep)
		if len(f) < 10 {
			continue
		}
		c := Commit{
			Hash:       f[0],
			Short:      f[1],
			Author:     f[2],
			Email:      f[3],
			Date:       f[4],
			Committer:  f[5],
			CommitDate: f[6],
			Subject:    f[8],
			Body:       strings.TrimSpace(f[9]),
		}
		if p := strings.Fields(f[7]); len(p) > 0 {
			c.Parents = p
		}
		if len(f) > 10 {
			if m := shortstatRe.FindStringSubmatch(f[10]); m != nil {
				c.Files, _ = strconv.Atoi(m[1])
				c.Insertions, _ = strconv.Atoi(m[2])
				c.Deletions, _ = strconv.Atoi(m[3])
			}
		}
		commits = append(commits, c)
	}
	return commits
}

// logArgs assembles the shared filter flags. log and search differ only in
// what they add on top, so they share this and cannot drift on what --since
// means.
func (r *Repos) logArgs(opts Options) []string {
	args := []string{"log", "--no-color", "--date=iso-strict", commitFormat, "--shortstat",
		"-n", strconv.Itoa(opts.commitLimit())}

	if opts.Author != "" {
		args = append(args, "--author="+opts.Author, "--regexp-ignore-case")
	}
	if opts.Since != "" {
		args = append(args, "--since="+opts.Since)
	}
	if opts.Until != "" {
		args = append(args, "--until="+opts.Until)
	}
	if opts.Grep != "" {
		args = append(args, "--grep="+opts.Grep, "--regexp-ignore-case")
	}
	switch opts.Merges {
	case "exclude":
		args = append(args, "--no-merges")
	case "only":
		args = append(args, "--merges")
	}
	return args
}

// commitRange turns from/to/rev into the single revision argument git wants,
// and the human-readable label the caller gets back.
func (o Options) commitRange() (string, string) {
	switch {
	case o.From != "" && o.To != "":
		return o.From + ".." + o.To, o.From + ".." + o.To
	case o.From != "":
		return o.From + "..HEAD", o.From + "..HEAD"
	case o.To != "":
		return o.To, o.To
	case o.Rev != "":
		return o.Rev, o.Rev
	}
	return "", ""
}

// --- log ---

func (r *Repos) log(ctx context.Context, dir string, opts Options, res *Result) error {
	args := r.logArgs(opts)

	// --follow tracks a file across renames, which is usually what somebody
	// asking for one file's history wants. It only works for a single path,
	// and only for a file: pointed at a directory git rejects it outright.
	if opts.Path != "" && !r.isDir(ctx, dir, opts.Path) {
		args = append(args, "--follow")
	}
	rev, label := opts.commitRange()
	if rev != "" {
		args = append(args, rev)
	}
	res.Range = label
	if opts.Path != "" {
		args = append(args, "--", opts.Path)
	}

	out, truncated, err := r.run(ctx, dir, args...)
	if err != nil {
		return err
	}
	res.Commits = parseCommits(out)
	res.Truncated = truncated || len(res.Commits) >= opts.commitLimit()
	return nil
}

// isDir reports whether a path is a directory in the working tree. It is a
// disk check rather than a git one because it decides whether --follow is
// safe, and a path that no longer exists is exactly the case --follow is for.
func (r *Repos) isDir(ctx context.Context, dir, path string) bool {
	info, err := statPath(dir, path)
	return err == nil && info.IsDir()
}

// --- show ---

func (r *Repos) show(ctx context.Context, dir string, opts Options, res *Result) error {
	rev := or(opts.Rev, "HEAD")
	res.Rev = rev

	out, _, err := r.run(ctx, dir, "log", "-1", "--no-color", "--date=iso-strict", commitFormat, "--shortstat", rev)
	if err != nil {
		return err
	}
	commits := parseCommits(out)
	if len(commits) == 0 {
		return fmt.Errorf("no commit %q", rev)
	}
	res.Commit = &commits[0]

	// -m splits a merge into its per-parent diffs, so that showing a merge
	// reports the files it touched instead of the empty diff git prints by
	// default and leaves the caller believing nothing changed.
	diffArgs := []string{"show", "--no-color", "--format=", "--find-renames"}
	if res.Commit.Merge() {
		diffArgs = append(diffArgs, "-m", "--first-parent")
	}
	res.Files, err = r.changes(ctx, dir, diffArgs, rev, opts.Path)
	if err != nil {
		return err
	}
	if opts.Patch {
		patchArgs := append(append([]string{}, diffArgs...), "--patch", rev)
		if opts.Path != "" {
			patchArgs = append(patchArgs, "--", opts.Path)
		}
		patch, truncated, err := r.run(ctx, dir, patchArgs...)
		if err != nil {
			return err
		}
		res.Patch, res.Truncated = clipPatch(patch, truncated)
	}
	return nil
}

// --- diff ---

func (r *Repos) diff(ctx context.Context, dir string, opts Options, res *Result) error {
	if opts.From == "" {
		return fmt.Errorf("diff needs `from`; give two revisions, e.g. from=HEAD~5 to=HEAD")
	}
	to := or(opts.To, "HEAD")
	res.Range = opts.From + " -> " + to

	base := []string{"diff", "--no-color", "--find-renames"}
	files, err := r.changes(ctx, dir, base, opts.From, opts.Path, to)
	if err != nil {
		return err
	}
	res.Files = files

	if opts.Patch {
		args := append(append([]string{}, base...), "--patch", opts.From, to)
		if opts.Path != "" {
			args = append(args, "--", opts.Path)
		}
		patch, truncated, err := r.run(ctx, dir, args...)
		if err != nil {
			return err
		}
		res.Patch, res.Truncated = clipPatch(patch, truncated)
	}
	return nil
}

func clipPatch(patch string, truncated bool) (string, bool) {
	if len(patch) > MaxPatchBytes {
		return patch[:MaxPatchBytes], true
	}
	return patch, truncated
}

// changes runs the same diff twice -- once for the status letters, once for
// the line counts -- and zips the two together.
//
// One call cannot give both: --name-status and --numstat are mutually
// exclusive, and --numstat alone cannot tell an addition from a modification.
// Parsed with -z the two outputs are in the same order, and --name-status is
// what says which records carry two paths because they are renames.
func (r *Repos) changes(ctx context.Context, dir string, base []string, rev, path string, extra ...string) ([]FileChange, error) {
	build := func(mode string) []string {
		args := append(append([]string{}, base...), mode, "-z", rev)
		args = append(args, extra...)
		if path != "" {
			args = append(args, "--", path)
		}
		return args
	}

	statusOut, _, err := r.run(ctx, dir, build("--name-status")...)
	if err != nil {
		return nil, err
	}
	files := parseNameStatus(statusOut)
	if len(files) == 0 {
		return []FileChange{}, nil
	}

	numOut, _, err := r.run(ctx, dir, build("--numstat")...)
	if err != nil {
		// The status list is the useful half; losing the counts is not worth
		// failing the whole call over.
		return files, nil
	}
	applyNumstat(files, numOut)
	return files, nil
}

// parseNameStatus reads `--name-status -z`. Records are NUL-terminated
// fields: a status letter, then one path, or two when the status is a rename
// or a copy.
func parseNameStatus(out string) []FileChange {
	fields := splitNUL(out)
	var files []FileChange
	for i := 0; i < len(fields); {
		status := fields[i]
		i++
		if status == "" || i >= len(fields) {
			break
		}
		c := FileChange{Status: status}
		if status[0] == 'R' || status[0] == 'C' {
			if i+1 >= len(fields) {
				break
			}
			c.OldPath, c.Path = fields[i], fields[i+1]
			i += 2
		} else {
			c.Path = fields[i]
			i++
		}
		files = append(files, c)
	}
	return files
}

// applyNumstat overlays `--numstat -z` counts onto an already-parsed status
// list, using it to know which records carry two paths.
func applyNumstat(files []FileChange, out string) {
	fields := splitNUL(out)
	idx := 0
	for i := 0; i < len(fields) && idx < len(files); {
		counts := strings.Split(fields[i], "\t")
		if len(counts) < 2 {
			break
		}
		i++
		f := &files[idx]
		if f.Status != "" && (f.Status[0] == 'R' || f.Status[0] == 'C') {
			i += 2
		} else {
			i++
		}
		// git writes "-" for a binary file, which is not zero changes.
		if counts[0] == "-" {
			f.Binary = true
		} else {
			f.Insertions, _ = strconv.Atoi(counts[0])
			f.Deletions, _ = strconv.Atoi(counts[1])
		}
		idx++
	}
}

func splitNUL(s string) []string {
	parts := strings.Split(s, "\x00")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// --- blame ---

func (r *Repos) blame(ctx context.Context, dir string, opts Options, res *Result) error {
	if opts.Path == "" {
		return fmt.Errorf("blame needs a path, e.g. internal/server/server.go")
	}
	rev := or(opts.Rev, "HEAD")
	res.Rev = rev

	start, end, err := parseLines(opts.Lines)
	if err != nil {
		return err
	}
	if end == 0 {
		// Blaming a whole file of any size fills a context window with the
		// same three commits. A window is the useful unit, so default to one
		// and say so.
		end = start + DefaultBlameLines - 1
		res.Note = fmt.Sprintf("showing lines %d-%d; pass lines (e.g. \"400-700\") for a different window", start, end)
	}
	if end-start+1 > MaxBlameLines {
		end = start + MaxBlameLines - 1
	}

	out, truncated, err := r.run(ctx, dir,
		"blame", "--line-porcelain", "-L", fmt.Sprintf("%d,%d", start, end), rev, "--", opts.Path)
	if err != nil {
		return err
	}
	res.Blame = parseBlame(out)
	res.Truncated = truncated
	return nil
}

// parseLines reads "120-180", "120,180" or "120".
func parseLines(s string) (int, int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 1, 0, nil
	}
	sep := strings.IndexAny(s, "-,:")
	if sep < 0 {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return 0, 0, fmt.Errorf("lines %q should be a range like \"120-180\"", s)
		}
		return n, 0, nil
	}
	start, err1 := strconv.Atoi(strings.TrimSpace(s[:sep]))
	end, err2 := strconv.Atoi(strings.TrimSpace(s[sep+1:]))
	if err1 != nil || err2 != nil || start < 1 || end < start {
		return 0, 0, fmt.Errorf("lines %q should be a range like \"120-180\"", s)
	}
	return start, end, nil
}

var blameHeaderRe = regexp.MustCompile(`^([0-9a-f]{7,40}) \d+ (\d+)`)

// parseBlame reads --line-porcelain: a header line per line of the file, then
// key/value lines, then the content prefixed with a tab.
func parseBlame(out string) []BlameLine {
	var lines []BlameLine
	var cur *BlameLine
	for _, raw := range strings.Split(out, "\n") {
		if strings.HasPrefix(raw, "\t") {
			if cur != nil {
				cur.Text = clip(strings.TrimRight(raw[1:], "\r"))
				lines = append(lines, *cur)
				cur = nil
			}
			continue
		}
		if m := blameHeaderRe.FindStringSubmatch(raw); m != nil {
			n, _ := strconv.Atoi(m[2])
			short := m[1]
			if len(short) > 8 {
				short = short[:8]
			}
			cur = &BlameLine{Line: n, Hash: m[1], Short: short}
			continue
		}
		if cur == nil {
			continue
		}
		key, value, _ := strings.Cut(raw, " ")
		switch key {
		case "author":
			cur.Author = value
		case "summary":
			cur.Summary = value
		case "author-time":
			if sec, err := strconv.ParseInt(value, 10, 64); err == nil {
				cur.Date = time.Unix(sec, 0).UTC().Format("2006-01-02")
			}
		}
	}
	return lines
}

func clip(s string) string {
	if len(s) <= MaxLineLen {
		return s
	}
	return s[:MaxLineLen] + "… (truncated)"
}

// --- search ---

// search is git's pickaxe: it finds the commits whose diff touched a string,
// which is how you answer "when did this appear" and "who deleted it". grep
// searches the tree as it is now; this searches every version there ever was.
func (r *Repos) search(ctx context.Context, dir string, opts Options, res *Result) error {
	if strings.TrimSpace(opts.Query) == "" {
		return fmt.Errorf("search needs a query -- the string, or with regex=true the pattern, to look for in the diffs")
	}
	args := r.logArgs(opts)
	if opts.Regex {
		args = append(args, "-G"+opts.Query)
	} else {
		args = append(args, "-S"+opts.Query)
	}
	rev, label := opts.commitRange()
	if rev != "" {
		args = append(args, rev)
	}
	res.Range = label
	if opts.Path != "" {
		args = append(args, "--", opts.Path)
	}

	out, truncated, err := r.run(ctx, dir, args...)
	if err != nil {
		return err
	}
	res.Commits = parseCommits(out)
	res.Truncated = truncated || len(res.Commits) >= opts.commitLimit()
	if len(res.Commits) == 0 {
		res.Note = "no commit changed the number of occurrences of that string"
		if opts.Regex {
			res.Note = "no commit had a diff hunk matching that pattern"
		}
	}
	return nil
}

// --- file ---

// file reads a path as it was at a revision, which is the one thing the read
// tool cannot do: it sees only the checked-out tip.
func (r *Repos) file(ctx context.Context, dir string, opts Options, res *Result) error {
	if opts.Path == "" {
		return fmt.Errorf("file needs a path, e.g. internal/server/server.go")
	}
	rev := or(opts.Rev, "HEAD")
	res.Rev = rev

	out, truncated, err := r.run(ctx, dir, "show", rev+":"+opts.Path)
	if err != nil {
		return err
	}
	if strings.IndexByte(out, 0) >= 0 {
		return fmt.Errorf("%s looks like a binary file at %s", opts.Path, rev)
	}
	if len(out) > MaxFileBytes {
		out = out[:MaxFileBytes]
		truncated = true
	}
	res.Content = out
	res.Truncated = truncated
	return nil
}

// --- branches and tags ---

var refFormat = "--format=" + strings.Join([]string{
	"%(refname:short)", "%(objectname)", "%(objectname:short)",
	"%(creatordate:iso-strict)", "%(contents:subject)", "%(HEAD)",
}, fieldSep)

func (r *Repos) refs(ctx context.Context, dir string, opts Options, res *Result, tags bool) error {
	args := []string{"for-each-ref", refFormat, "--sort=-creatordate", "--count=" + strconv.Itoa(refLimit(opts))}
	if tags {
		args = append(args, "refs/tags")
	} else {
		args = append(args, "refs/heads", "refs/remotes")
	}

	out, truncated, err := r.run(ctx, dir, args...)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, fieldSep)
		if len(f) < 5 {
			continue
		}
		ref := Ref{Name: f[0], Hash: f[1], Short: f[2], Date: f[3], Subject: f[4], Type: "branch"}
		switch {
		case tags:
			ref.Type = "tag"
		case strings.HasPrefix(f[0], "origin/") || strings.Contains(f[0], "/"):
			ref.Type = "remote"
		}
		if len(f) > 5 {
			ref.Head = strings.TrimSpace(f[5]) == "*"
		}
		res.Refs = append(res.Refs, ref)
	}
	res.Truncated = truncated || len(res.Refs) >= refLimit(opts)

	// Worth saying out loud: drover clones with --single-branch, so a
	// one-branch answer here means the mirror was narrowed on purpose, not
	// that the remote has one branch.
	if !tags && len(res.Refs) <= 2 {
		res.Note = "drover clones a single branch, so only the branch the Repository names is present locally"
	}
	return nil
}

func refLimit(opts Options) int {
	if opts.Limit > 0 && opts.Limit < MaxRefs {
		return opts.Limit
	}
	return MaxRefs
}

// --- contributors ---

// contributors tallies authors in Go rather than shelling out to shortlog,
// because that way the path, since and until filters mean exactly what they
// mean everywhere else in this tool.
func (r *Repos) contributors(ctx context.Context, dir string, opts Options, res *Result) error {
	args := []string{"log", "--no-color", "--date=short",
		"--format=%an" + fieldSep + "%ae" + fieldSep + "%ad"}
	if opts.Since != "" {
		args = append(args, "--since="+opts.Since)
	}
	if opts.Until != "" {
		args = append(args, "--until="+opts.Until)
	}
	if opts.Merges == "exclude" {
		args = append(args, "--no-merges")
	}
	if rev, label := opts.commitRange(); rev != "" {
		args = append(args, rev)
		res.Range = label
	}
	if opts.Path != "" {
		args = append(args, "--", opts.Path)
	}

	out, truncated, err := r.run(ctx, dir, args...)
	if err != nil {
		return err
	}

	byEmail := map[string]*Author{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(line), fieldSep)
		if len(f) < 3 {
			continue
		}
		a, ok := byEmail[f[1]]
		if !ok {
			a = &Author{Name: f[0], Email: f[1], First: f[2], Last: f[2]}
			byEmail[f[1]] = a
		}
		a.Commits++
		// The log walks newest first, so every later row is older.
		a.First = f[2]
	}

	for _, a := range byEmail {
		res.Authors = append(res.Authors, *a)
	}
	sort.Slice(res.Authors, func(i, j int) bool {
		if res.Authors[i].Commits != res.Authors[j].Commits {
			return res.Authors[i].Commits > res.Authors[j].Commits
		}
		return res.Authors[i].Email < res.Authors[j].Email
	})
	limit := MaxAuthors
	if opts.Limit > 0 && opts.Limit < limit {
		limit = opts.Limit
	}
	if len(res.Authors) > limit {
		res.Authors = res.Authors[:limit]
		truncated = true
	}
	res.Truncated = truncated
	return nil
}

// --- status ---

func (r *Repos) status(ctx context.Context, dir string, opts Options, res *Result) error {
	st := &Status{Repository: opts.Repository}
	st.Branch, _ = r.branch(ctx, dir)
	res.Branch = st.Branch

	if url, _, err := r.run(ctx, dir, "remote", "get-url", "origin"); err == nil {
		st.URL = strings.TrimSpace(url)
	}
	head, _, err := r.run(ctx, dir, "log", "-1", "--no-color", "--date=iso-strict", commitFormat, "--shortstat", "HEAD")
	if err != nil {
		return err
	}
	if commits := parseCommits(head); len(commits) > 0 {
		st.Head = commits[0]
	}
	if count, _, err := r.run(ctx, dir, "rev-list", "--count", "HEAD"); err == nil {
		st.Commits, _ = strconv.Atoi(strings.TrimSpace(count))
	}
	// The root commit is the cheap way to date a repository: walking to the
	// oldest commit with --reverse reads the whole history to print one line.
	if first, _, err := r.run(ctx, dir, "log", "--max-parents=0", "--format=%ad", "--date=short", "HEAD"); err == nil {
		lines := strings.Fields(first)
		if len(lines) > 0 {
			st.FirstDate = lines[len(lines)-1]
		}
	}

	// A dirty mirror means something wrote into a checkout that reconcile
	// resets on every sync, so the change is about to be lost. Worth
	// reporting rather than hiding.
	if porcelain, _, err := r.run(ctx, dir, "status", "--porcelain"); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(porcelain), "\n") {
			if strings.TrimSpace(line) != "" {
				st.Dirty = append(st.Dirty, strings.TrimSpace(line))
			}
		}
		st.Clean = len(st.Dirty) == 0
		if len(st.Dirty) > 20 {
			st.Dirty = st.Dirty[:20]
			res.Truncated = true
		}
	}
	if info, err := statPath(dir, ".git/FETCH_HEAD"); err == nil {
		st.LastFetch = info.ModTime().UTC().Format(time.RFC3339)
	}

	res.Status = st
	return nil
}
