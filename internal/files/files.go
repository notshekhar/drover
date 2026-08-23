// Package files implements the four filesystem tools drover hands to agents:
// ls, read, grep and find, all scoped to the repository checkouts.
//
// Everything here is jailed to $DROVER_DATA/repos. The jail is enforced by
// resolving symlinks and re-checking the result, not by rejecting ".." --
// a symlink inside a checked-out repository pointing at /etc/passwd contains
// no dots at all, and a repository is by definition content drover did not
// write.
package files

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// Limits keep a tool result inside a context window and keep one careless
// call from walking a million files.
const (
	MaxReadBytes   = 256 << 10 // per read
	MaxLineLen     = 2000      // per line, before truncation
	MaxGrepResults = 200
	MaxFindResults = 500
	MaxListEntries = 1000
	binarySniff    = 8 << 10

	// keepBufferBytes caps the read buffer a grep worker holds on to between
	// files. Without it, one 16MB blob in a repository would leave every
	// worker sitting on 16MB for the life of the process.
	keepBufferBytes = 1 << 20
)

// maxGrepFileBytes is how large a file grep will read in one piece. Anything
// bigger falls back to a streaming scan, so nothing is silently skipped -- it
// just does not get the fast path. A var so a test can reach the slow path
// without writing sixteen megabytes to do it.
var maxGrepFileBytes int64 = 16 << 20

// skipDirs are directories a search walks past.
//
// This is the difference between grep being useful on a JavaScript
// repository and being useless on one. A checkout of a real project is
// overwhelmingly dependencies and build output -- on one measured repository,
// 143,690 of 147,852 files were node_modules -- and searching them is worse
// than slow: the result cap fills up with vendored copies and minified
// bundles before the walk ever reaches the source someone asked about.
//
// The base of a search is never skipped, so naming one of these explicitly
// (grep with path=repo/node_modules/foo) still searches it. The list is for
// what a search walks *into*, not what it is pointed at.
var skipDirs = map[string]bool{
	".git":          true,
	"node_modules":  true,
	"vendor":        true,
	"dist":          true,
	"build":         true,
	"target":        true,
	"out":           true,
	".next":         true,
	".nuxt":         true,
	".svelte-kit":   true,
	".venv":         true,
	"venv":          true,
	"__pycache__":   true,
	".mypy_cache":   true,
	".pytest_cache": true,
	".gradle":       true,
	".tox":          true,
	".terraform":    true,
	"Pods":          true,
	".cargo":        true,
	".turbo":        true,
	".cache":        true,
}

// Root is the directory holding every checkout.
type Root struct {
	Dir string
}

// New returns a Root over the data directory's repos folder.
//
// The root is canonicalised once here. Containment is checked against the
// resolved form, and on macOS the data directory frequently sits under /var,
// which is itself a symlink to /private/var -- comparing a resolved path
// against an unresolved root would then reject every legitimate file.
func New(dataDir string) *Root {
	return &Root{Dir: canonicalDir(filepath.Join(dataDir, "repos"))}
}

// canonicalDir resolves a path even when it does not exist yet, by resolving
// the nearest ancestor that does and re-appending the rest.
//
// EvalSymlinks fails outright on a missing path, and the repos directory is
// routinely missing before the first clone. Silently leaving the root
// unresolved would then make every containment check fail later, once the
// directory exists and resolves to a different prefix.
func canonicalDir(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path
	}
	return filepath.Join(canonicalDir(parent), filepath.Base(path))
}

// ErrOutsideRoot is returned when a path would escape the checkouts.
var ErrOutsideRoot = errors.New("path is outside the repository root")

