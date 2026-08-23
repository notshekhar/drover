package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/notshekhar/drover/internal/api"
	"github.com/notshekhar/drover/internal/git"
	"github.com/notshekhar/drover/internal/object"
)

// gitTools is the one history tool.
//
// Ten operations behind one `operation` argument rather than ten tools, for
// the same reason api_call is one tool: a tool list is the most expensive
// place to put a catalogue, because every entry is re-sent on every
// tools/list and competes for attention with the eight that were already
// there. The operations belong in an enum.
func (s *Server) gitTools(ctx context.Context) []Tool {
	return []Tool{{
		Name:        "git",
		Description: gitDescription + s.repositoryCatalog(ctx),
		InputSchema: Schema{
			Type:       "object",
			Additional: boolPtr(false),
			Required:   []string{"operation"},
			Properties: map[string]Prop{
				"operation": {
					Type:        "string",
					Description: "Which question to ask. See this tool's description for what each one needs.",
					Enum:        git.Operations,
				},
				"repository": {Type: "string", Description: "Which checkout to ask about, e.g. `api`. Optional when drover holds exactly one."},
				"path":       {Type: "string", Description: "Narrow to one file or directory, e.g. `internal/server/server.go`. The repository-prefixed form the file tools return (`api/internal/server/server.go`) also works. Required for blame and file."},
				"rev":        {Type: "string", Description: "A single revision for show, blame and file: a sha, `HEAD`, `HEAD~3`, a branch or a tag. Defaults to HEAD."},
				"from":       {Type: "string", Description: "Start of a range. Required for diff; on log and search it means \"commits after this one\"."},
				"to":         {Type: "string", Description: "End of a range. Defaults to HEAD."},
				"author":     {Type: "string", Description: "Only commits whose author name or email matches this, case-insensitively."},
				"since":      {Type: "string", Description: "Only commits after this date. Git's own syntax: `2026-01-01`, `3 weeks ago`, `last monday`."},
				"until":      {Type: "string", Description: "Only commits before this date, same syntax as since."},
				"grep":       {Type: "string", Description: "Only commits whose message matches this pattern, case-insensitively."},
				"query":      {Type: "string", Description: "For search: the string whose appearances in the code you want to trace. Required for search."},
				"regex":      {Type: "boolean", Description: "For search: treat query as a regular expression and match it against the diff text, instead of counting occurrences of a fixed string.", Default: false},
				"merges":     {Type: "string", Description: "Leave out merge commits, or show only those.", Enum: []string{"exclude", "only"}},
				"patch":      {Type: "boolean", Description: "For show and diff: include the actual diff text, not just which files changed. Large -- ask for it once you know which commit you want.", Default: false},
				"lines":      {Type: "string", Description: "For blame: which lines, as `120-180`. Defaults to the first 300."},
				"limit":      {Type: "integer", Description: "How many commits, refs or contributors to return. Defaults to 30 commits."},
			},
		},
	}}
}

const gitDescription = "Read the git history of the repositories drover holds. " +
	"grep and read see the tree as it is now; this sees how it got that way -- who changed a line, when a function appeared, what a file used to look like. " +
	"Read-only: it can never write to a checkout or reach the network.\n\n" +
	"Operations:\n" +
	"  log           commits, newest first. Narrow with path, author, since, until, grep, merges, limit. Following one file's path also follows it through renames.\n" +
	"  show          one commit in full: message, and which files it touched. Add patch=true for the diff itself. rev selects the commit.\n" +
	"  diff          what changed between two revisions. from is required, to defaults to HEAD. Add patch=true for the diff text.\n" +
	"  blame         who last touched each line of a file. Needs path; lines takes a range like \"120-180\".\n" +
	"  search        the commits whose diff added or removed a string -- this is how you find when something was introduced or deleted. Needs query; regex=true matches a pattern against the diff instead.\n" +
	"  file          a file's contents as of a revision. This is the one thing read cannot do, since read only sees the checked-out tip.\n" +
	"  branches      local and remote-tracking branches with their tips.\n" +
	"  tags          tags, newest first.\n" +
	"  contributors  who commits and how often, narrowable by path, since and until.\n" +
	"  status        the checkout itself: branch, HEAD, how many commits, when it last synced.\n\n" +
	"A good sequence for \"why is this code like this\": grep to find the line, then blame that file, then show the commit blame names.\n\n"

// repositoryCatalog lists what can actually be asked about, the way sql_query
// lists its connections.
//
// It reports the applied Repository objects rather than the directories on
// disk, so a repository whose clone failed is named along with its error --
// otherwise it is simply missing from the list, and a model that was told it
// exists keeps asking for it.
func (s *Server) repositoryCatalog(ctx context.Context) string {
	items, err := s.Backend.List(ctx, object.KindRepository)
	if err != nil || len(items) == 0 {
		return "No repositories have been applied yet, so there is no history to read."
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })

	var b strings.Builder
	b.WriteString("Available repository values:\n")
	for _, v := range items {
		fmt.Fprintf(&b, "  - %s", v.Name)
		if v.Branch != "" {
			fmt.Fprintf(&b, " (branch %s)", v.Branch)
		}
		if v.Status != "" && v.Status != "ready" {
			fmt.Fprintf(&b, " -- %s", v.Status)
			if v.Error != "" {
				fmt.Fprintf(&b, ": %s", firstLine(v.Error))
			}
		}
		b.WriteByte('\n')
	}
	b.WriteString("\ndrover clones a single branch, so history outside the branch each Repository names is not present locally.")
	return b.String()
}