// resolve turns a caller-supplied relative path into an absolute one that is
// provably inside the root.
//
// Two checks, because they catch different things. The lexical check rejects
// "../.." before touching the disk. The symlink check catches a link inside a
// repository whose target is elsewhere -- the case that matters, since
// repository contents are written by whoever wrote the repository.
func (r *Root) resolve(rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: give a path relative to the repository root, like api/src/main.go", ErrOutsideRoot)
	}
	// Reject ".." outright rather than relying on Clean to neutralise it.
	// Clean would fold "../../etc/passwd" into "etc/passwd", which is safely
	// inside the root but then fails as "does not exist" -- an answer that
	// tells the caller nothing about why.
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if seg == ".." {
			return "", fmt.Errorf("%w: %q walks upwards", ErrOutsideRoot, rel)
		}
	}
	clean := filepath.Clean("/" + strings.TrimPrefix(rel, "/"))
	abs := filepath.Join(r.Dir, clean)

	if !within(abs, r.Dir) {
		return "", ErrOutsideRoot
	}

	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// The path may simply not exist; let the caller's own operation
		// report that, but make sure its parent is still inside the root.
		if os.IsNotExist(err) {
			parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
			if err != nil || within(parent, r.Dir) {
				return abs, nil
			}
			return "", ErrOutsideRoot
		}
		return "", err
	}
	if !within(resolved, r.Dir) {
		return "", fmt.Errorf("%w: %s is a link pointing out of the checkouts", ErrOutsideRoot, rel)
	}
	return resolved, nil
}

// Resolve turns a repository-relative path into an absolute one that is
// provably inside the checkouts. It is exported so the language-server layer
// jails paths through exactly this code rather than through a second copy of
// it that could drift.
func (r *Root) Resolve(rel string) (string, error) { return r.resolve(rel) }

// Display turns an absolute path back into the repo-relative form callers use.
func (r *Root) Display(abs string) string { return r.display(abs) }

// resolveDir resolves a search scope and insists it exists. Returning an
// empty result for a path that is not there reads as "no matches", which is a
// different and much more misleading answer than "no such directory".
func (r *Root) resolveDir(rel string) (string, error) {
	abs, err := r.resolve(rel)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", notFound(orRoot(rel), err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is a file, not a directory to search in", r.display(abs))
	}
	return abs, nil
}

func orRoot(rel string) string {
	if strings.TrimSpace(rel) == "" {
		return "the repository root"
	}
	return rel
}

// within reports whether path is dir or sits inside it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))
}

// display turns an absolute path back into the repo-relative form callers use.
func (r *Root) display(abs string) string {
	rel, err := filepath.Rel(r.Dir, abs)
	if err != nil {
		return abs
	}
	return filepath.ToSlash(rel)
}

// --- ls ---

// Entry is one directory entry.
type Entry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Type  string `json:"type"` // file, dir, symlink
	Size  int64  `json:"size,omitempty"`
	Lines int    `json:"-"`
}

// ListResult is what ls returns.
type ListResult struct {
	Path      string  `json:"path"`
	Entries   []Entry `json:"entries"`
	Truncated bool    `json:"truncated,omitempty"`
}

// List reads one directory. An empty path lists the repositories themselves,
// which is how an agent discovers what drover holds.
func (r *Root) List(rel string) (*ListResult, error) {
	abs, err := r.resolve(rel)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, notFound(rel, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is a file, not a directory; use read", r.display(abs))
	}

	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}

	out := &ListResult{Path: r.display(abs)}
	if out.Path == "." {
		out.Path = ""
	}
	for _, e := range entries {
		// .git is drover's bookkeeping and pure noise to an agent reading
		// source. Everything else, including .github and .gitignore, stays.
		if e.Name() == ".git" {
			continue
		}
		if len(out.Entries) >= MaxListEntries {
			out.Truncated = true
			break
		}

		entry := Entry{Name: e.Name(), Path: pathJoin(out.Path, e.Name())}
		switch {
		case e.Type()&os.ModeSymlink != 0:
			entry.Type = "symlink"
		case e.IsDir():
			entry.Type = "dir"
		default:
			entry.Type = "file"
			if info, err := e.Info(); err == nil {
				entry.Size = info.Size()
			}
		}
		out.Entries = append(out.Entries, entry)
	}

	sort.Slice(out.Entries, func(i, j int) bool {
		a, b := out.Entries[i], out.Entries[j]
		if (a.Type == "dir") != (b.Type == "dir") {
			return a.Type == "dir"
		}
		return a.Name < b.Name
	})
	return out, nil
}

func pathJoin(base, name string) string {
	if base == "" || base == "." {
		return name
	}
	return base + "/" + name
}

// --- read ---

// ReadResult is what read returns.
type ReadResult struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	StartLine  int    `json:"startLine"`
	EndLine    int    `json:"endLine"`
	TotalLines int    `json:"totalLines"`
	Truncated  bool   `json:"truncated,omitempty"`
}

// Read returns part of a file. offset is a 1-based line number; limit is a
// line count. Both zero means the whole file, up to the byte cap.
func (r *Root) Read(rel string, offset, limit int) (*ReadResult, error) {
	abs, err := r.resolve(rel)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, notFound(rel, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory; use ls", r.display(abs))
	}

	f, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if binary, err := looksBinary(f); err != nil {
		return nil, err
	} else if binary {
		return nil, fmt.Errorf("%s looks like a binary file", r.display(abs))
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	if offset < 1 {
		offset = 1
	}

	var b strings.Builder
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 10<<20)

	line, start, end, taken := 0, 0, 0, 0
	truncated := false
	for sc.Scan() {
		line++
		if line < offset {
			continue
		}
		if limit > 0 && taken >= limit {
			truncated = true
			break
		}
		if b.Len() >= MaxReadBytes {
			truncated = true
			break
		}
		if start == 0 {
			start = line
		}
		end = line
		taken++
		b.WriteString(clipLine(sc.Text()))
		b.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", r.display(abs), err)
	}

	// Keep counting past the window, so the caller is told how much file it
	// did not get rather than being left to guess from the line numbers.
	//
	// Counting newlines in bulk rather than running the Scanner to EOF: the
	// tail is not being read, only measured, and splitting twenty megabytes
	// into lines to learn how many there are is work with no output.
	// Counted from the start rather than from where the Scanner left off:
	// the Scanner reads ahead, so its file offset is somewhere past the last
	// line it handed back, and there is no cheap way to ask it where.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	total, err := countLines(f)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", r.display(abs), err)
	}
	if total > end {
		truncated = true
	}

	return &ReadResult{
		Path:       r.display(abs),
		Content:    b.String(),
		StartLine:  start,
		EndLine:    end,
		TotalLines: total,
		Truncated:  truncated,
	}, nil
}

// countLines counts the lines in f from its current offset, in bulk.
//
// The last line counts even without a trailing newline, which is what the
// Scanner this replaced did.
func countLines(f *os.File) (int, error) {
	buf := make([]byte, 64<<10)
	n := 0
	empty := true
	endsWithNewline := true
	for {
		read, err := f.Read(buf)
		if read > 0 {
			empty = false
			n += bytes.Count(buf[:read], []byte{'\n'})
			endsWithNewline = buf[read-1] == '\n'
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	if !empty && !endsWithNewline {
		n++
	}
	return n, nil
}

func clipLine(s string) string {
	if len(s) <= MaxLineLen {
		return s
	}
	return s[:MaxLineLen] + "… (line truncated)"
}

// looksBinary sniffs for a NUL byte, which no text file has.
func looksBinary(f *os.File) (bool, error) {
	buf := make([]byte, binarySniff)
	n, err := f.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return bytes.IndexByte(buf[:n], 0) >= 0, nil
}

// --- grep ---

// Match is one grep hit.
type Match struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// GrepResult is what grep returns.
type GrepResult struct {
	Matches   []Match `json:"matches"`
	Files     int     `json:"filesSearched"`
	Truncated bool    `json:"truncated,omitempty"`
}

// GrepOptions narrow a search.
type GrepOptions struct {
	Path          string // subtree to search, relative to the root
	Include       string // glob on the file name, e.g. *.go
	CaseSensitive bool
	MaxResults    int
}

// Grep searches file contents for a regular expression.
//
// Every matching file is searched and the results are sorted before the cap
// is applied, rather than stopping at the first N hits in walk order. That
// costs a full walk, but it is what makes the answer stable: the same query
// against the same checkout returns the same 200 lines, instead of whichever
// 200 the directory order happened to reach first.
func (r *Root) Grep(ctx context.Context, pattern string, opts GrepOptions) (*GrepResult, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, errors.New("the pattern is empty")
	}
	expr := pattern
	if !opts.CaseSensitive {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, fmt.Errorf("pattern is not a valid regular expression: %w", err)
	}
	probe, err := wholeFileProbe(expr, pattern)
	if err != nil {
		return nil, fmt.Errorf("pattern is not a valid regular expression: %w", err)
	}

	base, err := r.resolveDir(opts.Path)
	if err != nil {
		return nil, err
	}
	max := opts.MaxResults
	if max <= 0 || max > MaxGrepResults {
		max = MaxGrepResults
	}

	// Collect the candidate paths first. The walk is one directory-order pass
	// and cheap; reading and matching the files is the expensive half, and
	// that is what gets spread across cores.
	var paths []string
	var globErr error
	err = r.walk(ctx, base, func(abs string, d os.DirEntry) error {
		if opts.Include != "" {
			ok, err := filepath.Match(opts.Include, d.Name())
			if err != nil {
				globErr = fmt.Errorf("include %q is not a valid glob: %w", opts.Include, err)
				return errStop
			}
			if !ok {
				return nil
			}
		}
		paths = append(paths, abs)
		return nil
	})
	if globErr != nil {
		return nil, globErr
	}
	if err != nil && !errors.Is(err, errStop) {
		return nil, err
	}

	matches := r.grepAll(ctx, paths, re, probe)
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Path != matches[j].Path {
			return matches[i].Path < matches[j].Path
		}
		return matches[i].Line < matches[j].Line
	})

	out := &GrepResult{Matches: matches, Files: len(paths)}
	if out.Matches == nil {
		out.Matches = []Match{}
	}
	if len(out.Matches) > max {
		out.Matches = out.Matches[:max]
		out.Truncated = true
	}
	return out, nil
}