// --- the call ---

func (s *Server) toolGit(ctx context.Context, raw json.RawMessage) *CallResult {
	var args api.GitRequest
	if err := decodeArgs(raw, &args); err != nil {
		return toolError("invalid arguments: %v", err)
	}
	if strings.TrimSpace(args.Operation) == "" {
		return toolError("git needs an operation: one of %s", strings.Join(git.Operations, ", "))
	}
	res, err := s.Backend.Git(ctx, args)
	if err != nil {
		return toolError("%v", err)
	}
	return text("%s", renderGit(res))
}

// renderGit turns a result into the text a model reads. Each operation gets
// the shape that suits it: a table for commits, a patch as a patch, blame
// aligned so the runs of one commit are visible down the left.
func renderGit(res *api.GitResponse) string {
	var b strings.Builder
	writeGitHeader(&b, res)

	switch res.Operation {
	case "log", "search":
		writeCommitList(&b, res)
	case "show":
		writeCommit(&b, res)
	case "diff":
		writeDiff(&b, res)
	case "blame":
		writeBlame(&b, res)
	case "file":
		writeFileAtRev(&b, res)
	case "branches", "tags":
		writeRefs(&b, res)
	case "contributors":
		writeAuthors(&b, res)
	case "status":
		writeStatus(&b, res)
	}

	if res.Note != "" {
		fmt.Fprintf(&b, "\n(%s)\n", res.Note)
	}
	if res.Truncated {
		b.WriteString("\n(" + truncationHint(res.Operation) + ")\n")
	}
	return b.String()
}

// truncationHint says what to do about it, which depends on why there was
// more: a commit list stopped at the limit wants a bigger limit, while a
// patch that ran into the byte cap wants a narrower path.
func truncationHint(op string) string {
	switch op {
	case "log", "search", "contributors":
		return "there is more; raise limit, or narrow with path, author or a date range"
	case "show", "diff":
		return "the diff was cut short; narrow it with path"
	case "file":
		return "the file was cut short"
	default:
		return "truncated"
	}
}

func writeGitHeader(b *strings.Builder, res *api.GitResponse) {
	fmt.Fprintf(b, "%s: %s", res.Repository, res.Operation)
	if res.Branch != "" {
		fmt.Fprintf(b, " on %s", res.Branch)
	}
	switch {
	case res.Range != "":
		fmt.Fprintf(b, " (%s)", res.Range)
	case res.Rev != "" && res.Rev != "HEAD":
		fmt.Fprintf(b, " at %s", res.Rev)
	}
	if res.Path != "" {
		fmt.Fprintf(b, " -- %s", res.Path)
	}
	b.WriteString("\n\n")
}