// wholeFileProbe builds the regexp used to reject a file in one pass, before
// it is split into lines.
//
// The probe is the same pattern in multi-line mode, so that "a line matches"
// and "the file matches" mean the same thing and the probe can never hide a
// hit. Two exceptions get no probe at all, because for them the two are not
// the same thing:
//
//   - a pattern containing "$". Line scanning sees a line with its trailing
//     "\r" already stripped, so `foo$` matches on a CRLF file; the probe,
//     looking at the raw bytes, would see the "\r" and miss it.
//   - a pattern containing "\r", for the same reason in reverse.
//
// A probe that matches when no line does is harmless -- the line scan simply
// finds nothing. A probe that fails when a line would have matched is a
// wrong answer, so those two cases skip it.
func wholeFileProbe(expr, raw string) (*regexp.Regexp, error) {
	if strings.Contains(raw, "$") || strings.Contains(raw, `\r`) {
		return nil, nil
	}
	return regexp.Compile("(?m)" + expr)
}

// grepAll searches every path, one worker per core.
func (r *Root) grepAll(ctx context.Context, paths []string, re, probe *regexp.Regexp) []Match {
	workers := runtime.NumCPU()
	if workers > len(paths) {
		workers = len(paths)
	}
	if workers < 1 {
		return nil
	}

	// Each worker keeps its own slice and they are joined at the end, so the
	// hot loop never touches a shared lock.
	found := make([][]Match, workers)
	next := make(chan string, workers*4)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			var buf []byte
			for abs := range next {
				found[slot] = r.grepFile(abs, re, probe, &buf, found[slot])
			}
		}(i)
	}
	for _, p := range paths {
		select {
		case next <- p:
		case <-ctx.Done():
			// Stop feeding; the workers drain what is queued and exit.
			close(next)
			wg.Wait()
			return nil
		}
	}
	close(next)
	wg.Wait()

	total := 0
	for _, f := range found {
		total += len(f)
	}
	out := make([]Match, 0, total)
	for _, f := range found {
		out = append(out, f...)
	}
	return out
}

// grepFile appends this file's matches to out.
//
// The file is read in one piece into a buffer the worker reuses, and lines
// are matched as byte slices. The obvious version -- bufio.Scanner plus
// sc.Text() -- allocates a string for every line in the checkout, which on a
// large repository is tens of millions of allocations and gigabytes of
// garbage for a search that returns nothing.
func (r *Root) grepFile(abs string, re, probe *regexp.Regexp, buf *[]byte, out []Match) []Match {
	f, err := os.Open(abs)
	if err != nil {
		return out // unreadable file is not a search failure
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return out
	}
	if info.Size() > maxGrepFileBytes {
		return r.grepStream(abs, f, re, out)
	}

	if cap(*buf) < int(info.Size()) {
		*buf = make([]byte, info.Size())
	}
	data := (*buf)[:info.Size()]
	n, err := io.ReadFull(f, data)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return out
	}
	data = data[:n]
	if cap(*buf) > keepBufferBytes {
		*buf = nil
	}

	if isBinary(data) {
		return out
	}
	// One pass to reject the whole file. Most files in a checkout contain the
	// pattern nowhere, and this is far cheaper than running the matcher once
	// per line to learn that.
	if probe != nil && !probe.Match(data) {
		return out
	}

	line := 0
	for len(data) > 0 {
		line++
		row := data
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			row, data = data[:i], data[i+1:]
		} else {
			data = nil
		}
		// bufio.Scanner drops a trailing "\r" before the caller ever sees the
		// line, so matching has to as well or a CRLF checkout answers
		// differently from an LF one.
		row = bytes.TrimSuffix(row, []byte("\r"))
		if !re.Match(row) {
			continue
		}
		out = append(out, Match{Path: r.display(abs), Line: line, Text: clipLine(string(row))})
	}
	return out
}

// grepStream is the fallback for a file too large to hold in memory. It is
// the streaming scan, kept for exactly that case rather than deleted, so a
// giant file is searched slowly instead of not at all.
func (r *Root) grepStream(abs string, f *os.File, re *regexp.Regexp, out []Match) []Match {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return out
	}
	head := make([]byte, binarySniff)
	n, err := f.Read(head)
	if err != nil && !errors.Is(err, io.EOF) {
		return out
	}
	if isBinary(head[:n]) {
		return out
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return out
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 10<<20)
	line := 0
	for sc.Scan() {
		line++
		if !re.Match(sc.Bytes()) {
			continue
		}
		out = append(out, Match{Path: r.display(abs), Line: line, Text: clipLine(sc.Text())})
	}
	return out
}

// isBinary sniffs for a NUL byte, which no text file has.
func isBinary(data []byte) bool {
	if len(data) > binarySniff {
		data = data[:binarySniff]
	}
	return bytes.IndexByte(data, 0) >= 0
}

// --- find ---

// FindResult is what find returns.
type FindResult struct {
	Paths     []string `json:"paths"`
	Truncated bool     `json:"truncated,omitempty"`
}

// Find matches paths by glob. A pattern with no slash matches the file name;
// one with a slash matches the whole repo-relative path, so both `*.go` and
// `api/internal/*.go` do what they look like.
func (r *Root) Find(ctx context.Context, pattern, path string, maxResults int) (*FindResult, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, errors.New("the pattern is empty")
	}
	if _, err := filepath.Match(pattern, "probe"); err != nil {
		return nil, fmt.Errorf("pattern %q is not a valid glob: %w", pattern, err)
	}

	base, err := r.resolveDir(path)
	if err != nil {
		return nil, err
	}
	max := maxResults
	if max <= 0 || max > MaxFindResults {
		max = MaxFindResults
	}
	matchWholePath := strings.Contains(pattern, "/")

	out := &FindResult{Paths: []string{}}
	err = r.walk(ctx, base, func(abs string, d os.DirEntry) error {
		subject := d.Name()
		if matchWholePath {
			subject = r.display(abs)
		}
		ok, err := filepath.Match(pattern, subject)
		if err != nil || !ok {
			return nil
		}
		if len(out.Paths) >= max {
			return errStop
		}
		out.Paths = append(out.Paths, r.display(abs))
		return nil
	})
	if err != nil && !errors.Is(err, errStop) {
		return nil, err
	}
	if errors.Is(err, errStop) {
		out.Truncated = true
	}
	sort.Strings(out.Paths)
	return out, nil
}

// --- walking ---

var errStop = errors.New("enough")

// walk visits every regular file under base, skipping dependency and build
// directories and never following a symlink out of the root.
//
// base is exempt from the skip list. Someone who points a search at
// node_modules meant it; the list is only about what a walk wanders into on
// its way somewhere else.
//
// The context is checked per entry rather than per directory: a search across
// every checkout is one long CPU burn with no natural pause in it, and a
// caller that has hung up should stop paying for it within one stat, not at
// the next directory boundary.
func (r *Root) walk(ctx context.Context, base string, fn func(abs string, d os.DirEntry) error) error {
	return filepath.WalkDir(base, func(abs string, d os.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			// An unreadable directory should not abort the whole search.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if abs != base && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		// Do not follow symlinks while walking. Reading one deliberately is
		// still possible through read, where resolve checks the target.
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return fn(abs, d)
	})
}

func notFound(rel string, err error) error {
	if os.IsNotExist(err) {
		return fmt.Errorf("%s does not exist", rel)
	}
	return err
}