func writeCommitList(b *strings.Builder, res *api.GitResponse) {
	if len(res.Commits) == 0 {
		b.WriteString("No commits match.\n")
		return
	}
	for _, c := range res.Commits {
		fmt.Fprintf(b, "%s  %s  %s\n", c.Short, day(c.Date), c.Subject)
		fmt.Fprintf(b, "          %s", c.Author)
		if c.Files > 0 {
			fmt.Fprintf(b, " · %s", changeSummary(c))
		}
		if len(c.Parents) > 1 {
			b.WriteString(" · merge")
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(b, "\n%d commit(s). Use show with a sha for the detail.\n", len(res.Commits))
}

func writeCommit(b *strings.Builder, res *api.GitResponse) {
	c := res.Commit
	if c == nil {
		b.WriteString("No such commit.\n")
		return
	}
	fmt.Fprintf(b, "commit  %s\n", c.Hash)
	fmt.Fprintf(b, "author  %s <%s>\n", c.Author, c.Email)
	fmt.Fprintf(b, "date    %s\n", c.Date)
	if len(c.Parents) > 0 {
		fmt.Fprintf(b, "parents %s\n", strings.Join(shorten(c.Parents), " "))
	}
	fmt.Fprintf(b, "\n%s\n", c.Subject)
	if c.Body != "" {
		fmt.Fprintf(b, "\n%s\n", c.Body)
	}
	b.WriteString("\n")
	writeFiles(b, res.Files)
	writePatch(b, res.Patch, len(res.Files))
}

func writeDiff(b *strings.Builder, res *api.GitResponse) {
	if len(res.Files) == 0 {
		b.WriteString("Nothing changed between those revisions.\n")
		return
	}
	writeFiles(b, res.Files)
	writePatch(b, res.Patch, len(res.Files))
}

func writeFiles(b *strings.Builder, files []api.GitFileChange) {
	if len(files) == 0 {
		b.WriteString("No files changed.\n")
		return
	}
	var ins, del int
	for _, f := range files {
		name := f.Path
		if f.OldPath != "" {
			name = f.OldPath + " -> " + f.Path
		}
		fmt.Fprintf(b, "  %-2s %s", statusWord(f.Status), name)
		switch {
		case f.Binary:
			b.WriteString("  (binary)")
		case f.Insertions > 0 || f.Deletions > 0:
			fmt.Fprintf(b, "  +%d -%d", f.Insertions, f.Deletions)
		}
		b.WriteString("\n")
		ins += f.Insertions
		del += f.Deletions
	}
	fmt.Fprintf(b, "\n%d file(s), +%d -%d\n", len(files), ins, del)
}

func writePatch(b *strings.Builder, patch string, files int) {
	if patch != "" {
		fmt.Fprintf(b, "\n%s\n", strings.TrimRight(patch, "\n"))
		return
	}
	if files > 0 {
		b.WriteString("\n(call again with patch=true for the diff itself)\n")
	}
}

func writeBlame(b *strings.Builder, res *api.GitResponse) {
	if len(res.Blame) == 0 {
		b.WriteString("No lines in that range.\n")
		return
	}
	width := 0
	for _, l := range res.Blame {
		if n := len(l.Author); n > width {
			width = n
		}
	}
	if width > 20 {
		width = 20
	}
	for _, l := range res.Blame {
		author := l.Author
		if len(author) > width {
			author = author[:width]
		}
		fmt.Fprintf(b, "%s %s %-*s %5d| %s\n", l.Short, l.Date, width, author, l.Line, l.Text)
	}
	b.WriteString("\nThe first column is the commit that last touched the line; pass it to show.\n")
}

func writeFileAtRev(b *strings.Builder, res *api.GitResponse) {
	lines := strings.Split(strings.TrimSuffix(res.Content, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		b.WriteString("(the file is empty at that revision)\n")
		return
	}
	for i, l := range lines {
		fmt.Fprintf(b, "%6d\t%s\n", i+1, l)
	}
}

func writeRefs(b *strings.Builder, res *api.GitResponse) {
	if len(res.Refs) == 0 {
		b.WriteString("None.\n")
		return
	}
	for _, r := range res.Refs {
		marker := " "
		if r.Head {
			marker = "*"
		}
		fmt.Fprintf(b, "%s %-40s %s  %s  %s\n", marker, r.Name, r.Short, day(r.Date), r.Subject)
	}
}

func writeAuthors(b *strings.Builder, res *api.GitResponse) {
	if len(res.Authors) == 0 {
		b.WriteString("No commits match.\n")
		return
	}
	for _, a := range res.Authors {
		fmt.Fprintf(b, "%6d  %s <%s>", a.Commits, a.Name, a.Email)
		if a.First != "" {
			fmt.Fprintf(b, "  %s..%s", a.First, a.Last)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(b, "\n%d author(s), by commit count. The dates are their first and last commit here.\n", len(res.Authors))
}

func writeStatus(b *strings.Builder, res *api.GitResponse) {
	st := res.Status
	if st == nil {
		return
	}
	fmt.Fprintf(b, "url        %s\n", st.URL)
	fmt.Fprintf(b, "branch     %s\n", st.Branch)
	fmt.Fprintf(b, "head       %s  %s  %s\n", st.Head.Short, day(st.Head.Date), st.Head.Subject)
	fmt.Fprintf(b, "author     %s\n", st.Head.Author)
	if st.Commits > 0 {
		fmt.Fprintf(b, "history    %d commits", st.Commits)
		if st.FirstDate != "" {
			fmt.Fprintf(b, ", since %s", st.FirstDate)
		}
		b.WriteString("\n")
	}
	if st.LastFetch != "" {
		fmt.Fprintf(b, "last fetch %s\n", st.LastFetch)
	}
	if !st.Clean {
		// drover resets this tree on every sync, so anything uncommitted in it
		// is about to be destroyed. Say so rather than listing it neutrally.
		fmt.Fprintf(b, "\nThe working tree has %d uncommitted change(s). drover resets checkouts to their remote on every sync, so these will be discarded:\n", len(st.Dirty))
		for _, d := range st.Dirty {
			fmt.Fprintf(b, "  %s\n", d)
		}
	}
}

// --- small helpers ---

// statusWord turns git's status letter into something with a rename score
// stripped off: "R100" reads as noise next to "R".
func statusWord(s string) string {
	if s == "" {
		return "?"
	}
	return s[:1]
}

// day trims an ISO timestamp to its date. The time of day is rarely the
// point, and a column of full timestamps costs a lot of tokens to say so.
func day(s string) string {
	if i := strings.IndexAny(s, "T "); i > 0 {
		return s[:i]
	}
	return s
}

func changeSummary(c api.GitCommit) string {
	return fmt.Sprintf("%d file(s) +%d -%d", c.Files, c.Insertions, c.Deletions)
}

func shorten(hashes []string) []string {
	out := make([]string, 0, len(hashes))
	for _, h := range hashes {
		if len(h) > 8 {
			h = h[:8]
		}
		out = append(out, h)
	}
	return out
}
